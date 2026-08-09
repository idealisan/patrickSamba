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

// TestQAPersistentIDIsGloballyUnique 钉死 v1 登记表键的地基：
// Session.AddOpen 分配的 Persistent 必须**全进程唯一**，不是会话内唯一。
//
// 历史：这条原来叫 TestQAPersistentIDIsPerSessionCounter，断言的是相反的
// 事实（两个会话的首个句柄 Persistent 都是 1），作为「缺陷仍在」的地基探针。
// Persistent 改成全局分配后它按设计变红，于是翻转成现在这条正向回归。
//
// 判据可证伪：把 session.go 的 `o.Persistent = newPersistentID()` 改回
// `= s.nextVolatile`，本用例立刻变红。
func TestQAPersistentIDIsGloballyUnique(t *testing.T) {
	resetDurable()
	conn := NewConn(&Settings{}, "test", "test")
	_, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	_, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})

	// Volatile 仍是会话内计数器 —— 它只在本会话句柄表里查，撞了也没关系。
	// 这一条同时说明「两者不再是同一个值」是有意为之，不是笔误。
	if o1.Volatile != o2.Volatile {
		t.Errorf("Volatile 应仍是会话内计数器（两个新会话的首个句柄都该是 1），实得 %d / %d",
			o1.Volatile, o2.Volatile)
	}
	if o1.Persistent == o2.Persistent {
		t.Fatalf("两个会话的句柄拿到相同的 Persistent(%d) —— v1 durable 键必然碰撞",
			o1.Persistent)
	}
	if k1, k2 := durableKey(false, [16]byte{}, o1.Persistent), durableKey(false, [16]byte{}, o2.Persistent); k1 == k2 {
		t.Fatalf("durableKey 仍在碰撞：%q", k1)
	}

	// 规模化对照：单会话内连开 200 个句柄，Persistent 不许重复，
	// 且不许撞上两个保留值（0=未分配，全 1=复合请求占位）。
	seen := map[uint64]bool{o1.Persistent: true, o2.Persistent: true}
	for i := 0; i < 200; i++ {
		o := qaAddOpen(t, s1, tree1, "f.txt", &fakeHandle{})
		if seen[o.Persistent] {
			t.Fatalf("第 %d 个句柄的 Persistent(%d) 与之前重复", i, o.Persistent)
		}
		seen[o.Persistent] = true
	}
	for p := range seen {
		if p == 0 {
			t.Error("分配出了 Persistent=0（该值保留作「未分配」）")
		}
		if p == ^uint64(0) {
			t.Error("分配出了 Persistent=0xFFFFFFFFFFFFFFFF（wire.CompoundFileID 占位值）")
		}
	}
}

