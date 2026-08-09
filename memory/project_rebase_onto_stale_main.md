---
name: rebase/squash 的四张面孔（含 squash 是冲突放大器）
description: rebase 前先查祖先关系与必要性；diff 大片删除多半是基线漂移；队友若基于你的分支开分支，squash 合你会切断共同祖先把他打成大片冲突，解法是他用 --onto
type: project
---

`git pull --rebase origin main` 之前，**先 `git fetch` 并确认自己已推送的提交是不是已经进了 main**：

```sh
git fetch -q origin && git merge-base --is-ancestor <自己的提交> origin/main \
  && echo 已合并 || echo 未合并
```

**Why**：2026-08-09 实际发生。oscap-port 在 16:18 用当时的 `origin/main`（b6d16b4）做 rebase，
把自己两个提交 `d06ab2f`/`4bca43f` 重写成 `a761bfd`/`6395d88`。而就在前后脚，team-lead 已经
把 `d06ab2f` 通过 PR #121 合进了 main。结果是：

- 分支上多出一份**内容与已合并提交完全相同、SHA 不同**的重复提交；
- 更麻烦的是 `oscap/native` 与 `oscap/builtin` 两个队友分支**正基于被重写掉的 `d06ab2f`**
  （worktree 共享 `.git`，`git for-each-ref --contains <sha>` 一眼能看出来）；
- 推送被 non-fast-forward 拒绝，看起来像「落后于远端」，实际是自己把历史改岔了。

修法很简单但要知道：**rebase 到最新的 main**，git 按 patch-id 自动丢弃已在上游的提交
（会提示 `使用 --reapply-cherry-picks 来包括跳过的提交`，那行提示就是它干活了的证据）。
之后 `git push --force-with-lease` 到**自己的**分支即可（§7.3.2 允许，§7.5 实测 SAFE）。

**同一根因的另一面（2026-08-09 18:31，PM 亲踩）**：分叉不一定来自「main 陈旧」，
**「这次 rebase 本来就没必要」也会造出同样的分叉**。当时只是想往自己的文档分支加一个记忆文件，
顺手跑了 `git pull --rebase origin main`——已推的 5 个提交 SHA 全被重写，远端还指着旧 SHA，
下一条 `git push` 当场 non-fast-forward。**代价是白付的：那次改动跟 main 的新内容毫无关系。**
所以 rebase 前先问一句「我这次改动需要 main 上的什么东西吗」，不需要就别 rebase，
分叉从一开始就不会存在。真要强推，先 `git diff --numstat origin/<我的分支> HEAD -- <我负责的文件>`
确认自己那几个文件**只增不减**（补上第 29 行那条「远端独有提交全是自己的」核对）。

**第三张面孔：rebase 完了才发现 main 又前进了，此时 `git diff origin/main` 会吓死人**
（2026-08-09 19:11，ci-trigger 亲踩）。rebase 到 `62332cd` 完成、正准备推送时，
`git diff --stat origin/main` 报出 **19 个文件、删除 2006 行**（AGENTS.md、docs/、memory/
整片消失）。第一反应是「我 rebase 把队友的文档全干掉了」——**完全是假象**：
`origin/main` 在这几分钟内已被别人推到 `526944f`（4 个新提交），
`git diff origin/main` 比的是「我的树 vs **当下的** origin/main」，
于是别人刚加的内容在我这边显示为「被删除」。

判据三连（别看着 diff 就慌，也别看着 diff 就强推）：

```sh
git merge-base --is-ancestor <我的rebase基线> origin/main   # 是 → 无重复 SHA 风险，安全
git log --oneline <我的基线>..origin/main                   # main 新增了什么
git log --oneline <我的基线>..origin/main -- <我改的文件>    # 空 → 再 rebase 一次零冲突
```

第三条为空时直接再 `git rebase origin/main`，`git diff --stat origin/main` 立刻收敛回
「只有我那一个文件」。**「删除几千行」这个数字本身不是证据，它只说明基线不同。**

