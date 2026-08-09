---
name: smbclient 4.22 做加密/协商验证时的三个坑
description: --client-protection 的合法值、smbclient 总是宣告 CAP_ENCRYPTION、拒绝码 NOT_SUPPORTED 与 ACCESS_DENIED 的区分
type: reference
---

用 `smbclient`（Samba 4.22.x）验证 SMB3 加密与方言协商时，有三个会让人误判的行为。

1. **`--client-protection` 的合法值是 `off` / `sign` / `encrypt`。**
   写成 `plain` 会直接 `Failed to parse --client-protection` 而**不是**报错退出到你能注意到的地方，
   容易被当成"服务端把连接拒了"。

2. **smbclient 在 NEGOTIATE 里永远宣告 `SMB2_GLOBAL_CAP_ENCRYPTION`，
   即使 `--client-protection=off`。** 实测服务端日志照样是 `cipher=1`。
   后果：**"SMB 3.0 客户端不宣告加密能力 → 服务端 ACCESS_DENIED" 这一档
   没法用 smbclient 端到端造出来**，只能写单测或用裸 socket 客户端。
   别因为造不出来就以为那段 fail-closed 代码是死代码。

3. **两种拒绝码含义完全不同，断言时必须分开。**
   - `NT_STATUS_NOT_SUPPORTED` = 没有公共方言（服务端 `min_dialect` 把客户端挡在门外）；
   - `NT_STATUS_ACCESS_DENIED` = 方言协商成功了，但加密要求不满足（fail closed）。
   开着 `encryption_required: true` 时 `min_dialect` 被强制 ≥3.0，
   所以 `-m SMB2_02 / SMB2_10` 拿到的是 **NOT_SUPPORTED**，不是 ACCESS_DENIED。
   把两者写成同一个断言会掩盖"方言拦截失效但加密拦截还在"这类回归。

附：SMB1 多协议协商路径 smbclient 造不出"只报 SMB 2.002"的报文（它总带通配 `SMB 2.???`），
要验这条得用裸 socket：Direct TCP（1 字节 0 + 3 字节大端长度）+ 32 字节 SMB1 头
（`\xffSMB` + cmd `0x72`）+ `WordCount=0` + `ByteCount`(小端) + 若干 `0x02 + 方言串 + NUL`。
