---
name: 在陈旧 main 上 rebase 会造出「已合并提交」的重复 SHA
description: pull --rebase 前必须先确认自己已推送的提交是否已被合进 main；在旧 main 上 rebase 会复制已合并的提交，且队友分支可能正基于被复制掉的旧 SHA
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

**How to apply**：
- 团队合并节奏快时，rebase 前先查一次祖先关系，别默认「我的东西还没合」。
- **先判断这次 rebase 有没有必要**；纯文档/记忆类改动几乎永远不需要跟进 main。
- 强推自己分支前，先 `git log --format='%h %an %s' origin/<我的分支> ^HEAD` 核对
  远端独有的提交**全是自己的**；再 `git for-each-ref --contains <旧sha>` 看有没有队友
  分支挂在上面 —— 有的话旧提交不会丢（被别的 ref 保住），但要发消息告诉他们。
- 别用 merge 去「解决」这种分叉：那会把重复提交永久留在历史里。
