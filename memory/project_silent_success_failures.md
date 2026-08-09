---
name: 「成功回显」不等于事情真的发生了
description: 本项目反复出现同一类事故——命令/门禁/接口给出正反馈（或干脆沉默），但要做的事一件没做。已知八个实例与各自的自查命令
type: project
---

本项目已累计**八个**同形态事故：系统给了正反馈（打印成功、CI 有记录、测试 0 失败），
或者**什么都不说**（第 8 条），但要做的事根本没发生。它们看起来毫不相关，实际是同一个病。

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
| 7 | **`go build ./...` 不编译 `_test.go`**。改函数签名时分两步做（先改声明与产品代码调用方，测试调用方留到下一步），中间提交 `go build` 绿灯放行、推送成功，实际 `go vet`/`go test` 直接编译失败。该提交进了 main，成为 `git bisect` 地雷（撞上与被查 bug 无关的编译错误） | build 绿、push 成功 | 推送前用 `sh test/ci/check-test-compile.sh`（它跑 `go vet -tags ... ./...` × 4 平台），**不能只用 `go build ./...`**。AGENTS.md §7.2 已按此更新 |

| 8 | **`cnb pulls create-pull` 是不存在的子命令，CLI 打印帮助文本后退出 `0`，PR 从未被创建**。2026-08-09 16:0x pm 提 `pm/status` 的 PR，管道尾接了 `grep -E '^  (number\|state\|title):'`——帮助文本一行都匹配不上，于是**「子命令名写错」被伪装成「输出为空、结果不明」**；十几分钟后查开放 PR 列表才发现根本没有它。正确子命令是 **`cnb pulls post-pull`**（同族：`cnb issues list` 也不存在，正确的是 `list-issues`，同样是打印帮助 + 退出 0） | 退出码 0，无任何报错 | 改变共享状态的 CNB 命令（建 PR / 合 PR / 发评论 / 改 Issue）执行后，**用一条独立的读命令回查**（`list-pulls` / `get-pull` / 查 comment id），不以原命令 stdout 为准。**且不要用 `grep` 过滤这类命令的输出**——过滤器匹配不到时输出同样为空，「命令错了」与「成功但没回显」外观完全一致。先看原始输出里有没有 `status: 201` |

推论：**`skipped`、`0 failures`、`no tests to run`、`已推送` 四种输出都不构成证据。**
要么有独立探针，要么有反向对照（故意破坏一次，确认会红）。
**第 8 条把这条推到极端：输出为空时，「命令没生效」与「生效了但不回显」外观完全相同**，
只能靠独立回查区分。同一个人在同一小时内对 `post-issue-comment` 做了回查
（拿到 `status: 201` + comment id 才收工）、对 `create-pull` 却忘了 ——
**说明它不能靠「记得做」，必须写进流程。**

**第 8 条还揭出一条更普遍的**：`cnb` CLI 遇到**未知子命令时打印帮助并退出 `0`**，
不报错、不返回非零码。这意味着**拼错子命令名与执行成功在退出码上无法区分**，
而习惯性接一个 `| grep` 会把唯一的线索（帮助文本）也吃掉。
**用不熟的 `cnb` 子命令前先 `cnb <模块> --help` 确认名字存在**，比事后排查便宜得多。

**跨平台代码的特例（第 6 条的一般化）**：`internal/vfs/metadata_windows.go`、
`internal/meta/bolt.go` 这类 Windows-only 文件，在本容器（linux）的所有常规门禁下
**从未被编译过**。给它们留一个 `metabolt` 之类的构建 tag 逃生口，
并在 CI 里补 `GOOS=windows go vet ./...`，否则语法错误都能一路绿到发布。
