package command

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// durable_qa_test.go —— durable handle 的**独立验证**用例（qa-proto 所有）。
//
// 与 create_context_durable_test.go 的区别：那一份由实现者自己写，用例里的
// Open 都是手工构造并**手工指定 Persistent**（7/9/11 互不相同）；本文件一律
// 走真实的 Session.AddOpen 分配 FileId，因此能撞上实现者测不到的路径。
//
// 本文件里**通过**的用例是回归保护；**能复现缺陷**的用例带 qadefect build
// tag 放在 durable_defect_test.go，默认 go test 不会因它们变红。

// countingHandle 在 fakeHandle 之上记录 Close 次数，用于证明「超时回收是否
// 真的关掉了底层句柄」。判据可证伪：closes==0 就是泄漏。
type countingHandle struct {
	fakeHandle
	closes atomic.Int32
}

func (c *countingHandle) Close() error {
	c.closes.Add(1)
	return nil
}

func qaLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// qaSession 建一个已认证会话 + 一个树连接，共用同一个 Conn。
//
// 关键点：sid 不同但 user 相同 —— 现实中就是同一个用户开两个挂载/两条连接。
func qaSession(t *testing.T, conn *Conn, sid uint64, user, share string) (*Context, *Session, *Tree) {
	t.Helper()
	s := newSession(conn, sid)
	s.Establish(&auth.Identity{User: user})
	tree, st := s.NewTree(&Share{Name: share, Type: wire.ShareTypeDisk})
	if st != status.Success {
		t.Fatalf("NewTree: %v", st)
	}
	return &Context{Conn: conn, Session: s, Tree: tree, Log: qaLog()}, s, tree
}

// qaAddOpen 走真实的句柄登记路径（这才是生产路径分配 Persistent 的方式）。
func qaAddOpen(t *testing.T, s *Session, tree *Tree, path string, h vfs.Handle) *Open {
	t.Helper()
	o := &Open{Tree: tree, Path: path, Handle: h}
	if st := s.AddOpen(o); st != status.Success {
		t.Fatalf("AddOpen: %v", st)
	}
	return o
}

// ---------------------------------------------------------------------------
// 1. Persistent FileId 的实际取值域（v1 登记表键的地基）
// ---------------------------------------------------------------------------

// TestQAPersistentIDIsPerSessionCounter 记录一个事实：Session.AddOpen 把
// Persistent 设成**会话内**单调计数器，因此不同会话的第一个句柄
// Persistent 都是 1。durableKey 对 v1 只用 Persistent 做键，这个事实决定了
// v1 登记表键在跨会话时会碰撞（见 durable_defect_test.go）。
//
// 本用例不判断对错，只把地基钉死：将来若有人把 Persistent 改成全局唯一，
// 这条会变红，提醒他去看 durableKey。
func TestQAPersistentIDIsPerSessionCounter(t *testing.T) {
	resetDurable()
	conn := NewConn(&Settings{}, "test", "test")
	_, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	_, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})

	if o1.Persistent != o2.Persistent {
		t.Fatalf("前提已改变：两个会话的首个句柄 Persistent 不再相同（%d vs %d）；"+
			"请重新评估 durableKey 的 v1 分支", o1.Persistent, o2.Persistent)
	}
	if k1, k2 := durableKey(false, [16]byte{}, o1.Persistent), durableKey(false, [16]byte{}, o2.Persistent); k1 != k2 {
		t.Fatalf("durableKey 已不再碰撞（%q vs %q）", k1, k2)
	}
	t.Logf("v1 登记表键在两个会话上相同：%q", durableKey(false, [16]byte{}, o1.Persistent))
}

// ---------------------------------------------------------------------------
// 2. 并发：RemoveTree × reconnect（作者声称修复的死锁）
// ---------------------------------------------------------------------------

// TestQADurableRemoveTreeReconnectNoDeadlock 让 RemoveTree（s.mu → r.mu）与
// reconnect（r.mu → s.mu 的历史顺序）真正并发对撞。
//
// 可证伪性：若把 session.go 的 RemoveTree 改回「持 s.mu 调 disconnect」，
// 本用例会挂死并由 watchdog 判为失败（见 durable_defect_test.go 的说明）。
func TestQADurableRemoveTreeReconnectNoDeadlock(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	const rounds = 300
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			conn := NewConn(&Settings{}, "test", "test")
			ctxA, sA, treeA := qaSession(t, conn, 1, "alice", "share")
			_, sB, _ := qaSession(t, conn, 2, "alice", "share")

			open := qaAddOpen(t, sA, treeA, "f.txt", &fakeHandle{})
			grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
			intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
				Persistent: open.Persistent, Volatile: open.Volatile}}

			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); sA.RemoveTree(treeA.ID) }()
			go func() { defer wg.Done(); durableRegistry.reconnect(sB, intent, "share") }()
			wg.Wait()
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// 不用 t.Fatal：死锁时 goroutine 还挂着，让 panic 打印全部栈更有用。
		panic("RemoveTree × reconnect 并发在 30s 内未完成 —— 疑似死锁")
	}
}

