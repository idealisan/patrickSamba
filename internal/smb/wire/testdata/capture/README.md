# 真实 Samba 抓包 fixture

这些 `.bin` 是**真实**第三方客户端与**真实** Samba 4.22 服务器之间的线上字节，
每个文件是一帧（去掉 4 字节 Direct TCP 大端长度前缀之后的 SMB 报文本体）。

- 采集工具：`test/capture/proxy.go`（应用层 TCP 转发代理，不需要 root，不用 tcpdump）
- 采集脚本：`test/capture/capture.sh`（起 smbd、跑客户端、逐场景落盘）
- 比对测试：`internal/smb/wire/capture_test.go`（**跑测试不需要装 samba**）

文件名 `<序号>-<c2s|s2c>-<命令>.bin`，同目录 `manifest.txt` 是人可读摘要。

场景说明：

| 目录 | 客户端 | 看点 |
|---|---|---|
| `negotiate-smb202` | smbclient `-m SMB2_02` | 无 negotiate context 的定长 NEGOTIATE |
| `negotiate-smb210` | smbclient `-m SMB2_10` | LARGE_MTU / leasing 能力位 |
| `negotiate-smb311` | smbclient `-m SMB3_11` | negotiate context 与 8 字节对齐；SESSION_SETUP 之后是加密帧 |
| `session-setup` | smbclient | SPNEGO/NTLMSSP 三段握手 |
| `tree-connect` | smbclient `-L` | IPC$ + srvsvc DCERPC（bind / NetShareEnumAll）+ STATUS_PENDING 异步中间响应 |
| `create-read-write` | smbclient put/get/rm | CREATE / WRITE / READ / SET_INFO |
| `query-directory` | smbclient `ls` | FileIdBothDirectoryInformation 的对齐与末项 NextEntryOffset=0 |
| `query-info` | smbclient `allinfo` | FileAllInformation / FileStreamInformation |
| `set-info` | smbclient rename/setmode/rm | FileRenameInformation / FileBasicInformation / FileDispositionInformation |
| `gosmb2` | hirochachacha/go-smb2 | 与 smbclient 相互印证的第二套独立实现 |

重新采集：`./test/capture/capture.sh [场景名...]`（需要本机装 samba + smbclient）。
