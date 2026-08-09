---
name: 跨 agent 的 sha/计数出厂即过期 + 批量扫描要抽验
description: 别信队友给的 sha/计数，决策前先自己 fetch；批量扫描别换简单命令，结论出来后随机抽 1 个用精确方法复验
type: feedback
---

活跃开发期 `main` 几分钟一变，**任何跨 agent 传递的 sha / commit 计数出厂即过期**。
轮次内同一现象三轮出现了三个方向：§16.2 pm 落后于 win-meta、§17.1 win-meta 落后于 pm、
§17.9 qa-verify 落后于 pm（16:12 查 `main=b6d16b4`，16:35 复核已是 `ebd9b59`）。

**两条铁律（pm 在 §17.9 定的，对本仓库 CI 复核工作直接相关）：**

1. **收到别人的 sha/计数用于决策前，先自己 fetch 一次。** 不要直接采信。
   本轮 qa-verify 做对的关键一步就是：没信 pm 给的那批 sha，而是重新 `git ls-remote` /
   `build/logs` 取了 live 数据才回话 —— 结果 pm 的 sha 当时已经又被顶掉了。

2. **批量扫描/批量查状态时，别为了省事换更简单的命令；批量结论出来后，
   随机抽 1 个样本用精确方法复验一遍。**
   判据会随任务规模退化：逐个核对 7 个 PR 时用对了三点写法 `git diff --name-only A...B`，
   转成 55 个分支批量扫描时图快换成两点，把「落后于 main」误计成「改动」
   （声称 44 个分支动过 check-test-compile.sh，真实是 0，整节撤回）。
   批量统计产出的是拿去做决策的聚合数字，且**没有单点可供人肉核对**，最怕判据错。

**Why:** 两点 diff `A..B` 与三点 `A...B` 语义不同；用错会把「已落后于 main」算成「有改动」。
只要随机抽任意一个分支跑 `git rev-list --count <merge-base>..<branch> -- <文件>` 或
`git diff --name-only origin/main...<分支>`，当场就能看到 0。

**How to apply（qa-verify 的 CI 复核场景）：**
- 接到队友的 sha/计数并要据此行动 → 先 `git fetch` + `git ls-remote` / CNB `build/logs` 自取。
- 做全仓/全 PR 批量扫描 → 别替换命令；扫完从结果里随机挑 1 个，用最精确的单点方法复验，
  不一致就怀疑整批口径。
- 报告里始终带时间戳（AGENTS.md §7.6），让分歧可诊断。
