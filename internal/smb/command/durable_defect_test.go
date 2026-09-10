//go:build qadefect

package command

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
//	persistent 位打死 CREATE → durable_qa_test.go TestQADurablePersistentFlagDegrades
//	                          （另见 create_context_durable_test.go
//	                            TestDurableGrantV2PersistentDegrades，含响应侧断言）

// --- 本文件当前**没有**残留缺陷 ---
//
// 最后一条（无任何后续流量时最后一批过期项不被回收）已修：durable.go 给每条
// 记录挂了一次性 time.AfterFunc（armLocked），销毁点是 dropLocked。用例已
// 转为回归保护，见 durable_qa_test.go 的
// TestQADurableExpiredEntryReclaimedWithoutReconnect。
//
// 按项目规矩，缺陷修好后用例必须**搬出本文件**而不是删掉 —— 用例总数只许
// 增加，不许靠删用例让 CI 变绿。
