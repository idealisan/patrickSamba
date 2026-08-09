# Time Machine 验收报告 — v0.1.0

- 验收人：`tmverify`
- 被测基线：`76458e3`（从 `origin/main` 新建干净工作树重新编译，`CGO_ENABLED=0`）
- 验收日期：2026-08-09
- 客户端栈：impacket（低阶 SMB2 报文构造）、原始 UDP mDNS 报文收发

---

## 结论：**C 档 —— 未通过端到端验收**

> v0.1.0 **未通过 Time Machine 端到端验收**：容器内没有 macOS，真机「备份并成功恢复」
> 一次都没有跑过；服务端侧的 TM 前置能力已用非 macOS 客户端逐项实测，但**真实
> oplock/lease 与 AAPL resolveID 均未实现**，可能导致 Time Machine 备份不稳定甚至失败。
> 请勿用真实备份数据试用，仅建议用测试数据验证。

**需要特别说明**：这不是「备份成功了但恢复没验」，而是**备份与恢复两头都没有在真实
macOS 上跑过**。本报告的全部证据来自非 macOS 客户端对服务端行为的探针，
证明的是「服务端具备 TM 所需的前置能力」，**不等于**「Time Machine 可用」。

### 为什么不是 B 档

B 档要求「TM 所需的服务端前置能力都实现了」。该前提不成立，且不是主观判断，是代码事实：

| 缺口 | 代码位置 | 事实 |
|---|---|---|
| 真实 oplock/lease | `internal/smb/command/notify.go:55` | NEGOTIATE 中**未**声明 `SMB2_GLOBAL_CAP_LEASING`，从不授予任何 oplock/lease；收到 lease break 直接回 `STATUS_INVALID_PARAMETER`。`wire/` 里虽有 lease 报文结构，但无人调用。 |
| AAPL resolveID | `internal/smb/command/aapl.go:240` | `aaplSupportResolveID` 恒不置位（`_ = aaplSupportResolveID`），`kAAPL_RESOLVE_ID` 命令走「收到即异常」分支。 |

### 为什么也不是 D 档

前置能力并非空白 —— 下表 7 项均已实现且实测通过，其中多项带反向对照。

---

## 逐项实测结果

| # | 项目 | 结果 | 关键证据 |
|---|---|---|---|
| 1 | AAPL create context 协商 | ✅ 通过 | `time_machine: true` 共享回 `ServerCaps=0x5`(READ_DIR_ATTR+UNIX_BASED)、`VolumeCaps=0x4`(FULL_SYNC)、`Model=MacSamba`；bitmap 子集请求（0x1/0x4/0x0）均按位裁剪响应 |
| 2 | `SUPPORTS_FULL_SYNC` 宣告 | ✅ 通过（含反向对照） | 仅 `time_machine: true` 的共享回 `VolumeCaps=FULL_SYNC`；普通共享回 `VolumeCaps=0`。**反向对照成立**，不是恒真 |
| 3 | AAPL 畸形输入 | ✅ 通过 | 长度 23/32（±8 字节）、`CommandCode=2`(RESOLVE_ID)、`CommandCode=99` 一律 `STATUS_INVALID_PARAMETER`，无 panic |
| 4 | `readdir_attr` | ✅ 通过（含反向对照） | 协商 AAPL 后目录项携带 `max_access=0x001f01ff`、资源派生大小（`rfork=777`）、FinderInfo 与 dateAdded；**未协商时同一目录回标准布局全零**；仅请求 `bitmap=0x4`（不含 SERVER_CAPS）时亦回标准布局，与 Samba 语义一致 |
| 5 | 稀疏文件 FSCTL 三件套 | ✅ 通过 | `SET_SPARSE(TRUE)`/空输入均 ok；64 MiB 文件两端各写 64 KiB 后磁盘占用仅 128 KiB；`QUERY_ALLOCATED_RANGES` 精确回 `[(0,65536),(67043328,65536)]`；`SET_ZERO_DATA` 打洞后占用降至 64 KiB 且读回全零，QAR 随之更新。畸形输入（8 字节、负 offset、逆序区间）均 `STATUS_INVALID_PARAMETER` |
| 6 | 命名流 / ADS | ✅ 通过 | `AFP_AfpInfo`、`AFP_Resource`、任意命名流读写正常，落盘为 `._f.txt` 旁路；`FileStreamInformation` 枚举正确，缓冲不足时按 `STATUS_INFO_LENGTH_MISMATCH` / `STATUS_BUFFER_OVERFLOW` 分级返回；全零 FinderInfo 触发元数据删除、可重建；目录上三种流三种待遇（`AFP_AfpInfo` ok / `AFP_Resource` NOT_FOUND / 其他 ok），与 macOS 行为一致；部分写语义对齐 Samba `fruit_pwrite_meta` |
| 7 | `FLUSH` / `F_FULLFSYNC` 语义 | ✅ 通过（含反向对照） | `read_write.go:194` 调 `h.Sync(true)`；darwin 走 `unix.FcntlInt(F_FULLFSYNC)` 并在 ENOTSUP 时退化（`sys_darwin.go:35`），linux 走 `f.Sync()`，windows/other 各有实现。普通文件/命名流 FLUSH 成功且数据可见，目录句柄按 MS-SMB2 跳过，**无效 FileID 回 `STATUS_INVALID_HANDLE`**（反向对照成立） |
| 8 | `quota_bytes` 卷容量上报 | ✅ 通过（含反向对照） | 10 GiB 配额共享上报 10.000 GiB（宿主为 256 GiB），`FileFsSizeInformation` 与 `FileFsFullSizeInformation` 一致；无配额共享上报宿主真实容量；配额大于宿主时按宿主上报。**详见下方「发现 1」** |
| 9 | `_adisk._tcp` mDNS 宣告 | ✅ 通过（含反向对照） | `dk0=adVN=backup,adVF=0x82`、`sys=waMa=0,adVF=0x100`；`_device-info._tcp` 回 `model=TimeCapsule8,119`；`_smb._tcp` SRV port=4462、`_adisk._tcp` SRV port=0；legacy 单播回应 TTL 压到 ≤10s（RFC 6762 §6.7）并复用查询 ID。**非 `time_machine` 共享未被宣告为备份卷**，反向对照成立 |
| 10 | 大目录（`.sparsebundle` bands）性能 | ✅ 通过 | 50,002 条目目录：`FileIdBothDirectoryInformation` 全量枚举 0.46s（约 10.8 万条/秒），1 MiB 缓冲时 0.27s；协商 readdir_attr 后 0.63s，仍在可用区间；连续 3 次全量枚举 RSS 稳定在 11~16 MiB（HWM 55 MiB），**无内存泄漏**；精确名字单查 2.03 ms/次（含网络往返） |

