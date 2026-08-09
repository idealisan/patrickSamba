# Time Machine 支持状态（v0.1.0）

> 本文由 `tmverify` 出具，是 v0.1.0 对 Time Machine 支持程度的**唯一权威定级依据**。
> README 与 CHANGELOG 的相关表述以本文为准。

## 结论

**定级：C 档。**

> v0.1.0 已实现 Time Machine 所需的全部服务端前置能力（AAPL 协商、命名流、稀疏文件、
> 卷容量与 quota 上报、F_FULLFSYNC、大目录性能、`_adisk._tcp` 广播），并用 impacket
> 低阶 SMB2 客户端逐项实测通过；但**尚未经过 macOS 真机端到端备份与恢复验收**，
> 且 durable handle、oplock/lease、AAPL resolve_id 均未实现，长时间备份遇到网络抖动
> 可能中断。**请勿用它承载唯一一份备份。**

### 定级标准

| 档 | 含义 | 是否适用 |
|---|---|---|
| A | macOS 真机端到端备份并成功恢复 | ❌ 排除。开发容器内没有 macOS，无法验证 |
| B | TM 所需服务端前置能力全部实现且逐项验证，仅缺真机验收 | ❌ 未采用，理由见下 |
| **C** | **主要能力具备，但有明确的已知缺口，可能导致备份失败或不稳定** | **✅ 本次定级** |
| D | 关键能力缺失，不建议尝试 | ❌ |

### 为什么是 C 而不是 B

下表 9 项前置能力确实全绿，按字面看够得上 B。压到 C 的是**枚举清单之外**的三个缺口，
它们不属于"TM 前置能力"，却直接决定备份能不能跑完：

1. **durable / persistent handle 未实现**（`internal/smb/command/session.go:289`、
   `internal/smb/command/open.go:13` —— Persistent 与 Volatile 取同一个单调递增值）。
   风险最高的一条。TM 一次备份动辄数小时，durable handle 的存在意义就是让客户端在
   TCP 断开后重连并接管原句柄；没有它，一次网络抖动 = 句柄全丢 = 备份中断重来。
   这不是"性能差一点"，是"可能永远备不完"。
2. **oplock / lease 未实现**（`internal/smb/command/create.go:153`、`:225` 恒回
   `OplockLevelNone`）。客户端退化成不缓存，band 那种小文件密集写的吞吐明显受损。
3. **零 macOS 真机数据**。B 档的措辞容易被读成"就差走个过场"，而 1 和 2 都有可能
   让真机第一次备份就失败。

**明确澄清**：本项目从未跑过一次真实的 Time Machine 备份，更没有做过恢复。
下表验证的是"TM 依赖的服务端能力逐项是否可用"，**不是备份本身**。

## 验证方法

- **被测版本**：发布 HEAD `c7e98e3`。下表所有数字均为在该版本上的复测结果，非历史数据。
- **客户端**：`impacket`（低阶 SMB2，直接构造 CREATE context / IOCTL / QUERY_INFO 报文），
  以及自写的 mDNS 抓包脚本。选低阶客户端是因为 AAPL create context、FSCTL 稀疏三件套
  这些都不是高阶 API 能触及的。
- **权威对照**：Samba `vfs_fruit.c`（`check_aapl`、`fruit_pwrite_meta`）、
  `smb2_trans2.c`（readdir_attr 布局）、MS-FSCC、MS-SMB2、RFC 6762/6763。
- **调试端口**：4462。三个共享 `share`（无 quota）/ `backup`（quota 2 TiB，
  `time_machine: true`）/ `tmsmall`（quota 10 GiB，`time_machine: true`）用于对照。

## 逐项结论

