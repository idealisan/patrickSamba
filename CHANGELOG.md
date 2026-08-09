# Changelog

本文件记录 stupidSamba 的版本变更。**v0.1.0 的正文同时作为本版本 Release 页面的说明
正文**。所有内容均以实测与代码核实为依据；「未实现」项如实列出，不做夸大。

---

## v0.2.0（2026-08-09，**实际发布渠道仍是 prerelease**，见「发布物形态」末条）

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
>    **⚠️ 但这条命令有盲区，别把它的沉默当成「查无此 PR」**：`--merges` 只保留多父提交，
>    而本仓库有一部分 PR 是以**单父提交**落进 `main` 的（squash / fast-forward），
>    它们的 subject 照样写着 `Merge pull request #NNN`，却会被 `--merges` 整个滤掉。
>    实测（2026-08-09 18:12 CST）：`git log --oneline --merges origin/main | grep '#135'`
>    返回**空**，而 `git merge-base --is-ancestor ff77acb origin/main` 返回 **0（是祖先）**，
>    `git rev-list --parents -n1 ff77acb` 只有一个父提交 —— PR #135 与 #138 都是这种。
>    也就是说照上面那条命令核对，会得出「#135 不存在」或误挂到后面某个真 merge 上。
>    **稳妥查法**（不依赖父提交个数）：
>    `git log --oneline origin/main | grep -a "#<号> "`，再用
>    `git merge-base --is-ancestor <sha> origin/main` 确认它确实在主干上。
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
- 镜像**在本文写作时尚未推送**至远端制品库；打 `v0.2.0` tag 时由 `tag_push` 流水线
  自动构建并 `--push`（PR #134，`.cnb.yml:174` 起的 `tag_push:` 段，第 234 行附近
  `sh scripts/docker-build.sh --tag "$CNB_BRANCH" --push`；该步骤刻意排在
  `git:release` **之前**，镜像出不来就不建 Release）。目标地址
  `docker.cnb.cool/finalappstore/stupidsamba:<版本>`（**私密仓库，拉取前必须先
  `docker login docker.cnb.cool -u cnb -p <访问令牌>`**，用户名是固定字面量 `cnb`）。
  本地已构建并核验：`docker create --platform` 解析探针确认 manifest 含
  `linux/amd64` + `linux/arm64`；把二进制从镜像两个架构分别抠出来看 ELF `e_machine`
  分别是 `0x003e`(x86-64) 与 `0x00b7`(AArch64)，构建信息 `CGO_ENABLED=0`；
  两份 sha256 与 `dist/` 裸包里解出来的二进制**逐位相同**（实测取 v0.2.0-rc0
  预发布验证构建，前缀 `81c242c16b3852a6…` / `d2fbdf7b7c79064f…`；注意二进制里
  嵌了版本号，打 `v0.2.0` tag 时 sha256 会随之变化，届时按实际产物重新核对），
  即镜像与裸包确系同一份字节。
- 端到端镜像验证脚本 [`scripts/verify-image.sh`](scripts/verify-image.sh)：用
  `smbclient` + `impacket` 对容器做 8 项可证伪校验（启动、`scratch` 中静态可执行、
  smbclient 读写往返、共享不存在被拒的反向对照、数据确实落到命名卷、impacket 独立
  客户端栈往返、guest 警告、mDNS 关闭）。
  **已实测：8/8 通过、0 skip**（2026-08-09 17:00 CST，docker 29.6.2 / buildx v0.35.0
  实跑，smbclient 与 impacket 均在场；协商到的方言 `0x300`）。
  并做过**变异对照**：把内置配置的共享路径改成 `/nonexistent-share-dir` 重打镜像，
  同一脚本退出码 1 并打出「配置校验失败: shares[0].path: 共享 "public" 的目录不存在」，
  正常镜像退出码 0 —— 证明这 8 个 PASS 有鉴别力，不是恒真。
