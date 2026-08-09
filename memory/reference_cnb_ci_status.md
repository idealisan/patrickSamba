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

## 第三种形态：**push 绿 / PR 红，而且 PR 的红才是真的**（2026-08-09 第 12 轮实测）

上面写的是「push 红 / PR 绿」。**反过来也会发生，且方向相反时红的那个是真的**：

`win-vfs/open-seam`（PR #31）push=`success`、pull_request=`error`。
原因是分支从**旧 main** 切出，push 只编译分支自身（不含新合入的 `qadefect` 测试文件）；
PR 事件跑「与当前 main 的合并预览」，把新文件带进来才暴露问题。

**所以「push 绿就没事」是错的。** 判据统一为一句话：**只看 `event=pull_request`。**
（老结论「别拿 push 的红拦 PR」仍然成立，两条不矛盾——都是「push 那条不可信」。）

## 该 API **不返回 stage 级明细**，排障必须本地实跑

`.data[].pipelines[].stages` 实测是**空数组**，只有
`pipelineSuccessCount / pipelineFailCount / pipelineTotalCount` 三个计数
（典型 `0/1/1`）。**光靠 API 查不出「红在哪一关」。**

要定位就在干净 worktree 上照 `.cnb.yml` 逐关手跑，例如：
`sh test/ci/check-test-compile.sh`、`sh scripts/check-constraints.sh`、
`CGO_ENABLED=0 go test ./...`、`CGO_ENABLED=1 go test -race ./...`。

## ⚠️ stage 顺序执行、失败即中断 → **第一道红灯会掩盖后面所有关卡**

2026-08-09 实测：`main` 红在第 ④ 关（`qadefect` build tag 未注册），
于是第 ⑤~⑪ 关**从未执行过**，其中第 ⑦ 关 `竞态检测` 藏着一个真实的产品级 data race
（`durable.go` 的 `remove()` 与 `disconnectedAtZero()`）。
当时全队都以为「合了修第 ④ 关的那个 PR 就绿了」——**错的，修完只是红灯前移一关。**

**Why**：这是 R1「CI 假绿」的第三次同构复发。三次的共同本质都是
**「没报错」被当成了「检查过了」**。

**How to apply**：
- 判断「修了这个红灯是不是就绿了」时，**必须把整条链跑到底**，只验自己那一关等于没验。
- 报「CI 已修复」之前，先确认自己看到的是**最后一关**的绿，而不是**下一关**还没跑。
