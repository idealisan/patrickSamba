---
name: stupidSamba 里反复出现的四类"隐形 bug"
description: 死字段、被上游架空的逻辑、示例配置跑不起来、安全要求有第二个出口——只有端到端才能发现的缺陷
type: project
---

在 stupidSamba 上，**读代码读不出来、只有端到端才能发现**的缺陷反复出现了三类。
排查任何"功能明明写了却不生效"的现象时，先按这三类对号入座。

**Why:** 这三类都在本项目真实发生过并被逐一定位：
1. `config.Share.TimeMachineMaxSize` 声明了、注释写得很详细、**全仓库没有一个读它的地方**。
2. `cmd` 把信号 ctx 一路传给 `srv.Serve(ctx)`，而连接层有
   `context.AfterFunc(ctx, ...nc.Close())` —— 于是"停 accept → 排空 → 超时"整套优雅退出
   代码一行都没生效，客户端看到的一直是硬断。
3. `configs/example.yaml` 里 `addresses: [0.0.0.0, "::"]` 照抄就起不来
   （Go 的 tcp 监听是双栈的，两个通配地址必然 EADDRINUSE）。
4. 同类：`internal/smb/command/ioctl.go` 曾对 SMB < 3.0 的 VALIDATE_NEGOTIATE 回
   `STATUS_FILE_CLOSED`，导致 SMB 2.0.2/2.1 **整条连接被客户端放弃**，而 3.x 正常 ——
   只测一个方言就会漏掉。
5. **第四类：一条安全要求有第二个出口。** `encryption_required` 曾经在三个地方同时失效：
   `session_setup.go` 写的是 `EncryptionRequired && Cipher != 0` —— 多出来的半个条件
   在低方言下恒为假，把安全要求变成**空操作**（不是报错，是静默明文放行）；
   `negotiate.go` 的加密协商挂在 3.1.1 独有的 negotiate context 上，3.0/3.0.2
   **明明支持加密却从没接上**；而 `smb1.go` 的 `AppendSMB1NegotiateReply` 是
   **独立于 handleNegotiate 的第二个协商出口**，客户端只报 `"SMB 2.002"` 时就地定型
   2.0.2 并 `NegotiateDone = true`，写在 handleNegotiate 里的拒绝逻辑一次都不执行。
   三处都修完才算修完，只修最显眼的那处等于没修。

6. **第四类的教科书实例：删除操作有两条完全不同的代码路径。** [实测]
   客户端 CREATE 时直接带 `FILE_DELETE_ON_CLOSE` → `create.go` 置 `open.vfsOwnsDelete = true`，
   命令层**主动让出**，改由 `internal/vfs/local_handle.go` 的 `localHandle.Close()` 自己
   `os.Remove` —— **完全不经过 `LocalFS.Remove`**；只有「先 CREATE 再发
   SET_INFO/FileDispositionInformation」那条路才走 `close.go` 的 `ctx.deleteOnClose` → `fs.Remove()`。
   实测分布：smbclient 与 impacket 删**文件**走前者、删**目录**走后者；go-smb2 两者都走后者。
   含义：任何只加在 `LocalFS.Remove` 上的逻辑（权限检查、审计、元数据清理、回收站语义）
   对最常见的两个客户端的删文件路径**完全不生效**。
   这条是变异测试逼出来的：`remove-noop` 变异没能打红预期的判据，追下去才发现有第二个出口——
   **当时如果图省事把没红的判据从期望清单里划掉，这个事实就永远埋住了。**

**How to apply:**
- 新增配置字段时，**同时**搜一次它的读取点；没有读取点就不要合入。
- **门禁里新加的数值下界，要先证明它够得着。** 同一个脚本里两道判据可能存在
  「支配关系」：严的那道先判红，松的那道永远轮不到出手。实例：
  `test/ci/portable-mode.sh` 曾同时有「枚举用例数 ≥ 12」与「含子测试结果行 ≥ 50」，
  实测把 12 条之外的用例全改名后枚举恰好 12（放行）而结果行掉到 18（判红）——
  那个 `if` 在任何输入下都不可达，已删。判定方法就是给它单独做一个变异体：
  **打不红它的变异体做不出来，就说明它是装饰**。留着不可达判据比没有更糟，
  因为它让读者以为那件事已经有人管，于是不再追问到底谁管。
- **变异测试里「预期该红却没红」的判据是金矿，不是噪音。** 不要调整期望去迁就现实，
  先问「为什么没红」——答案通常是存在你不知道的第二条代码路径。
- 「优雅关闭/超时/重试」这类逻辑必须写端到端冒烟（起服务→操作→信号→断言退出码与耗时），
  因为它们的失效方式是"被上游架空"而不是"报错"。
- `configs/example.yaml` 要纳入 CI 冒烟：示例配置必须能通过校验并真的起得来。
- 协议改动要**每个方言都跑一遍**（2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1），不能只测最高方言。
- 修一条安全要求前，先**穷举它的所有出口**：`grep` 出所有给被守护状态赋值的地方
  （例：`conn.Dialect =` / `NegotiateDone =` 有几处），每一处都要 fail closed。
  条件里出现 `&& x != 0` 这种"多余的半个条件"要格外警惕 —— 它经常是把
  "不满足就拒绝"悄悄变成了"不满足就跳过"。安全语义只有两种正确写法：**满足则放行，
  否则失败**；绝不允许"不满足则静默降级"。
- 验证一个安全边界是否真的生效，最可靠的做法是 **worktree 负向实验**：
  在 `/tmp` 单独 checkout 一份，故意把那条边界打断，确认客户端**确实拒绝**；
  只看"基准能连上"证明不了边界生效（可能是碰巧绕过了）。