- **发布渠道：打 tag 后 Release 页面会被标成「预发布」，不是正式版。**
  `.cnb.yml:235-240` 的 Release 步骤硬编码 `preRelease: true` / `latest: false`
  （stage 名字就叫「创建 Release（预发布）」），且 `tag_push` 对**任何** tag 都触发、
  不按 tag 名区分。所以 v0.2.0 与 v0.1.0 走的是同一条渠道。
  想发成正式版就改那两行（`preRelease: false` / `latest: true`），
  **不要靠 tag 名去猜**——那两行的上方注释里已经写明了这个决定必须显式做。
  本条如实记录当前状态，不代表已经决定要改。
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
  改成真实 slog 输出格式、路径字段按运行平台判定绝对性；`metadata_path` 则如实说明
  **校验只在 Windows 做**、非 Windows 上仅 WARN 不报错（`internal/config/validate.go:394-396`，
  Linux 二进制 + `C:\...` 配置实测仅 WARN、照常启动）——旧版「会被忽略」的表述
  没讲清它在非 Windows 上**不进任何校验**，容易读成「随便填」，现已订正。

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
  `log.file` 校验的是**当前运行平台**意义上的绝对路径，`/srv/share/public`
  在 Windows 上不算绝对路径、会直接启动失败（复算：`GOOS=windows go run ./cmd/stupidsamba -config <win-path.yaml>`
  应退出非 0）。
- `metadata_path` 的校验**仅 Windows 参与**（PR #18，代码 `internal/config/validate.go:390-396`）：
  非 Windows 上 `hostOS != "windows"` 直接 `return`，**完全不校验**，只打一条 WARN
  「该字段仅在 Windows 上生效…会忽略它」，服务照常启动。因此「跨平台复用同一份配置
  在 non-Windows 上会启动失败」是**已修正的旧行为**——旧版曾无论平台都按本平台语义
  卡绝对路径，导致 Windows 配置拿到 Linux 硬报错。复算：`internal/config/validate.go:394-396`；
  实测：Linux 二进制 + `metadata_path: C:\ProgramData\stupidsamba\x.db` → 仅 WARN，
  监听照常拉起（退出 0）。文档此前写作「会被忽略」是被读成「随便填」，
  现已在「配置」段与 README 共享表写明 non-Windows 上的唯一出口是那条 WARN。

### 内部：OS 能力抽象（**六项能力的两套适配器都已建成，但一处都还没接进数据路径**）

> **一句话结论**：port 层 + native 适配器 + builtin 适配器 + portable 模式 CI 门禁
> 四块**全部已在 `main`**，测试是真跑的、门禁是有牙的；但
> **`internal/vfs` / `internal/server` / `cmd/` 里没有任何一处调用它们**，
> 所以本版本二进制在这一块的运行行为与 v0.1.0 **逐字节等同**。
> 「代码建成」和「行为生效」在这里是两件事，下面分开写，判据附在每条后面。

**已建成（可复算）**：

- **`internal/oscap`：OS 能力抽象 port（PR #123，合并提交 `6413e1d`）**。按 AGENTS.md
  §1.2 C9 / §5 P7 定义六项可选能力的接口——`xattr` / 稀疏文件 / 命名流 / 稳定 FileID /
  创建时间 / DOS 属性位——外加**逐能力**降级矩阵（`oscap.SelectMatrix`，不是整体二选一）、
  `auto`/`native`/`portable` 三态与平台探测。
  （判据：`ls internal/oscap/*.go | wc -l` = 20；`go test -v ./internal/oscap/` = 31 PASS / 0 SKIP / 0 FAIL）
- **`internal/oscap/native`：借助宿主能力的适配器（PR #138，提交 `d351683`）**。
  六项能力各一套实现，走 xattr / `FALLOC_FL_PUNCH_HOLE` / NTFS ADS / 平台 stat 扩展。
  测试**不用 `t.Skip` 兜底**，而是「探测说支持就断言完整往返，说不支持就断言诚实报错」的
  双分支写法——所以在任何宿主上都不会出现「整个文件一行没跑还报绿」。
  （判据：`go test -v ./internal/oscap/native/` = 37 PASS / **0 SKIP** / 0 FAIL，实测 2026-08-09 18:15 CST；
  `grep -rn 't.Skip' internal/oscap/native/*_test.go` 唯一命中在 `native_posix_test.go` 的**注释**里）
- **`internal/oscap/builtin`：不依赖宿主可选能力的自带适配器（PR #129，合并提交 `e4f0f80`）**。
  只用「普通文件 + 一份 bbolt 旁路存储」把同一份语义做出来，六项能力齐全，无一项赊账。
  （判据：`go test -v ./internal/oscap/builtin/` = 49 PASS / 0 SKIP / 0 FAIL）