---

## 发现 1：`quota_bytes` 的「可用空间」按宿主整盘已用量扣减，空共享也可能上报 0 可用

这是本轮唯一具有用户可见影响的新发现，**不是崩溃，是一个有意的设计取舍**，
已在 `internal/vfs/local.go:573-611` 的注释中写明（递归统计十万级 band 目录要几秒，
每次 `QUERY_FS_INFO` 都做一遍不可接受）。

实测（宿主 `/tmp` 总 256 GiB、已用 3.679 GiB）：

| 共享 | `quota_bytes` | 上报总容量 | 上报可用 | 共享自身实际占用 |
|---|---|---|---|---|
| `share` | 未设 | 256.000 GiB | 252.328 GiB | — |
| `backup` | 2 TiB（> 宿主） | 256.000 GiB | 252.328 GiB | — |
| `tmsmall` | 10 GiB | 10.000 GiB | 6.328 GiB | 0 |
| `tmtiny` | 2 GiB | 2.000 GiB | **0.000 GiB** | **0 字节（空目录）** |

即公式为 `可用 = min(宿主可用, 配额 - 宿主整盘已用)`，钳位到 0（不会为负）。

**用户可见后果**：当 `quota_bytes` 小于宿主卷的已用空间时，即使共享目录完全是空的，
客户端也会看到「可用 0 字节」。macOS 会直接判定磁盘已满并拒绝开始备份。

**给用户的建议**（已同步给 docs）：`quota_bytes` 应设为大于「宿主卷已用空间 + 期望备份体积」，
否则会得到一个看起来无缘无故就是满的备份卷。

**给后续版本的建议**（不在 v0.1.0 范围）：`internal/config/validate.go:483` 目前只在
`quota_bytes < 1 GiB` 时告警，建议增加一条启动期检查 —— 当 `quota_bytes` 小于宿主卷
已用空间时给出明确告警，把这个「上报 0 可用」的坑在启动时就暴露出来。

## 发现 2：探针本身曾经不可证伪（方法论教训）

本轮复核发现，早先的 `quota_bytes` 探针只用了 **2 TiB（大于宿主 256 GiB）** 的配额，
观察到「上报 256 GiB」就记为通过 —— 但这个结果在 `quota_bytes` 是**死字段**的情况下
完全相同，因此原探针**不具备证伪能力**，等于没测。改用 10 GiB（小于宿主）后才真正
验证了该字段生效，并顺带发现了发现 1。

配额、上限、阈值类字段必须用**跨过阈值两侧**的取值各测一次，只测不触发的一侧无意义。

---

## 未测项（诚实声明）

以下内容**完全没有验证**，任何关于它们的结论都不应出现在文档中：

- **真实 macOS 客户端的任何行为** —— 容器内无 macOS，所有 macOS 版本均未实测。
- **Time Machine 备份流程本身** —— 未创建过真实 `.sparsebundle` 备份、未完成过一次备份。
- **Time Machine 恢复流程** —— 未执行过任何恢复。
- **长时间 / 大体量备份的稳定性** —— 最大规模测试为 50k 空文件枚举与 64 MiB 稀疏文件。
- **Finder 交互**（挂载、图标、卷属性显示）。
- **断网重连 / 会话中断后的备份续传**。

---

## 风险提示（按可能性排序）

1. **无 oplock/lease** —— macOS 客户端拿不到任何缓存租约。这是最可能引发问题的缺口：
   长时间、海量小文件（band）的备份过程中表现未知，可能显著变慢，也可能中途报错。
2. **`quota_bytes` 配置不当直接表现为「磁盘已满」** —— 见发现 1，且当前无启动告警。
3. **resolveID 未实现** —— 影响相对可控：该能力未在 VolumeCapabilities 中宣告，
   macOS 在未被宣告时不会使用它。
