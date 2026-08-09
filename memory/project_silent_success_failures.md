---
name: 「成功回显」不等于事情真的发生了
description: 本项目反复出现同一类事故——命令/门禁/接口给出正反馈（或干脆沉默），但要做的事一件没做。已知十四个实例与各自的自查命令
type: project
---

本项目已累计**十四个**同形态事故：系统给了正反馈（打印成功、CI 有记录、测试 0 失败），
或者**什么都不说**（第 8 条），但要做的事根本没发生。它们看起来毫不相关，实际是同一个病。

**Why:** 这类失败不会报错，只会「安静地什么都没做」，所以从来不是被发现的，
都是隔了很久由别的调查顺带撞出来的。v0.1.0 就是在这五个洞全开的情况下发布的。

**How to apply:** 任何「已完成」的判断都必须有**独立的第二证据**，不能只看命令回显。
下面每条都配了自查命令，做完对应动作顺手跑一次。

| # | 事故 | 假象 | 自查 |
|---|---|---|---|
| 1 | `save.sh` 写死 `git push origin main`，在特性分支 worktree 里推的是别人的本地 main | 打印「已推送」 | `git log --oneline origin/$(git branch --show-current)..HEAD` 有输出＝没推出去 |
| 2 | PR 合并关闭后继续往原分支推提交 → 孤儿提交，CI 照跑照绿，但永远进不了 main | 分支有提交、CI 全绿、**`git push` 打印 ok 且退出 0** | **每次 push 前**查自己的 PR 是否仍 `state=open`（`cnb pulls list-pulls --repo <repo> --state open` 里还有没有自己那条），关了就新开一个。**注意是「推前」不是「推完」**——失效是**别人合并你的 PR** 造成的，不是你自己的动作，所以任何一次 push 之前它都可能已经发生。2026-08-09 pm 复现：16:23 建 #125 并回查确认 open，~16:30 被人合并，16:30:48 再 push 时 push 照样打印 ok，提交却成了孤儿，另开 #130 才救回 |
| 3 | `CGO_ENABLED=0 go test -race` 无法执行（race 依赖 cgo），0.1s 退出码 2；它是第一个失败 stage，其后 4 关全被 skip，**自建项目起一次都没执行过** | 流水线「有在跑」 | 别只看有没有构建记录，要看**每个 stage 的 status**，`skipped` 和 `success` 不是一回事 |
| 4 | 变异测试用 `grep '^    --- FAIL'` 计数，只匹配带缩进的子测试；顶层 FAIL 没缩进 → 报 0 失败。编译失败同样让计数变 0 | 「变异没被捕获」的误判 | 变异前先跑一次**未变异基线**，确认计数器读数符合预期，否则分不清「没抓到」和「根本没跑」 |
| 5 | 安全检查写好了但没接线（`validateWindowsName` 未被 `ValidateComponent` 调用）；旧表被架空后 Go 不报 unused | 代码在、测试绿 | 新增校验函数后 grep 它的调用点；替换旧实现时**必须删掉旧的包级变量**，留着就是负资产 |
| 6 | **build tag 后面的代码在 linux 上一行都没被编译**。`internal/meta/bolt.go` 是 `//go:build windows \|\| metabolt`，裸跑 `go list -deps ./internal/meta` 只吐出包自己（bbolt 不在依赖图里）、`go test ./internal/meta` 报 `no tests to run` | 依赖核验、vet、test 全绿 | 查 `go list -deps` 结果里**有没有你要查的那个依赖**；`go test` 输出出现 `[no tests to run]` 就是没在测。带 `-tags` 或 `GOOS=windows` 重跑 |
| 7 | **`go build ./...` 不编译 `_test.go`**。改函数签名时分两步做（先改声明与产品代码调用方，测试调用方留到下一步），中间提交 `go build` 绿灯放行、推送成功，实际 `go vet`/`go test` 直接编译失败。该提交进了 main，成为 `git bisect` 地雷（撞上与被查 bug 无关的编译错误） | build 绿、push 成功 | 推送前用 `sh test/ci/check-test-compile.sh`（它跑 `go vet -tags ... ./...` × 4 平台），**不能只用 `go build ./...`**。AGENTS.md §7.2 已按此更新 |