// TestQAPersistentIDNeverCollidesWithCompoundSentinel 钉死 newPersistentID 的
// 不变量：分配值不得等于 wire.CompoundFileID 的任一半。
//
// 那个哨兵（全 0xFF）在复合请求里表示「复用上一条 CREATE 返回的句柄」
// （§3.2.4.1.4，macOS 大量使用）。分配器一旦吐出它，IsCompound() 会把一个
// 正常句柄误判成占位符，客户端拿到的句柄直接串号。
//
// 当前实现单调递增、首值为 1，天然碰不到；本用例的意义是把这条不变量**钉在
// 测试里**，防止将来改成「复用回收的 ID / 时间戳 / 随机数」时悄悄破坏它 ——
// 那时递增性不再成立，必须显式排除这两个值。
func TestQAPersistentIDNeverCollidesWithCompoundSentinel(t *testing.T) {
	prev := uint64(0)
	for i := 0; i < 1000; i++ {
		got := newPersistentID()
		if got == 0 {
			t.Fatal("newPersistentID 返回 0（保留作「未分配」）")
		}
		if got == ^uint64(0) {
			t.Fatal("newPersistentID 返回 0xFFFFFFFFFFFFFFFF（复合请求占位值）")
		}
		if got <= prev {
			t.Fatalf("newPersistentID 不再单调递增：%d 之后是 %d", prev, got)
		}
		prev = got
	}

	// 反向对照：确认哨兵确实要求两半同时为全 1，且它真的还在。
	// wire 侧若改了定义，这里会提醒回来重新评估上面的不变量。
	if !(wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}).IsCompound() {
		t.Error("wire.CompoundFileID 的定义已变，请重新评估 newPersistentID 的不变量")
	}
	if (wire.FileID{Persistent: 1, Volatile: 1}).IsCompound() {
		t.Error("正常 FileId 被判成复合占位符")
	}
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
			_, sB, treeB := qaSession(t, conn, 2, "alice", "share")

			open := qaAddOpen(t, sA, treeA, "f.txt", &fakeHandle{})
			grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
			intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
				Persistent: open.Persistent, Volatile: open.Volatile}}

			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); sA.RemoveTree(treeA.ID) }()
			go func() { defer wg.Done(); durableRegistry.reconnect(sB, treeB, intent) }()
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
			_, sB, treeB := qaSession(t, conn, 2, "alice", "share")

			open := qaAddOpen(t, sA, treeA, "f.txt", &fakeHandle{})
			grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
			intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
				Persistent: open.Persistent, Volatile: open.Volatile}}

			var wg sync.WaitGroup
			wg.Add(3)
			go func() { defer wg.Done(); sA.Close() }()
			go func() { defer wg.Done(); durableRegistry.reconnect(sB, treeB, intent) }()
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
	if _, st := durableRegistry.reconnect(s, tree, i1); st != status.Success {
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
	if _, st := durableRegistry.reconnect(s2, tree2, i2); st == status.Success {
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
	got, st := durableRegistry.reconnect(sB, treeB, intent)
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

// ---------------------------------------------------------------------------
// 6. v1 键不再跨会话碰撞（原 durable_defect_test.go，已修，转回归）
// ---------------------------------------------------------------------------

// TestQADurableV1KeyNoCrossSessionCollision
//
// 原缺陷：Persistent 是会话内计数器，两个会话的首个句柄都是 1；durableKey
// 的 v1 分支只用 Persistent 做键，于是两个不同文件的 durable 句柄共用键
// "v1:1"，后登记的静默覆盖先登记的。客户端用 a.txt 的 FileId 重连，服务端
// 把 b.txt 的句柄交回去 —— 跨会话交叉句柄泄漏，身份校验拦不住（同一用户
// 开两条连接完全正常）。
//
// 判据可证伪：把 session.go 的 Persistent 改回会话内计数器，
// 登记表会只剩 1 条，且重连拿回的 Path 变成 "b.txt"。
func TestQADurableV1KeyNoCrossSessionCollision(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctx1, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	ctx2, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	grantDurable(t, ctx1, o1, dhqReq(wire.OplockLevelBatch))
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})
	grantDurable(t, ctx2, o2, dhqReq(wire.OplockLevelBatch))

	if n := len(durableRegistry.entries); n != 2 {
		t.Fatalf("两个不同文件的 durable 句柄应各占一条登记，实得 %d 条（键碰撞）", n)
	}

	s1.Close() // 只有 s1 掉线；s2/o2 仍在正常使用

	// 真实重连时序：新连接、新会话、新树，同一个用户。
	conn3 := NewConn(&Settings{}, "test", "test")
	_, s3, tree3 := qaSession(t, conn3, 1, "alice", "share")

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: o1.Persistent, Volatile: o1.Volatile}}
	got, st := durableRegistry.reconnect(s3, tree3, intent)
	if st != status.Success {
		t.Fatalf("重连本应成功（同一用户、同一 share、未超时），实得 %v", st)
	}
	if got.Path != "a.txt" {
		t.Errorf("用 a.txt 的 FileId 重连，却拿回 %q 的句柄 —— 交叉句柄泄漏", got.Path)
	}
	if got == o2 {
		t.Error("拿回的是另一个会话仍在使用中的句柄 o2")
	}
	// o2 的登记必须原封不动：它那条连接根本没断。
	if o2.Durable == nil || o2.Durable.key == "" {
		t.Error("o2 的登记被 o1 的重连顺手清掉了")
	}
}

