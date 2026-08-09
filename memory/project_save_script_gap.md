---
name: save.sh 的三道门（历史盲区已修，别再拆掉）
description: scripts/save.sh 现在有 go vet / gofmt / 路径归约三道门；记录它们各自是为堵哪个真实事故而加的
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

**How to apply:**
- 改 save.sh 前先想清楚要拆的是哪一道门、它当初堵的是什么。`scripts/` 属 qa。
- 给这类"门禁"做改动后**必须做反向测试**：造一个应当被拦的输入，确认它真的
  非零退出且没有产生提交。验证方法：在 `/tmp` 里 `git clone` 一份、把 origin 指向
  一次性裸库再测，这样"万一没拦住"也只会污染那个裸库，不会推上 main。
