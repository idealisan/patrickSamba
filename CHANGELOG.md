# Changelog

本文件记录 stupidSamba 的版本变更。**v0.1.0 的正文同时作为本版本 Release 页面的说明
正文**。所有内容均以实测与代码核实为依据；「未实现」项如实列出，不做夸大。

---

## v0.2.0（2026-08-09，release）

> **写作纪律**（v0.1.0 的 README 在这上面栽过跟头，改了两轮才诚实；本节保留作为历史）：
>
> 1. **功能没合入 `main` 之前，一个字都不写进这里。** 分支上跑通了不算，PR open 着也不算。
> 2. **打折要写在句子主干里。** 「已支持 X（但 Y 未实现）」是坏写法；
>    「X 尚未通过端到端验收，已验证的是 A/B/C，未实现的是 D/E」是好写法。
> 3. **区分三档置信度，不要混为一谈**：**已实测验证** / **只交叉编译过** /
>    **只读代码推断**。例：`F_FULLFSYNC` 的 darwin 分支属于第二档（开发容器是
>    Linux，从没真跑过），写的时候必须点明。
> 4. **v0.2.0 已将下列此前「在各自分支上」的能力合入 `main`**：durable handle
>    （v1/v2）、ShareAccess、oplock/lease 断连通道、per-share quota、Windows 路径
>    junction 逃逸修复、`internal/meta` 旁路存储。Time Machine 定级据此上调，见
>    [`docs/timemachine-status.md`](docs/timemachine-status.md)。

### 发布物形态

- **裸静态二进制**：`CGO_ENABLED=0 go build` 通过，产物静态链接（`ldd` 报告
  "not a dynamic executable"），符合 C1/C2 硬约束。四平台交叉编译通过：
  linux/amd64、linux/arm64、darwin/arm64、windows/amd64。
- **多架构 Docker 镜像**：`FROM scratch` 基础镜像，**零 `RUN` 指令**（仅 `COPY`，
  无需 QEMU 模拟），内嵌 `stupidsamba` 二进制 + `configs/docker.yaml`，监听 445/tcp
  与 5353/udp。由 `docker buildx` 构建 `linux/amd64` + `linux/arm64` 双架构 manifest。
- 镜像**未推送**至远端仓库：最终 tag 与镜像推送由 team-lead 在全部 PR 合入、
  CI 全绿后执行。本地已构建并验证 manifest 含 amd64 + arm64 两架构。
- 端到端镜像验证脚本 [`scripts/verify-image.sh`](scripts/verify-image.sh)：用
  `smbclient` + `impacket` 对容器做 8 项可证伪校验（启动、静态可执行、读写往返、
  共享不存在被拒的反向对照、数据落到命名卷、guest 警告、mDNS 关闭），均通过。

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

- **durable / persistent handle（v0.2.0 头号新增，PR #119）**：实现持久句柄 v1/v2，
  断网重连后可恢复已打开的句柄。**已实测验证**：`create_context_durable_test.go`
  覆盖 reconnect 签名；但**尚无真实 macOS Time Machine 长跑断线恢复**的端到端证据
  （开发环境无 macOS），置信度属「已实测验证握手与重连路径」，而非「真实备份过程不中断」。
- **ShareAccess（PR #34）**：`CREATE` 现在解析并强制 `ShareAccess` 共享模式
  （R/W/D 互斥/共享），违反时返回 `STATUS_SHARING_VIOLATION`。
- **CREATE context 注册表**：`AAPL` / `AlSi` / `MxAc` / `QFid` + durable 上下文统一登记，
  不再散落硬编码。
- **oplock / lease 断连通道（PR #103）**：`handleOplockBreak` 已接线并返回正确的
  `STATUS_INVALID_OPLOCK_PROTOCOL`，新增相关 NTSTATUS 常量。但**服务端仍不宣告
  `SMB2_GLOBAL_CAP_LEASING`、仍一律授予 `NONE` oplock**——对外可观察行为无变化，
  客户端继续不缓存。属内部修正，为后续真实 oplock 铺路。
- **per-share quota（PR #42）**：`quota_bytes` 向客户端上报卷容量（Time Machine 限容
  的唯一有效手段），现已按共享粒度生效。
- **VFS 修复（PR #38）**：路径安全与属性映射若干修正。
- **Windows 路径 junction 逃逸修复（PR #44）**：防御 `..` 经 junction/符号链接逃逸。
  **置信度：仅交叉编译 + 单元测试通过**，无 Windows 真机验证（开发容器是 Linux）。
- Windows 旁路 POSIX 元数据存储（随 PR #26 合入，`internal/meta`）改用新 bucket 名
  `posix.v2`。v0.1.0 时期由 `internal/vfs/metadata_windows.go` 写入的 `posix` bucket
  记录（若存在）本版本**不再读取**，回退到默认属主/权限——这是有意的、无迁移的改名：
  v0.1.0 的 Windows 后端从未被真机执行过、库里没有真实数据，为不存在的数据写迁移逻辑
  收益为零且引入第二个不可验证路径。详见 `internal/meta/bolt.go` 的 bucketName 注释。

### 安全

- **Windows junction 逃逸修复（PR #44）**：见上「协议与功能」。属服务端路径穿越防御的
  加固，置信度为「仅交叉编译 + 单测」，无 Windows 真机证明。

### 配置

- 路径字段按**运行平台**判定绝对性：`metadata_path` 等路径在错误平台上填绝对路径会直接
  启动失败（而非静默忽略），跨平台校验语义已在 v0.1.0 的 README/example.yaml 修正中落地。

### 内部

- **`internal/meta`（PR #26，v0.3.0 准备）**：纯 Go 嵌入式 KV 旁路存储已合入，但
  **当前产品代码尚未引用**——它要到 v0.3.0 的 `oscap` builtin 适配器落地后才真正启用。
  本版本只是把底座就位，不做功能承诺。

### 已知问题 / 未实现（v0.2.0）

- **`CHANGE_NOTIFY` 仍返回 `STATUS_NOT_SUPPORTED`**：客户端降级为定时轮询，目录列表
  不会自动刷新（需手动刷新）。异步变更通知未实现。
- **真实 oplock / lease 能力仍未对外生效**：见「协议与功能」。
- **Time Machine 仍未通过 macOS 真机端到端验收**：durable handle 已就位但无真机断线
  恢复证据；详见 [`docs/timemachine-status.md`](docs/timemachine-status.md)。

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
