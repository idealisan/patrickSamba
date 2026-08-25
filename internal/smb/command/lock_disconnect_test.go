package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件钉的是 bh4-A#2：字节范围锁的释放此前只挂在 CLOSE 命令路径上，
// 而句柄消失还有 TREE_DISCONNECT / LOGOFF / 连接断开 / durable 过期回收
// 等多条路 —— 它们全走 Open.close()，该方法却从不碰锁表。客户端异常退出
// 后锁会一直泄漏到进程重启，对应区段对其他客户端永久 LOCK_NOT_GRANTED。
// Samba 所有关闭路径汇于 close_file→brl_close_fnum
// （source3/smbd/close.c:503 → locking/locking.c:397）。

// newLockSession 造一个带真实共享与会话的最小环境：session→tree→share→fs，
// 外加一个已登记进会话表、指向文件 "f" 的句柄。
func newLockSession(t *testing.T) (sess *Session, tree *Tree, open *Open, tbl *lockTable, third *Open) {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "f",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenExisting,
	})
	if err != nil {
		t.Fatalf("vfs Open: %v", err)
	}

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	sess = newSession(&Conn{Settings: &Settings{}}, 1)
	tree, _ = sess.NewTree(share)
	open = &Open{Path: "f", Handle: h, Tree: tree, GrantedAccess: wire.FileReadData | wire.FileWriteData}
	if st := sess.AddOpen(open); st != 0 {
		t.Fatalf("AddOpen: %v", st)
	}
	tbl = &share.locks
	// third 是"断连后的新客户端"，用来验证锁真的可再获得。
	third = &Open{Path: "f", Tree: tree}
	return sess, tree, open, tbl, third
}

// TestOpenCloseReleasesByteRangeLocks：直接调 Open.close()（TREE_DISCONNECT /
// LOGOFF / 断连 / durable 回收共用的汇合处）必须释放该句柄全部字节范围锁。
func TestOpenCloseReleasesByteRangeLocks(t *testing.T) {
	_, _, open, tbl, third := newLockSession(t)

	if !lockOne(tbl, "f", open, 0, 100, true) {
		t.Fatal("加独占锁应成功")
	}

	open.close()

	if n := countLocks(tbl, open); n != 0 {
		t.Fatalf("Open.close() 后仍持有 %d 把锁 —— 非 CLOSE 关闭路径在泄漏锁", n)
	}
	if !lockOne(tbl, "f", third, 0, 100, true) {
		t.Error("句柄消失后，原区间应可被新句柄锁定")
	}
}

// TestSessionCloseReleasesByteRangeLocks：LOGOFF / 连接断开
// （Session.Close）路径必须释放锁。
func TestSessionCloseReleasesByteRangeLocks(t *testing.T) {
	sess, _, open, tbl, third := newLockSession(t)

	if !lockOne(tbl, "f", open, 0, 100, true) {
		t.Fatal("加独占锁应成功")
	}

	sess.Close()

	if n := countLocks(tbl, open); n != 0 {
		t.Fatalf("会话拆除后句柄仍持有 %d 把锁", n)
	}
	if !lockOne(tbl, "f", third, 0, 100, true) {
		t.Error("会话拆除后，原区间应可被新句柄锁定")
	}
}

// TestTreeDisconnectReleasesByteRangeLocks：TREE_DISCONNECT 路径必须释放锁。
func TestTreeDisconnectReleasesByteRangeLocks(t *testing.T) {
	sess, tree, open, tbl, third := newLockSession(t)

	if !lockOne(tbl, "f", open, 0, 100, true) {
		t.Fatal("加独占锁应成功")
	}

	if st := sess.RemoveTree(tree.ID); st != 0 {
		t.Fatalf("RemoveTree: %v", st)
	}

	if n := countLocks(tbl, open); n != 0 {
		t.Fatalf("树断开后句柄仍持有 %d 把锁", n)
	}
	if !lockOne(tbl, "f", third, 0, 100, true) {
		t.Error("树断开后，原区间应可被新句柄锁定")
	}
}

// TestDurableWaitingKeepsLocksUntilReap：等待重连的持久句柄**保留**锁是正确
// 的（重连要拿回同一个句柄），但过期回收（reap → close）后必须释放。
func TestDurableWaitingKeepsLocksUntilReap(t *testing.T) {
	sess, _, open, tbl, third := newLockSession(t)
	defer sess.Close()

	open.Durable = &DurableState{Granted: true, timeout: time.Hour}
	if !durableRegistry.disconnect(open) {
		t.Fatal("持久句柄应能进入等待重连态")
	}

	if !lockOne(tbl, "f", open, 0, 100, true) {
		t.Fatal("加独占锁应成功")
	}

	// 等待重连期间：锁保留。
	durableRegistry.reap(time.Now().Add(time.Minute))
	if n := countLocks(tbl, open); n != 1 {
		t.Fatalf("等待重连期间不应释放锁，实际剩 %d 把", n)
	}

	// 超时回收：锁必须随句柄一起消失。
	durableRegistry.reap(time.Now().Add(2 * time.Hour))
	if n := countLocks(tbl, open); n != 0 {
		t.Fatalf("durable 过期回收后仍持有 %d 把锁 —— 泄漏到进程重启", n)
	}
	if !lockOne(tbl, "f", third, 0, 100, true) {
		t.Error("回收后，原区间应可被新句柄锁定")
	}
}
