---
name: save.sh 的校验盲区（传文件路径 = 零校验）
description: scripts/save.sh 传文件路径时完全跳过编译校验；且 go build 不编译 _test.go，要用 go vet
type: project
---

`scripts/save.sh` 的「绿色」在两种常见情形下是**假的**，别信它。

**盲区一：传文件路径时，编译校验被整个跳过。**
`scripts/save.sh` 里的守卫是 `if [ -d "$REPO/$p" ] && ...`，要求参数是**目录**。
而 AGENTS.md §7.2 与每个 agent 的 prompt 都要求传**文件路径**（多 agent 共用工作树，
传路径才能彼此隔离）。传文件时 `[ -d ]` 为假 → 循环体跳过 → 直接 commit + push，
**一行 `go build` 都没跑**。

识别方法：save.sh 输出里如果**没有** `>>> go build ./...` 那一行，就说明什么都没校验。

**盲区二：`go build` 不编译 `_test.go`。**
所以「产品代码是好的、测试桩没跟上接口变更」这类断裂照样能推上去，
表现为别人 `go test ./<包>/` 整包 build failed 而 `go build ./...` 是绿的。
多 agent 并行改共享接口（如 vfs 的可选接口加方法）时这类断裂非常频繁。

**Why:** 本轮全队反复出现「有人推了编译不过的代码」并互相阻塞（info、server 都被卡过、
无法验证自己的模块），根因就是这两条，不是个人疏忽。

**How to apply:**
- 提交前**手动**跑 `CGO_ENABLED=0 go vet ./internal/<你的包>/...`。
  `go vet` 会类型检查测试文件，同时覆盖编译 + 测试编译；实测同一棵树上
  `go build ./...` 绿而 `go vet` 能报出「测试桩没实现新接口」。
- 修 save.sh 的方向：把文件路径 `dirname` 成包目录去重后校验，并把 `go build`
  换成 `go vet`。`scripts/` 属 qa，改之前先和 qa/team-lead 打招呼。
