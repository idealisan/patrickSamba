---
name: save.sh 的三道门（历史盲区已修，别再拆掉）
description: scripts/save.sh 的 go vet / gofmt / 路径归约三道门各堵哪次事故；外加推送段三个「照常打印、事情没发生」的已修 bug 与现成的反向测试
type: project
---

`scripts/save.sh` 现在在 commit 之前有三道门。它们**都是为堵一次真实事故加的**，
看起来啰嗦，别顺手简化掉。

**门一：路径归约后再 vet。**
以前的守卫是 `if [ -d "$REPO/$p" ]`，要求参数是**目录**；而 AGENTS.md 要求各 agent 传
**文件路径**（共用工作树，传路径才能彼此隔离）。传文件时条件为假 → 校验被整个跳过 →
直接 commit + push，**一行编译都没跑**。现在改成把文件路径 `dirname` 成包目录、
去重、再用 `go list` 展开成真实存在的包（`configs/` 没有包、`scripts/clients/gosmb2`
是独立 module，直接喂给 vet 会因 pattern 匹配不到而 `set -e` 退出）。

**门二：用 `go vet -tags=integration,smoke` 而不是 `go build`。**
`go build` **根本不编译 `_test.go`**，"产品代码好的、测试桩没跟上接口变更"照样能推上去；
不带 tag 则 `test/integration`、`responder_integration_test.go`、`shutdown_smoke_test.go`
被整体排除，等于没校验。vet 还不落产物，不会像 `go build` 那样把二进制吐在仓库根被
别人 `git add -A` 裹挟。

**门三：gofmt 门禁（只查本次提交涉及的路径）。**
只跑 vet 不跑 gofmt 时，不合规代码一路进 main，CI 的 gofmt 门禁在推之后才跑又没人盯，
"门禁看着存在、实际拦不住东西"。范围刻意不扫全仓库——共用工作树里全仓库扫会把别人
未提交的中间态一起报出来，养成"忽略这个报错"的习惯，门禁就又废了。
发现不合规**直接 exit 1 并打印 `gofmt -l` 结果，不自动改写**（自动改写会让提交内容
和作者以为的不一致）。

**Why:** 本项目栽过两类事故——全队互相推不能编译的代码而彼此阻塞；以及不合规/半成品
代码直到发布前才被发现。这三道门分别对应这些根因。

**推送段（第 3 节）另有三个已修 bug（PR #17，2026-08-09）**，形态都是"照常打印、
事情没发生"，改这段前务必了解：
1. 写死 `git push origin main` → worktree 共享 .git，refspec `main` 解析成**本地 main**，
   自己的提交一个都没出去还打印成功。已改为读当前分支。
2. retry 里 `git pull --rebase origin main` 恒定 rebase 到 main → 特性分支上既拿不到
   `origin/$BRANCH` 的新提交（6 次重试全空转），又白改写本分支历史。已改为
   rebase 到 `origin/$BRANCH`，且**只在 `git ls-remote --heads` 确认远端有该分支时才 rebase**
   （远端没有时失败原因是鉴权/网络，rebase 治不了病只添乱）。
3. 锁写在 `$REPO/.git/` 下 → worktree 里 `.git` 是**文件**，mkdir 恒 ENOTDIR，
   空转 120 秒后超时退出，**worktree 工作流下 save.sh 100% 推不出去**。
   已改为 `git rev-parse --git-common-dir`；rebase 状态检测改 `--git-path`。

**How to apply:**
- 改 save.sh 前先想清楚要拆的是哪一道门、它当初堵的是什么。`scripts/` 属 qa。
- 给这类"门禁"做改动后**必须做反向测试**。现在已有现成的：
  `test/ci/save-sh-scenarios.sh`（离线，本地裸库当 origin，4 场景 17 断言，
  断言查的是**远端实际状态**不是脚本输出）。改完必须再跑一次
  `test/ci/save-sh-scenarios.sh test/ci/testdata/save-legacy.sh`（修复前的冻结快照），
  **它必须 FAIL**；若它也全绿，说明测试没覆盖到被修的东西，是测试先坏了。
- 沙箱里 git 提交要 `git config commit.gpgsign false`，否则全局签名配置会让
  假作者直接 403 `Author is invalid`。