**第四张面孔：队友在你的分支上开分支 + 你被 squash 合入 = 他当场从 mergeable 变成大片冲突**
（2026-08-09 20:29，docs-honesty 在合并前用模拟测出来，**属于事前拦截，不是事故**）。

形态：oscap-wire 的 `#159` 不是从 main 开的，是从 docs-honesty 的 `3bf55ab` 开的
（`git merge-base --is-ancestor 3bf55ab <他的HEAD>` → 真），于是 `#159` 相对 main 的 diff
里含着**别人的 3 个提交**。此时 `#171` 一旦 **squash** 合入 main，那 3 个提交在 main 上
变成一个**全新 SHA**，patch-id 对不上，**共同祖先当场断裂**，merge-base 退回分叉点，
两人碰过的每一段都变成「双方都改了」。

实测冲突面（同一对分支，只换合并方式/顺序）：

| 方案 | 冲突文件数 |
|---|---|
| A squash 先合 | **7** |
| B squash 先合（反序） | **7**，完全对称——换顺序没用 |
| A 走**真合并提交**（非 squash） | **2**（才是真正的双方都改） |
| A squash + B 用 `--onto` 摘掉 A 的提交 | **4 轮，全在文档，代码 0 冲突** |

**关键认识：squash 是冲突放大器，放大倍数等于「你的提交里被别人当基座的那部分」。**
仓库用 squash 惯例没错，但它与「在队友分支上开分支」组合会咬人。

**⚠️ 「本仓库用哪种合并方式」不是一个稳定属性，是每次合并现场的选择，必须现查。**
2026-08-09 21:01 实测本仓库 `main`：父数≥2 的真合并提交 **64 个，全部在 17:17:41 之前**
（最后一个 `8b48f91` = PR #132）；**17:17 之后落地的 13 个 PR 提交父数全 = 1**
（`d351683` #138 17:36:31 起）。也就是说仓库**中途换过合并形态**。
两个人分别断言「CNB 是 squash」和「CNB 默认保留提交」，**各自都只抽到了一个时间窗口，都错了一半**
（这也是同日第二例抽样偏差，前一例是用 `head -25` 抽分支）。

**⚠️ 提交标题分不出 squash 与真合并 —— CNB 的 squash 也生成「Merge pull request #N — …」。**
同一系列相邻两个 PR，标题模板一致、结构相反：

```
e4f0f80  父数=2  Merge pull request #129 — oscap/builtin: …   ← 真合并
d351683  父数=1  Merge pull request #138 — oscap/native: …    ← squash
```

所以 `git log --oneline | grep 'Merge pull request'` 判「提交被保留了」是**恒定假绿**，
与「英文正则 grep 中文输出」同族（见 project_silent_success_failures.md 第 10 条）。
判据只能看结构：

```sh
git log -1 --format='%p' <sha> | wc -w                          # 1=squash  2=真合并
git merge-base --is-ancestor <分支tip> origin/main; echo $?      # 0=提交被保留  1=被 squash
```

**下游那个人不要预先决定跑不跑 `--onto`**：上游合入后先跑第二条，rc=1 才需要 rebase，
rc=0 时原样可合，跑了反而白改一遍 SHA。

解法（给下游那个人的一条命令，不是给合并者的）：

```sh
git rebase --onto origin/main <上游最后一个提交> <我的分支>
```

不要用普通 `git pull --rebase origin main` —— 它会重放上游那几个提交，直奔 7 文件冲突。

测冲突面不需要动任何人的分支，**两条命令就能在合并前算出来**：

```sh
SIM=$(git commit-tree $(git rev-parse <我的HEAD>^{tree}) -p origin/main -m sim)  # 模拟 squash 后的 main
git merge-tree --write-tree $SIM <队友HEAD> >/dev/null 2>&1; echo $?   # 0=干净 1=冲突
git merge-tree --write-tree $SIM <队友HEAD> | awk '$3==1 {print $4}'   # 冲突文件名，语言无关
```