| # | 能力 | 状态 | 对 TM 的影响 |
|---|---|---|---|
| 1 | AAPL create context — server query | 已实现且实测通过 | 前置，缺则 macOS 全程走通用 SMB 路径 |
| 2 | AAPL create context — resolve id | **未实现** | **无实际影响**，见下文 |
| 3 | AAPL create context — readdir_attr | 已实现且实测通过 | 缺则 Finder 逐个文件回查元数据，大目录极慢 |
| 4 | 命名流 / Alternate Data Stream | 已实现且实测通过 | 硬前置，缺则 `.sparsebundle` 无法建立 |
| 5 | 稀疏文件（FSCTL 三件套） | 已实现且实测通过 | 缺则 band 空洞占满真实磁盘 |
| 6 | 卷容量与 quota 上报 | 已实现且实测通过 | 缺则 TM 无法判断空间、会吃满宿主磁盘 |
| 7 | `F_FULLFSYNC` 语义 | 已实现（**仅代码级证据**） | 数据完整性，掉电时影响备份可用性 |
| 8 | 大目录性能（band 目录） | 已实现且实测通过 | 缺则每次备份卡在枚举上 |
| 9 | mDNS `_adisk._tcp` 广播 | 已实现且实测通过 | 缺则 TM 偏好设置里看不到本服务 |
| — | **durable / persistent handle** | **未实现** | **对 TM 影响最大的缺口**，见"为什么是 C" |
| — | **oplock / lease** | **未实现** | 小文件写吞吐低于 Samba，不影响正确性 |

### 1. AAPL server query

**已实现且实测通过。**

- `ServerCapabilities = 0x5` = `kAAPL_SUPPORTS_READ_DIR_ATTR | kAAPL_UNIX_BASED`。
- `VolumeCapabilities = kAAPL_SUPPORTS_FULL_SYNC`，**仅在 `time_machine: true` 的共享上置位**
  （`internal/smb/command/aapl.go:227`，同 Samba 的 `fruit:time machine` 语义）；
  在普通 `share` 上实测为 0。
- 未实现的能力一律不宣告：`kAAPL_SUPPORTS_OSX_COPYFILE`、`kAAPL_SUPPORTS_NFS_ACE`
  （`aapl.go:216`）、`kAAPL_SUPPORT_RESOLVE_ID`（`aapl.go:240`）。
  这是有意的原则 —— **宣告了客户端就会真的去用**。
- 健壮性：畸形长度（23 字节 / 32 字节）与未知 CommandCode（2、99、0xffffffff）
  一律回 `STATUS_INVALID_PARAMETER`，不 panic。

### 2. AAPL resolve id

**未实现。对 Time Machine 没有实际影响。**

`aapl.go:240` 恒不置 `kAAPL_SUPPORT_RESOLVE_ID`，收到 `CommandCode=2` 直接拒绝。
因为我们从不宣告该能力，**客户端根本不会发这种请求** —— 实测发过去也只是拿到
`STATUS_INVALID_PARAMETER`，属于安全降级。

真正受影响的是 Finder 的别名 / 最近项目按 64 位 file id 反查路径，会退化为按路径查找。
要实现它需要一张持久化的 file id → 路径映射表，成本远高于收益，v0.1.0 不做。

### 3. readdir_attr

**已实现且实测通过。**

协商后 `withmeta.txt` 的目录项实测回：

```
rfork=777  FinderInfo=544558547474787401000022320aad7c
dateAdded(BE)=0x320aad7c  EaSize=0x001f01ff(max access)  ShortNameLength=24
```

关键的**负向对照**：客户端发 `bitmap=0x4`（只请求 ModelInfo、未请求 ServerCaps）时，
枚举**正确退回标准布局**（`rfork=0`、`SNLen=0`、`EaSize=0`）。

这条对齐 Samba `check_aapl()` —— 它把 `config->readdir_attr_enabled = true` 写在
`if (req_bitmap & SMB2_CRTCTX_AAPL_SERVER_CAPS)` 分支**内部**。没请求这一段的客户端
收不到 ServerCapabilities，也就无从得知我们支持 readdir_attr；此时若仍改写目录项布局，
它会把 `rfork_size` 与压缩 FinderInfo 当成真正的 8.3 短名去解析。
**本轮之前这里是 bug（无条件启用），已修，见 `6688ceb`。**

### 4. 命名流 / Alternate Data Stream

**已实现且实测通过。** 这是 TM 的硬前置：`.sparsebundle` 靠命名流存放 Apple 元数据。

- `AFP_AfpInfo`（固定 60 字节）、`AFP_Resource`（资源派生，落为 `._<name>` 旁车文件）、
  任意 `:name:$DATA` 的建立 / 读 / 写 / 枚举（`FileStreamInformation`，class 22）全部通过。
- **目录上的流也支持** —— `.sparsebundle` 本身是目录，实测其 `:AFP_AfpInfo:$DATA` 可读写。
- 写全零 FinderInfo 正确删除 xattr（流从枚举中消失），再写非空可重建。
- `FILE_NAMED_STREAMS`(0x40000) 已在 `FileFsAttributeInformation` 中宣告
  （`internal/smb/command/query_info.go:26-30`）—— macOS 只有看到这一位才会去查
  `FileStreamInformation`。

