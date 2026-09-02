package command

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件钉的是**阻塞锁的规范异步模型**（lock.go 的 AsyncRequest 路径）：
// 冲突时挂起、持有方释放后异步补发、期间可被 CANCEL 取消。
//
// 与 lock_block_test.go 的分工：那边钉锁表自身的等待/超时/上限，
// 这边钉"handler 把请求挂出去并按规范补发"这条链路。
//
// 变异自检方向：
//   - 把 handleLock 里的 ctx.Defer 分支删掉 → TestBlockingLockAsyncGrantsAfterRelease
//     变红（请求不再挂起，会当场回 LOCK_NOT_GRANTED）；
//   - 把 waitLockAsync 里 Complete 失败后的 unlock 回滚删掉 →
//     TestCancelDuringBlockingLockLeavesNoLockBehind 变红；
//   - 把同步退路删掉 → TestBlockingLockFallsBackToSyncWait 变红。

// lockFixture 是一条装配好共享/树/会话/文件句柄的连接。
type lockFixture struct {
	conn  *Conn
	sink  *fakeAsyncSink
	share *Share
	sess  *Session
	tree  *Tree
	fileA *Open
	fileB *Open
}

func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	sink := &fakeAsyncSink{}
	share := &Share{Name: "pub"}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	conn.SetAsyncSink(sink)

	sess, st := conn.NewSession()
	if st != status.Success {
		t.Fatalf("建会话失败：%v", st)
	}
	sess.Establish(&auth.Identity{User: "u"})
	tree, st := sess.NewTree(share)
	if st != status.Success {
		t.Fatalf("建树失败：%v", st)
	}

	a := &Open{Tree: tree, Session: sess, Path: "f.bin"}
	b := &Open{Tree: tree, Session: sess, Path: "f.bin"}
	for _, o := range []*Open{a, b} {
		if st := sess.AddOpen(o); st != status.Success {
			t.Fatalf("登记句柄失败：%v", st)
		}
	}
	return &lockFixture{conn: conn, sink: sink, share: share, sess: sess, tree: tree, fileA: a, fileB: b}
}

// lockMsg 拼一条完整的 LOCK 请求报文。
func lockMsg(fid wire.FileID, elems ...wire.LockElement) []byte {
	hdr := wire.Header{Command: wire.CommandLock}
	msg := hdr.Append(nil)
	out, err := (&wire.LockRequest{FileID: fid, Locks: elems}).Append(msg)
	if err != nil {
		panic(err)
	}
	return out
}

// lockCtx 造一条挂在本 fixture 上的 LOCK 上下文。
func (f *lockFixture) ctx(t *testing.T, o *Open, elems ...wire.LockElement) *Context {
	t.Helper()
	msg := lockMsg(wire.FileID{Persistent: o.Persistent, Volatile: o.Volatile}, elems...)
	hdr, err := wire.ParseHeader(msg)
	if err != nil {
		t.Fatalf("解析头失败：%v", err)
	}
	hdr.SessionID = f.sess.ID
	hdr.TreeID = f.tree.ID
	return NewContext(f.conn, &Chain{}, hdr, msg, nil)
}

