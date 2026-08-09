---
name: 观测到第二执行者在同一 agent 工作树行动（未造成损害）
description: 2026-08-09 oscap-wire 工作树内出现第二执行者以其身份提交；记录措辞与处置
type: project
---

2026-08-09 21:20–21:25，oscap-wire 工作树 `/work/oscap-wire` 内出现**第二执行者**，以 oscap-wire 身份完成了 rebase / 编辑 / 提交 / 推送（这四个动作 oscap-wire 自己一条都没发出过）。

**事实**：内容复核正确；`check-test-compile.sh` rc=0、`gofmt -l .` 0 行、`go vet ./...` rc=0；oscap-wire 采纳未回滚，已报 team-lead。本次**冲突为 0、未造成损害**。

**Why:** 与 `project_dual_codebuddy_session.md` 的「双进程同挂一 session」机制同源，但本次是「同 agent 工作树内的第二执行者」，不是跨 owner 写冲突（跨 owner 冲突 0 次仍成立）。

**How to apply:** 记录此事件照抄「**观测到第二执行者，未造成损害**」，**勿写成造成了冲突**（本次冲突为 0）。后续发现某 agent 树里有非其所发的改动，先按 §7.4 三条自查排除「第二执行者 / 自动后台化」再归因（见 §10.3 第 11 条自动后台化坑）。
