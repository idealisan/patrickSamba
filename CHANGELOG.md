# Changelog

本文件记录 stupidSamba 各版本的变更。**v0.1.0 的正文同时用作 GitHub / CNB Release
说明**，所以写得像「一个陌生人打开 Release 页面第一眼看到的东西」。

---

## v0.1.0 (prerelease)

> **这是第一个公开发布版本，标记为 prerelease。** 它提供了**可用的 SMB2/3 文件共享**
> （macOS / Windows / Linux 客户端均可连接），但仍有若干已知限制（见下方
> 「已知问题 / 未实现」）。Time Machine 端到端验收仍在进行，结论以 tmverify 报告为准。

stupidSamba 是一个**用纯粹 Go 实现的、极简易用的 SMB/CIFS 文件共享服务器**：
单个静态二进制、零外部依赖、不读宿主系统用户、自带进程内 mDNS 服务发现。
下载对应平台的压缩包，写一个配置文件，运行即可。

### 能力概览

**SMB 协议（核心文件共享）**

- 支持方言 **SMB 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1**，协商范围可在配置里限定。
- 完整的 SMB2 命令处理：NEGOTIATE / SESSION_SETUP / LOGOFF / ECHO / TREE_CONNECT /
  TREE_DISCONNECT / CREATE / CLOSE / READ / WRITE / FLUSH / LOCK / QUERY_INFO /
  SET_INFO / QUERY_DIRECTORY / IOCTL / CANCEL，共 19 个。
- 多目录共享、只读共享、按用户授权（`valid_users`）、隐藏共享（`browseable`）。
- 进程内 mDNS/DNS-SD 广播：`_smb._tcp` / `_device-info._tcp` / `_adisk._tcp`，
  **不依赖** avahi / Bonjour / systemd-resolved。

**认证与安全**

- NTLMv2 服务端校验，走 SPNEGO/NTLMSSP（仅 NTLM，不含 Kerberos）。
- 账户完全由本软件配置文件管理，与宿主系统用户零关系（不读 `/etc/passwd`、
  不走 PAM/NSS）。
- SMB 签名：2.x 用 HMAC-SHA256，3.x 用 AES-128-CMAC（RFC 4493，自实现）。
  可由 `signing_required` 强制。
- SMB3 加密：3.0/3.0.2 用 AES-128-CCM，3.1.1 协商 AES-128/256-CCM 或 GCM。
  可由 `encryption_required` 强制。

**Apple 生态扩展（阶段二，进行中）**

- `AAPL` create context：server query / volume caps / model info 协商；
  `readdir_attr`（目录项携带 FinderInfo / 资源派生大小）在客户端请求且后端支持时启用。
- 稀疏文件 FSCTL 三件套 `SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`
  已实现 —— 这是 `.sparsebundle` 备份卷能正确打洞、回收空间的关键。
- `FLUSH` 走强制刷盘（`F_FULLFSYNC` 语义），并在 `time_machine` 共享上通过 AAPL
  宣告 `SUPPORTS_FULL_SYNC`。
- 通过 `quota_bytes` 上报卷容量（限制 Time Machine 备份体积的**唯一有效手段**），
  并广播 `_adisk._tcp` 让 Finder 发现备份磁盘。

**VFS（虚拟文件系统）**

- 可写的 `vfs.FileSystem` 接口抽象，当前提供本地磁盘实现；句柄语义
  （Open → Handle → 操作 → Close），不是路径语义。
- 路径穿越防护（`..` / 绝对路径 / 符号链接逃逸一律拒绝）。
- 所有来自网络的 offset/length 字段使用前都做边界校验；单帧大小、并发连接数、
  每连接句柄数、目录枚举缓冲均有上限。

**构建与分发**

- 纯 Go、`CGO_ENABLED=0` 编译，产物为静态链接的单一二进制（四个平台均可交叉编译）。
- 发布脚本 `scripts/build-release.sh` 一键产出四平台压缩包 + `SHA256SUMS`，并对每个
  产物自检：CGO 已关、`-trimpath` 生效、目标平台正确、版本号已注入、静态链接实证。

### 已知问题 / 未实现

- **Time Machine 端到端验收未定级**：AAPL 协商、稀疏文件 FSCTL、F_FULLFSYNC、
  `_adisk` 广播、容量上报等前置能力均已实现，但「macOS 能否完成一次备份并成功恢复」
  以 tmverify 报告为准，本版本不据此下结论。
- **`encryption_required` 可被方言降级绕过**：客户端主动协商到 SMB 2.1 或更低方言时，
  可以避开加密要求（服务端不会因此拒绝）。需配合 `min_dialect: "3.0"` + `signing_required`
  使用。后续版本会修正。
- **目录变更通知未实现**：`CHANGE_NOTIFY` 返回 `STATUS_NOT_SUPPORTED`，客户端降级为
  定时轮询；Finder / 资源管理器不会自动刷新目录列表（需手动刷新）。
- **真实 oplock / lease 能力未实现**：`CREATE` 一律授予 `NONE` oplock，未宣告 leasing；
  收到 oplock/lease break 按协议返回错误。
- **SMB1 仅作多协议协商入口**：支持把 SMB1 协商升级到 SMB2，但**不提供任何 SMB1
  文件操作**。
- 不支持：DFS、持久句柄（durable handle）、SMB 多通道（multichannel）、目录租约
  （directory leasing）、Kerberos/AD 域、打印机共享。
- `max_connections` 当前 `0` 或不填 = 默认上限 256，不支持「完全不限」。
- `445` 是特权端口：非 root 需要 `setcap` 或改用 `>=1024` 端口。

### 升级说明

首个版本，无升级事项。配置示例见 `configs/example.yaml`。

---

## 格式说明

后续版本沿用：

- `新增` / `变更` / `修复` / `已知问题` 分组；
- 每条尽量点出「用户能感知到什么」；
- 涉及协议/安全的行为变化必须在「升级说明」里写明。
