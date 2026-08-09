---
name: 「成功回显」不等于事情真的发生了
description: 本项目反复出现同一类事故——命令/门禁/接口给出正反馈，但要做的事一件没做。已知六个实例与各自的自查命令
type: project
---

本项目已累计**六个**同形态事故：系统给了正反馈（打印成功、CI 有记录、测试 0 失败），
但要做的事根本没发生。它们看起来毫不相关，实际是同一个病。

**Why:** 这类失败不会报错，只会「安静地什么都没做」，所以从来不是被发现的，
都是隔了很久由别的调查顺带撞出来的。v0.1.0 就是在这五个洞全开的情况下发布的。

**How to apply:** 任何「已完成」的判断都必须有**独立的第二证据**，不能只看命令回显。
下面每条都配了自查命令，做完对应动作顺手跑一次。

| # | 事故 | 假象 | 自查 |
|---|---|---|---|
| 1 | `save.sh` 写死 `git push origin main`，在特性分支 worktree 里推的是别人的本地 main | 打印「已推送」 | `git log --oneline origin/$(git branch --show-current)..HEAD` 有输出＝没推出去 |
| 2 | PR 合并关闭后继续往原分支推提交 → 孤儿提交，CI 照跑照绿，但永远进不了 main | 分支有提交、CI 全绿 | 推完查 PR 是否仍 `state=open`，关了就新开一个 |
| 3 | `CGO_ENABLED=0 go test -race` 无法执行（race 依赖 cgo），0.1s 退出码 2；它是第一个失败 stage，其后 4 关全被 skip，**自建项目起一次都没执行过** | 流水线「有在跑」 | 别只看有没有构建记录，要看**每个 stage 的 status**，`skipped` 和 `success` 不是一回事 |
| 4 | 变异测试用 `grep '^    --- FAIL'` 计数，只匹配带缩进的子测试；顶层 FAIL 没缩进 → 报 0 失败。编译失败同样让计数变 0 | 「变异没被捕获」的误判 | 变异前先跑一次**未变异基线**，确认计数器读数符合预期，否则分不清「没抓到」和「根本没跑」 |
| 5 | 安全检查写好了但没接线（`validateWindowsName` 未被 `ValidateComponent` 调用）；旧表被架空后 Go 不报 unused | 代码在、测试绿 | 新增校验函数后 grep 它的调用点；替换旧实现时**必须删掉旧的包级变量**，留着就是负资产 |
| 6 | **build tag 后面的代码在 linux 上一行都没被编译**。`internal/meta/bolt.go` 是 `//go:build windows \|\| metabolt`，裸跑 `go list -deps ./internal/meta` 只吐出包自己（bbolt 不在依赖图里）、`go test ./internal/meta` 报 `no tests to run` | 依赖核验、vet、test 全绿 | 查 `go list -deps` 结果里**有没有你要查的那个依赖**；`go test` 输出出现 `[no tests to run]` 就是没在测。带 `-tags` 或 `GOOS=windows` 重跑 |

推论：**`skipped`、`0 failures`、`no tests to run`、`已推送` 四种输出都不构成证据。**
要么有独立探针，要么有反向对照（故意破坏一次，确认会红）。

**跨平台代码的特例（第 6 条的一般化）**：`internal/vfs/metadata_windows.go`、
`internal/meta/bolt.go` 这类 Windows-only 文件，在本容器（linux）的所有常规门禁下
**从未被编译过**。给它们留一个 `metabolt` 之类的构建 tag 逃生口，
并在 CI 里补 `GOOS=windows go vet ./...`，否则语法错误都能一路绿到发布。