- **`portable` 模式 CI 门禁（PR #135，提交 `ff77acb`）**：`test/ci/portable-mode.sh`，
  挂在 `.cnb.yml` 的 push 与 pull_request 两条路径上（`&gate_portable`，`.cnb.yml:132-134`、
  `:154`、`:201`），**每次构建真跑**，满足 AGENTS.md §1.2 那条「没有 CI 覆盖的 builtin
  就是一份薛定谔的实现」的前置要求。
  门禁自带**反向对照**，四个变异体逐一验证它有牙：A 让 portable 偷偷调 native factory、
  B 把 portable 用例改名（门禁空转）、C 子测试全 SKIP 但父测试仍 PASS、D 子测试整体不执行
  且不留 SKIP 痕迹 —— **4/4 都让门禁变红**，且红在不同的判据上（业务断言 / 用例清单 /
  SKIP 计数 / 结果行下界）。
  （判据：`sh test/ci/portable-mode.sh` → rc=0，输出「顶层通过 35 条…含子测试的结果行共 50 条」
  与四行 `[OK] 变异体 X … 门禁如期变红 (rc=1)`；实测 2026-08-09 18:10 CST）
- **配置项 `filesystem_mode`（PR #126，合并提交 `212d6f9`）**：三态全局开关，默认 `auto`，
  取值合法性校验委托 `oscap.ParseMode`（单一真源，防止配置层与 oscap 层各写一份取值表而漂移）。
  设计上它是**全局策略**而非逐共享设置——表达的是「这台机器上我们信不信任宿主能力」；
  各共享的落点由探测逐个决定，同一次运行里 ext4 目录可走 native、exFAT 目录落 builtin。

<!-- BEGIN-OSCAP-WIRING-STATUS：oscap-wire 的接线 PR 一合入 main，整段替换本块，不要散改别处 -->
**尚未接线（本版本的实际行为边界；本块截至 2026-08-09 18:35 CST 为当下事实，接线 PR 合入后整段替换）**：

- ⚠️ **整个 oscap 子系统没有任何产品调用点**，因此
  **`filesystem_mode` 填 `auto` / `native` / `portable` 跑起来行为完全一样**。
  上面那段「全局策略 / 逐共享探测」描述的是**设计**，不是 v0.2.0 的运行时事实。
  四条互相独立的判据：
  1. `go list -f '{{.ImportPath}} {{.Imports}}' ./... | grep oscap` —— 全仓只有
     `internal/config` import 了 `internal/oscap`，**没有任何包** import
     `oscap/native` 或 `oscap/builtin`。
  2. `go list -deps ./cmd/stupidsamba | grep -c oscap` = **1**（只有 port 包，被 config
     为了 `ParseMode` 拉进来）—— 也就是说两个适配器**根本没被链进发布二进制**。
  3. `grep -rn 'oscap\.Open\|oscap\.New\|SelectMatrix\|ProbeNative\|native\.New\|builtin\.New' --include='*.go' . | grep -v '^./internal/oscap/'`
     —— 包外唯一命中是 `internal/config/config.go:25` 的一句**注释**，零个真实调用。
  4. `FilesystemMode` 在产品代码里只出现在 `internal/config/defaults.go:48-49`（填默认值）
     与 `internal/config/validate.go:238-245`（校验取值），**没有运行期消费者**。
- ⚠️ **真实数据路径仍走 v0.1.0 那套自己的实现**，与 oscap 并存但互不相干：
  例如 xattr 在数据路径上是 `internal/vfs/xattr_unix.go`，而 `internal/oscap/native/xattr_posix.go`
  是另一份、当前无人调用。将来把 vfs 改为经由 port 取能力时**必须一并拆掉旧的那份**，
  否则会重演本版本 `internal/meta` 与 `internal/vfs/metadata_windows.go` 的双实现撞车（见下节 R11）。
- ⚠️ **`native` 档不兑现它自己报错文案里的承诺**。配置校验失败时打印的可选值说明写着
  「native=强制原生、不支持则启动报错」，但既然没有消费方，这个报错**不会发生**：
  实测 2026-08-09 18:35（Linux/amd64，`origin/main` 基线构建），`filesystem_mode: native`
  正常启动、无任何告警（`timeout 3` 杀掉，rc=124）；反向对照填 `Native`（大写）
  rc=1 报「非法取值」，说明这个探针有鉴别力、不是恒真。
  **顺带一条接线后才会显现的事实**（写在这里免得日后被当成回归）：POSIX 平台对
  `dos_attributes` 的探测恒为 `false`（`internal/oscap/probe_linux.go` /
  `probe_darwin.go` 的 `CapDOSAttributes` 无条件 `return false`，因为 POSIX 没有存放
  DOS 属性位的地方；macOS 上 `sparse_file` 目前同样恒 `false`），
  而 `native` 档的契约是「有一项不支持就报错、不降级」——两者相乘意味着
  **一旦接线，`native` 在 Linux/macOS 上会恒定启动失败，它实际只对 Windows 有意义**。
