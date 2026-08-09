---
name: 经常把 memory 备份进仓库
description: 运行环境常丢失文件并重启，必须把 memory 复制到项目目录并用 git 提交，防止记忆丢失
type: feedback
---

`/root/.codebuddy/.../memory` 这类 Agent 记忆目录位于仓库之外，运行环境经常丢失文件并重启，重启后记忆会消失。应**经常**把记忆内容复制到项目目录（如 `/workspace/memory/`）并用 `git commit` + `git push` 提交上去。

**Why:** 用户明确指出「这个机器环境经常丢失文件重启」。上一轮写入的 `MEMORY.md` 与 `memory/feedback_conversation_exports.md` 在重启后确实被清空，验证了这一点。

**How to apply:**
- 任何值得长期保留的记忆（feedback / project / user / reference）都要有一份副本落在 `/workspace`（仓库内），并尽快 commit + push。
- 完成一段有意义的工作、或环境可能重启前，主动执行一次记忆备份提交。
- 仓库外的真实记忆位置（`/root/.codebuddy/projects/workspace/memory`）也要写，以便当前会话的 CodeBuddy 能直接读取；但仓库副本才是抗重启的持久备份。

**⚠️ 「已备份」必须三方对账，MEMORY.md 索引不算证据（2026-08-09 21:47 实测）**

一轮盘点里 **7 份记忆不在任何分支**，其中 3 份不在任何 ref、只躺在 overlay 上的工作副本（环境回收即 100% 丢失）。
最坑的是 `feedback_freeze_triple_check.md`：**MEMORY.md 第 40 行早就索引了它，文件却不存在于任何 ref** ——
只看索引会把它算成已落库。反方向同样有：工作副本索引 38 条 / 实际 39 文件，且 4 条索引指向的文件
**已从工作副本消失**（只在别人的 rescue ref 里找得到）。
结论：**索引、工作副本、ref 三者两两都会不一致，谁都不能单独当权威。**

对账命令（对任一 ref 跑，两条 comm 都应为空）：

```sh
git show <ref>:memory/MEMORY.md | grep -o '](\([^)]*\)\.md)' | sed 's/](//;s/)$//' | grep -v '^\.\./' | sort > /tmp/idx
git ls-tree --name-only <ref> memory/ | sed 's|memory/||' | grep -v MEMORY.md | sort > /tmp/files
comm -23 /tmp/idx /tmp/files   # 索引有、文件无 = 悬空（最危险：看起来已备份）
comm -13 /tmp/idx /tmp/files   # 文件有、索引无 = 将来没人找得到它
```

来不及开 PR 时用 rescue ref 手法把工作副本直接钉进对象库（零流水线、不碰任何人的工作树），
见 `project_rescue_ref_inflight.md`。**父提交挑目标 PR 的 head**，产出就是「该 PR 树 + 新文件」，
对方一条 `git merge --ff-only` 即可吃下，不必 cherry-pick、不会冲突。

**⚠️ 但 ff-only 有前提：目标 PR 的 head 会漂。** 实测 `docs-honesty/memory-buffer` 一小时内漂了三次
（`2d869d6` → `7acf00d` → `d0beb47`），我按 `7acf00d` 造的 ref 到交付时已不是它的祖先，ff-only 直接失效。
**head 漂了就别再追着造新 ref**（追的过程里它还会再漂），改用**整目录取用**——
对方一条 `git checkout <ref> -- memory/` 就完事，不 merge、不 rebase、零冲突，代价只是要由造 ref 的人
先把「对方独有的内容」并进去。判断哪些是对方独有：`git diff --numstat <我的ref> <对方head> -- memory/`
挑 `additions>0` 的，逐个看那几行是真内容还是旧措辞。

**⚠️ 但「整目录取用」是一把删除刀，发这条命令之前必须现场重算一次。**
我写完上面这段之后 20 分钟内被同一个坑绊了两次：造 ref 时对方 head 是 `d0beb47`，
消息发出去时已经是 `d813daa`。那条 `git checkout <ref> -- memory/` 若被执行，会**整份删掉**
对方新写的一个文件、回退两个文件、并**撤销他有意做的一次去重**。

**铁律：发「整目录取用」之前，现场 `git fetch` 对方分支，重跑**

```sh
git diff --numstat <我的ref> <对方现在的head> -- memory/ | awk '$1>0'
```

**只要有任何一行输出（additions>0），就说明我已经落后，这条命令不许发。**
落后时改发**文件级取用**，并且只挑 `-0`（纯增量、我的版本是严格超集）的那几份：

```sh
git checkout <我的ref> -- memory/<只挑纯增量的文件> ...
```

**判「对方删了东西」是遗漏还是有意，别靠数量。** 实测：对方少一份 `feedback_block_rot.md`，
我第一反应是「第 10 份孤儿丢了」，差点塞回去；一查他的 `MEMORY.md`
（`grep -c feedback_block_rot.md` = 0，且悬空/隐形对账双双归零，另一份文件的钩子里
写明「已去重掉第三份草稿」）才确认是**有意去重**。
**文件连同索引一起消失 = 有意删除；文件没了但索引还在 = 才是事故。**

---

**⚠️ 三项对账全过，仍可能整份掉文件（2026-08-09 22:12 实测）**

一份快照的双向 comm 都归零、索引内 `uniq -d` 也空，**但它比另一份快照整整少一个文件**——
因为**索引和文件一起少了那一条**，两边同步缺失，自洽性检查天然查不出来。

**Why:** 对账检查的是「索引 ↔ 文件」的**内部一致性**，而完备性是相对**外部**而言的。
一份只有 1 个文件、索引也只有 1 条的快照，对账一样完美归零。

**How to apply:** 完备性必须拿**外部集合**去比，至少比这三个：
`origin/main` ∪ 活工作副本 ∪ 各人的 rescue ref。
并且**两个方向都要比** —— 实测有 4 份文件「工作副本里没有、`origin/main` 里有」：
**工作副本不是仓库的镜像**，它会缺仓库里的东西，别一看缺就当成「丢了」而去抢救。

**顺带一条计数纪律**：同一批孤儿在不同人嘴里是 7 / 9 / 10 三个数，因为各自的基准集不同。
报数时必须连判据一起报（「`comm -23 <(live) <(origin/main)` = 10 份」），光报数字必然对不上。

---

**⚠️ 工具收尾标签会漏进记忆正文，而且已经合进 main 了**

扫描发现 `origin/main:memory/feedback_ci_quota_frugality.md:32` 是一行裸的 `</content>`，
PR #172 还会再带 2 行进来（`reference_cnb_workspace_lifetime.md:67-68` 的 `</content>` / `</invoke>`）。
是 Write 工具的收尾标签漏进了内容。它不影响 Markdown 渲染以外的任何门禁，所以能一路合进主干。

落库前扫一次（**行首锚定**，否则会误伤正文里在描述该现象的段落）：

```sh
grep -rnE '^</(content|invoke|parameter)>' memory/
```