// TestQADurableRemoveChecksEntryOwnership
//
// 原缺陷：durableTable.remove 只按 open.Durable.key 删除，**不校验表里那条
// 记录是否真的属于这个 open**。当年靠 v1 键碰撞就能触发：A 的 CLOSE 会把
// B 的登记删掉，B 从此再也无法重连（静默失效，日志里什么都看不到）。
//
// v1 键全局唯一之后这条路走不通了，但守卫本身**不能撤** —— v2 的键是客户端
// 自己给的 CreateGuid，服务端管不住重复。这里直接对着 detachLocked 的归属
// 检查做白盒验证：手工把 o2 的 key 指向 o1 的登记（模拟任何未来会重新引入
// 键碰撞的改动），o1 的记录必须活下来。
//
// 判据可证伪：把 detachLocked 里的 `e.open == open` 去掉，本用例立刻变红。
func TestQADurableRemoveChecksEntryOwnership(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctx1, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	ctx2, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	grantDurable(t, ctx1, o1, dhqReq(wire.OplockLevelBatch))
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})
	grantDurable(t, ctx2, o2, dhqReq(wire.OplockLevelBatch))

	// 前提检查写成 nil 安全的：键一旦重新碰撞，register() 会拒绝给 o2 授予，
	// o2.Durable 直接是 nil。裸取字段会 panic，把「前提不成立」伪装成崩溃。
	if o1.Durable == nil || o2.Durable == nil {
		t.Fatalf("前提被破坏：两个句柄都该拿到 durable 授予，实得 o1=%v o2=%v",
			o1.Durable != nil, o2.Durable != nil)
	}
	o1Key := o1.Durable.key
	if o1Key == "" || o1Key == o2.Durable.key {
		t.Fatalf("前提被破坏：o1.key=%q o2.key=%q", o1Key, o2.Durable.key)
	}

	// o1 掉线进等待重连态。
	durableRegistry.disconnect(o1)

	// 制造「o2 的 key 指向 o1 那条记录」的状态，然后让 o2 走显式 CLOSE。
	o2.Durable.key = o1Key
	o2.close()

	if _, ok := durableRegistry.entries[o1Key]; !ok {
		t.Fatal("o2 的 CLOSE 删掉了 o1 的登记 —— remove 没做归属校验")
	}

	// 而且 o1 必须真的还能重连回来。
	conn3 := NewConn(&Settings{}, "test", "test")
	_, s3, tree3 := qaSession(t, conn3, 1, "alice", "share")
	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: o1.Persistent, Volatile: o1.Volatile}}
	got, st := durableRegistry.reconnect(s3, tree3, intent)
	if st != status.Success {
		t.Fatalf("o1 重连失败：%v", st)
	}
	if got != o1 {
		t.Errorf("重连拿回的不是 o1（Path=%q）", got.Path)
	}
}

// ---------------------------------------------------------------------------
// 7. 超时回收必须真的关闭底层句柄（原 durable_defect_test.go，已修，转回归）
// ---------------------------------------------------------------------------