**判据用退出码，别 grep 文案**（本容器 git 说中文，`grep CONFLICT` 恒为 0；
`grep '^<<<<<<<'` 更是永远匹配不到，标记在 blob 里不在 stdout）。

想连 `--onto` 之后的结果一起验，就 `git worktree add --detach /tmp/xxx $SIM` 再在里面
真跑一次 rebase（用完留着别删，§7.5）。跑完记得在合并后的树上跑一遍
`gofmt -l .` + `check-test-compile.sh`，确认代码层面确实没有实质冲突。

**更快的一层：先比树，相同就不必跑 rebase 也知道零冲突。**
上游 `U` 相对 main 干净、且 main 是 `U` 的祖先时，squash 后 main 的树 ==
`git merge-tree --write-tree origin/main U` == `git rev-parse U^{tree}`。
若这两个 OID 相同，则 `--onto` 的新基点与旧基点**内容逐字节一致**，重放**不可能**冲突。
2026-08-09 21:03 用 `#171`(`531cb34`) 实证：两侧同为 `cd3dca28…`，
随后实跑 `--onto` → 7 个提交全重放、rc=0、结果树与原 HEAD 同 OID（零漂移），与推断一致。

**反过来的坑（同日实测，否掉了一个看似更省事的方案）**：
「让下游先合，上游后合就变零增量」是**错的**。模拟下游 `#159` 被 squash 进 main 后
算 `#171 × 新 main` → **rc=1，4 个文件全冲突**。虽然下游内容已完全包含上游，
但 squash 断掉共同祖先，merge-base 退回分叉点，两边碰过的每一段都成了「双方都改」。
**上游先合永远是对的，换顺序只会把冲突甩给另一个人。**

**`--onto` 的「基点选宽了」通常不出事：变空的提交会被自动丢弃 —— 但这条要探针证实，别靠推理。**
2026-08-09 21:46 实测。当时 pm 认同的应急命令用了**旧上游 tip** `531cb34` 当基点
（`531cb34..我的HEAD` = 9 笔，比正确基点 `524bb43` 多含 #171 自己那一笔），
我第一反应是「这会把上游的提交重放一遍，撞 squash 冲突」，**准备发更正 —— 幸好先跑了探针**：

| 变体 | 命令 | 结果 |
|---|---|---|
| A（窄基点） | `rebase --onto <main'> 524bb43` | rc=0，重放 **8** 笔 |
| B（宽基点） | `rebase --onto <main'> 531cb34` | rc=0，重放 **8** 笔 |

**两者结果树 OID 完全相同**（`36a4e847…`），且都等于原 HEAD 的树（零漂移）。
多出的那一笔在应用到已含该改动的 main' 上时**变成空提交，被 rebase 自动丢弃**，
一声不响，rc 照样 0 —— 与本文件开头那条「按 patch-id 丢弃已在上游的提交」是同一机制的两种触发方式。

**所以窄基点的价值不是「宽的会坏」，而是「重放得少，出问题的机会少」。**
真正会咬人的是**上游那笔在 main' 上被改成了别的样子**（例如 squash 时连同别的 PR 一起解了冲突）——
那时补丁应用不再是 no-op，宽基点会冲突而窄基点不会。这个场景本次**没有实测**，不要写成已验证。

复现配方（零 CI、不碰任何人的分支）：

```sh
git worktree add -q --detach /tmp/probe-$$-$RANDOM origin/main && cd /tmp/probe-$$-$RANDOM
git merge --squash <上游tip> && git commit -m sim      # 造一个「上游被 squash 进去」的 main'
git checkout -q --detach <我的HEAD>
git rebase --onto <main'> <基点候选>; echo rc=$?        # 换基点各跑一次
git rev-parse HEAD^{tree}                              # 比树，相同即等价
```

