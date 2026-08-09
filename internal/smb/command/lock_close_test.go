package command

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件补的是 lock_test.go 覆盖不到的那一环：**CLOSE handler 有没有真的去调
// releaseAll**。
//
// lock_test.go 里的 TestReleaseAllOnClose 直接调 tbl.releaseAll，验证的是锁表
// 自身的正确性 —— 把 close.go 里那行调用删掉，它照样全绿。而那行一旦丢失，
// 后果是「客户端断开后文件被永久锁死」，且只能在真机压测里偶发暴露。
// 这类"胶水掉了但单测无感"的缺口值得单独钉一根钉子。

// TestCloseHandlerReleasesLocks：走完整的 handleClose 流程，确认句柄持有的
// 字节范围锁被释放，且别的句柄的锁不受影响（MS-SMB2 §3.3.5.10）。
func TestCloseHandlerReleasesLocks(t *testing.T) {
	ctx, open, other := newCloseLockCtx(t)
	tbl := &ctx.Tree.Share.locks

	// 被关闭的句柄持有两把锁（一把独占、一把共享），另一个句柄持有一把。
	if !lockOne(tbl, open.Path, open, 0, 100, true) {
		t.Fatal("A 的独占锁应授予")
	}
	if !lockOne(tbl, open.Path, open, 1000, 100, false) {
		t.Fatal("A 的共享锁应授予")
	}
	if !lockOne(tbl, open.Path, other, 5000, 100, true) {
		t.Fatal("B 的独占锁应授予")
	}
	if n := countLocks(tbl, open); n != 2 {
		t.Fatalf("关闭前 A 应持有 2 把锁，实际 %d", n)
	}

	if err := handleClose(ctx); err != nil {
		t.Fatalf("handleClose: %v", err)
	}

	if n := countLocks(tbl, open); n != 0 {
		t.Errorf("CLOSE 后 A 仍持有 %d 把锁，close.go 是不是漏了 releaseAll？", n)
	}
	if n := countLocks(tbl, other); n != 1 {
		t.Errorf("B 的锁不应被 A 的 CLOSE 带走，实际剩 %d 把", n)
	}

	// 锁真的没了：别的句柄现在能锁原先被 A 独占的区间。
	if !lockOne(tbl, open.Path, other, 0, 100, true) {
		t.Error("A 关闭后它原先独占的区间应可被 B 重新锁定")
	}
}

// TestCloseHandlerReleaseSurvivesRelock：A 关闭并释放锁后，同一路径上新开的
// 句柄不会"继承"到已死句柄的锁。
//
// 这道用例挡的是「按路径清空整桶」这类图省事的实现：那样写能让上一个用例通过，
// 却会把同路径上其他句柄的锁一并抹掉。
func TestCloseHandlerReleaseSurvivesRelock(t *testing.T) {
	ctx, open, other := newCloseLockCtx(t)
	tbl := &ctx.Tree.Share.locks

	if !lockOne(tbl, open.Path, other, 0, 100, true) {
		t.Fatal("B 的独占锁应授予")
	}
	if err := handleClose(ctx); err != nil {
		t.Fatalf("handleClose: %v", err)
	}
	// B 的锁还在，所以第三方仍然锁不上这段。
	third := &Open{Path: open.Path, Tree: ctx.Tree}
	if lockOne(tbl, open.Path, third, 50, 10, true) {
		t.Error("B 的独占锁仍应挡住重叠区间，A 的 CLOSE 不该清空整个路径")
	}
}

// newCloseLockCtx 造一个可以直接喂给 handleClose 的 Context，
// 外加一个"另一个客户端的句柄"用来验证隔离性。
//
// 用复合句柄（FileId 全 FF）+ Chain.LastOpen 绕开会话句柄表：
// resolveOpen 会走 §3.3.5.2.7 的复用分支，省掉一整套 Conn/Settings 装配。
// Session 仍要给一个非 nil 的空壳 —— handleClose 会无条件调 RemoveOpen，
// 对 nil map 的 delete 是合法的无操作。
func newCloseLockCtx(t *testing.T) (ctx *Context, open, other *Open) {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("data"), 0o644); err != nil {
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
	tree := &Tree{ID: 1, Share: share}
	open = &Open{
		Path:          "f",
		Handle:        h,
		Tree:          tree,
		GrantedAccess: wire.FileReadData | wire.FileWriteData,
	}
	// other 只当锁表里的另一个 owner 用，不需要真句柄。
	other = &Open{Path: "f", Tree: tree}

	req := &wire.CloseRequest{FileID: wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}}
	ctx = &Context{
		Session: &Session{},
		Tree:    tree,
		Chain:   &Chain{},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:     req.Append(make([]byte, wire.HeaderSize)),
		Out:     make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	return ctx, open, other
}

// TestCloseHandlerNoLocksNoPanic：句柄一把锁都没有时 CLOSE 也要正常返回，
// 且不会给锁表留下空桶。
func TestCloseHandlerNoLocksNoPanic(t *testing.T) {
	ctx, open, _ := newCloseLockCtx(t)
	tbl := &ctx.Tree.Share.locks

	if err := handleClose(ctx); err != nil {
		t.Fatalf("无锁句柄的 CLOSE 不应出错: %v", err)
	}
	tbl.mu.Lock()
	n := len(tbl.m)
	tbl.mu.Unlock()
	if n != 0 {
		t.Errorf("锁表残留 %d 个路径条目，期望 0", n)
	}
	if !open.Closed() {
		t.Error("CLOSE 后句柄应处于已关闭状态")
	}
	if ctx.Status != 0 && ctx.Status != status.Success {
		t.Errorf("ctx.Status = %v, 期望成功", ctx.Status)
	}
}