// TestQADurableExpiredEntryClosesHandle
//
// 原缺陷：reap()（以及 reconnect 里的过期分支）只 `delete(r.entries, k)`，
// 从不调用 e.open.close()。于是超时的持久句柄底层 vfs.Handle 永不关闭
// → fd 泄漏；Windows 上还会顶着文件不让删/改名；FILE_DELETE_ON_CLOSE
// 创建的句柄也永远不会执行那次删除。
//
// 判据可证伪：countingHandle.closes 必须从 0 变成 1。把 reap() 尾部那个
// `o.close()` 循环删掉，本用例立刻变红。
func TestQADurableExpiredEntryClosesHandle(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 5 * time.Millisecond

	conn := NewConn(&Settings{}, "test", "test")
	ctx, s, tree := qaSession(t, conn, 1, "alice", "share")
	h := &countingHandle{}
	open := qaAddOpen(t, s, tree, "f.txt", h)
	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	if n := h.closes.Load(); n != 0 {
		t.Fatalf("前提被破坏：进入等待重连态时不该关闭底层句柄，实得 closes=%d", n)
	}

	time.Sleep(30 * time.Millisecond) // 远超 5ms 超时
	durableRegistry.reap(time.Now())

	if len(durableRegistry.entries) != 0 {
		t.Fatalf("reap 后登记表应为空，实得 %d 条", len(durableRegistry.entries))
	}
	if n := h.closes.Load(); n == 0 {
		t.Error("超时回收未关闭底层 vfs 句柄 —— fd 泄漏")
	}
	if !open.Closed() {
		t.Error("超时回收未把 Open 标记为 closed")
	}
}

// TestQADurableExpiredEntriesReclaimedByNewRegistrations
//
// 原缺陷：reap() 在整个代码库里只有 reconnect() 一个调用点，没有定时器、
// 没有常驻 goroutine，Session.Close 与 Conn.Close 都不调它。客户端断线后
// **再也不回来**（最常见的情形）时，记录连同 *Open 与 fd 永久留在包级
// map 里 —— 一条无界增长的内存/fd 泄漏，攻击者只要反复「连上→开一个
// batch-oplock durable 句柄→掉线」即可持续制造。
//
// 现在改为机会式回收：register()（有新句柄进来）与 Session.Close()
// （有旧连接离开）各扫一遍。本用例是「无界增长已被堵死」的正向证据：
// 50 轮「建会话→授予 durable→断连」之后，登记表**不随轮数增长**。
//
// 判据可证伪：把 register() 里的 r.reap() 与 Session.Close() 尾部的
// r.reap() 两处都删掉，entries 会线性涨到 50 附近，本用例变红。
func TestQADurableExpiredEntriesReclaimedByNewRegistrations(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = time.Millisecond

	const rounds = 50
	for i := 0; i < rounds; i++ {
		conn := NewConn(&Settings{}, "test", "test")
		ctx, s, tree := qaSession(t, conn, uint64(i+1), "alice", "share")
		open := qaAddOpen(t, s, tree, "f.txt", &countingHandle{})
		grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
		s.Close() // 断连，进等待重连态，1ms 后过期
		time.Sleep(2 * time.Millisecond)
	}

	// 上界取 2 而非 0：最后一轮断连之后再没有任何流量触发回收，那一批
	// 记录会留到下次有人连进来为止（已知残留，见 durable_defect_test.go
	// 的 TestQADefectExpiryHappensWithoutReconnect）。要证明的是**不随
	// 轮数增长**，不是恒为零。
	if n := len(durableRegistry.entries); n > 2 {
		t.Errorf("跑了 %d 轮后登记表有 %d 条 —— 过期项没被机会式回收，仍在无界增长",
			rounds, n)
	}
}

// ---------------------------------------------------------------------------
// 8. 驱逐必须发生在鉴权之后（原 durable_defect_test.go，已修，转回归）
// ---------------------------------------------------------------------------

