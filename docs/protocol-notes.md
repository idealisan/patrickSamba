# SMB2/3 协议实现备忘（研究结论）

> 依据 MS-SMB2 / MS-FSCC / MS-NLMP / MS-DTYP / MS-ERREF、RFC 1001/1002/4178/4493、SP800-108。
> 常量值均来自规范原文。**不确定的必须标注 `TODO: 待验证`，禁止凭空编造。**

## 1. 传输层

只监听 **445（Direct TCP）**。4 字节头：`Zero(1)=0x00` + `Length(3, 大端)`，长度不含头本身。

```go
n := binary.BigEndian.Uint32(hdr) & 0x00FFFFFF
```

139/NBSS 不做：MS-SMB2 §2.1 明确 SMB 3.1.1 **不允许**走 NetBIOS；所有方言都支持 Direct TCP。

帧解析必须严格「读 4 字节 → 读满 body」。单帧上限 = MaxTransactSize + 余量（建议 1 MiB + 512 B），超限断连。

## 2. SMB2 Packet Header（64 字节）

| 偏移 | 长 | 字段 |
|---|---|---|
| 0x00 | 4 | ProtocolId `0xFE 'S' 'M' 'B'` |
| 0x04 | 2 | StructureSize = 64 |
| 0x06 | 2 | CreditCharge（2.0.2 保留为 0） |
| 0x08 | 4 | Status（响应）/ ChannelSequence+Reserved（3.x 请求） |
| 0x0C | 2 | Command |
| 0x0E | 2 | CreditRequest / CreditResponse |
| 0x10 | 4 | Flags |
| 0x14 | 4 | NextCommand（复合链偏移，相对本头起点） |
| 0x18 | 8 | MessageId |
| 0x20 | 4 | Reserved（ASYNC 头此处起 8 字节为 AsyncId） |
| 0x24 | 4 | TreeId |
| 0x28 | 8 | SessionId |
| 0x30 | 16 | Signature |

**报文体一律小端。**

Flags：`SERVER_TO_REDIR=0x01`、`ASYNC_COMMAND=0x02`、`RELATED_OPERATIONS=0x04`、`SIGNED=0x08`、`PRIORITY_MASK=0x70`、`DFS_OPERATIONS=0x10000000`、`REPLAY_OPERATION=0x20000000`。

### 复合请求（compound）