⚠️ 探针提交**不要加 `-c user.name=… -c user.email=…`**：本仓库开了提交签名，
覆盖身份会让签名服务返回 `403 | Author is invalid`，`git commit` 直接失败
（错误文案是「gpg 无法为数据签名」，很容易误判成本地缺 gpg key）。用默认身份即可。

**How to apply**：
- **怀疑队友给的 git 命令有误时，先探针再更正。** 上面这次若直接发「更正」，就是一条错误的更正
  广播给全队 —— 而 rebase 的自动丢弃行为不是常识，光靠读命令推不出来。
- 团队合并节奏快时，rebase 前先查一次祖先关系，别默认「我的东西还没合」。
- **判「上游落地后我要不要 rebase」用 `merge-base --is-ancestor <上游tip> origin/main` 的退出码**，
  不要看合并提交的标题（squash 与真合并共用同一个标题模板），也不要预设仓库的合并惯例。
- **合并队列里有人排在你后面时，先查一句「他的分支基点是不是我」**
  （`git merge-base --is-ancestor <我的提交> <他的HEAD>`）。是 → 合并前就把
  `--onto` recipe 发给他，别等他撞上 7 文件冲突再来问。
- **`git diff origin/main` 出现大片删除时，先怀疑基线漂移，不要怀疑自己删了东西**；
  跑上面三连判定，别急着报警（§7.3.4：指控别人前先排除自己，这条同样适用于指控自己）。
- **先判断这次 rebase 有没有必要**；纯文档/记忆类改动几乎永远不需要跟进 main。
- 强推自己分支前，先 `git log --format='%h %an %s' origin/<我的分支> ^HEAD` 核对
  远端独有的提交**全是自己的**；再 `git for-each-ref --contains <旧sha>` 看有没有队友
  分支挂在上面 —— 有的话旧提交不会丢（被别的 ref 保住），但要发消息告诉他们。
- 别用 merge 去「解决」这种分叉：那会把重复提交永久留在历史里。

**第五张面孔（实测补充，2026-08-09）：「下游先合、再由上游 rebase」这个看似省事的备选，在 squash 合并下反而最糟。**
oscap-wire 模拟过：#159 被 squash 进 main 后，`#171 × 新 main` 的 merge-base 被退回 `762e335`，
于是 `AGENTS.md` / `CHANGELOG.md` / `README.md` / `configs/example.yaml` **四文件全冲突**甩给 #171。
根因正是 squash 切断共同祖先——与第四张面孔同源。
**结论：「上游先合」在两种合并方式下都成立**：非 squash 时下游快进；
squash 时下游用 `git rebase --onto origin/main <上游冻结tip>` 重放（已预演零冲突、~2 分钟）。

> ⚠️ **本节初稿把上游 tip 写死成 `531cb34`，两小时后就过期了**（#171 冻结点后来是 `524bb43`，
> #159 相应重做成 `b955e5b`）。跨 agent 的 SHA 出厂即过期，**判据里一律写角色名/分支名，
> 要用具体 SHA 就当场 `git ls-remote` 取**。
>
> ⚠️ **本节初稿给的判形态命令 `merge-base --is-ancestor <上游tip> origin/main` 有假阳性**：
> rc=1 同时覆盖「被 squash 了」和「压根还没合」两种情况（pm 于 21:46 实测推翻：
> 当时 rc=1，而真相是 `origin/main` 还停在 `762e335`、一个 PR 都没合）。
> **必须先过内容门证明「合了」，再判「以什么形态合的」**——
> `git show origin/main:<文件> | grep -c '<上游删掉的那句话>'`，1=没合、0=已合；
> 确认已合之后 `is-ancestor` 的 rc 才有意义（0=真合并，1=squash）。
> 详见 `feedback_content_gate_before_shape.md` 与 `project_silent_success_failures.md` 第 12 条。
