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
> 4. **PR 号必须逐条核对过再写。** 本节初稿把 durable handle 记成 PR #119、
>    oplock 通道记成 #103、per-share quota 记成 #42、VFS 修复记成 #38 ——
>    这四个号实际上分别是两篇 memory 文档和两个测试 PR，全错。核对方法：
>    `git log --oneline --merges --ancestry-path <功能提交>..origin/main | tail -1`。
>
> **本节只写 v0.1.0 之后的增量。** v0.1.0 已交付的能力（SMB 2.0.2~3.1.1 协商、
> NTLMv2、SMB 签名、SMB3 加密与 `encryption_required` 修复、19 个 SMB2 命令、
> 进程内 mDNS/DNS-SD、多共享、AAPL / 命名流 / 稀疏文件等 Apple 扩展）在本版本
> **继续有效且未做改动**，完整描述见下方 v0.1.0 小节，此处不复述。
> 特别地：`internal/smb/crypto`、`internal/auth`、`internal/smb/dialect` 三个目录
> 自 v0.1.0 起**零改动**（`git diff --stat v0.1.0..main` 对这三个目录输出为空），
> 因此本版本没有任何「加密 / 签名修复」可写，不要把 v0.1.0 的那条搬过来。

### 发布物形态

- **裸静态二进制**：`CGO_ENABLED=0 go build` 通过，产物静态链接（`ldd` 报告
  "not a dynamic executable"），符合 C1/C2 硬约束。四平台交叉编译通过：
  linux/amd64、linux/arm64、darwin/arm64、windows/amd64。
- **多架构 Docker 镜像**：`FROM scratch` 基础镜像，**零 `RUN` 指令**（仅 `COPY`，
  无需 QEMU 模拟），内嵌 `stupidsamba` 二进制 + `configs/docker.yaml`，监听 445/tcp
  与 5353/udp。由 `docker buildx` 构建 `linux/amd64` + `linux/arm64` 双架构 manifest。
  镜像里的二进制**与裸包里的是同一份字节**：`scripts/docker-build.sh` 从
  `scripts/build-release.sh` 产出的 `.tar.gz` 里解出二进制再 `COPY` 进镜像，
  于是裸包那五项自检（CGO 关没关、`-trimpath` 生效没、GOOS/GOARCH 对不对、
  版本号有没有真注入、`ldd` 静态链接）自动覆盖到镜像。若改成在 Dockerfile 里
  `RUN go build`，镜像里的二进制反而会成为整个发布物中唯一没被自检过的东西。
- 镜像**尚未推送**至远端制品库。目标地址
  `docker.cnb.cool/finalappstore/stupidsamba:<版本>`（**私密仓库，拉取前必须先
  `docker login docker.cnb.cool -u cnb -p <访问令牌>`**，用户名是固定字面量 `cnb`）。
  本地已构建并核验：`docker create --platform` 解析探针确认 manifest 含
  `linux/amd64` + `linux/arm64`；把二进制从镜像两个架构分别抠出来看 ELF `e_machine`
  分别是 `0x003e`(x86-64) 与 `0x00b7`(AArch64)，构建信息 `CGO_ENABLED=0`；
  两份 sha256 与 `dist/` 裸包里解出来的二进制**逐位相同**（`81c242c16b3852a6…` /
  `d2fbdf7b7c79064f…`），即镜像与裸包确系同一份字节。
- 端到端镜像验证脚本 [`scripts/verify-image.sh`](scripts/verify-image.sh)：用
  `smbclient` + `impacket` 对容器做 8 项可证伪校验（启动、`scratch` 中静态可执行、
  smbclient 读写往返、共享不存在被拒的反向对照、数据确实落到命名卷、impacket 独立
  客户端栈往返、guest 警告、mDNS 关闭）。
  **已实测：8/8 通过、0 skip**（2026-08-09 17:00 CST，docker 29.6.2 / buildx v0.35.0
  实跑，smbclient 与 impacket 均在场；协商到的方言 `0x300`）。
  并做过**变异对照**：把内置配置的共享路径改成 `/nonexistent-share-dir` 重打镜像，
  同一脚本退出码 1 并打出「配置校验失败: shares[0].path: 共享 "public" 的目录不存在」，
  正常镜像退出码 0 —— 证明这 8 个 PASS 有鉴别力，不是恒真。
- `scripts/docker-build.sh` 的多架构自检**不再在本地构建时跳过**：改用
  `docker create --platform` 做解析探针，并以一个未构建的架构（`linux/s390x`）
  做反向对照。此前本地路径直接打印「跳过 manifest 核对」，等于「多架构」在推送前
  从来没被验证过——buildx 只出宿主架构时命令照样返回 0，要等 arm64 用户拉下来
  `exec format error` 才发现。

### 开发流程与工具（不影响运行时行为）

- 新增 [`docs/dev-workflow.md`](docs/dev-workflow.md)：开发工作流 SOP
  （独立 git worktree + 独立分支 + PR）。v0.2.0 起全员适用。
- 新增 `scripts/devenv.sh`：开发环境削峰配置（编译串行锁、`GOFLAGS=-p=1`、
  git 重打包内存上限、`gobuild` / `gocheck` / `gocross` 快捷命令）。
