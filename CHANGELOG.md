# Changelog

本文件记录 stupidSamba 的版本变更。**v0.1.0 的正文同时作为本版本 Release 页面的说明
正文**。所有内容均以实测与代码核实为依据；「未实现」项如实列出，不做夸大。

---

## Unreleased（v0.2.0 开发中）

> **写作纪律**（v0.1.0 的 README 在这上面栽过跟头，改了两轮才诚实）：
>
> 1. **功能没合入 `main` 之前，一个字都不写进这里。** 分支上跑通了不算，PR open 着也不算。
> 2. **打折要写在句子主干里。** 「已支持 X（但 Y 未实现）」是坏写法；
>    「X 尚未通过端到端验收，已验证的是 A/B/C，未实现的是 D/E」是好写法。
> 3. **区分三档置信度，不要混为一谈**：**已实测验证** / **只交叉编译过** /
>    **只读代码推断**。例：`F_FULLFSYNC` 的 darwin 分支属于第二档（开发容器是
>    Linux，从没真跑过），写的时候必须点明。
> 4. **Time Machine 定级在相关能力合入 `main` 之前保持不变**（当前 **C 档**，
>    依据见 [`docs/timemachine-status.md`](docs/timemachine-status.md)）。
>    durable handle / oplock-lease / quota 的改动正在各自分支上，未合入前不动结论。

### 开发流程与工具（不影响运行时行为）

- 新增 [`docs/dev-workflow.md`](docs/dev-workflow.md)：开发工作流 SOP
  （独立 git worktree + 独立分支 + PR）。v0.2.0 起全员适用。
- 新增 `scripts/devenv.sh`：开发环境削峰配置（编译串行锁、`GOFLAGS=-p=1`、
  git 重打包内存上限、`gobuild` / `gocheck` / `gocross` 快捷命令）。
- CI 新增**测试代码编译门禁**：对所有 build tag × 全平台组合执行 `go build ./...`，
  确保 `//go:build integration` 等被隔离的测试代码也真实可编译（此前它们从未被编译校验过）。
  门禁自带**负向验证**（故意写坏一处应当失败），避免门禁本身形同虚设。
- 修复 `scripts/save.sh` 的 `git push` refspec（**影响使用者，请注意**）：v0.1.0 时期在
  非 `main` 分支的 worktree 里跑 `save.sh` 会**静默把提交推丢**——脚本写死
  `git push -q origin main`，而各 worktree 共享同一份 `.git`，`main` 解析到的是
  **别人的本地 main**。现已改为推当前分支，并在分离 HEAD 时拒绝推送。
  ⚠️ 若你曾在自己的 worktree 用过旧版 `save.sh`，请立即
  `git log --oneline origin/<你的分支>..HEAD` 自查是否有未推送的提交。
- 修正 `README.md` / `configs/example.yaml` 中**与真二进制实测不符**的描述：启动日志
  改成真实 slog 输出格式、路径字段按运行平台判定绝对性、metadata_path 的「会被忽略」
  改为如实说明跨平台校验行为（填错平台的绝对路径会直接启动失败）。

### 协议与功能

- 新增 oplock / lease 相关的三个 NTSTATUS 常量；`handleOplockBreak` 改回返回
  `STATUS_INVALID_OPLOCK_PROTOCOL`（此前误用 `STATUS_INVALID_PARAMETER`）。
  属协议状态机内部修正，不改变对外可观察行为。
- Windows 旁路 POSIX 元数据存储（随 PR #26 合入，`internal/meta`）改用新 bucket 名
  `posix.v2`。v0.1.0 时期由 `internal/vfs/metadata_windows.go` 写入的 `posix` bucket
  记录（若存在）本版本**不再读取**，回退到默认属主/权限——这是有意的、无迁移的改名：
  v0.1.0 的 Windows 后端从未被真机执行过、库里没有真实数据，为不存在的数据写迁移逻辑
  收益为零且引入第二个不可验证路径。详见 `internal/meta/bolt.go` 的 bucketName 注释。

---

## v0.1.0（2026-08-09，prerelease）

第一个可对外试用的版本。目标是提供一个**可用的 SMB2/3 文件共享服务**：单个静态
二进制、零外部依赖、账户自管理、自带服务发现。

### Security（本次发布最重要的一条）

