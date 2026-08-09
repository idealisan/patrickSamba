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

## 一人一 worktree 之后，事故换了形态：别人在**你的**树里跑 git 操作

worktree 隔离消灭了「未提交中间态砸到别人」，但**没有**消灭「别人直接进你的目录动手」。
2026-08-09 21:20:38 实测一例：`/work/oscap-wire` 里冒出一次
`git rebase --onto <某个刚推上来的文档分支>`，撞 `UU CHANGELOG.md` 停住，约 90 秒后被 abort。
本人当时正在跑冻结自检，看到的现象是 **HEAD 变成陌生 sha + 5 笔未推 + 4 行脏**，
第一反应是自己的成果没了。实际零损失：分支 ref 与远端都没动过。

**诊断入口是 reflog 的 `rebase (start)` 行，它直接给出意图指纹**：

```sh
git reflog --date=iso -12          # 找 "rebase (start): checkout <sha>"
git log --oneline -1 <那个 sha>     # <sha> 是谁刚推的、改了什么 → 反推动机
```

本例里 `<sha>` 是 10 分钟前刚推的一条文档分支，一眼就能看出是「有人想预判我的分支能不能
rebase 上去」，动机正当、地点错了 —— 这种预判应按 §10.3 第 7 条开临时 worktree
（`git worktree add --detach /tmp/probe-$$ <你的head>`），别在别人的树里。

**⚠️ 与「先排除自己」的关系，两条都要记住**：AGENTS.md §7.3.4 列的 3 起指控全是自伤，
所以纪律是「广播前先排除自己」；**但那不等于「永远是自己」**。本例排除自己之后
（本会话零 rebase 调用、无后台任务、`sessionId` 计数=1、`codebuddy` node 进程只有 1 个
→ 连重影实例都排除了），剩下的就是真的第四种可能。
正确处置是**单发问当事人 + 报 team-lead，不广播** —— 排除自己的目的是把
「广播一次假警报」降级成「单发一次询问」，不是把真事故也咽下去。

**顺带：这次是冻结三项自检（tip 相等 / 0 未推 / 工作树 0 脏）抓到的**，
它本来是防自己漏推的，附带作用是**任何外来在途操作都会让三项里至少两项变红**。
所以那条自检值得在每次「我以为我什么都没动」的时刻跑一遍。

**相关坑（同属共享环境）：** `pkill -x stupidsamba4452` 杀不掉进程 ——
Linux comm 名截断到 15 字符，二进制叫 `stupidsamba4452` 时 `-x` 匹配不上。
用 `ss -ltnp | grep <端口>` 拿 PID 再 `kill <pid>`，比按名字杀更安全，
也不会误伤队友在别的端口上跑的同名服务。