| 8 | **`cnb pulls create-pull` 是不存在的子命令，CLI 打印帮助文本后退出 `0`，PR 从未被创建**。2026-08-09 16:0x pm 提 `pm/status` 的 PR，管道尾接了 `grep -E '^  (number\|state\|title):'`——帮助文本一行都匹配不上，于是**「子命令名写错」被伪装成「输出为空、结果不明」**；十几分钟后查开放 PR 列表才发现根本没有它。正确子命令是 **`cnb pulls post-pull`**（同族：`cnb issues list` 也不存在，正确的是 `list-issues`，同样是打印帮助 + 退出 0） | 退出码 0，无任何报错 | 改变共享状态的 CNB 命令（建 PR / 合 PR / 发评论 / 改 Issue）执行后，**用一条独立的读命令回查**（`list-pulls` / `get-pull` / 查 comment id），不以原命令 stdout 为准。**且不要用 `grep` 过滤这类命令的输出**——过滤器匹配不到时输出同样为空，「命令错了」与「成功但没回显」外观完全一致。先看原始输出里有没有 `status: 201` |
| 9 | **Go 不向上传播 skip：子测试全 `t.Skip`，父测试照报 `--- PASS`**。而 `-v` 输出里子测试的结果行是**缩进**的、顶层的顶格，于是「顶层 PASS 条数 == `-list` 枚举条数」与「`^--- SKIP` 为 0」两条判据同时成立——六项能力往返一条没跑，门禁全绿。同源变体更狠：把 `t.Run(name, fn)` 换成立即调用的空壳，**连 SKIP 痕迹都不留** | 35/35/0 三个数字全对 | 数 SKIP 时匹配**任意缩进层级**（`^[[:space:]]*--- SKIP:`），再加一条「**所有层级**结果行总数」的下界；缺后者则「子测试整体不执行」完全隐形。2026-08-09 `test/ci/portable-mode.sh` 变异体 C/D 实证，两条判据各由一个变异体守着 |
| 10 | **本容器 git 输出是中文，用英文正则 grep 判冲突恒定假绿**。`git merge-tree --write-tree A B \| grep -c '^<<<<<<<\|^CONFLICT\|changed in both'` 恒返回 0，于是「有冲突」被读成「零冲突」，据此给出过合并绿灯 | `grep -c` 给一个漂亮的 `0` | **用退出码**：`git merge-tree --write-tree A B >/dev/null 2>&1; echo $?`（0=干净 1=冲突）。要文件名用结构化列：`git merge-tree --write-tree A B \| awk '$3==1 {print $4}'`（第 3 列是 stage 位，语义不随文案变） |
| 11 | **CNB 的 squash 也生成「Merge pull request #N — …」这个标题**，于是靠标题判断「提交被保留了」恒定假绿。2026-08-09 21:0x 实测：`e4f0f80`「Merge pull request #129 — oscap/builtin: …」**父数=2**（真合并），`d351683`「Merge pull request #138 — oscap/native: …」**父数=1**（squash）——同系列相邻两个 PR，标题模板一字不差，结构相反 | `git log --oneline \| grep 'Merge pull request'` 满屏命中 | `git log -1 --format='%p' <sha> \| wc -w`（1=squash 2=真合并）；判自己分支祖先链断没断用 `git merge-base --is-ancestor <分支tip> origin/main; echo $?`（0=提交被保留 1=被 squash）。**⚠️ 这条 `is-ancestor` 本身有假阳性，必须先过一道「合了没」的门，见第 12 条** |
| 12 | **`--is-ancestor` 把「还没合入」和「squash 合入」压成同一个 `rc=1`**，于是「PR 还在排队」被读成「已被 squash、快去 rebase」。2026-08-09 21:41 实测：`is-ancestor 524bb43 origin/main` → rc=1，而真相是 `origin/main` 还停在 `762e335`、#171 一行都没进去。**同一族的第二个实例出在校验脚本自己身上**：用 `grep -qxF "$l"` 逐行核对索引超集，而每行都以 `- [` 开头 → grep 把它当选项、报「无效的选项」，循环照跑，最后打印出「7 行全部缺失」这个**看起来完全合理的结论**（真值是缺 1 行）。**校验器坏掉时不会说自己坏了，它会给你一个答案** | rc 是个「合法」的值；汇总行给出干净的数字 | 判据要能区分**三种**状态而不是两种：先用**内容门**确认事情发生了没（`git show origin/main:<文件> \| grep -c '<被删掉的那句>'`，1=没合 0=已合），再用 `is-ancestor` 判形态。逐行核对一律写 `grep -qxF -- "$l"`（`--` 终止选项解析），并**先跑一次已知答案的基线**确认计数器读数正确（同第 4 条） |
| 13 | **`git merge-tree --write-tree` 冲突时照样输出一个合法的树 SHA，而那棵树里的文件带着 `<<<<<<<` / `=======` / `>>>>>>>` 冲突标记**。于是 `TREE=$(git merge-tree --write-tree A B)` 这种写法拿到的是一棵**污染树**，再 `git commit-tree "$TREE"` 落地就把冲突标记提交进了仓库，全程零报错。2026-08-09 21:47 实测：模拟「#171 被 squash 进 main」后与 #159 求合并树，退出码 1，但仍吐出 `cd21017a`；拿它与 rebase 结果树 diff，**135 行「差异」全部是冲突标记**，四个文件（AGENTS/CHANGELOG/README/example.yaml）无一幸免 | 命令替换成功、拿到一个像模像样的 40 位 SHA | **必须接退出码**：`TREE=$(git merge-tree --write-tree A B) || { echo 冲突; exit 1; }`。⚠️ 与第 10 条区分：那条是**只看 stdout 用错了 grep**，这条是**stdout 本身就不能用**；`merge-tree ... >/dev/null 2>&1; echo $?` 这种只取退出码的写法两条都不沾，是安全的 |
| 14 | **`git merge` 干干净净地 rc=0、零冲突，却把已经在 `main` 里的内容悄悄删掉了** —— 因为参与合并的一侧是**比 main 还旧的快照**，那些「它没有的行」被三方合并判成「这一侧删除了它们」而照单执行。2026-08-09 22:03 实测：把 `~/.codebuddy` 记忆工作副本的并集快照 `aa97d67` 合进 PR #172 的 `d0beb47`，`project_timing_criteria_flaky.md` **净删 62 行**（main 是 70 行的详版，工作副本是 32 行的旧缩写稿）、`project_config_path_platform_semantics.md` **净删 13 行**（整节 2.1 消失），两个文件**都不在冲突列表里**，`git status` 一片干净 | 合并报告只列「冲突」，不列「一侧删了什么」；文件还在、内容变短了没人看得出来 | 合并后必查一次删除面：`git diff --numstat <base> HEAD -- <目录>` 挑出 `deletions>0` 的文件**逐个人工确认是有意替换**（整行改写会记成 1 删 1 加，属正常）。**工作副本不是权威源**——它可能停在几小时前，凡是拿它做并集都要以 `origin/main` 为 base 做三方合并，不能拿它整棵树覆盖 |