**本轮修掉一个真 bug（`bc0a38e`）**：`AFP_AfpInfo` 的首段写原本只在长度恰为 60 时校验，
59 字节或垃圾签名会被静默收进缓冲，直到 flush 才失败 —— 而 SMB2 CLOSE 承载不了错误，
表现为「WRITE 成功 → CLOSE 成功 → xattr 根本没建 → 读回 `OBJECT_NAME_NOT_FOUND`」，
**数据无声蒸发**。现已对齐 Samba `fruit_pwrite_meta` 的两道早期拦截：

```c
if (n < 3)                       { errno = EINVAL; return -1; }
if (memcmp(data, "AFP", 3) != 0) { errno = EINVAL; return -1; }
```

分段写的第一段必然带完整签名（`"AFP\0"` 在 `[0,4)`），所以拦签名不会误伤合法分段写 ——
测试里有一条专门的**反向对照**用例（`"AFP\0" + 12 字节`必须放行）保证这一点。

与 Samba 的行为差异（实测记录，均为有意取舍）：

| 写入 | Samba | 本实现 |
|---|---|---|
| `n=2` | EINVAL | `STATUS_INVALID_PARAMETER` ✅ 一致 |
| `n=16`（带合法签名） | no-op 返回 16 | 接受 16（进缓冲） |
| `n=20`（带合法签名） | 补齐到 60 写入 | 接受 20（进缓冲） |
| `n=61` | 截到 60 写入 | `STATUS_INVALID_PARAMETER` |
| `offset=8, n=60` | 强制 offset=0 | `STATUS_INVALID_PARAMETER` |

我们比 Samba 严格的两处（`n=61`、`offset≠0`）是刻意的：Samba 那种"悄悄纠正客户端"的做法
会掩盖客户端 bug，而我们保留了分段写缓冲能力，正常客户端不会触发。

### 5. 稀疏文件

**已实现且实测通过。** 全部三个 FSCTL 都真正落到文件系统的稀疏语义上，不是空转返回成功。

| 操作 | 结果 |
|---|---|
| `FSCTL_SET_SPARSE(TRUE)` | ok |
| `FSCTL_SET_SPARSE(FALSE)` | `STATUS_NOT_SUPPORTED` —— **诚实拒绝**，我们真的无法取消稀疏 |
| `FSCTL_SET_SPARSE`（空输入，隐含 TRUE） | ok |
| 64 MiB 文件两端各写 64 KiB | `size=67108864`，**实占 131072 字节** |
| `FSCTL_QUERY_ALLOCATED_RANGES` | `[(0, 65536), (67043328, 65536)]` |
| `FSCTL_SET_ZERO_DATA(0, 65536)` | ok，**实占降至 65536**，打洞区读回全零 |
| 打洞后再 QAR | `[(67043328, 65536)]` —— 前段已消失 |

畸形输入全部正确拒绝：QAR 输入仅 8 字节（不足 16）、负 offset、ZERO_DATA 区间逆序，
均回 `STATUS_INVALID_PARAMETER`。

`FILE_SUPPORTS_SPARSE_FILES`(0x40) 已在卷属性中宣告 —— macOS 看到这一位才会
对 band 文件发这些 FSCTL。

### 6. 卷容量与 quota 上报

**已实现且实测通过。** 三个共享横向对照：

| 共享 | 配置 | 上报总容量 | 上报可用 |
|---|---|---|---|
| `share` | 无 quota | 256.00 GiB | 252.58 GiB |
| `backup` | `quota_bytes` = 2 TiB（**大于**宿主） | 256.00 GiB | 252.58 GiB |
| `tmsmall` | `quota_bytes` = 10 GiB（**小于**宿主） | **10.00 GiB** | **6.58 GiB** |

- 无 quota 时与宿主 `statvfs` 完全一致（256.00 GiB / 252.59 GiB）。
- quota **大于**宿主真实容量时**不放大** —— 只做上限钳制，不虚报。
- quota **小于**宿主时正确钳制（`internal/vfs/local.go:586-594`）。
- `FileFsSizeInformation`(3) 与 `FileFsFullSizeInformation`(7) 两个 info class 都正确。
- 实测 `FileFsAttributeInformation = 0x00040046`
  = `CASE_PRESERVED_NAMES | UNICODE_ON_DISK | SUPPORTS_SPARSE_FILES | SUPPORTS_OBJECT_IDS`
  （`FILE_CASE_SENSITIVE_SEARCH` 按卷真实能力动态清除，本地磁盘后端报大小写不敏感，
  符合 SMB 语义）。

