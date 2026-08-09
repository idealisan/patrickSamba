---
name: git worktree remove --force 会静默删除未提交工作
description: 删工作树前必须先 git status 确认干净，绝不带未提交改动 --force 删除，否则未 git add 的工作永久丢失
type: feedback
---

删除 git worktree 前必须先 `git status` 确认无未提交改动；**绝不**用 `git worktree remove --force` 删带脏状态的工作树。

**Why**: win-vfs 实测——一个 session 在 `/work/win-vfs-openhost` 里改了 7 个文件 + 新增 2 个文件但没 `git add`/commit，另一个 session 用 `git worktree remove --force` 把它删了，因从未进索引、对象库里无可恢复 blob，那段未提交实现**永久丢失**（只能重做）。tui-diag 的风险表里 `git worktree remove` 已标 HIGH。

**How to apply**: 
- 删工作树前先 `git status --short`，有未提交改动就先 commit+push（§7.2「尽快提交尽快推送」不是客套——未提交中间态在重启/误删时就是永久丢失）。
- 宁可 `git worktree remove`（不带 --force，脏状态会拒绝）也不要 `--force`。
- 想放弃未提交改动再删：先 `git stash` 或显式 `git checkout .` 确认过，再删。
- 同样危险的是在别人的分支上 `git reset --hard` / `git push --force`（§7.3 已禁）。
