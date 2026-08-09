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

// --- 已修复并移出本文件的缺陷（用例已转为回归保护，勿在此重建）---
//
//	v1 键跨会话碰撞     → durable_qa_test.go TestQADurableV1KeyNoCrossSessionCollision
//	remove 误删他人登记 → durable_qa_test.go TestQADurableRemoveChecksEntryOwnership
//	超时回收不关句柄    → durable_qa_test.go TestQADurableExpiredEntryClosesHandle
//	驱逐先于鉴权        → durable_qa_test.go TestQADurableEvictionRequiresAuthorization

// --- 残留：无任何后续流量时，最后一批过期项不会被回收 ---

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