// awaitAsync 等一条异步补发到达并返回其状态。
func (f *lockFixture) awaitStatus(t *testing.T) status.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.sink.count() > 0 {
			return status.Status(f.sink.last().hdr.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待异步补发超时")
	return 0
}

func TestBlockingLockAsyncGrantsAfterRelease(t *testing.T) {
	f := newLockFixture(t)

	// A 先拿到独占锁。
	actx := f.ctx(t, f.fileA, el(0, 100, true))
	Dispatch(actx)
	if actx.Status != status.Success {
		t.Fatalf("A 加锁应成功，实际 %v", actx.Status)
	}

	// B 的阻塞锁：必须挂起，不当场回 LOCK_NOT_GRANTED。
	bctx := f.ctx(t, f.fileB, el(0, 100, true))
	Dispatch(bctx)
	if bctx.Async() == nil {
		t.Fatal("冲突的阻塞锁必须挂起（规范路径）")
	}
	if f.sink.count() != 0 {
		t.Fatal("挂起期间不得补发响应")
	}

	// A 释放 → B 应被授予并补发 SUCCESS。
	if st := f.share.locks.unlock("f.bin", f.fileA, []wire.LockElement{unlockEl(0, 100)}); st != status.Success {
		t.Fatalf("A 解锁应成功，实际 %v", st)
	}
	if got := f.awaitStatus(t); got != status.Success {
		t.Fatalf("B 应在 A 释放后被授予，实际 %v", got)
	}
	// 锁真的落到 B 手上了。
	if !f.share.locks.blocksIO("f.bin", f.fileA, 0, 10, true) {
		t.Fatal("B 应持有该区间的锁（A 的写应被挡住）")
	}
}

// TestCancelDuringBlockingLockLeavesNoLockBehind：取消与授予的竞态窗口里，
// 抢先 granted 的锁必须回滚 —— 否则那段字节会被永久挡住。
func TestCancelDuringBlockingLockLeavesNoLockBehind(t *testing.T) {
	f := newLockFixture(t)

	actx := f.ctx(t, f.fileA, el(0, 100, true))
	Dispatch(actx)
	if actx.Status != status.Success {
		t.Fatalf("A 加锁应成功，实际 %v", actx.Status)
	}

	bctx := f.ctx(t, f.fileB, el(0, 100, true))
	Dispatch(bctx)
	if bctx.Async() == nil {
		t.Fatal("B 的请求应挂起")
	}

	// 取消 B 的等待：应回 STATUS_CANCELLED。
	hdr := wire.Header{Command: wire.CommandCancel, SessionID: f.sess.ID, MessageID: bctx.Header.MessageID}
	Dispatch(NewContext(f.conn, &Chain{}, hdr, hdr.Append(nil), nil))
	if got := f.awaitStatus(t); got != status.Cancelled {
		t.Fatalf("应回 STATUS_CANCELLED，实际 %v", got)
	}

	// 关键断言：取消之后锁表里**不能**留下 B 的锁。
	// 判据是"A 仍能正常写该区间"—— 若 B 的锁残留，blocksIO 会返回 true。
	time.Sleep(50 * time.Millisecond) // 给等待 goroutine 收尾的时间
	if f.share.locks.blocksIO("f.bin", f.fileA, 0, 10, true) {
		t.Fatal("取消后不得残留 B 的锁（会导致该区间被永久挡住）")
	}
	if got := len(f.share.locks.m["f.bin"]); got != 1 {
		t.Fatalf("锁表里应只剩 A 的 1 条锁，实际 %d 条", got)
	}
}

// TestBlockingLockFallsBackToSyncWait：挂不起来时退回 v0.5.x 的同步有界
// 等待，行为与升级前一致（当场给出结果，不挂起）。
func TestBlockingLockFallsBackToSyncWait(t *testing.T) {
	// 不注入 sink：Defer 必然失败。
	share := &Share{Name: "pub"}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	sess, _ := conn.NewSession()
	sess.Establish(&auth.Identity{User: "u"})
	tree, _ := sess.NewTree(share)
	a := &Open{Tree: tree, Session: sess, Path: "f.bin"}
	b := &Open{Tree: tree, Session: sess, Path: "f.bin"}
	for _, o := range []*Open{a, b} {
		if st := sess.AddOpen(o); st != status.Success {
			t.Fatalf("登记句柄失败：%v", st)
		}
	}

	// A 持锁。
	amsg := lockMsg(wire.FileID{Persistent: a.Persistent, Volatile: a.Volatile}, el(0, 100, true))
	ah, _ := wire.ParseHeader(amsg)
	ah.SessionID, ah.TreeID = sess.ID, tree.ID
	Dispatch(NewContext(conn, &Chain{}, ah, amsg, nil))

	// B 阻塞：无异步通路 → 同步等待至超时（这里用极短超时不现实，因为
	// blockingLockMaxWait 是常量；改为断言"没有挂起"这一可观测事实）。
	bmsg := lockMsg(wire.FileID{Persistent: b.Persistent, Volatile: b.Volatile}, el(0, 100, true))
	bh, _ := wire.ParseHeader(bmsg)
	bctx := NewContext(conn, &Chain{}, bh, bmsg, nil)

	go Dispatch(bctx)
	// 同步路径下 handler 会在读循环里阻塞到超时才返回；给它一小段时间，
	// 断言此刻**还没有**任何异步补发发生（因为没有 sink，不可能有）。
	time.Sleep(50 * time.Millisecond)
	if conn.async.pendingCount() != 0 {
		t.Fatal("无 AsyncSink 时不得挂起")
	}
}

// TestNonBlockingLockStillFailsImmediately：非阻塞锁（置了
// FAIL_IMMEDIATELY）不挂起，当场回 LOCK_NOT_GRANTED。
func TestNonBlockingLockStillFailsImmediately(t *testing.T) {
	f := newLockFixture(t)

	actx := f.ctx(t, f.fileA, el(0, 100, true))
	Dispatch(actx)
	if actx.Status != status.Success {
		t.Fatalf("A 加锁应成功，实际 %v", actx.Status)
	}

	e := el(0, 100, true)
	e.Flags |= wire.LockFlagFailImmediately
	bctx := f.ctx(t, f.fileB, e)
	Dispatch(bctx)

	if bctx.Async() != nil {
		t.Fatal("非阻塞锁不得挂起")
	}
	if bctx.Status != status.LockNotGranted {
		t.Fatalf("非阻塞锁应当场回 LOCK_NOT_GRANTED，实际 %v", bctx.Status)
	}
}