- **修复 `encryption_required` 在 SMB 3.0 / 3.0.2 及更低方言下被静默忽略、导致明文传输
  的问题**，并补齐 **3.0 / 3.0.2 的 AES-128-CCM 加密**（此前仅 3.1.1 支持加密）。
  修复后 `encryption_required: true` 会**拒绝**协商到 SMB 2.0.2 / 2.1 的客户端，
  而非降级为明文。此前版本在 3.0/3.0.2 上静默以明文传输、且 `encryption_required`
  开关在低方言下完全失效，属安全缺陷，已在 v0.1.0 修正。

### 协议与方言

- 支持 **SMB 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1** 协商与文件共享，协商范围可配置。
- **SMB1 仅作为多协议协商入口**：识别客户端先发的 SMB1 `SMB_COM_NEGOTIATE`
  （带 `"SMB 2.???"`）并用 SMB2 响应将其升级到 SMB2；**不提供任何 SMB1 文件操作**，
  因此无 SMB1 文件操作相关的历史漏洞攻击面。

### 认证与安全

- **NTLMv2 服务端校验**（纯 Go 实现，不依赖系统用户库）。
- **SPNEGO/NTLMSSP** 协商，**仅 NTLM，不支持 Kerberos**。
- **账户完全自管理**：用户名与口令（明文或 `nt_hash`）只来自配置文件，
  与宿主系统用户无任何关系（不读 `/etc/passwd`、不走 PAM/NSS、`os/user.Lookup`
  类查询一律不用）。
- **SMB 签名**：2.x 用 HMAC-SHA256（取前 16 字节），3.x 用 AES-128-CMAC（RFC 4493，
  纯 Go 实现）；可由 `signing_required` 强制。
- **SMB3 加密**：3.0 / 3.0.2 用 AES-128-CCM（经 `SMB2_GLOBAL_CAP_ENCRYPTION` 能力位
  隐式启用），3.1.1 经 `ENCRYPTION_CAPABILITIES` 协商上下文选择密码套件
  （AES-128/256-CCM 或 GCM）；`encryption_required: true` 会拒绝协商到
  SMB 2.0.2 / 2.1 的客户端，而非降级明文。

### 文件操作（SMB2 命令）

共实现 19 个 SMB2 命令：NEGOTIATE、SESSION_SETUP、LOGOFF、ECHO、TREE_CONNECT、
TREE_DISCONNECT、CREATE、CLOSE、READ、WRITE、FLUSH、LOCK、QUERY_INFO、SET_INFO、
QUERY_DIRECTORY、IOCTL、CANCEL、CHANGE_NOTIFY、OPLOCK_BREAK。未实现命令统一返回
`STATUS_NOT_SUPPORTED`，连接不会被打崩。

- 多目录共享、只读共享、按用户授权（`valid_users`）、隐藏共享（`browseable: false`）。
- 路径穿越、符号链接逃逸、超界 offset/length 均在服务端统一拦截。
- `IOCTL` 实现：`VALIDATE_NEGOTIATE_INFO`（防降级复核）、稀疏文件三件套
  `SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`、`ENUMERATE_SNAPSHOTS`
  （回 0 快照）、`QUERY_NETWORK_INTERFACE` 等。
- `IPC$` 命名管道（DCERPC/srvsvc）可用，支持共享枚举。

### 服务发现（mDNS / DNS-SD）

- **进程内** mDNS/DNS-SD responder，在 `224.0.0.251:5353` / `[ff02::fb]:5353` 收发
  报文，**不依赖** `avahi` / Bonjour / `systemd-resolved`。
- 广播 `_smb._tcp`、`_device-info._tcp`（Apple 扩展）、`_adisk._tcp`
  （Time Machine 磁盘宣告）。

### Apple 扩展（AAPL）

- `AAPL` create context：server query / volume caps / model info 协商。
- `readdir_attr`：目录项携带 FinderInfo 与资源派生大小（客户端请求且后端能提供
  Apple 元数据时启用）。
- `FLUSH` 走强制刷盘（`F_FULLFSYNC` 语义），并在 `time_machine` 共享上宣告
  `SUPPORTS_FULL_SYNC`。`F_FULLFSYNC` 的 Linux / Windows 分支实测通过；Darwin 分支
  在本开发容器里编不了也跑不了，仅以交叉编译通过做保证。
