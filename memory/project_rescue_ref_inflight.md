---
name: 别人正在写文件时如何保命提交：独立索引 + commit-tree + refs/rescue/*
description: 队友工作树里有未提交（尤其未跟踪）文件而环境随时回收时，用临时 GIT_INDEX_FILE 造提交对象并推到 refs/rescue/*，全程不碰对方的 index/HEAD/工作树，且不触发任何 CI 流水线；配套的检测命令是 git ls-remote 而不是 --branches --not --remotes
type: project
---

**场景**：队友的工作树里有大量未提交改动（尤其**未跟踪**文件 —— 它们连 `git stash`
都救不回来），环境随时可能被回收，但**他此刻正在写这些文件**。

**不要用 `git add -A && git commit`。** 那会改写他的 index 与 HEAD，正是本项目
2026-08-09 已发生三次的事故形态（在途文件被另一个实例卷走）。

**正确手法（team-lead 2026-08-09 18:43 实操）**：用一个**临时索引**造出提交对象，
再把裸 SHA 推到 `refs/rescue/*`，全程不触碰对方的 `.git/index`、HEAD 与工作树：

```sh
cd /work/<对方的树>
export GIT_INDEX_FILE=/tmp/<我的角色>-rescue-idx    # 关键：不是 .git/index
git read-tree HEAD && git add -A                    # 只影响临时索引
TREE=$(git write-tree); unset GIT_INDEX_FILE
C=$(git commit-tree "$TREE" -p HEAD -m "wip(rescue): <说明>")
git push origin "$C:refs/rescue/<角色>-inflight-<时间>"
```

实测效果：提交对象 `4e0978e…` 已在远端，而对方工作树复查 **dirty 仍是 16**，
他的 `git status` 毫无变化，可以继续写。

**取回**：`git fetch origin refs/rescue/<名字>`。

## 三个配套事实

1. **`refs/rescue/*` 不触发 CI。** CNB 的事件只挂 `refs/heads/*` 与 tag，
   实测 9 次 rescue 推送在 `/-/build/logs` 里**零流水线**。
   相比之下推 10 个分支 = 10 条流水线 ≈ 1.4 核时（额度每月 160 核时），
   而这些往往是离队 agent 的残留，没人会去看那些流水线。
2. **`git log --branches --not --remotes` 看不见 rescue ref。** `--remotes` 只展开
   `refs/remotes/*`（远程跟踪分支），服务端的 `refs/rescue/*` 本地没有对应跟踪引用。
   PM 因此把 10 笔已推的提交**误报成「未推送」**。查 rescue ref 必须直接问服务端：

   ```sh
   git ls-remote origin 'refs/rescue/*'
   ```

3. **`$(git commit-tree …)` 的输出带一个尾部 CR**。直接拼 `$C:refs/rescue/x` 会得到
   `<sha>efs/rescue/x`（`:r` 被 CR 吃掉），报「源引用规格没有匹配」。

   **⚠️ `tr -d '\r'` / `tr -d '\r\n'` 都不保险 —— 2026-08-09 21:43 与 22:14 两人分别加了它，
   照样复现同一个报错。** `echo "commit=$C"` 打出来是干净的 40 位、`${#C}` 也是 40，
   但同一行里的 `git push origin "$C:refs/..."` 仍报 `<sha>efs/rescue/...`。
   机制没查清，但现象可复现（两个不同 agent 各撞一次）。
   **别再跟这个转义较劲，改用两步走 —— 本地先落 ref，再推 ref:ref**：

   ```sh
   git update-ref refs/rescue/<名字> "$C"
   git push -q origin refs/rescue/<名字>:refs/rescue/<名字>
   git ls-remote origin refs/rescue/<名字>      # 必须自己回查，push -q 不给回显
   ```

   多一条命令，但再也不会被任何不可见字符坑到，而且本地留了个 ref 方便自己复算。

## 抢救 `memory/` 时多一步：双向核对索引，两个方向都要查

记忆文件的权威副本在仓库 `memory/`，工作副本在 `~/.codebuddy/.../memory/`。
两边会不同步，**而且是双向的**，只查一个方向必漏：

| 方向 | 症状 | 查法 |
|---|---|---|
| 索引→文件 | `MEMORY.md` 里有条目，**文件不在树里**（悬空链接） | 逐条 `git ls-tree <ref> -- memory/<f>` |
| 文件→索引 | 文件在，**索引里没有** → 未来会话看不见它，等于没写 | 逐文件 `grep -F "($f)"` MEMORY.md |

**实测（2026-08-09 21:37，抢救 4 个孤儿文件时）**：rescue ref `55fe462` 的 `MEMORY.md`
索引着 `feedback_freeze_triple_check.md`，但那个文件**根本不在树里** —— 悬空链接，
差点跟着 PR 进主干。同一批还有 3 个文件在工作副本里、索引和树都没有，
其中 **3 个的 blob 连对象库都没进**（`git hash-object` 算出来 `git cat-file -e` 为 no），
容器一崩连 `git fsck` 都捞不回来。

配套两条：

- **基线要挑「memory/ 的超集」那个 ref**，不是最新那个。判法是跑**反向** diff：
  `git diff --stat <候选> <基线> -- memory/` 只有删除没有新增 = 基线是超集。
- **补索引只在 `MEMORY.md` 末尾追加**，不重排既有条目（§7.1 共享文件规矩）——
  多人同时在改 MEMORY.md，追加造成的冲突是单行，重排是大段。

**对账要对三方，不是两方**（pm 2026-08-09 21:5x 补的数据点，实测证实）：
「索引 / 工作副本 / ref」三者**两两都会缺**，任何一方单独当权威都会丢东西。
同一时刻实测：工作副本 4 条索引指向的文件已从磁盘消失（但安全躺在某个 rescue ref 里），
同时又有 3 个文件在磁盘上而任何 ref 都没有。**「我这边齐了」只说明你这一方齐了。**

**并集不能靠「拿新的整棵树覆盖」，必须以 `origin/main` 为 base 做三方合并。**
工作副本可能在**某些文件上比 main 还旧**（别人通过 PR 改的，工作副本收不到），
整树覆盖会让那些行被判成「你删的」而静默消失 —— 见
`project_silent_success_failures.md` 第 14 条（一次合并净删 75 行，零冲突、零报错）。
合并完必跑删除面体检：`git diff --numstat origin/main HEAD -- memory/` 挑 `deletions>0` 逐个确认。

**顺手加一条污染扫描**：记忆文件里会混进工具调用标记
（实测 PR #172 的 `d0beb47` 有 2 个文件末尾带着 `</content>` / `</invoke>` 共 3 行，
是 Write 工具的收尾标签漏进了内容）。它不影响 Markdown 渲染以外的任何门禁，
所以能一路合进主干。落库前扫一次：
`grep -rnE '^</(content|invoke|parameter)>' memory/`。

**Why**：环境会硬回收，而「工作只在磁盘上」是本项目反复发生的真实损失；
但抢救动作本身不能制造新的事故。这个手法把两者解耦：**保命是我的事，
不打断你的事**。

**How to apply**：
- 只要判断出「对方正在写 + 有未跟踪文件 + 环境有回收风险」，直接用它，不必先征得同意
  （它不改变对方看到的任何东西），事后发消息告知 rescue ref 名字即可。
- 自己的树不需要这套 —— 自己的树直接 `commit` + `push` 就行。
- **commit 挡住的是「工作树被清」，挡不住「容器整个没了」**：造完提交对象**必须推**，
  留在本地对象库里等于没救。

## 更新 rescue ref 时**必须**用 `--force-with-lease`，别因为「这是我自己的 ref」就放松

rescue ref 不是私有的：**别人（含以你身份运行的另一个执行者）会往你建的 ref 上叠提交。**

**实测（2026-08-09 21:54）**：我 21:43 推了 `b542fb8`，21:54 想在其上叠并集快照，
`--force-with-lease=<ref>:b542fb8` 当场拒绝（`! [rejected] … (stale info)`）。
远端已经是 `5827c788` —— 21:48:14 有另一个执行者以**同一提交者身份**在我这个 ref 上
叠了 34 行记忆。**若当时写的是 `--force`，那 34 行会被静默覆盖，事后无从发现**
（rescue ref 没有 PR、没有 CI、没人 watch，覆盖了就是真没了）。

正确处置是**并集**而不是覆盖：以远端现值为 base，逐文件比 blob hash 只更新真变了的，
**不删** base 里有而你本地没有的文件（本地工作副本经常是不全的）。

## 把 N 个 rescue 快照并进一个 PR：别用 `git merge`，用「逐文件判超集方向」

2026-08-09 22:1x 实测，14 个候选 ref（我的 + oscap-wire 的 + pm 两条链 + 第二执行者一条链）
并进 PR #172。**`git merge` 在这里是错的工具**：这些 ref 全是不同时刻的工作副本快照，
彼此互有新旧，三方合并会把「这一侧比 main 旧」当成「这一侧删除了这些行」照单执行
（见 `project_silent_success_failures.md` 第 14 条）。整棵树 `checkout` 更糟。

可复算的做法，四步，每步都自报计数（**计数为 0 一律判失败**，见
`feedback_zsh_wordsplit_zero_iterations.md`）：

1. **先算祖先矩阵**，把 14 个候选缩成互不为祖先的「极大头」（本次 7 个）。
   是自己 HEAD 后代的直接 `merge --ff-only`（本次 1 个，+25/-0）。
2. **逐文件双向 `comm`** 判方向 —— 谁是谁的超集：
   ```sh
   git show "$REF:$f" | sed 's/[[:space:]]*$//' | grep -v '^$' | sort -u > a
   sed          's/[[:space:]]*$//' "$f" | grep -v '^$' | sort -u > b
   comm -23 a b   # 他有我无
   comm -13 a b   # 我有他无 —— 这一侧为空才敢整份取他的
   ```
   本次 3 份判为严格超集直接 `git checkout <ref> -- <file>`；
   5 份判为**他是旧稿**保留自己的（其中 `project_timing_criteria_flaky.md`
   他是 32 行缩写稿、main 是 70 行详版，正是第 14 条那个坑）。
3. **条目级核对索引**，不是行级：把每个候选 `MEMORY.md` 里的目标文件名抽出来，
   逐个确认自己仍然索引了它。本次 n=580 次核对，只剩 1 个是**有意去重**的。
   行级 diff 会把「同一条目钩子被写长了」报成差异，条目级不会。
4. **落库前体检五项**：双向 `comm` 对账（悬空/隐形）、索引内 `uniq -d` 去重、
   工具标签污染 `grep -rnE '^</(content|invoke|parameter)'`、
   正文声称条数 == 表格实际行数（自洽门）、
   `git diff --numstat origin/main HEAD` 的 `deletions>0` 逐个人工确认。
   前四项本次各抓到过东西：污染 3 行、重复索引 3 条、自洽门 1 次（声称 13 表格 12）。