**第 11 条附带一个抽样教训**：同一个仓库**两种合并形态都用过**——main 上 64 个父数≥2 的
真合并提交**全部在 2026-08-09 17:17:41 之前**，17:36:31 起连续 13 次 PR 落地**父数全 = 1**。
两个人各自抽到一段，一个断言「CNB 保留提交」、一个断言「CNB 是 squash」，**都错在把
自己看到的那一段当成仓库属性**。合并形态是**每次合并时的选择**，不是仓库常量，
所以**只能在合并发生之后现场判定**，任何提前写死的结论都会在某一次翻车。

**第 10 条有两个互相独立的失效原因，别合并成一条**（2026-08-09 20:54 实测，
用 `d573375 × 531cb34` 这对已知冲突复现，stdout 共 12 行）：

- `^CONFLICT` / `changed in both` —— **找对了地方，用错了语言**。这些信息行**确实打印了**，
  只是被本地化成「冲突（内容）：合并冲突于 CHANGELOG.md」。同一条命令加 `LC_ALL=C`
  立刻变回 `CONFLICT (content): ...`。
- `^<<<<<<<` —— **找错了地方**。冲突标记写在 blob 对象里，**任何 locale 下都不出现在 stdout**。

stdout 的真实构成是：树 OID 一行 + 每个冲突文件三行 `100644 <oid> <1|2|3>\t<路径>`（stage 索引项，
结构化、语言无关）+ 若干本地化信息行。**「stdout 只有树 OID」是错的**，
按那个理解会白白放弃唯一一条语言无关的结构化判据。

**通用防身法（第 8/10 条共同的教训）**：判据要么用**退出码**，要么用**结构化字段**，
**不要拿人类可读文案做匹配**——文案会因语言、版本、locale 而变，
而它变的时候**不会报错，只会静默给你一个好看的数字**。

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