// TestQADurableSessionCloseReconnectNoDeadlock 同上，换成整会话销毁路径
// （Conn.Close → Session.Close），这是真实断连走的那条。
func TestQADurableSessionCloseReconnectNoDeadlock(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	const rounds = 300
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			conn := NewConn(&Settings{}, "test", "test")
			ctxA, sA, treeA := qaSession(t, conn, 1, "alice", "share")
			_, sB, _ := qaSession(t, conn, 2, "alice", "share")

			open := qaAddOpen(t, sA, treeA, "f.txt", &fakeHandle{})
			grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
			intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
				Persistent: open.Persistent, Volatile: open.Volatile}}

			var wg sync.WaitGroup
			wg.Add(3)
			go func() { defer wg.Done(); sA.Close() }()
			go func() { defer wg.Done(); durableRegistry.reconnect(sB, intent, "share") }()
			go func() { defer wg.Done(); open.close() }()
			wg.Wait()
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		panic("Session.Close × reconnect × close 并发在 30s 内未完成 —— 疑似死锁")
	}
}

// ---------------------------------------------------------------------------
// 3. 授予前提的注入点是否真的接线
// ---------------------------------------------------------------------------

// TestQADurableGrantRequiresBatchOplockOnly 证明：在**当前代码库**里，
// durable 授予的唯一入口是 batch oplock —— lease 注入点从未被调用。
//
// 判据可证伪：一旦 tm-lease 调了 SetLeaseDurableEligible，本用例的第二段
// （非 batch 请求不授予）就会变红，提示注入点已接线、需要重新评估。
func TestQADurableGrantRequiresBatchOplockOnly(t *testing.T) {
	resetDurable()
	ctx, s, tree := func() (*Context, *Session, *Tree) {
		conn := NewConn(&Settings{}, "test", "test")
		return qaSession(t, conn, 1, "alice", "share")
	}()
	_ = s

	for _, lvl := range []wire.OplockLevel{
		wire.OplockLevelNone, wire.OplockLevelII, wire.OplockLevelExclusive, wire.OplockLevelLease,
	} {
		o := qaAddOpen(t, ctx.Session, tree, "f.txt", &fakeHandle{})
		grantDurable(t, ctx, o, dhqReq(lvl))
		if o.Durable != nil && o.Durable.Granted {
			t.Errorf("oplock=%#x 竟然授予了 durable；lease 注入点已接线？", lvl)
		}
	}

	o := qaAddOpen(t, ctx.Session, tree, "g.txt", &fakeHandle{})
	grantDurable(t, ctx, o, dhqReq(wire.OplockLevelBatch))
	if o.Durable == nil || !o.Durable.Granted {
		t.Error("batch oplock 应授予 durable")
	}
}

// ---------------------------------------------------------------------------
// 4. 显式 CLOSE 之后不应还能重连
// ---------------------------------------------------------------------------

// TestQADurableClosedHandleNotReconnectable 验证 Open.close() 会把句柄从
// 登记表摘掉。反向对照：不调用 close 时同样的重连是成功的。
func TestQADurableClosedHandleNotReconnectable(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second
	conn := NewConn(&Settings{}, "test", "test")
	ctx, s, tree := qaSession(t, conn, 1, "alice", "share")

	// 正向对照：断连后可以重连。
	o1 := qaAddOpen(t, s, tree, "a.txt", &fakeHandle{})
	grantDurable(t, ctx, o1, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(o1)
	i1 := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: o1.Persistent, Volatile: o1.Volatile}}
	if _, st := durableRegistry.reconnect(s, i1, "share"); st != status.Success {
		t.Fatalf("对照组：断连后应可重连，实得 %v", st)
	}

	// 实验组：断连后显式 close，再重连必须失败。
	resetDurable()
	defaultDurableTimeout = 30 * time.Second
	conn2 := NewConn(&Settings{}, "test", "test")
	ctx2, s2, tree2 := qaSession(t, conn2, 1, "alice", "share")
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})
	grantDurable(t, ctx2, o2, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(o2)
	o2.close()
	i2 := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: o2.Persistent, Volatile: o2.Volatile}}
	if _, st := durableRegistry.reconnect(s2, i2, "share"); st == status.Success {
		t.Error("已 CLOSE 的句柄不应还能被重连认领")
	}
}

// ---------------------------------------------------------------------------
// 5. 重连之后句柄是否真的可用（Tree 绑定）
// ---------------------------------------------------------------------------

// TestQADurableReconnectRebindsTree 检查重连后 Open 的归属是否完整。
//
// reconnect 只改了 open.Session，没有改 open.Tree。CLOSE handler 的
// delete-on-close、READ/WRITE 的只读判定都要读 Open 所属树的共享配置，
// 若 Tree 还指向已销毁会话的树，行为就落在一个不存在的会话上。
func TestQADurableReconnectRebindsTree(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctxA, sA, treeA := qaSession(t, conn, 1, "alice", "share")
	open := qaAddOpen(t, sA, treeA, "f.txt", &fakeHandle{})
	grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))

	sA.Close() // 模拟断连：durable 进 waiting 表

	// 新连接、新会话、新树。
	conn2 := NewConn(&Settings{}, "test", "test")
	_, sB, treeB := qaSession(t, conn2, 1, "alice", "share")

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: open.Persistent, Volatile: open.Volatile}}
	got, st := durableRegistry.reconnect(sB, intent, "share")
	if st != status.Success {
		t.Fatalf("重连失败: %v", st)
	}
	if got.Session != sB {
		t.Fatal("重连后 Session 未改绑")
	}
	if got.Tree == treeA {
		t.Errorf("重连后 Open.Tree 仍指向旧会话的树（旧树 Session.closed=%v）；"+
			"期望改绑到新树 %p", sA.closedForTest(), treeB)
	}
}

// closedForTest 暴露 Session.closed 供本文件断言（只读，不改产品语义）。
func (s *Session) closedForTest() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
