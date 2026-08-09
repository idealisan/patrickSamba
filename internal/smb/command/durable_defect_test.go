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
	got, st := durableRegistry.reconnect(s1, tree1, intent)
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
	if _, st := durableRegistry.reconnect(s2, tree2, intent); st != status.Success {
		t.Errorf("A 的 CLOSE 摧毁了 B 的 durable 登记：B 重连得到 %v", st)
	}
}

// --- 缺陷 2 的残留：无任何后续流量时，最后一批过期项不会被回收 ---

// TestQADefectExpiryHappensWithoutReconnect —— **已知残留，团队已裁决接受**。
//
// 原缺陷是「reap() 全库只有 reconnect() 一个调用点」，导致无界增长。现已改为
// 机会式回收：register()（有新句柄进来）与 Session.Close()（有旧连接离开）
// 各扫一遍。**无界增长因此被堵死**，正向证据见 durable_qa_test.go 的
// TestQADurableExpiredEntriesReclaimedByNewRegistrations（50 轮，表恒定）。
//
// 残留的是本用例这个极端情形：最后一批句柄断连之后，服务端**再没有任何
// SMB 流量**，于是没人触发回收，那一批记录会留到下一次有人连进来为止。
// 数量上界 = 最后一条连接持有的 durable 句柄数，不随时间增长。
//
// team-lead 的设计约束是「不要起常驻定时器 goroutine」（理由：本项目里
// 『起了个 goroutine 但没人管它生命周期』是另一类坑），所以这条**故意不修**，
// 留在 qadefect tag 下当作已知残留的记录。若日后要消掉它，成本最低的做法是
// 给每条 entry 挂一个 time.AfterFunc，并在 reconnect/remove 时 Stop()
// —— 那是有明确宿主与销毁点的一次性定时器，不是常驻 goroutine。
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
