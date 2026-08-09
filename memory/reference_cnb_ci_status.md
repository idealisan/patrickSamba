---
name: 查 CNB 流水线状态的正确姿势，以及「假红」陷阱
description: CNB 没有 commit statuses API（404）；查构建用 /-/build/logs?sourceRef=；同一 commit 的 push 与 pull_request 事件跑不同版本的 .cnb.yml，结论可以相反
type: reference
---

## 查构建状态的端点

CNB **没有** GitHub 那种 `/-/commits/<sha>/statuses`——直接返回
`{"errcode":5,"errmsg":"Resource not found."}`。正确端点是：

```sh
curl -sS -H "Authorization: Bearer $CNB_TOKEN" \
  -H "Content-Type: application/json" -H "Accept: application/json" \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/build/logs?sourceRef=<分支名>&page_size=20"
```

返回 `.data[]`，每条含 `status`（`success` / `error` / `pending`）、`sha`、
**`event`（`push` 或 `pull_request`）**。仓库里已有封装：`scripts/ci-status.sh`
（退出码 0=绿 1=红 2=跑着 3=无记录 4=调用失败，可直接用在 `&&` 里）。

## 「假红」陷阱（**同一个 commit 会有两条结论相反的记录**）

`push` 事件用**分支自己那份** `.cnb.yml` 跑；
`pull_request` 事件跑的是**与 `main` 的合并预览**，用的是合并后的 `.cnb.yml`。

于是：**分支是在某次 CI 修复之前切出去的 → push 必红、PR 事件却是绿的**，
而这跟分支上的代码质量毫无关系。

2026-08-09 实测（v0.2.0 期间，9 个开着的 PR）：`.cnb.yml` 旧 → push 红，
新 → push 绿，**相关性 100%、零例外**。根因是 PR #9 之前的
`gate_test` 写成 `CGO_ENABLED=0 go test -race`，race 依赖 cgo，
go 拒绝执行、0.1 秒退出码 2，它是第一个 stage，后面四关全被 skip。

**最危险的场景不是 PR，是没有 PR 的分支**：它只有 push 记录，全红，
任何人扫一眼都会判定「这些代码是坏的」。当时 `r-infra/test-infra` 只改了
**一个纯 Markdown 文件**，照样红——这就是判定该结论的反向对照。

**Why**：R1「CI 假绿」（配置坏了却显示通过）刚修完，立刻出现镜像版的「假红」。
两次都说明同一件事：**CI 的颜色本身不是证据，
「这个颜色是哪条流水线、在哪份配置下、对哪个 commit 得出的」才是证据。**

**How to apply**：
- 判断某分支能不能合，看 **`event=pull_request` 且 `sha` 等于分支 tip** 的那条，
  不要看 push 那条，也不要只取 `page_size=1`（最新一条可能是另一种事件）。
- 看到红灯先比一句
  `git rev-parse origin/<分支>:.cnb.yml` 与 `git rev-parse origin/main:.cnb.yml`，
  blob 不同就先怀疑假红。
- 让 push 也变绿不需要改代码，分支上 `git pull --rebase origin main` 即可。