- CI 新增**测试代码编译门禁** [`test/ci/check-test-compile.sh`](test/ci/check-test-compile.sh)：
  对所有 build tag × 四个目标平台的组合执行 **`go vet`**（不是 `go build`），
  确保 `//go:build integration` 等被隔离的测试代码也真实通过类型检查
  （此前它们从未被编译校验过）。
  **刻意用 `go vet` 而非 `go build`**：后者根本不编译 `_test.go`，正是本门禁要堵的
  第二个洞——只跑 `go build` 时，写坏的测试文件在四平台交叉编译那关也照样绿。
  门禁自带**负向验证**（故意写坏一处应当失败），避免门禁本身形同虚设。
  新增 build tag 必须同步登记进该脚本的 `TAGS=`，否则带该 tag 的文件没有任何一关会编译它
  （脚本自己会检查这件事并报错）。
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

- **durable / persistent handle v1 / v2（本版本头号新增，PR #20，缺陷修复 PR #40，
  独立验证用例 PR #29）**：授予、断线后重连认领、超时回收。这是 v0.1.0「已知问题」
  里列的头号缺口——一次 Time Machine 备份动辄数小时，此前网络抖动会让已打开的句柄
  无法恢复、备份中断重来。
  PR #40 修掉 6 个缺陷：登记表键跨会话碰撞、归属校验缺失、data race、超时未关句柄、
  「先授权后驱逐」的次序、重连未改绑树；`DH2Q` 的 persistent 位改为**降级**而非
  打死整个 `CREATE`。
  **置信度：单元测试 + impacket 线级用例（手工拼 create context）验证了授予/重连/
  超时路径**；**没有** macOS 真机长跑断线恢复的证据（开发环境无 macOS）。
  也就是说已验证的是「握手与重连协议正确」，不是「真实备份过程不会中断」。
- **共享模式（ShareAccess）冲突判定（PR #34，MS-FSA §2.1.5.1.2）**：此前
  `ShareAccess` 三个位一路解析到 `create.go` 就断了，`STATUS_SHARING_VIOLATION`
  定义了却从没有人返回——典型的「字段解析出来了但从未接线」。
  判定是**双向**的：新请求的 `DesiredAccess` 要被所有已存在句柄的 `ShareAccess` 允许，
  且新请求的 `ShareAccess` 要允许所有已存在句柄的 `DesiredAccess`；只做前者的话
  「先以 SHARE_ALL 打开、再以 `ShareAccess=0` 打开」这种反向独占请求会被放行，
  第二个客户端会以为自己拿到了独占而实际没有。
  **已知边界（刻意不修）**：两个**不同的共享**指向同一个宿主目录时，彼此看不到
  对方的句柄——跨共享检测需要 (设备号, inode) 这一级的全局身份，当前 VFS 不暴露。
- **CREATE context 处理重构成注册表（随 PR #20）**：新增一种 context 只需在自己的
  文件里 `init()` 登记，不再改 `create.go`。现登记 `AAPL` / `AlSi` / `MxAc` / `QFid`
  与 durable 族。**注意前四种在 v0.1.0 就已支持**，这里变的只是组织方式。
- **oplock / lease break 主动推送通道（PR #12；NTSTATUS 常量 PR #4）**：
  MS-SMB2 §3.3.4.6 / §3.3.4.7，break 通知的 `MessageId` 固定为 `0xFFFFFFFFFFFFFFFF`。
  `handleOplockBreak` 改回返回 `STATUS_INVALID_OPLOCK_PROTOCOL`（此前误用
  `STATUS_INVALID_PARAMETER`）。
  **这只是通道**：服务端**仍不宣告** `SMB2_GLOBAL_CAP_LEASING`、`CREATE` 仍一律授予
  `NONE` oplock，因此**对客户端可观察行为没有任何变化**，客户端继续不缓存。
- **配额按共享粒度计算（PR #14；启动自检 PR #15）**：可用空间改按**本共享**的实际
  用量计算，修掉空共享被报 0 可用而直接阻断 Time Machine 的问题；启动时自检
  `quota_bytes` 是否已经小于共享现有用量并告警。
- **VFS 修复（PR #13）**：`SET_INFO` 纯 mode 变更曾丢失、`Mode` 被归零、
  `Rename` 大小写别名路径上的数据丢失（改用 `os.SameFile` 判定别名）。
  另有 `ResolveParent` 改为先精确匹配再回退（PR #10）。

### 安全

- **堵住 Windows junction（目录联接）逃逸（PR #44）**：Go 在 Windows 上用
  `fi.Mode()&os.ModeSymlink` 判断链接会**漏掉 junction**，于是 v0.1.0 的路径穿越
  防护在 Windows 上留了一个缺口——共享内的 junction 可以指向共享外。现按平台分别判定。
  **置信度：只交叉编译过 + 纯判定逻辑的表驱动单测通过，无 Windows 真机验证。**
  实现刻意把不依赖系统调用的判定规则剥进无 build tag 的文件里以便在 Linux 上测试，
  真正调用 `FindFirstFileW` / `GetFinalPathNameByHandleW` 的那一半跑不了。