- ⚠️ `configs/example.yaml` 对 `filesystem_mode` 的注释此前是按**设计意图**写的，
  没提它当前无运行期效果，读者照着改会以为生效。**本版本已在该段补上「实际行为」
  三条**（无消费方 + `go list -deps` 复算命令、`native` 不报错的实测、接线后 POSIX
  上 `native` 必失败），两处口径现已一致。
<!-- END-OSCAP-WIRING-STATUS -->

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
  **2026-08-09 项目所有者决定：TM 真机验收从 v0.2.0 的强制项降为可选项**，
  不再是发布阻塞项，原因是没有可用真机环境与时间（AGENTS.md §2 阶段二已记录）。
  **降的是验收要求，不是功能**：Apple 扩展代码全部保留、单测与协议级用例继续跑。
  反过来说，这条也意味着**该定级短期内不会有新证据**——不要因为版本号往前走
  就推断它变可靠了。
- **`filesystem_mode` 三档目前等价**（接线状态见上节 `BEGIN-OSCAP-WIRING-STATUS` 块，
  那是单一真相块；本行不再重复判据）：port、`native/`、`builtin/`、portable CI 门禁
  **四块都已建成并有测试**，但**没有一处产品代码调用它们**，两个适配器根本没被链进发布二进制。
  选 `auto` / `native` / `portable` 跑起来行为完全一样。**唯一缺的是接线**
  （把 `internal/vfs` / `internal/server` 改为经由 port 取能力）—— 此事无版本承诺，
  以真正合入 `main` 的那一版为准。在接线之前不要根据这个开关下任何部署结论，
  尤其**不要因为「native 适配器已经写好了」就以为设成 `native` 会走原生路径**。
- ~~**6 个带 `!linux && !darwin && !windows` 约束的文件从未被任何一关编译过**~~
  —— **已修复，本条不再是已知问题**（`e664a47`：`vfs: 判据抽成纯函数并补自身反向对照
  + check-test-compile 补 freebsd 编译盲区`）。留下记录是因为它的**形态**值得记住：
  `internal/oscap/native/native_other.go`、`internal/oscap/probe_other.go`、
  `internal/vfs/attr_other.go`、`internal/vfs/sparse_other.go`、`internal/vfs/sys_other.go`
  与 `internal/oscap/probe_helper_other_test.go` 六个文件，因为编译门禁
  `test/ci/check-test-compile.sh` 的平台列表 `linux/amd64 linux/arm64 darwin/arm64
  windows/amd64` **没有一个满足那个约束**，从进仓库起一行都没被编译过——
  正是 AGENTS.md §1.2 点名的「薛定谔的实现」同型。
  修法是往平台列表末尾追加 `freebsd/amd64`，现已落在 `main`
  （判据：`git show origin/main:test/ci/check-test-compile.sh | grep 'for t in'`
  → 末尾含 `freebsd/amd64`；且该处注释写明「故意多出来的一档，不在 C7 支持矩阵里，
  别当成手滑删掉」）。
  ⚠️ **但这一档目前没有反向对照**：`test/ci/check-test-compile.sh:113` 的注释写着
  「负向对照见 `test/ci/negative-verify.sh` 的 freebsd 段」，而那个文件里
  **一处 `freebsd` 都没有**（判据：`git show origin/main:test/ci/negative-verify.sh
  | grep -c freebsd` → **0**，实测 2026-08-09 18:59 CST）。也就是说：这一档若哪天
  被人从平台列表里删掉、或被 `continue` 提前跳过，**没有任何一关会变红**——
  按本仓库自己的标准（「一个从来没红过的门禁，和没有门禁是一回事」），
  它现在只是「跑了」，还谈不上「有牙」。
  本条不改脚本（那是 vfs-deflake 的文件），只如实登记：这是本仓库「写了但从未被验证过」
  的**第 10 例**，且形态与前 9 例不同——前 9 例是**代码**没被执行，这一例是
  **注释里引用了一个不存在的实体**，读者会据此以为反向对照已经存在而不再去补。
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
