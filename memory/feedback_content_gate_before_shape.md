---
name: 判据设计：先内容门确认「发生了」，再判「以什么形态发生」
description: 单一 rc 判据若同时覆盖「没发生」与「以另一形态发生了」，就不能用来判形态；is-ancestor 判 squash 是活例（假红）
type: feedback
---

**一个判据只要同时覆盖「事情没发生」和「事情以另一种形态发生了」两种情况，它就不能用来判形态。**
必须拆成两步：**先用内容证明事情发生了，再判它以什么形态发生。**

**Why:** 2026-08-09 我给全队广播过「合完 #171 后跑
`git merge-base --is-ancestor <tip> origin/main`，rc=1 = 被 squash 了，去 rebase」。
实测 rc 确实是 1，但真相是 **#171 根本还没合**（`origin/main` 纹丝未动）。
这条判据把「未合入」和「squash 合入」压成同一个 rc=1，谁先跑谁就会误判成「该 rebase 了」，白跑一次 rebase。
这是「成功回显 ≠ 事情真的发生」的**反向变体**：不是假绿，是**假红**——把没发生读成了发生。

同一族的第二个坑：把 is-ancestor 当**长期**门禁。squash 之后 main 上是全新 SHA，
`is-ancestor <原 head> origin/main` **永远 rc=1**，内容明明已经进 main，自检却一直说「没合」，
于是发布环节会被自己的门禁永久卡住。

**How to apply:**
- 判「某个 PR 的内容是否已进 main」→ **查内容，不查 SHA 关系、更不查提交标题**
  （CNB squash 也生成 `Merge pull request #N`，标题恒定假绿）：
  ```sh
  git show origin/main:CHANGELOG.md | grep -c '<该 PR 删掉/新增的那句话>'
  git ls-tree --name-only origin/main <该 PR 唯一新增的文件>
  ```
  探针优先挑「该 PR 唯一新增/唯一删除」的那一处，零歧义。
- 只有在内容门已经证明「进了 main」之后，才用 `is-ancestor` 判形态：
  rc=0 = 真合并（下游可原样合）/ rc=1 = squash（下游需 `git rebase --onto origin/main <旧基>`）。
- 给别人的判据尤其要这样写。判据一旦广播出去，就会有 5 个人照着跑，错一条要发第二封更正。

**姊妹条：失败回显同样不可信 —— 非 0 也可能只是「命令写错了」**

`git merge-tree --write-tree A B` 的非 0 同时表示「有冲突」和「参数不对/分支不存在」。
2026-08-09 我批量跑三对 PR 的两两可合性，**三对全 rc=1**，差点报「三个 PR 互相冲突」；
真因是 **zsh 默认不对未加引号的变量做词分割**，`set -- $pair` 没拆开，
命令实际变成 `merge-tree origin/"a b" origin/` 直接报错退 1。
拿「不存在的分支」做负向对照坐实：**同样 rc=1**。重跑后真实结果只有一对冲突。

所以任何「靠退出码下结论」的批量扫描，**必须配一个已知结果的正向对照**证明命令本身是通的
（我用 `main + <已知干净的分支>` → 期望 rc=0）。这与「探针要有反向对照」是同一条纪律，
只是这次要的是**正向**对照：先证明工具在这套参数下能给出 0，rc=1 才有意义。

配套的另一半（oscap-wire 实测）：`merge-tree --write-tree` **冲突时照样输出一棵合法的树 SHA**，
那棵树里是带 `<<<<<<<` 的污染文件。拿 stdout 当结果 `commit-tree` 落地，冲突标记直接进 main 且零报错。
合起来记：**stdout 不可信，单看 rc 也不足以区分「冲突」与「命令错」。**