- `validateWindowsName` 接线进 `ValidateComponent`（PR #19）——此前同样是「写了没接」。

### 配置

- `configs/example.yaml` 澄清**路径字段按运行平台判定绝对性**：`shares[].path`、
  `shares[].metadata_path`、`log.file` 校验的是**当前运行平台**意义上的绝对路径，
  `/srv/share/public` 在 Windows 上不算绝对路径、会直接启动失败。
- `metadata_path` 的校验按平台判定（PR #18）：非 Windows 上跳过运行时使用，
  但**所有平台都参与启动校验**。因此同一份配置跨平台复用时，这一项要么留空、
  要么按平台分开写。文档此前写作「会被忽略」，容易被读成「随便填」。

### 内部：OS 能力抽象的地基（**接口与开关已就位，适配器尚未落地**）

- **`internal/oscap`：OS 能力抽象 port（PR #123）**。按 AGENTS.md §1.2 C9 / §5 P7
  定义六项可选能力的接口——`xattr` / 稀疏文件 / 命名流 / 稳定 FileID / 创建时间 /
  DOS 属性位——外加逐能力降级矩阵、`auto`/`native`/`portable` 三态模式与平台探测，
  并配 31 例单元测试。
- **配置项 `filesystem_mode`（PR #126）**：三态全局开关，默认 `auto`，取值合法性
  校验委托 `oscap.ParseMode`（单一真源，防止配置层与 oscap 层各写一份取值表而漂移）。
  `configs/example.yaml` 已列出该字段。
  它是**全局策略**而非逐共享设置——表达的是「这台机器上我们信不信任宿主能力」；
  各共享的落点仍逐个探测决定，同一次运行里 ext4 目录可走 native、exFAT 目录落 builtin。
- ⚠️ **但此刻它还改变不了任何行为**：`native/` 与 `builtin/` 两个适配器**都还不存在**，
  `internal/vfs` 也尚未改为经由 port 取能力。也就是说这三档现在**选哪个跑起来都一样**。
  真正生效在 v0.3.0。在那之前不要根据这个开关下任何部署结论。

### 内部（已合入但**尚未接线**，本版本二进制行为不受影响）

- **`internal/meta`：POSIX 元数据旁路 KV 存储（PR #26）**。Windows 用
  `go.etcd.io/bbolt`（纯 Go，符合 C1 禁 CGO），非 Windows 为 noop 实现，bucket 名
  `posix.v2`。
  ⚠️ **本版本实际生效的 Windows 旁路存储仍然是 `internal/vfs/metadata_windows.go`
  那一份**（bucket 名 `posix`），`internal/meta` 没有任何产品调用点。
  两份实现会派生出**同一个数据库文件路径**
  （`%AppData%\stupidsamba\metadata-<共享根哈希>.db`），只是 bucket 不同——
  在 v0.3.0 把 `internal/meta` 接线时必须一并拆掉旧实现，否则同一个文件会被两套
  代码用两个 bucket 各写各的。此项已作为风险 R11 记录在
  [`docs/status-v0.2.0.md`](docs/status-v0.2.0.md)。
  另注：`internal/meta` 的 bbolt 分支挂在 `windows || metabolt` build tag 下，
  默认 CI 不编译它，需 `-tags metabolt` 才能跑到。

### 已知问题 / 未实现（v0.2.0）

- **`CHANGE_NOTIFY` 仍返回 `STATUS_NOT_SUPPORTED`**：客户端降级为定时轮询，目录列表
  不会自动刷新（需手动刷新）。异步变更通知未实现。
- **真实 oplock / lease 能力仍未对外生效**：通道通了，但不宣告 `CAP_LEASING`、
  一律授予 `NONE` oplock，客户端继续不缓存，band 文件密集写吞吐仍受损。
- **Time Machine 定级仍为 C 档，未上调。** durable handle 这个头号缺口已经补上，
  但定级的依据是 [`docs/timemachine-status.md`](docs/timemachine-status.md)，
  而该文档的 B 档要求包含「真机断线恢复证据」，本版本一条都没有（开发环境无 macOS）。
  **能力就位 ≠ 定级上调**，在真机跑过之前不动这个结论。**请勿用于唯一备份。**
- **`filesystem_mode` 三档目前等价**：开关和接口都在了，但 `native/` 与 `builtin/`
  适配器未实现、`internal/vfs` 未改为经由 port 取能力，所以选 `auto` / `native` /
  `portable` 跑起来行为完全一样。v0.3.0 才真正生效。
- **非 Windows 平台仍无元数据旁路兜底**：`internal/vfs/metadata_other.go` 直接
  返回 nil。宿主文件系统不支持 xattr 时（FAT32/exFAT 外置盘、`nouser_xattr` 挂载、
  只读根）这些元数据会**静默丢失**且不报错。这是 v0.3.0 builtin 完整化的第一优先级。
- **不支持**（与 v0.1.0 相同）：完整 SMB1 文件操作、Kerberos/AD、DFS、打印机共享、
  多通道（multichannel）、目录租约（directory leasing）。

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
