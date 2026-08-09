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

**How to apply:**
- 新增配置字段时，**同时**搜一次它的读取点；没有读取点就不要合入。
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
