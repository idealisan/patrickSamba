---
name: 合入引入新 build tag 的 PR 会当场打红整个仓库
description: test/ci/check-test-compile.sh 有未知 tag 守卫，新 tag 必须先登记进 TAGS，否则合入瞬间所有分支全红
type: project
---

`test/ci/check-test-compile.sh` 维护一份 build tag 白名单（`TAGS` / `KNOWN`）。
**任何带新 build tag 的文件一旦合进 main，这道门立刻失败，于是 main 和所有基于它的分支同时变红。**

**Why:** 这道守卫是故意的，防的是本项目栽过的「build tag 后面的代码从未被编译」——
带某个 tag 的文件如果没有任何一关会编译它，它就成了永远不会被发现的死代码
（`test/` 目录曾整体处于这个状态）。所以脚本宁可报错也不放行未知 tag。

真实事故（2026-08-09）：team-lead 合入 PR #29（带 `//go:build qadefect` 的缺陷复现用例）时
没先跑这道门，结果 main 当场变红，`win-meta/metadata-store`(#26) 与 `win-vfs/openhost-seam`(#30)
都在 19~20 秒内失败——**两个 agent 差点去排查自己根本没有的 bug**。
同源的第二例：`internal/meta/noop.go` 的 `!metabolt` 是仓库里第一个**反向**约束，
脚本的 `case "!*"` 分支直接 `exit 1`，同样是「守卫工作正常，只是没登记」。

**How to apply:**
- **合入任何引入新 build tag 的 PR 之前，先在本地跑 `bash test/ci/check-test-compile.sh`。**
  这一步几秒钟，比事后救火便宜得多。
- 登记新 tag 时要分清两件事：进 **union 编译**那一关（`go vet -tags`）是必须的；
  **是否要进 `gate_test` 实际运行则要单独判断**。像 `qadefect` 这种「缺陷复现用例，
  修好之前故意失败」的 tag，只能编译不能运行，否则 CI 恒红。
- 反向约束（`!tag`）的正确处理是把该 tag 并进 `KNOWN`/`TAGS`、与 `!windows` 走同一套：
  union pass 编译带 tag 的那一份，不带 tag 的那一份由无 tag 的 `gate_build`/`gate_test` 覆盖，
  两侧都有人管，没有死角。
- 排查「一合入就全仓变红、且失败发生在 20 秒内」时，**先怀疑这道门**，
  不要让各分支的 owner 去查自己的代码。
