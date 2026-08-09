---
name: stupidSamba 开发环境的固有限制与重启后恢复步骤
description: 容器会重启并清空工具链与仓外文件；mount.cifs 因缺 CAP_SYS_ADMIN 永远跑不了；smbclient/pkill 的两个致命坑
type: project
---

stupidSamba 的开发容器**会不定期重启并清空仓库以外的一切**。恢复与避坑清单：

**Why:** 用户明确说过「这个机器环境经常丢失文件重启」。已经实际发生过多次：
Go 工具链消失、python3/smbclient 消失、`/root/.codebuddy/.../memory` 被清空。
凡是不在 git 里的东西都不可信。

**How to apply:**

1. **重启后先探活再动手**：`ls /usr/local/go/bin`。Go 通常还在
   `/usr/local/go/bin` 但**不在 PATH**，每个 shell 都要
   `export PATH=$PATH:/usr/local/go/bin`。真没了就重装
   `go1.25.0.linux-amd64.tar.gz` 到 `/usr/local/go`。
2. **记忆必须写进 `/workspace/memory/` 并 git 提交**，仓外的
   `/root/.codebuddy/projects/workspace/memory` 只是当次会话的工作副本，重启即失。
3. **`mount.cifs` 在本容器永远跑不通** —— 缺 `CAP_SYS_ADMIN`，报
   `Unable to apply new capability set`。这是环境限制不是服务端 bug，
   `scripts/acceptance.sh` 已把它做成 skip(rc=77)。AGENTS.md §3 要求的"至少三种
   第三方客户端通过"由 **smbclient + impacket + go-smb2** 三家满足，不要在
   mount.cifs 上浪费时间，也不要因为它 fail 就判定验收不通过。
4. **impacket 用 apt 装，不要用 pip**：`apt-get install python3-impacket`
   （pip 装会和已有的 cryptography 版本冲突）。
5. **致命坑一：`pkill -f <路径>` 会自杀**。该模式会匹配到执行它的 shell 自己的命令行，
   把父 shell 一起杀掉，表现为"命令无输出 / 被 SIGTERM / 服务起不来"，极难排查。
   一律用 `pkill -x <进程名>`（只匹配进程名）。
   起后台服务用 `setsid nohup ... > log 2>&1 < /dev/null & disown`。
   **但 `pkill -x` 对超过 15 字符的进程名无效**（内核 comm 字段只有 15 字符），
   报 `pattern that searches for process name longer than 15 characters will
   result in zero matches` 之后**静默匹配不到**：旧进程还活着占着端口，
   新进程绑不上，表现和"服务起不来"一模一样。**调试二进制名控制在 15 字符内**
   （`stupidsamba-info` 就是 16 字符，踩过）。
6. **致命坑二：smbclient 4.22 的 `-c` 不按换行分割命令**。多条命令必须用**分号**分隔。
   写成多行会产生 `NT_STATUS_NO_SUCH_FILE listing \get` 这种**假故障**，
   看起来像服务端 bug，其实是测试脚本的问题。
7. **致命坑三：`setsid nohup ... &` 之后 `$!` 不是监听进程**。`$!` 拿到的是 setsid
   包装进程的 PID，真正 listen 的是它的子进程，`kill $!` 杀不掉，端口一直被占，
   下一次起服务报 address already in use。找真实 PID 用 **`fuser <port>/tcp`**
   （本容器里 `ss -ltnp` 拿不到 pid 列）。
8. **多 agent 共用工作树时，未提交的中间态同样会砸到别人**。真实发生过：
   某 agent 分两步做重命名（先 Edit 改声明、再 sed 改引用），中间约 1 分钟窗口里
   工作树是 `undefined: xxx`，另一个 agent 正好在那时跑 `go build ./...` 撞上，
   花了时间排查一个根本不存在的 bug。**重命名/跨文件改动必须一次原子改完再落盘**，
   不只是 commit 要保证可编译。

9. **调试端口不够分，起服务前先查占用**。AGENTS.md §7.1 只分了 4451~4455 给 5 个 agent，
   但团队已扩到 8 人，端口会撞。实际踩过：在别人占用的 4452 上起代理，bind 静默失败，
   客户端连到了**别人的服务**，报 `STATUS_LOGON_FAILURE`（因为账号不同），
   看起来像自己的认证代码有 bug，白排查。起任何监听前先
   `fuser <port>/tcp`，辅助工具（代理/抓包）用 1xxxx 段避开 agent 端口。
10. **impacket 的 `preferredDialect` 要传常量不能传字符串**：
    `SMBConnection(..., preferredDialect=SMB2_DIALECT_311)`。传 `'3.1.1'` 会抛
    `Exception: Unknown dialect %s`（impacket 自己的报错串没格式化），
    极易误判成"服务端不支持 3.1.1"。

**在共享工作树里做破坏性实验的正确姿势**：用 `git worktree add --detach /tmp/wt HEAD`
另开一份，在 /tmp 里随便改随便编译，做完 `git worktree remove --force /tmp/wt`。
这样既能做"故意改坏看客户端是否报错"的负向验证，又完全不碰 `/workspace`，
不会波及其他 agent。负向验证很有价值：只读代码判断"看起来对"，
远不如把它改坏、确认客户端确实拒绝，来得可信。

**会话历史的备份与恢复**见 AGENTS.md §10 与 `scripts/save-history.sh` /
`scripts/restore-history.sh`。要点：`codebuddy --resume=<uuid>` 可用且不重跑工作，
但 `codebuddy -c` 是陷阱（接的是最近一次会话，崩溃后往往是新开的空壳）。
