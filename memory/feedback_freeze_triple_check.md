---
name: 分支「冻结」判定必须 triple check
description: 宣称某分支/工作树已冻结前，必须同时确认 tip 相等 + 0 未推提交 + 工作树 0 脏；只看前两项会漏掉未提交的在途编辑
type: feedback
---

宣称一个分支或工作树「已冻结 / 不再改动」时，判定必须 **triple check**，三者缺一不可：
1. `git rev-parse` 的 tip 与目标 SHA 相等；
2. `git rev-parse origin/<branch>..HEAD`（或 `origin..HEAD`）的未推提交数 = 0；
3. `git status -s` 工作树 **0 脏**（没有未提交的修改）。

**Why:** stupidSamba v0.2.0 收尾时，PM 让 docs-honesty 在 #171 删一句 CHANGELOG，他确实把改动写进了工作树（未提交）但没推；PM 撤回指令后他没动那笔改动，于是 `git status` 里一直挂着未提交的编辑。他当时报「frozen=YES」只查了 tip 和未推数，**没查工作树脏否**，差点带着脏树继续。后来自检 `git status -s` 才抓出，用 `git checkout HEAD -- <file>`（SAFE 形式，非 `git restore`/`git checkout --`，见 AGENTS.md §7.5）还原，树才干净。

**How to apply:** 任何「我不动了 / 冻结了 / 没改」的声明，尤其在并行多 agent 共用或相邻工作树时，都要自己跑 `git status -s` 复核工作树，不能只信对方说的 tip/未推数。还原未提交改动用 `git checkout HEAD -- <path>`，不要用 `git restore`（HIGH 风险面板）。

**补一条环境坑（v0.2.0 收尾实测）**：本环境工作树可能在命令之间被**无声回退**——未提交改动被环境重置打回旧 tip，导致 `git status` 暂时显示「干净」、而随后的 `git commit` 报「无文件要提交」。所以「0 脏」不是一锤定音：合 #171/#159 这类关键操作前，必须重新 `git fetch` + `git status` **现验**，不能信几分钟前的快照。规避办法：把 编辑 + `git add` + `commit` + `push` 用一条 shell 命令串起来，缩短回退窗口（docs-honesty 踩中后这么做的）。

**追加（v0.2.0 收尾实测，2026-08-09 21:xx CST）**：「0 脏」判据本身也会被环境推翻——docs-honesty 编辑 CHANGELOG 后，工作树被环境**无声回退**（未提交改动在两次命令之间被打回旧 tip `531cb34`），导致下一句 `git commit` 报「无文件要提交」。即 `git status -s` 在命令之间可能失效，几秒前干净的树不代表现在还干净。**`git status` / `git diff` 必须在「要据此下结论」的那一刻重新跑，不要复用几分钟前的快照**；编辑+`git add`+`commit`+`push` 尽量压成一条命令，规避回退窗口。合 #171/#159 前 team-lead 也要重新 `git fetch`+`git status` 现验，别信合并前的冻结声明。
