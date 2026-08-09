---
name: 会话导出文件必须入库
description: 用户刻意将 conversation-*.txt 提交到仓库作为项目历史资料，切勿自动移除或加入 .gitignore
type: feedback
---

`conversation-*.txt`（会话导出文件）由用户**刻意提交**到仓库，作为项目历史资料，必须保留。

**Why:** 用户在上一轮明确说「会话导出文件是我故意提交的，作为项目历史资料很重要，要加进去，以免丢失记录」。之前我曾误判为误提交并执行 `git rm --cached` + 加入 .gitignore，用户纠正了我。

**How to apply:** 永远不要删除仓库里的 `conversation-*.txt`、把它们加入 `.gitignore`、或阻止其被提交。若看到此类文件，当作受保护的项目资产对待。
