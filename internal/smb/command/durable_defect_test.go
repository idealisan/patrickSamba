//go:build qadefect

package command

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// durable_defect_test.go —— **能复现缺陷的用例**（qa-proto 所有）。
//
// 这些用例断言的是「应该怎样」，在当前 HEAD 上会**失败**——失败本身就是
// 缺陷报告的证据。为了不把主干 CI 弄红，它们挂在 qadefect build tag 下：
//
//	go test -tags qadefect -run 'TestQADefect' ./internal/smb/command/
//
// 每修好一个缺陷，就把对应用例挪进 durable_qa_test.go（去掉 tag），
// 从此作为回归保护。**不要靠删除用例来「修复」失败。**

// --- 缺陷 1：v1 登记表键跨会话碰撞，重连会拿到别人的文件 ---

// TestQADefectV1KeyCollisionReturnsWrongFile
//
// Session.AddOpen 把 Persistent 设成**会话内**计数器，两个会话的首个句柄
// Persistent 都是 1；durableKey 的 v1 分支只用 Persistent 做键，于是两个
// 不同文件的 durable 句柄共用键 "v1:1"，后登记的静默覆盖先登记的。
//
// 判据：alice 用 a.txt 的 FileId 重连，若拿回的 Open.Path 是 b.txt，
// 就证明服务端把**另一个文件的句柄**交给了客户端。
// 身份校验拦不住这个——同一个用户开两条连接是完全正常的场景。
func TestQADefectV1KeyCollisionReturnsWrongFile(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctx1, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	ctx2, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	grantDurable(t, ctx1, o1, dhqReq(wire.OplockLevelBatch))

	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})
	grantDurable(t, ctx2, o2, dhqReq(wire.OplockLevelBatch))

	if len(durableRegistry.entries) != 2 {
		t.Errorf("两个不同文件的 durable 句柄应各占一条登记，实得 %d 条（键碰撞）",
			len(durableRegistry.entries))
	}

	// s1 断连 —— 注意 disconnect 命中的其实是 o2 的记录。
	s1.Close()

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: o1.Persistent, Volatile: o1.Volatile}}
	got, st := durableRegistry.reconnect(s1, intent, "share")
	if st != status.Success {
		t.Fatalf("重连本应成功（同一用户、同一 share、未超时），实得 %v", st)
	}
	if got.Path != "a.txt" {
		t.Errorf("用 a.txt 的 FileId 重连，却拿回 %q 的句柄 —— 交叉句柄泄漏", got.Path)
	}
	if got == o2 {
		t.Error("拿回的是另一个会话仍在使用中的句柄 o2")
	}
}

// TestQADefectRemoveDeletesForeignEntry
//
// durableTable.remove 只按 open.Durable.key 删除，**不校验表里那条记录是否
// 真的属于这个 open**。碰上键碰撞时，A 的 CLOSE 会把 B 的登记删掉，
// B 之后再也无法重连（静默失效，日志里什么都看不到）。
func TestQADefectRemoveDeletesForeignEntry(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctx1, s1, tree1 := qaSession(t, conn, 1, "alice", "share")
	ctx2, s2, tree2 := qaSession(t, conn, 2, "alice", "share")

	o1 := qaAddOpen(t, s1, tree1, "a.txt", &fakeHandle{})
	grantDurable(t, ctx1, o1, dhqReq(wire.OplockLevelBatch))
	o2 := qaAddOpen(t, s2, tree2, "b.txt", &fakeHandle{})
	grantDurable(t, ctx2, o2, dhqReq(wire.OplockLevelBatch))

	// B 先断连进入等待重连态（真实时序：掉线的那条连接先走）。
	durableRegistry.disconnect(o2)
	// A 随后在自己那条连接上显式 CLOSE —— 它按键删除，删掉的是 B 的记录。
	o1.close()

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: o2.Persistent, Volatile: o2.Volatile}}
	if _, st := durableRegistry.reconnect(s2, intent, "share"); st != status.Success {
		t.Errorf("A 的 CLOSE 摧毁了 B 的 durable 登记：B 重连得到 %v", st)
	}
}

// --- 缺陷 2：超时回收只从 map 删记录，不关底层句柄 ---

