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
