---
name: 同一分支 squash 合入后继续写，下一个 PR 必然 add/add 冲突
description: PR 从分支 squash 进 main 后不 rebase 就接着写，会让同一文件在两边"各自被创建"，出现 add/add 冲突；附识别指纹与三类文件的解法
type: project
---

**squash 合并造出的是一个与分支原始提交没有祖先关系的全新提交。** 分支若不 rebase 就继续往前写，
下一次开 PR 时 git 会认为「同一个文件在两边被各自独立地创建/修改」。

**Why**：2026-08-09 v0.2.0 收尾，`docs/v020-honesty-audit` 分支的 #150 被 squash 成 `c939fd5` 进 main，
分支没 rebase 继续写了 12 笔，再开 #155 时 CNB 直接回
`{"errcode":2009014,"errmsg":"the pull can not merge yet"}` —— **这条报错不说原因**，
要自己在临时 worktree 里 `git merge origin/main` 才看得到真实冲突。
同期 vfs-deflake 的 #151/#154 也是同一形态（同一份工作开了两个 PR，两个都合了，收敛干净纯属运气）。

**How to apply**：

- **指纹**：冲突里出现 `冲突（添加/添加）` = squash 后遗症，几乎可以确诊。内容冲突可能只是普通并行修改，
  add/add 不会。
- **诊断命令**（不要靠 PR 页面，它只说 "can not merge yet"）：
  ```sh
  git worktree add -q --detach /work/lead-verify-<n> origin/<分支>
  cd /work/lead-verify-<n> && git merge --no-commit --no-ff origin/main   # 看真实冲突清单
  git merge --abort
  ```
- **三类文件三种解法**，不要一律 `--ours` / `--theirs`：
  1. **只有本分支在改的文件**（如本分支新建的审计文档）→ 取分支版，但**先验超集**：
     `diff <(git show origin/main:F) <(git show origin/<分支>:F) | grep -c '^<'`，
     结果 0 = 分支是严格超集，可放心取；非 0 就逐行看那几行是不是别人的。
  2. **双方都在末尾追加的索引类文件**（`memory/MEMORY.md`）→ **两边的行都留**。
     注意 tip-to-tip diff 会显示成 `32c32`「一行被换掉」，**那是幻觉不是覆写**——
     两边行数相同、各自在同一位置追加了不同的行而已。
  3. **双方都在中间改的长文档**（`CHANGELOG.md` / `README.md`）→ 逐块看，两边意图都保留。
     **整段取一边会静默回退别人的工作，且 CI 不会红。**
- **预防**：PR 被 squash 合入后，原分支要么弃用另起，要么立刻 `git merge origin/main` 跟上。
  **同一份工作不要同时挂两个 PR。**
