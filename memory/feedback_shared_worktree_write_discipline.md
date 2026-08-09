---
name: 共享工作树里的写入纪律
description: 8 个 agent 共用 /workspace 一棵树，Write 前必须 Read；曾把队友已完成的 lock.go 整个覆盖
type: feedback
---

**在共享工作树里，任何 `Write` 之前必须先 `Read`；只要文件已存在就改用 `Edit`。**

**Why:** 实际发生过：server agent 认领"补齐 LOCK"任务后，没有先确认
`internal/smb/command/lock.go` 是否已存在就直接 `Write`，把一份已完成、
已被 `close.go` 和 `lock_test.go` 依赖的实现（234 行）整个覆盖成自己的版本，
导致 `lockTable.lock/unlock` 方法消失、包编译失败。
`Write` 工具返回的是 "Successfully **overwrote**" 而不是 "created" ——
这个词是唯一的警报，很容易被忽略。

同一时期 `internal/smb/command/ioctl.go` 出现 `ioctlEnumerateSnapshots`
重复声明 + 重复 case，是同一类事故的另一个案例：两个写入者各写了一遍。

**How to apply:**
- 认领任务后，第一步先 `ls` 目标目录 + `git log --oneline -- <文件>`
  确认这块工作是否已经有人做过。任务描述说"缺失"不等于现在还缺失 ——
  任务是几十分钟前下发的，树在这期间一直在变。
- 判断"是不是我搞坏的"用 `git status --short <我的目录>` +
  `git diff --stat HEAD -- <文件>`；确认要还原就
  `git checkout HEAD -- <文件>`（还原到 HEAD，不是还原到某个历史提交 ——
  历史版本可能与其它文件的当前签名对不上，会换一种方式编译失败）。
- 编译错误先分辨归属：报错在别人的文件里（vfs / ioctl / query_directory）
  多半是队友的中间态，等 60~90 秒重试即可，不要去替他们改。

**相关坑（同属共享环境）：** `pkill -x stupidsamba4452` 杀不掉进程 ——
Linux comm 名截断到 15 字符，二进制叫 `stupidsamba4452` 时 `-x` 匹配不上。
用 `ss -ltnp | grep <端口>` 拿 PID 再 `kill <pid>`，比按名字杀更安全，
也不会误伤队友在别的端口上跑的同名服务。
