---
name: 必须用 3~5 个子 agent 组队并行开发
description: 用户明确要求在 stupidSamba 项目上以 3~5 个子 agent 分工组队推进，串行开发被判定为太慢
type: feedback
---

在 stupidSamba 项目上推进工作时，**默认就要组建 3~5 个子 agent 的团队并行开发**，
不要一个人串行地写。

**Why:** 用户原话是「现在的工作速度太慢了，你必须使用 3~5 个子 agent，分工合作形成团队
去完成工作，从而加速开发进度」。在此之前用户已经提示过一次「需要的话你应该使用子 agent
并行加速开发」，说明这是他反复强调的诉求，不是一次性的临时指令。项目本身的
`AGENTS.md` §7.1 也把并行分工写成了准则，并按分层给出了 5 个 agent 角色划分
（wire / auth / vfs / server / mdns）。

**How to apply:**
- 按 `AGENTS.md` §5 的分层切分任务，**先定接口契约再并行实现**。
- **必须给每个 agent 明确的文件所有权清单**（能改哪些、明确不能改哪些）。5 个 agent
  共用同一个 `/workspace` 工作树，不做文件级隔离一定会互相破坏。
  公共文件（如 `internal/smb/wire/const.go`）规定为"只追加、加完立刻单独提交"。
- 给每个 agent 分配**专属调试端口**，否则起服务会互相抢 4445。
- 提交走 `scripts/save.sh "<模块>: <说明>" <路径...>`，它自带按路径的编译校验和
  带互斥锁的 `pull --rebase` + push，专为多 agent 共享工作树设计。
- agent 需要改别人的文件时，要求它发消息给 team-lead 协调，不要自己动手。
- 作为 lead，收到 agent 的调研结论后**自己消化再下发具体规格**（文件路径+行号+改什么），
  不要写"based on your findings"这种把理解甩回给 agent 的指令。