**给用户的提示**：备份共享应显式设置 `quota_bytes`，否则 TM 会一直备份到吃满宿主磁盘。
配置校验对 `quota_bytes < 1 GiB` 会给出启动告警（`internal/config/validate.go:478-481`）。

### 7. `F_FULLFSYNC` 语义

**已实现，但只有代码级证据 —— 这是本表唯一一项无法在本环境实测的能力。**

调用链：`handleFlush`（`internal/smb/command/read_write.go:189-217`）
→ `Handle.Sync(true)` → `platformFullSync`：

| 平台 | 实现 | 文件 |
|---|---|---|
| Linux | `f.Sync()`（`fsync`） | `internal/vfs/sys_linux.go:40` |
| **Darwin** | **`unix.FcntlInt(fd, F_FULLFSYNC, 0)`**，失败退化为 `f.Sync()` | `internal/vfs/sys_darwin.go:35` |
| Windows | `FlushFileBuffers` | `internal/vfs/sys_windows.go:59` |
| 其他 | `f.Sync()` | `internal/vfs/sys_other.go:15` |

资源派生流的 `Sync(full)` 同样走 `platformFullSync`。因此 `aapl.go:227` 宣告
`kAAPL_SUPPORTS_FULL_SYNC` 是**名副其实**的，不是空头承诺。

**诚实的缺口**：Darwin 那条分支在本容器里既编不了也跑不了，只有交叉编译通过作为保证。
`F_FULLFSYNC` 与普通 `fsync` 的区别（前者要求磁盘固件真正落盘、不停留在磁盘缓存）
只能在真机上验证。

### 8. 大目录性能（`.sparsebundle/bands`）

**已实现且实测通过。** 构造了 50 000 个文件的 band 目录（TM 的真实形态）。

全量枚举，`FileIdBothDirectoryInformation`（class 37）：

| 客户端 OutputBufferLength | 轮次 | 总耗时 | 吞吐 |
|---|---|---|---|
| 8 KiB | 685 | 1.884 s | 26 535 条/秒 |
| 64 KiB | 86 | 0.480 s | 104 106 条/秒 |
| 128 KiB | 43 | 0.370 s | 135 188 条/秒 |
| 1 MiB | 6 | **0.279 s** | **179 458 条/秒** |

主要成本是**往返轮次**，不是服务端处理 —— 客户端给的缓冲越大越快。首轮固定约 185 ms
（一次性快照 5 万个名字并排序），之后每轮都很便宜。

- **协商 AAPL readdir_attr 后**：1 MiB 缓冲下 0.401 s，额外开销约 30%（要为每个条目
  取 FinderInfo 与资源派生大小），可接受。
- **内存**：连续 3 轮全量枚举，RSS 稳定在 10～17 MiB，HWM 60 MiB **不增长**（无泄漏）。
- **前缀通配 `1*`**：命中 4369 条，8 轮，0.056 s。
- **单名精确查询：0.76 ms**（200 次平均）。

最后一项是 TM 的**最高频操作** —— 增量备份时它要反复询问"band X 是否存在"。
**本轮为此做了优化（`5132cfd`）**：模式里没有通配符时直接 `stat` 目标名，
不再 `readdirnames` + 排序整个目录。单次查询从 **22.9 ms 降到 0.76 ms（33 倍）**。
微基准 `BenchmarkReadDirExactName` 为 12 µs（不含网络往返）。

配套 7 个测试（`internal/vfs/readdir_exact_test.go`）覆盖：与通配扫描结果一致、
大小写不敏感匹配、`.` / `..` / 路径穿越 / `._` 前缀的排除、EOF 语义、
以及快照缺失时资源派生信息的正确回退。

### 9. mDNS `_adisk._tcp` 广播

**已实现且实测通过。** 用自写脚本在 172.17.0.44 上实抓报文（不是读代码推断）。

`_adisk._tcp` TXT 实测内容：

```
dk0=adVN=backup,adVF=0x82
sys=waMa=0,adVF=0x100
```

`_device-info._tcp` TXT：`model=TimeCapsule8,119`。

核对结果：