// TestQADurableEvictionRequiresAuthorization
//
// 原缺陷：reconnect 的检查次序是「存在 → 未作废 → 未过期 → share → 身份」，
// 两个「删除登记」的分支都排在 share/身份校验**之前**。于是任何一个已认证
// 会话（含 guest）只要猜中键，就能把别人**正在使用中**的 durable 登记删掉。
// v1 的键当年就是 1、2、3… 这样的小整数，猜中成本约等于零（拒绝服务）。
//
// 判据可证伪：把 durable.go 的 reconnect 里「先授权后驱逐」两段的次序调回来，
// bob 的探测会命中 deadline-zero 分支并删掉 alice 的记录，本用例立刻变红。
func TestQADurableEvictionRequiresAuthorization(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctxA, sA, treeA := qaSession(t, conn, 1, "alice", "share")
	open := qaAddOpen(t, sA, treeA, "secret.txt", &fakeHandle{})
	grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
	before := len(durableRegistry.entries)
	if before == 0 {
		t.Fatal("前提被破坏：alice 的 durable 没有登记成功")
	}

	// bob 猜键探测（open 仍在使用中 → deadline 为零分支）。
	conn2 := NewConn(&Settings{}, "test", "test")
	_, sB, treeB := qaSession(t, conn2, 1, "bob", "share")
	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: open.Persistent, Volatile: open.Volatile}}
	if _, st := durableRegistry.reconnect(sB, treeB, intent); st != status.AccessDenied {
		t.Fatalf("bob 越权重连应得 ACCESS_DENIED，实得 %v", st)
	}

	if after := len(durableRegistry.entries); after != before {
		t.Errorf("bob 的越权探测删掉了 alice 的登记：%d → %d 条（删除动作发生在身份校验之前）",
			before, after)
	}

	// alice 自己随后仍能正常断连重连 —— 证明记录不但还在，而且是可用的。
	durableRegistry.disconnect(open)
	if _, st := durableRegistry.reconnect(sA, treeA, intent); st != status.Success {
		t.Errorf("越权探测之后 alice 的合法重连被破坏：%v", st)
	}
}

// ---------------------------------------------------------------------------
// 9. persistent 位应降级而非打死 CREATE（原 durable_defect_test.go，已修）
// ---------------------------------------------------------------------------

// TestQADurablePersistentFlagDegrades
//
// 原缺陷：DH2Q 里带 SMB2_DHANDLE_FLAG_PERSISTENT 时直接
// return STATUS_NOT_SUPPORTED，把整个 CREATE 打掉 —— 客户端顺手带上这个位
// 就连文件都打不开。
//
// 正确行为（§3.3.5.9.12，且经 Samba smb2_create.c L1481-1503/L1988 交叉验证）：
// 该位只是条件升级请求，条件不满足就**忽略它**，继续走普通 durable v2 授予。
// 这条分支在规范里没有失败出口。
//
// 本用例走 qaAddOpen 的真实 FileId 分配路径（与 create_context_durable_test.go
// 里手工构造 Open 的那条互为补充）。判据可证伪：把那句 return 加回去即变红。
func TestQADurablePersistentFlagDegrades(t *testing.T) {
	resetDurable()
	conn := NewConn(&Settings{}, "test", "test")
	ctx, s, tree := qaSession(t, conn, 1, "alice", "share")
	open := qaAddOpen(t, s, tree, "f.txt", &fakeHandle{})

	req := &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelBatch,
		Contexts: []wire.CreateContext{{
			Name: wire.CreateContextDH2Q,
			Data: (&wire.DurableRequestV2{Timeout: 10000, Flags: wire.DurableHandlePersistent}).Encode(),
		}},
	}
	h := &durableHandler{req: req}
	if err := h.Parse(ctx, wire.CreateContextDH2Q, req.Contexts[0].Data); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := h.Registered(ctx, open); err != nil {
		t.Fatalf("persistent 位应降级为普通 durable 而非让 CREATE 失败，实得 %v", err)
	}
	if open.Durable == nil || !open.Durable.Granted {
		t.Fatal("降级后应授予普通 durable v2")
	}

	// 降级出来的必须是**真能用**的 durable，不是一个空壳：断连后可重连。
	durableRegistry.disconnect(open)
	conn2 := NewConn(&Settings{}, "test", "test")
	_, s2, tree2 := qaSession(t, conn2, 1, "alice", "share")
	intent := &wire.DurableIntent{ReconnectV2: &wire.DurableReconnectV2{
		FileID: wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile}}}
	if _, st := durableRegistry.reconnect(s2, tree2, intent); st != status.Success {
		t.Errorf("降级授予的 durable 无法重连：%v", st)
	}
}

// closedForTest 暴露 Session.closed 供本文件断言（只读，不改产品语义）。
func (s *Session) closedForTest() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