- `NextCommand != 0` → 本消息长度即 NextCommand；`== 0` → 延伸到帧末尾。每段起点 **8 字节对齐**。
- 置 `RELATED_OPERATIONS` 时复用前一条的 SessionId/TreeId/**FileId**；客户端把 FileId 写成全 `0xFF` 表示"用上一个 CREATE 的句柄"。macOS 大量使用 `CREATE+QUERY_INFO+CLOSE`。
- 链中前序失败 → 后续 related 消息必须直接失败，不得执行。
- 响应也必须复合返回，一次性提交给传输层。
- **签名在复合链中逐条单独计算，且计算范围包含尾部对齐填充字节。**（极易踩坑）

## 3. 命令优先级

P0（挂载必需）：NEGOTIATE 0x00、SESSION_SETUP 0x01、LOGOFF 0x02、TREE_CONNECT 0x03、TREE_DISCONNECT 0x04、CREATE 0x05、CLOSE 0x06、READ 0x08、WRITE 0x09、ECHO 0x0D、QUERY_DIRECTORY 0x0E、QUERY_INFO 0x10、SET_INFO 0x11。

P1：FLUSH 0x07、LOCK 0x0A、IOCTL 0x0B、CANCEL 0x0C。
P2：CHANGE_NOTIFY 0x0F、OPLOCK_BREAK 0x12（可回 `STATUS_NOT_SUPPORTED`）。

**不要**对 FLUSH / LOCK 回 NOT_SUPPORTED —— macOS 与 Office 会判定挂载不可用。

## 4. NEGOTIATE

方言：`0x0202` 2.0.2、`0x0210` 2.1、`0x0300` 3.0、`0x0302` 3.0.2、`0x0311` 3.1.1、`0x02FF` 通配（仅用于回应 SMB1 的 `"SMB 2.???"`）。

Request StructureSize=**36**，Response StructureSize=**65**。

SecurityMode：`SIGNING_ENABLED=0x0001`、`SIGNING_REQUIRED=0x0002`。

Capabilities：`DFS=0x01`、`LEASING=0x02`、`LARGE_MTU=0x04`（**必须设**）、`MULTI_CHANNEL=0x08`、`PERSISTENT_HANDLES=0x10`、`DIRECTORY_LEASING=0x20`、`ENCRYPTION=0x40`。

建议：`MaxTransactSize = MaxReadSize = MaxWriteSize = 0x00100000`（1 MiB；2.0.2 降为 0x10000）。

### SMB1 多协议协商入口（尽量支持）

客户端发 SMB1 `SMB_COM_NEGOTIATE (0x72)`，头 32 字节起始 `0xFF 'S' 'M' 'B'`，body 为 `WordCount=0` + `ByteCount(2)` + 若干 `0x02 + NUL 结尾 ASCII 方言串`。

- 含 `"SMB 2.???"` → 回 SMB2 NEGOTIATE Response，`DialectRevision=0x02FF`，等客户端再发真正的 SMB2 NEGOTIATE。
- 只含 `"SMB 2.002"` → 直接回 `0x0202` 完成协商。
- 都没有 → 关闭连接。

> **证据强度现状（2026-08-25 记录，v0.4.0）**：这条升级入口目前只有**单测档**证据
> （构造 SMB1 报文的单元测试），没有任何第三方客户端实测覆盖——本环境可用的三家
> 客户端里 smbclient 4.22 已剔除 SMB1、impacket 直发 SMB2，都触达不了这条路径
> （详见 `test/reports/client-matrix-v030-20260825.md` §SKIP）。按项目所有者决定
> （2026-08-25），此项暂不投入补测；将来若要宣称「支持 SMB1 客户端升级」，
> 必须先补一条能真实发出 `SMB_COM_NEGOTIATE "SMB 2.???"` 的裸报文回归。

### 3.1.1 NegotiateContext

头：`ContextType(2)+DataLength(2)+Reserved(4)+Data`，context 间 **8 字节对齐**（最后一个后不填充）。

| Type | 值 | 说明 |
|---|---|---|
| PREAUTH_INTEGRITY_CAPABILITIES | 0x0001 | **必须**，客户端恰好发一个，服务端必须回 |
| ENCRYPTION_CAPABILITIES | 0x0002 | 收到必须回（不支持则回 `Ciphers[0]=0x0000`） |
| COMPRESSION_CAPABILITIES | 0x0003 | 收到才回 |
| NETNAME_NEGOTIATE_CONTEXT_ID | 0x0005 | 忽略，不回 |
| TRANSPORT_CAPABILITIES | 0x0006 | 忽略 |
| RDMA_TRANSFORM_CAPABILITIES | 0x0007 | 忽略 |
| SIGNING_CAPABILITIES | 0x0008 | 收到才回 |

PREAUTH Data：`HashAlgorithmCount(2)+SaltLength(2)+HashAlgorithms[](2×N)+Salt`。唯一算法 ID = **0x0001 (SHA-512)**。服务端回 Count=1 + 32 字节随机 Salt。无交集 → `STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP (0xC05D0000)`。

ENCRYPTION Data：`CipherCount(2)+Ciphers[]`。`0x0001 AES-128-CCM`、`0x0002 AES-128-GCM`、`0x0003 AES-256-CCM`、`0x0004 AES-256-GCM`。

SIGNING Data：`SigningAlgorithmCount(2)+SigningAlgorithms[]`。`0x0000 HMAC-SHA256`、`0x0001 AES-CMAC`、`0x0002 AES-GMAC`。

## 5. SESSION_SETUP 与认证

Request StructureSize=**25**，Response StructureSize=**9**。
SessionFlags：`IS_GUEST=0x0001`、`IS_NULL=0x0002`、`ENCRYPT_DATA=0x0004`。

流程：
1. 收到 NTLMSSP_NEGOTIATE → **分配非 0 SessionId 写入响应头**，状态 `STATUS_MORE_PROCESSING_REQUIRED (0xC0000016)`，Buffer 带 NTLMSSP_CHALLENGE。**注意这是错误码但必须带完整的 SESSION_SETUP Response 体，不能回 ERROR Response。**
2. 收到 NTLMSSP_AUTHENTICATE → 成功 `STATUS_SUCCESS`，失败 `STATUS_LOGON_FAILURE (0xC000006D)`。

### SPNEGO

服务端 NEGOTIATE 里的 negTokenInit2（`0x60` APPLICATION 0 包裹，OID `1.3.6.1.5.5.2`），mechTypes 放 NTLMSSP OID `1.3.6.1.4.1.311.2.2.10`。

`negHints` 的 `hintName GeneralString "not_defined_in_RFC4178@please_ignore"` —— **Go 标准库 encoding/asn1 无法 Marshal GeneralString**（golang/go#18832 未修），必须硬编码 DER 字节（`0x1b` = GeneralString tag）。

服务端第一次响应：裸 negTokenResp `0xA1 0x30 { negState[0]=accept-incomplete(1), supportedMech[1]=NTLM OID, responseToken[2]=CHALLENGE }`。
最终：`A1 30 { negState[0]=accept-completed(0) }`。

negState：0=accept-completed、1=accept-incomplete、2=reject、3=request-mic。

简化：先检测 buffer 是否以 `"NTLMSSP\0"` 开头（裸 NTLM），否则浅层 DER 遍历取 `[2] OCTET STRING`。

### NTLM 消息

- NEGOTIATE(Type1)：`"NTLMSSP\0"` + `MessageType=1` + Flags + DomainFields(8) + WorkstationFields(8) + Version(8)
- CHALLENGE(Type2)：固定头 **56 字节**，含 `ServerChallenge(8)`、TargetInfoFields
- AUTHENTICATE(Type3)：固定头 **88 字节**，含 MIC(16)

`*Fields` = `Len(2)+MaxLen(2)+BufferOffset(4)`。

关键 Flags：`UNICODE=0x01`、`REQUEST_TARGET=0x04`、`SIGN=0x10`、`SEAL=0x20`、`NTLM=0x200`、`ALWAYS_SIGN=0x8000`、`TARGET_TYPE_SERVER=0x20000`、`EXTENDED_SESSIONSECURITY=0x80000`、`TARGET_INFO=0x800000`、`VERSION=0x2000000`、`128=0x20000000`、`KEY_EXCH=0x40000000`、`56=0x80000000`。

AV_PAIR（`AvId(2)+AvLen(2)+Value`）：`EOL=0x0000`、`NbComputerName=0x0001`、`NbDomainName=0x0002`、`DnsComputerName=0x0003`、`DnsDomainName=0x0004`、`Flags=0x0006`、`Timestamp=0x0007`、`TargetName=0x0009`、`ChannelBindings=0x000A`。CHALLENGE 的 TargetInfo **必须**含前 5 项，否则 Windows 可能拒绝。

### NTLMv2 服务端校验

```
NTOWFv2 = HMAC_MD5(MD4(UTF16LE(password)), UTF16LE(UPPER(user) + domain))   // domain 不转大写
temp    = 0x01 || 0x01 || Z(6) || Time(8) || ClientChallenge(8) || Z(4) || ServerName || Z(4)
NTProofStr = HMAC_MD5(NTOWFv2, ServerChallenge(8) || temp)
NtChallengeResponse = NTProofStr(16) || temp
SessionBaseKey = HMAC_MD5(NTOWFv2, NTProofStr)
```

服务端取响应前 16 字节与自算 NTProofStr 做 **常量时间比较**（`crypto/subtle`）。

密钥：NTLMv2 下 `KeyExchangeKey = SessionBaseKey`；若 `KEY_EXCH` 置位则 `ExportedSessionKey = RC4K(KeyExchangeKey, EncryptedRandomSessionKey)`，否则等于 KeyExchangeKey。**ExportedSessionKey 即 SMB2 的 Session.SessionKey。**

### Guest / 匿名

匿名：User="" 且 NtChallengeResponseLen=0 → `STATUS_SUCCESS` + `IS_NULL(0x0002)`，SessionKey 全 0，**必须不要求签名**。
Guest：`IS_GUEST(0x0001)`。

⚠️ Windows 10/11 默认禁止不安全 guest（`AllowInsecureGuestAuth=0`）；Windows 11 24H2 客户端**默认要求签名**，而 guest/匿名无 SessionKey 无法签名 → **Windows 目标必须走真实 NTLMv2**。

## 6. 签名与加密

| 方言 | 签名算法 | 密钥 |
|---|---|---|
| 2.0.2 / 2.1 | HMAC-SHA256 取前 16 字节 | Session.SessionKey |
| 3.0 / 3.0.2 | AES-128-CMAC (RFC 4493) | Session.SigningKey |
| 3.1.1 | AES-128-CMAC，或协商的 AES-GMAC (RFC 4543) | Session.SigningKey |

步骤：Signature 字段清零 → 置 `SMB2_FLAGS_SIGNED` → 对整条消息算 MAC → 写回。
NEGOTIATE 响应**不签**；加密的消息**不再单独签名**；SESSION_SETUP 最终成功响应**必须签**。

AES-GMAC 的 12 字节 Nonce：前 8 字节 MessageId；后 4 字节最低位 服务端=1/客户端=0，倒数第二位 CANCEL=1。

### 3.1.1 Preauth Integrity Hash

`H` 初始 64 字节全 0，`H = SHA512(H_prev || 整条消息字节)`（不含 Direct TCP 4 字节头），顺序：

1. NEGOTIATE Request → 2. NEGOTIATE Response → （Connection 级值定型）
3. SESSION_SETUP Req#1 → 4. SESSION_SETUP Resp#1 → 5. SESSION_SETUP Req#2（最后一条）

**最后一条成功的 SESSION_SETUP Response 不参与。** 建立会话时把 Connection 级值**复制**到 Session 级再继续更新，不要污染 Connection 级（多会话）。

### KDF（SP800-108 CTR-HMAC-SHA256，r=32，L=128/256）

单次迭代输入：`[i]_32 || Label || 0x00 || Context || [L]_32`。

| 密钥 | 3.1.1 Label / Context | 3.0/3.0.2 Label / Context |
|---|---|---|
| SigningKey | `"SMBSigningKey"` / PreauthHash | `"SMB2AESCMAC"` / `"SmbSign"` |
| ApplicationKey | `"SMBAppKey"` / PreauthHash | `"SMB2APP"` / `"SmbRpc"` |
| 服务端加密 S→C | `"SMBS2CCipherKey"` / PreauthHash | `"SMB2AESCCM"` / `"ServerOut"` |
| 服务端解密 C→S | `"SMBC2SCipherKey"` / PreauthHash | `"SMB2AESCCM"` / `"ServerIn "`（**结尾有空格**） |

所有 Label/Context 均为**大小写敏感 ASCII 且带结尾 NUL**。

### TRANSFORM_HEADER（52 字节）

`ProtocolId=0x424D53FD`(4) + `Signature`(16) + `Nonce`(16, CCM 用前 11 / GCM 用前 12) + `OriginalMessageSize`(4) + `Reserved`(2) + `Flags/EncryptionAlgorithm`(2) + `SessionId`(8)。

AAD = 头的偏移 0x14–0x33 共 **32 字节**。Nonce 用单调递增计数器，禁止随机（生日碰撞）。

## 7. TREE_CONNECT

Request StructureSize=**9**，路径 UTF-16LE `\\SERVER\share`（**双反斜杠开头，无结尾 NUL**）。服务端**忽略主机名部分**，只取最后一段做 share 名匹配（**大小写不敏感**）。

Response StructureSize=**16**：`ShareType(1)`+`Reserved(1)`+`ShareFlags(4)`+`Capabilities(4)`+`MaximalAccess(4)`。

ShareType：`DISK=0x01`、`PIPE=0x02`、`PRINT=0x03`。
ShareFlags 建议：`MANUAL_CACHING=0x0`，推荐加 `FORCE_LEVELII_OPLOCK=0x1000`。
MaximalAccess：读写 `0x001F01FF`，只读 `0x00120089`。

share 不存在 → `STATUS_BAD_NETWORK_NAME (0xC00000CC)`。

### IPC$

- 直接挂载已知 share：**不需要** IPC$。
- 浏览服务器根（`\\host` 回车 / Finder `smb://host`）：需要 IPC$ + `srvsvc` 命名管道 + MS-RPC `NetrShareEnum`。不实现则看不到共享列表。

低成本折中：接受 IPC$ 的 TREE_CONNECT 回 `ShareType=0x02`；IPC$ 上未知管道回 `STATUS_OBJECT_NAME_NOT_FOUND`；`FSCTL_DFS_GET_REFERRALS (0x00060194)` 回 `STATUS_NOT_FOUND (0xC0000225)`，且 NEGOTIATE **不要**声明 `CAP_DFS`。

⚠️ `FSCTL_VALIDATE_NEGOTIATE_INFO (0x00140204)`：SMB 3.0/3.0.2 + 签名时 Windows 会在 TREE_CONNECT 后立刻发；响应错误会**立即 TCP RESET**。要么正确实现（回显 Capabilities/ClientGuid/SecurityMode/Dialect），要么回 `STATUS_NOT_SUPPORTED`；只协商 3.1.1 则客户端不会发它。

## 8. CREATE

Request StructureSize=**57**，Response StructureSize=**89**。
路径为 UTF-16LE **相对路径，无前导反斜杠**，根目录为空名。
CreateAction：`SUPERSEDED=0`、`OPENED=1`、`CREATED=2`、`OVERWRITTEN=3`。
RequestedOplockLevel：`NONE=0x00`、`II=0x01`、`EXCLUSIVE=0x08`、`BATCH=0x09`、`LEASE=0xFF`。不实现 oplock 时响应一律回 `0x00`。

### CreateDisposition → POSIX

| 值 | 名 | 存在时 | 不存在时 | POSIX |
|---|---|---|---|---|
| 0 | FILE_SUPERSEDE | 删除重建 | 创建 | `O_CREAT\|O_TRUNC` |
| 1 | FILE_OPEN | 打开 | `STATUS_OBJECT_NAME_NOT_FOUND` | `0` |
| 2 | FILE_CREATE | `STATUS_OBJECT_NAME_COLLISION` | 创建 | `O_CREAT\|O_EXCL` |
| 3 | FILE_OPEN_IF | 打开 | 创建 | `O_CREAT` |
| 4 | FILE_OVERWRITE | 截断 | `STATUS_OBJECT_NAME_NOT_FOUND` | `O_TRUNC` |
| 5 | FILE_OVERWRITE_IF | 截断 | 创建 | `O_CREAT\|O_TRUNC` |

### DesiredAccess

`FILE_READ_DATA=0x01`、`FILE_WRITE_DATA=0x02`、`FILE_APPEND_DATA=0x04`、`FILE_READ_EA=0x08`、`FILE_WRITE_EA=0x10`、`FILE_EXECUTE=0x20`、`FILE_DELETE_CHILD=0x40`、`FILE_READ_ATTRIBUTES=0x80`、`FILE_WRITE_ATTRIBUTES=0x100`、`DELETE=0x10000`、`READ_CONTROL=0x20000`、`WRITE_DAC=0x40000`、`WRITE_OWNER=0x80000`、`SYNCHRONIZE=0x100000`（**忽略**）、`ACCESS_SYSTEM_SECURITY=0x1000000`、`MAXIMUM_ALLOWED=0x2000000`、`GENERIC_ALL=0x10000000`、`GENERIC_EXECUTE=0x20000000`、`GENERIC_WRITE=0x40000000`、`GENERIC_READ=0x80000000`。

先展开 GENERIC_*，再决定 `O_RDONLY/O_WRONLY/O_RDWR`。
**只请求 `FILE_READ_ATTRIBUTES` 时不要真的 open()**，用 lstat 即可（Explorer 大量属性探测，真 open 性能很差）→ 对应 `vfs.OpenAttrOnly`。

### CreateOptions

`FILE_DIRECTORY_FILE=0x01`（否则 `STATUS_NOT_A_DIRECTORY 0xC0000103`）、`FILE_WRITE_THROUGH=0x02`、`FILE_SEQUENTIAL_ONLY=0x04`、`FILE_NO_INTERMEDIATE_BUFFERING=0x08`、`FILE_NON_DIRECTORY_FILE=0x40`（否则 `STATUS_FILE_IS_A_DIRECTORY 0xC00000BA`）、`FILE_DELETE_ON_CLOSE=0x1000`（**必须实现**）、`FILE_OPEN_BY_FILE_ID=0x2000`（回 NOT_SUPPORTED）、`FILE_OPEN_FOR_BACKUP_INTENT=0x4000`（忽略）、`FILE_OPEN_REPARSE_POINT=0x200000`（`O_NOFOLLOW`）。

ShareAccess：`READ=0x1`、`WRITE=0x2`、`DELETE=0x4`。

> **实现现状（2026-08-25）**：share-mode 冲突检测**已实现** —— 按 (FileID, 流名) 键控，
> 在 CREATE 的 open 登记处做原子检查（`internal/smb/command/create.go` 的
> `shareModes.check` / `shareModes.add`），冲突回 `STATUS_SHARING_VIOLATION (0xC0000043)`。

### Create Contexts

结构：`Next(4)+NameOffset(2)+NameLength(2)+Reserved(2)+DataOffset(2)+DataLength(4)+Buffer`，Name/Data 各 8 字节对齐，`Next` 相对本 context 起点。

| Name | 处理 |
|---|---|
| `ExtA` | 忽略或存 xattr |
| `SecD` | 忽略 |
| `DHnQ`/`DH2Q` | **不回**该 context = 不授予 durable，客户端可正常工作 |
| `DHnC`/`DH2C` | 回 `STATUS_OBJECT_NAME_NOT_FOUND` |
| `AlSi` | 可 fallocate 或忽略 |
| `MxAc` | **建议实现**：Data = `QueryStatus(4)=0` + `MaximalAccess(4)` |
| `TWrp` | 忽略 |
| `QFid` | **建议实现**：响应 Data 32 字节 = `FileId(8)` + `VolumeId(8)` + `Reserved(16)` |
| `RqLs` | 不回（V1 DataLength=32，V2=52） |
| `AAPL` | Apple 扩展，见 §12 |

## 9. QUERY_DIRECTORY

FileInformationClass：`FileDirectoryInformation=0x01`、`FileFullDirectoryInformation=0x02`、`FileBothDirectoryInformation=0x03`、`FileNamesInformation=0x0C`、`FileIdBothDirectoryInformation=0x25`、`FileIdFullDirectoryInformation=0x26`。

- 条目用 `NextEntryOffset` 链接，**8 字节对齐**，最后一条 `NextEntryOffset=0`。
- Flags：`SMB2_RESTART_SCANS=0x01`、`SMB2_RETURN_SINGLE_ENTRY=0x02`、`SMB2_INDEX_SPECIFIED=0x04`、`SMB2_REOPEN=0x10`。
- 通配符：`*` `?` 以及 DOS 特殊字符 `<`(DOS_STAR) `>`(DOS_QM) `"`(DOS_DOT)。匹配**大小写不敏感**。
- 首次调用无匹配 → `STATUS_NO_SUCH_FILE (0xC000000F)`；后续调用枚举完毕 → `STATUS_NO_MORE_FILES (0x80000006)`。
- 必须包含 `.` 与 `..` 条目（Windows 期待）。

## 10. QUERY_INFO / SET_INFO

InfoType：`FILE=0x01`、`FILESYSTEM=0x02`、`SECURITY=0x03`、`QUOTA=0x04`。

必备 FileInfoClass：`FileBasicInformation=4`、`FileStandardInformation=5`、`FileInternalInformation=6`、`FileEaInformation=7`、`FileAccessInformation=8`、`FileRenameInformation=10`、`FileDispositionInformation=13`、`FilePositionInformation=14`、`FileAllInformation=18`、`FileAllocationInformation=19`、`FileEndOfFileInformation=20`、`FileStreamInformation=22`、`FileNetworkOpenInformation=34`、`FileAttributeTagInformation=35`、`FileIdInformation=59`。

必备 FsInfoClass：`FileFsVolumeInformation=1`、`FileFsSizeInformation=3`、`FileFsDeviceInformation=4`、`FileFsAttributeInformation=5`、`FileFsFullSizeInformation=7`、`FileFsObjectIdInformation=8`、`FileFsSectorSizeInformation=11`。

SECURITY：Windows 属性页会查，返回一个最小可用的自相对 SECURITY_DESCRIPTOR（Owner/Group/DACL 允许 Everyone）即可。

## 11. 时间与属性

FILETIME = 1601-01-01 UTC 起的 100ns 数。

```
FILETIME = (unixNanos / 100) + 116444736000000000
```

FILE_ATTRIBUTE：`READONLY=0x01`、`HIDDEN=0x02`、`SYSTEM=0x04`、`DIRECTORY=0x10`、`ARCHIVE=0x20`、`NORMAL=0x80`、`TEMPORARY=0x100`、`SPARSE_FILE=0x200`、`REPARSE_POINT=0x400`、`COMPRESSED=0x800`、`OFFLINE=0x1000`、`NOT_CONTENT_INDEXED=0x2000`、`ENCRYPTED=0x4000`。

POSIX 映射：目录 → DIRECTORY；点开头 → HIDDEN；无写权限 → READONLY；否则至少给 ARCHIVE 或 NORMAL（**不能返回 0**）。

> **实现现状（2026-08-25，v0.5 开发版 bughunt B3/B4/bh3-F4/bh4-A 之后）**，
> 与上文的合成基线并存，均可在代码里核对：
>
> - **客户端 CREATE 携带的 FileAttributes 按 Samba 语义落地**（`applyCreateDOSAttrs`）：
>   只对 created / overwritten / superseded 生效，FILE_WAS_OPENED 不动属性；
>   目录静默剥掉 DIRECTORY 位；普通文件叠 ARCHIVE；raw==0 的新建不落旁路记录
>   （合成已报 ARCHIVE，避免 TM 十万级 band 目录的创建写库风暴）；覆盖时读改写补 ARCHIVE。
>   （`internal/vfs`，commit `d873f72`）
> - **创建时间（btime）在新建时真正落进旁路库**：挂在 openFile/openDir/Mkdir 的
>   created 分支与 SUPERSEDE 作废旧账之后；能力矩阵把 CapCreationTime 交给 native 时
>   跳过旁路写入（内核 birthtime 已是真值）。（commit `5795e3b`）
> - **READONLY 属性目标的 WRITE 被拒绝**（`STATUS_ACCESS_DENIED`）：按打开时的属性快照判定，
>   判据是配置与属性位，不读宿主 ACL。（commit `9177e6c`）
> - **字节范围锁已接入 READ/WRITE 强制检查**（strict locking，对齐 Samba
>   `smb2_read.c` / `smb2_write.c` 的 STRICT_LOCK_CHECK）：读只被外句柄独占锁阻挡，
>   写被任何重叠的外句柄锁阻挡；豁免单位是句柄（同句柄自己的锁不妨碍自己）；
>   句柄消失的全部路径统一释放锁。冲突回 `STATUS_FILE_LOCK_CONFLICT (0xC0000054)`。
>   （commits `a4d3839`、`1791a43`、`d22fbd0`）

## 12. Credit 管理

- `CreditCharge = ceil(max(SendPayloadSize, Expected ResponsePayloadSize) / 65536)`，至少 1。
- 服务端在响应的 `CreditResponse` 里授予；初始给 1，之后维持客户端请求量（常见做法：授予 = 请求量，上限 512）。
- credit 耗尽客户端会停止发送 → 表现为挂起。**必须保证任何响应都至少授予 1 个 credit。**

## 13. 常用 NTSTATUS

| 值 | 名 |
|---|---|
| 0x00000000 | STATUS_SUCCESS |
| 0x00000103 | STATUS_PENDING |
| 0x80000005 | STATUS_BUFFER_OVERFLOW |
| 0x80000006 | STATUS_NO_MORE_FILES |
| 0xC0000001 | STATUS_UNSUCCESSFUL |
| 0xC0000002 | STATUS_NOT_IMPLEMENTED |
| 0xC000000D | STATUS_INVALID_PARAMETER |
| 0xC000000F | STATUS_NO_SUCH_FILE |
| 0xC0000010 | STATUS_INVALID_DEVICE_REQUEST |
| 0xC0000011 | STATUS_END_OF_FILE |
| 0xC0000016 | STATUS_MORE_PROCESSING_REQUIRED |
| 0xC0000022 | STATUS_ACCESS_DENIED |
| 0xC0000023 | STATUS_BUFFER_TOO_SMALL |
| 0xC0000034 | STATUS_OBJECT_NAME_NOT_FOUND |
| 0xC0000035 | STATUS_OBJECT_NAME_COLLISION |
| 0xC000003A | STATUS_OBJECT_PATH_NOT_FOUND |
| 0xC0000043 | STATUS_SHARING_VIOLATION |
| 0xC000006D | STATUS_LOGON_FAILURE |
| 0xC00000BA | STATUS_FILE_IS_A_DIRECTORY |
| 0xC00000BB | STATUS_NOT_SUPPORTED |
| 0xC00000C9 | STATUS_NETWORK_NAME_DELETED |
| 0xC00000CC | STATUS_BAD_NETWORK_NAME |
| 0xC0000101 | STATUS_DIRECTORY_NOT_EMPTY |
| 0xC0000103 | STATUS_NOT_A_DIRECTORY |
| 0xC000010E | STATUS_FILE_CLOSED |
| 0xC0000120 | STATUS_CANCELLED |
| 0xC0000128 | STATUS_FILE_INVALID |
| 0xC000019C | STATUS_FS_DRIVER_REQUIRED |
| 0xC0000203 | STATUS_USER_SESSION_DELETED |
| 0xC0000225 | STATUS_NOT_FOUND |
| 0xC0000257 | STATUS_PATH_NOT_COVERED |
| 0xC000027E | STATUS_INSUFF_SERVER_RESOURCES |
| 0xC05D0000 | STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP |

## 14. Apple 扩展（阶段二）

`AAPL` create context 请求 Data：`CommandCode(4)` + `Reserved(4)` + `RequestBitmap(8)` + `ClientCapabilities(8)`。
`CommandCode`：`kAAPL_SERVER_QUERY=1`、`kAAPL_RESOLVE_ID=2`。

响应 Data：`CommandCode(4)` + `Reserved(4)` + `ReplyBitmap(8)` + 按位可选的 `ServerCapabilities(8)` / `VolumeCapabilities(8)` / `ModelString`。

ReplyBitmap：`kAAPL_SERVER_CAPS=0x1`、`kAAPL_VOLUME_CAPS=0x2`、`kAAPL_MODEL_INFO=0x4`。
ServerCapabilities：`kAAPL_SUPPORTS_READ_DIR_ATTR=0x1`、`kAAPL_SUPPORTS_OSX_COPYFILE=0x2`、`kAAPL_UNIX_BASED=0x4`、`kAAPL_SUPPORTS_NFS_ACE=0x8`。
VolumeCapabilities：`kAAPL_SUPPORT_RESOLVE_ID=0x1`、`kAAPL_CASE_SENSITIVE=0x2`、`kAAPL_SUPPORTS_FULL_SYNC=0x4`。

> `TODO: 待验证` —— readdir_attr 扩展后 QUERY_DIRECTORY 条目的确切追加布局，以及 Time Machine 的完整判定条件，需对照 Samba `vfs_fruit` 源码与真实抓包确认后再实现。

mDNS 需要广播三组服务（阶段二细化）：

- `_smb._tcp` 端口 445
- `_device-info._tcp` TXT `model=<Model>`
- `_adisk._tcp` TXT `dk0=adVN=<share>,adVF=0x82` 与 `sys=waMa=0,adVF=0x100`

## 15. 实现里程碑

1. **M1 能被 mount**：Direct TCP 帧 + Header 编解码 + NEGOTIATE + SESSION_SETUP(NTLMv2) + TREE_CONNECT + ECHO + LOGOFF
2. **M2 能浏览**：CREATE/CLOSE + QUERY_INFO(File/FS) + QUERY_DIRECTORY
3. **M3 能读写**：READ/WRITE/FLUSH + SET_INFO（rename/delete/EOF）+ Mkdir
4. **M4 稳**：签名、复合请求、credit、错误映射、并发与资源上限、IOCTL 兜底
5. **M5 SMB3 加密** + 3.1.1 preauth
6. **M6 Apple 扩展 + Time Machine**