- `dk0` 的 `adVN` 是共享名 `backup`；**非 `time_machine` 的 `share` 未被宣告为备份卷** ✅
- `_smb._tcp` SRV `port=4462`（真实监听端口）；`_adisk` / `_device-info` SRV `port=0`
  （这两个不是真实服务，无端口）✅
- A 记录 `tmverify.local → 172.17.0.44` ✅
- **legacy 单播查询**（源端口 ≠ 5353）：复用查询 ID `0xbeef`、所有 TTL 压到 ≤10 秒，
  符合 RFC 6762 §6.7 ✅
- **QM 组播查询**（源端口 = 5353）：组播回应，cache-flush 位置位，TTL 120（SRV/A）
  / 4500（PTR/TXT）✅
- 附带 NSEC 记录（声明不存在的记录类型，抑制客户端追问）✅
- 启动时 probe + announce、停止时 goodbye（TTL=0）均实见 ✅

**待验证项**：`adVF=0x82` 与 `waMa=0,adVF=0x100` 这两个魔数来自 avahi 社区广泛使用的
Samba Time Machine 配方，**尚未从 Apple 官方文档或真机抓包确认**，代码中已标注
`TODO: 待真实抓包验证`（`internal/mdns/apple.go`）。它们不影响服务被发现，
但可能影响 TM 偏好设置里的图标与卷类型显示。

## 已知缺口与对用户的影响

按对 Time Machine 的实际危害排序：

| 缺口 | 危害 | 说明 |
|---|---|---|
| durable / persistent handle | **高** | 断线重连无法接管原句柄。TM 备份持续数小时，网络抖动、Wi-Fi 漫游、服务端重启都会导致备份中断重来 |
| oplock / lease | 中 | 客户端无法缓存，band 小文件密集写吞吐低于 Samba。**不影响正确性** |
| `F_FULLFSYNC` 无真机验证 | 中 | 代码路径正确，但掉电场景下的实际落盘行为未验证 |
| AAPL resolve_id | **无** | 不宣告则客户端不使用。仅影响 Finder 别名解析，退化为按路径查找 |
| `adVF` 魔数未经真机确认 | 低 | 可能影响 TM 偏好设置中的图标 / 卷类型显示，不影响发现与连接 |

### 另记一处宣告与行为不一致

`internal/smb/command/tree_connect.go:56-58` 设置了 `ShareFlagForceLevelIIOplock`，
但 `create.go` 恒回 `OplockLevelNone`，`notify.go:64` 的注释也明说"本服务从不授予 oplock"。
FORCE_LEVELII_OPLOCK 的语义是"本共享强制把 oplock 降级为 Level II 授予"，
而我们一个都不授予。

**不是 TM 的阻断项**（客户端拿不到 oplock 就是不缓存，行为安全），但与本项目
"做不到的一律不宣告"的原则（见 §1 对 AAPL 能力位的处理）不一致。
v0.1.0 登记为已知问题，不修改。

## 给用户的使用建议

1. **本功能从未在真实 macOS 上验证过。** 首次备份就失败是完全可能的结果。
2. **不要把这里当作唯一备份目的地。** 先用可随便清空的机器或无关紧要的测试数据试跑，
   确认能完成一轮完整备份并成功浏览快照后，再考虑放真实数据。
3. **优先使用有线局域网连接。** 没有 durable handle，网络稳定性直接决定备份能否完成。
4. **给备份共享显式设置 `quota_bytes`。** 否则 TM 会一直备份到吃满宿主磁盘。
5. **备份失败时请提供 `log.level: debug` 的服务端日志** —— 内含每个 SMB 命令与
   返回的 NTSTATUS，比 macOS 侧的报错信息有用得多。

## 后续版本的改进方向

按优先级：

1. **durable handle（含 durable v2 / persistent handle）** —— 唯一能把定级从 C 推到 B
   之上的改动。需要在 Session 层保留断连后的句柄状态与超时回收。
2. **oplock / lease** —— 至少实现 Level II 与 read-caching lease，让客户端敢缓存。
3. **真机验收** —— 拿到 macOS 环境后跑完整的备份 + 恢复，这是 A 档的唯一路径。
4. **`adVF` 魔数真机抓包确认**，去掉代码里的 `TODO`。
5. AAPL resolve_id（需要持久化 file id 映射表，收益最低）。

---

*本文所有实测数据来自发布 HEAD `c7e98e3`。若后续代码变更，请重新验证后更新本文。*
