---
name: 不要把失败用例搬到 build tag 后面来止血
description: 缺陷复现用例只在根因未修时才挂 qadefect tag；根因已在别人分支修好时，用例应留在默认路径裸奔，搬走等于用隐藏症状代替修根因
type: feedback
---

CI 因某条**故意失败**的缺陷复现用例而红时，**先查根因是不是已经有人在修**，
不要顺手把它挪到 `//go:build qadefect` 后面让 CI 变绿。

**Why**：2026-08-09 修 main 红灯时，我把 `TestQADurableReconnectRebindsTree`
从 `durable_qa_test.go` 搬进 `durable_defect_test.go`（还改名加了 `TestQADefect` 前缀）。
team-lead 当场缩范围并要求回退：qa-proto 已经在 `qa-proto/durable-fix` 上修掉了根因
（`reconnect()` 只写 `open.Session` 不写 `open.Tree`，导致 `close.go` 的跨树守卫把重连后
所有命令打回 `STATUS_INVALID_PARAMETER`、句柄永久泄漏）。也就是说那已经是一条
**即将转正的回归用例**，搬走等于「用隐藏症状代替修根因，方向反了」。

`durable_defect_test.go` 文件头本来就写明了这套规矩，我当时读了却理解反了：
- tag 后面放的是**根因未修**的复现用例（失败即缺陷报告的证据）；
- **每修好一个就去掉 tag 挪回** `durable_qa_test.go` 当回归保护；
- 「不要靠删除用例来『修复』失败」——加 tag 藏起来和删除是同一类动作。

**How to apply**：接到「让 CI 变绿」的任务时，对每条失败用例先分类：
(a) 门禁/配置类问题（如 build tag 未注册）→ 归我修；
(b) 产品代码真 bug → 查 `git log --all --oneline` / 问 team-lead 有没有人在修，
有人修就**别动测试**，红灯让它红着，在报告里写明归属。
判断标准是「根因有没有人负责」，不是「哪种改法能最快变绿」。
拿不准就问 team-lead 要范围，别自作主张扩大改动面。