- 命名流 / Alternate Data Stream（`AFP_AfpInfo` / `AFP_Resource` 与任意 `:name:$DATA`，
  文件与目录上均支持，`FILE_NAMED_STREAMS` 已在卷属性中宣告）—— `.sparsebundle` 依赖此能力。
- 稀疏文件 FSCTL 三件套已支持 `.sparsebundle` 打洞与回收。
- **修复**（`bc0a38e`）：畸形 `AFP_AfpInfo` 写入曾**静默丢失数据**，现已拒绝非法结构并保留既有内容。
- **优化**（`5132cfd`）：判断 `.sparsebundle` band 是否存在从约 22.9 ms 降到约 0.76 ms（约 30 倍），
  大目录枚举不再随 band 数量线性变慢。

### 配置与运维

- 严格 YAML 加载（未知字段直接启动失败）+ 启动前一次性校验（共享目录必须已存在、
  方言合法、`valid_users` 必须在 `auth.users` 中定义、通配地址不与其他地址并列等）。
- 多 IP 监听、单端口；`max_connections` 并发上限（默认 256）。
- `quota_bytes`：向客户端上报卷容量（Time Machine 限制备份体积的唯一有效手段）。
- 日志级别 / 格式 / 文件可配置；`-check` 仅校验不启动；`-version` 打印版本/commit/
  构建时间。

### 构建与分发

- 纯 Go、关 CGO，`CGO_ENABLED=0 go build` 通过；产物静态链接（`ldd` 非动态可执行）。
- 四平台交叉编译通过：linux/amd64、linux/arm64、darwin/arm64、windows/amd64。
- 发布物：`stupidsamba_<version>_<os>_<arch>.tar.gz`（Windows 为 `.zip`）+ `SHA256SUMS`，
  包内含 README / CHANGELOG / `configs/example.yaml`。

### Time Machine 状态

**定级：C 档。** Apple SMB 扩展（AAPL create context、`readdir_attr`、命名流 / Alternate
Data Stream、稀疏文件 FSCTL、`_adisk._tcp` 广播）已实现，Time Machine 所需的服务端前置
能力已具备，并经非 macOS 客户端（impacket 低阶 SMB2）逐项实测通过。

但 **v0.1.0 尚未通过 macOS 真机端到端备份与恢复验收**——开发环境没有 macOS，「备份并成功
恢复」一次都没有跑过。且以下能力**未实现**，可能导致备份不稳定甚至失败：

- **durable / persistent handle** —— 影响最大：一次备份动辄数小时，断网后已打开的句柄无法
  恢复，网络抖动会导致备份中断重来。
- **oplock / lease** —— 服务端不声明 `SMB2_GLOBAL_CAP_LEASING`、不授予任何 oplock，
  客户端退化为不缓存，band 文件密集写吞吐受损。
- **AAPL `resolveID`** —— 对 Time Machine 本身无实际影响（不宣告则客户端不会使用），
  仅 Finder 别名 / 最近项目按 file id 反查退化为按路径查找。

**请勿用于唯一备份。** 详细逐项验证证据见仓库
[`docs/timemachine-status.md`](docs/timemachine-status.md)。普通文件共享功能不受
Time Machine 验收进度影响。

### 已知问题 / 未实现

- **`CHANGE_NOTIFY` 返回 `STATUS_NOT_SUPPORTED`**：客户端降级为定时轮询，Finder /
  资源管理器目录列表不会自动刷新（需手动刷新）。异步变更通知未实现。
- **真实 oplock / lease 能力未实现**：`CREATE` 一律授予 `NONE` oplock，不宣告 leasing。
- **不支持**：完整 SMB1 文件操作、Kerberos/AD、DFS、打印机共享、持久句柄
  （durable handle）、多通道（multichannel）、目录租约（directory leasing）。
- **445 为特权端口**：非 root 需 `setcap cap_net_bind_service=+ep` 或用 `>=1024` 端口。
- **guest 登录**：`allow_guest: true` 后任意口令（含错误口令）均可 guest 登录（SMB
  语义本身）；Windows 10/11 默认拒绝不安全 guest。
- **明文口令**：配置 `password` 会明文落盘并触发启动 `WARN`，建议改用 `nt_hash`。

---

## 版本说明

- 版本号遵循语义化；v0.x 表示「可用但未全部验收」的内部测试阶段。
- 本版本标记为 **prerelease**，许可证尚未确定（许可证待定）。