// TestQADefectExpiredEntryClosesHandle
//
// reap()（以及 reconnect 里的过期分支）只 `delete(r.entries, k)`，
// 从不调用 e.open.close()。于是超时的持久句柄：
//   - 底层 vfs.Handle 永不关闭 → fd 泄漏，Windows 上还会顶着文件不让删/改名；
//   - delete-on-close 语义永远不会触发；
//   - Open 自身也不会标记 closed。
//
// 判据可证伪：countingHandle.closes 从 0 变成 1 即为修好。
func TestQADefectExpiredEntryClosesHandle(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 5 * time.Millisecond

	conn := NewConn(&Settings{}, "test", "test")
	ctx, s, tree := qaSession(t, conn, 1, "alice", "share")
	h := &countingHandle{}
	open := qaAddOpen(t, s, tree, "f.txt", h)
	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	time.Sleep(30 * time.Millisecond)
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

// TestQADefectExpiryHappensWithoutReconnect
//
// reap() 在整个代码库里只有一个调用点：reconnect()。没有定时器、没有常驻
// goroutine、Session.Close 与 Conn.Close 都不调它。
// 后果：客户端断线后**再也不回来**（最常见的情形）时，超时形同虚设——
// 记录连同 *Open 与 fd 永久留在包级 map 里。这是一条无界增长的内存/fd 泄漏，
// 且不需要认证之外的任何条件即可持续制造（每次连接开一个 batch-oplock
// durable 句柄然后掉线）。
//
// 判据：等到超时时间的 6 倍之后，登记表应自行清空。
func TestQADefectExpiryHappensWithoutReconnect(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 20 * time.Millisecond

	conn := NewConn(&Settings{}, "test", "test")
	ctx, s, tree := qaSession(t, conn, 1, "alice", "share")
	open := qaAddOpen(t, s, tree, "f.txt", &countingHandle{})
	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	s.Close() // 真实断连路径

	time.Sleep(120 * time.Millisecond) // 6 倍超时

	if n := len(durableRegistry.entries); n != 0 {
		t.Errorf("超时后无人重连时登记表未被回收，仍有 %d 条 —— 没有后台回收者", n)
	}
}

// --- 缺陷 3：DH2Q 的 persistent 位让整个 CREATE 失败 ---

// TestQADefectPersistentFlagDegradesNotFails
//
// MS-SMB2 §3.3.5.9.12：DH2Q 里 SMB2_DHANDLE_FLAG_PERSISTENT 只有在
// TreeConnect.Share.IsCA 且服务端宣告 SMB2_GLOBAL_CAP_PERSISTENT_HANDLES
// 时才升级为 persistent；否则处理继续走普通 durable v2 授予流程，
// **不是**让 CREATE 失败。当前实现直接返回 STATUS_NOT_SUPPORTED，
// 会把「顺手带上 persistent 位」的客户端整个 CREATE 打掉（文件都打不开）。
//
// 判据：带 persistent 位 + batch oplock 应当降级授予普通 durable v2，
// CREATE 成功。
func TestQADefectPersistentFlagDegradesNotFails(t *testing.T) {
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
		t.Error("降级后应授予普通 durable v2")
	}
}

// --- 缺陷 4：未授权的请求可以驱逐别人的登记 ---

// TestQADefectEvictionRequiresAuthorization
//
// reconnect 的检查次序是：存在 → 未作废 → 未过期 → **share** → **身份**。
// 前两个「删除」分支（Invalidated、deadline 为零或已过期）都发生在
// share/身份校验**之前**，因此任何一个已认证会话（含 guest）都能用
// 猜到的键（v1 的键就是 1,2,3… 这样的小整数）把别人**正在使用中**的
// durable 登记删掉。
//
// 判据：bob 的探测不应改变 alice 的登记表状态。
func TestQADefectEvictionRequiresAuthorization(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 30 * time.Second

	conn := NewConn(&Settings{}, "test", "test")
	ctxA, sA, treeA := qaSession(t, conn, 1, "alice", "share")
	open := qaAddOpen(t, sA, treeA, "secret.txt", &fakeHandle{})
	grantDurable(t, ctxA, open, dhqReq(wire.OplockLevelBatch))
	before := len(durableRegistry.entries)

	// bob 猜键探测（open 仍在使用中 → deadline 为零分支）。
	conn2 := NewConn(&Settings{}, "test", "test")
	_, sB, _ := qaSession(t, conn2, 1, "bob", "share")
	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{
		Persistent: open.Persistent, Volatile: open.Volatile}}
	if _, st := durableRegistry.reconnect(sB, intent, "share"); st == status.Success {
		t.Fatal("bob 竟然重连成功")
	}

	if after := len(durableRegistry.entries); after != before {
		t.Errorf("bob 的越权探测删掉了 alice 的登记：%d → %d 条（删除动作发生在身份校验之前）",
			before, after)
	}
}
