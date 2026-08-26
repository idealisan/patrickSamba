# Changelog

本文件记录 stupidSamba 的版本变更。**v0.1.0 的正文同时作为本版本 Release 页面的说明
正文**。所有内容均以实测与代码核实为依据；「未实现」项如实列出，不做夸大。

---

## v0.5.1（2026-08-26，正式版）

> 写作纪律同前：只写已合入 `main` 的内容、按证据强度分档。本版是 Windows 配置解析
> 缺陷的补丁发布，只含一项修复。

- **修复：配置文件带 UTF-8 BOM 时无法启动（Windows 用户真实报告，v0.5.0 Windows 包）**。
  用记事本、PowerShell 重定向等编辑过 `example.yaml` 后保存的文件常带 UTF-8 BOM
  （`EF BB BF`，编辑器里不可见），YAML 解析器不认它，启动即报
  `[1:1] unexpected key name`，用户对着一个看不见的字符毫无办法。
  - 现在解析前自动剥掉 UTF-8 BOM，配置照常加载；UTF-16/UTF-32 编码的配置没法静默转码，
    改为人话报错并明确告知「另存为 UTF-8」。
  - 证据强度分档：单测级回归用例（带真实 BOM 字节的 testdata 必须能加载；
    UTF-16LE 编码必须报错且文案含 UTF-16/UTF-8）；`CGO_ENABLED=0 go build/vet/test ./...`
    全绿（17 包）；五平台 × 全 build tag 编译门禁绿。仓库自带的 `configs/example.yaml`
    本就不含 BOM，发布包内容未变——BOM 来自用户本机的编辑器保存行为。

---

## v0.5.0（2026-08-26，正式版）

> 写作纪律同前：只写已合入 `main` 的内容、按证据强度分档（真机实测 > 第三方客户端
> 协议级实测 > 单测 > 仅代码存在）。本节所有条目均已合入 `main`。
>
> **发布门禁实测记录**（2026-08-26 18:02–18:07 CST，linux/amd64 go1.25.0）：
> `CGO_ENABLED=0 go build/vet/test ./...` 全绿（17 包）；`go test -race` 覆盖
> vfs/server/smb 七包零数据竞争；`test/ci/check-test-compile.sh` 五平台 × 全 build tag 绿；
> `scripts/check-constraints.sh` C1/C3/C4/C8/C9 与无 AGPL 依赖全绿。

**本版主题**：对照 Samba 源码排查产出的缺陷大修波。第一波高危 B1–B6、第二波 15 条
（锁边界 / 元数据 / FSCTL / 命名流）全部闭环，外加三项协议栈性能优化与 SMB 3.1.1
签名算法协商。行为变化集中在**锁强制与元数据语义**——升级前请阅读各条目的
「行为变化提示」。

- **移除 `filesystem_mode` 的 `native` 档，配置收敛为两态：`auto`（默认）/ `portable`**。
  移除原因：`native` 的契约是「全部能力强制走原生实现，缺一项启动即报错」，但每个平台都至少有
  一项能力被源码硬编码为没有原生实现（linux/darwin 的 DOS 属性位、darwin 另缺稀疏文件、
  windows 缺 xattr、其余平台六项全无），该契约在任何平台上都无法满足——三个平台恒定启动失败
  （v0.3.0/v0.4.0 已知问题），`native` 从未有过可用场景。项目所有者 2026-08-25 拍板整档删除。
  - 行为变化：旧配置写 `filesystem_mode: native` 会在配置校验/启动时失败，错误信息说明该档已在
    v0.5 开发版移除及原因，并建议改用 `auto` 或 `portable`。未显式写 `native` 的用户不受影响；
    `auto` 本就逐项优先选原生，「钉死原生路径」的测试需求由 auto + 生效矩阵断言覆盖。
  - 同步改动：`configs/example.yaml` 与 AGENTS.md §1.2/§5 P7 的取值表改为两态并记录移除缘由。
  - 证据强度分档：单测级反向对照（`ParseMode("native")` 必须报错且文案含替代取值 auto/portable；
    `TestNoModeForcesAllNative` 对全部合法取值断言「探测全失败也必须成功落 builtin」，防止
    「全原生强制」语义以任何形式回归）；linux/amd64、linux/arm64、darwin/arm64、windows/amd64、
    freebsd/amd64 五个目标交叉编译 vet 通过。真机行为验证未做（本档此前也从未在真机可用过）。
- **v0.4.0 性能基线报告入仓（PR #192，merge `0d33643`）**：`test/reports/perf-v040-20260825.md`，
  七场景 × auto/portable × 加密对比，用第三方纯 Go 客户端（go-smb2 v1.1.0）在容器 loopback 上测得，
  测试角色产出、未改任何产品代码。报告自述其性质是 **loopback 协议栈基线，不是真实网络吞吐**，
  只用于相对比较与回归基线，不代表任何真实部署的绝对性能——引用数字时必须带上这个前提。核心数字：
  单流大文件约 95 MB/s 写 / 200 MB/s 读（SMB 3.1.1 + 签名），16 并发流把聚合读推到约 716 MB/s
  （p95 延迟同步从 4.6 ms 恶化到 45 ms）；auto 与 portable 在该宿主负载下性能几乎无差（比值
  0.98×–1.05×）；开 SMB3 加密反而比仅签名快约 1.5×（AES-NI 硬件路径替代 HMAC-SHA256 软件路径）。
- **B3/B4/B5 元数据三连修（PR #193，merge `2cd1710`）**——对照 Samba 源码排查产出的三个高危缺陷：
  - B3：CREATE 请求携带的 FileAttributes 此前被整体丢弃（连初始 ARCHIVE 位都不落库）；现按 Samba
    语义消费——剥 DIRECTORY、叠 ARCHIVE 后落地。
  - B4：新建对象此前从不把创建时间写进旁路库（portable 模式与无 birthtime 的文件系统上 btime
    永不持久，代码注释声称的行为并不存在）；现在真正落库，native 有 birthtime 时跳过旁路。
  - B5：rename/remove/DELETE_ON_CLOSE 此前不迁移不清 caps 旁路账本，路径复用会继承陌生文件的
    创建时间与 DOS 属性位（跨对象元数据泄漏）。oscap 新增 MetadataMigration port
    （RenameMetadata/DeleteMetadata 单事务前缀迁移），vfs 在三条删除/改名路径挂接。
  - 可见行为：改名/删除后重建的同名对象不再带前任的创建时间与 DOS 属性；新建文件的属性位与
    btime 首次真实生效。证据档位：单测级（16 个用例先红后绿）+ 四门禁绿，协议级实测未做。
- **B6 流删除粒度修复 + 流创建语义对齐 Samba（PR #195，merge `a75a643`）**：
  - 高危：SET_INFO(FileDispositionInformation) 删一个备用数据流（ADS）此前会把整个基础文件连同
    全部流一起删掉（close.go 一律按不含流名的基础路径 Remove）；FILE_DELETE_ON_CLOSE 打开流时
    标志静默无效。现按打开时携带的流名分派删除粒度（新增 `vfs.StreamRemover.RemoveStream`，
    Samba streams_xattr_unlinkat 语义）：删流只删该流；DELETE_ON_CLOSE 对流真正生效。
  - 中危×2：OPEN_IF / OVERWRITE_IF 打开「基础文件不存在」的流时先建基础文件再开流
    （Samba open.c:6508 语义）；SUPERSEDE / OVERWRITE 打开时清掉目标残留的全部 ADS
    （clear_ads / delete_all_streams 语义）。
  - 可见行为：删流从「丢整个文件」变为只丢该流（数据丢失级缺陷修复）；SUPERSEDE/OVERWRITE
    不再保留历史残留 ADS。证据档位：单测级 + 四门禁绿，协议级实测未做。
- **B1/B2 字节范围锁强制与全路径释放，附两项读写语义修正（PR #196，merge `1156053`）**：
  - B1（高危）：字节范围锁此前对 READ/WRITE 完全无阻挡——LOCK 命令只在记账，IO 入口根本不查表，
    A 句柄独占锁住的区间 B 句柄照常穿透读写。现两个 IO 入口接入 strict locking 检查（对齐 Samba
    STRICT_LOCK_CHECK）：读只被外句柄的独占锁阻挡、写被任何重叠的外句柄锁阻挡、豁免单位是句柄
    （自己的锁不妨碍自己），冲突回 STATUS_FILE_LOCK_CONFLICT。**行为变化提示：此前依赖「锁不生效」
    旧行为的应用（无论有意无意绕开了锁互斥）从本版起会开始收到 FILE_LOCK_CONFLICT** ——这是把
    Excel/SQLite 类互斥场景修对的必然代价。
  - B2（高危）：锁释放此前只挂在 CLOSE 命令路径，TREE_DISCONNECT / LOGOFF / 连接断开 /
    durable 过期回收全不释放，客户端异常退出后锁一直泄漏到服务进程重启。现把释放下沉进
    `Open.close()`（所有关闭路径的唯一汇合点）；durable 等待重连期间保留锁，回收或显式关闭才释放。
  - 零长度 READ 改判成功回 0 字节：此前 `Length==0` 无条件回 END_OF_FILE；现按 Samba/Windows 归纳
    改为合法探测返回成功（length=0 且 min_count>0 的边界仍回 END_OF_FILE），同时把目录/权限检查
    提前到长度判定之前（堵住无权句柄借零长读拿到不同状态码的信息泄露）。
    **行为变化提示：此前把 END_OF_FILE 当成功处理的客户端需要适配 SUCCESS + 0 字节的新返回**；
    offset ≥ EOF 且请求非零字节时的 END_OF_FILE 行为不变（有回归钉子）。
  - 对 FILE_ATTRIBUTE_READONLY 目标的 WRITE 从「照常写入」改为拒绝（STATUS_ACCESS_DENIED，
    按打开时的属性快照判，与 Windows「打开时定生死」一致；delete/setinfo 面不在本轮范围）。
  - 证据档位：单测级（11 个用例先红后绿）+ 四门禁绿；真机多客户端互斥场景未测。
  - 合入后两笔热修直接落 main（如实记录，也是门禁有效的实证）：gofmt 门禁拦下 #196 带来的
    `lock_disconnect_test.go` 格式红点（热修 `d067ede`）；race 门禁（`CGO_ENABLED=1 go test -race`）
    拦下 Open.close 锁释放与 durable 重连改绑之间的数据竞争，以 o.mu 配对修复（热修 `5550015`）。
- **pm 进度板与 bug 排查成果入库（PR #194，merge `434a53e`）**：新增 `docs/status-v050.md`
  （v0.5 修复波进度板：B1–B6 分派表、时间线、修复分支盘点与待决事项）与
  `docs/bughunt-20260825/findings-bh{3,4,5}.md`（三份对照 Samba 的排查报告，92/122/128 行共 342 行，
  自 `/tmp/opencode` 原样抢救拷贝入库；bh1/bh2 未找到对应文件，已在板内列为待决事项）。
  纯文档入库，二进制行为不受影响。
- **默认配置统一为「Windows 11 开箱即用」**：镜像内置配置（`configs/docker.yaml`）弃用
  guest 匿名方案，改为内置演示账号 `stupidsamba`/`stupidsamba`——Windows 10/11 默认拒绝
  不安全的 guest 登录，guest 配置对 Windows 用户等于连不上（2026-08-26 实测复现并定位）；
  演示账号是公开凭据，启动日志新增对应的 WARN 提醒（`config.Warnings`）。README 快速开始、
  Docker 运行一节与最小配置示例同步对齐同一套默认值：`-p 445:445` 端口映射 + 凭据连接，
  并补「从 Windows 访问」步骤与两个高频坑的说明（Windows 资源管理器可经 `\\IP:端口\share`
  直连非默认端口——项目所有者 Windows 11 真机实测可用；guest 匿名被 Windows 默认拒绝不是
  bug）。**已发布的 v0.4.0 镜像不受影响**，新内置配置随下一个镜像版本生效。
  证据档位：单测级（演示账号 WARN 用例）+ 本地 go-smb2 客户端连通实测
  （登录读写回环 + 匿名被拒）；Windows 真机回归由项目所有者完成（带端口直连成功）。
- **`mdns.apple.enabled` 默认值改为 true**：此前默认关闭，现按项目所有者要求改为
  默认开启 Apple 扩展记录（`_device-info._tcp` 等）——没有苹果设备时这些 TXT 记录对
  其他客户端没有影响，开着方便用 Finder 调试。实现沿用 `*bool` 三态模式
  （同 `server.smb1` / `share.browseable`）：未设置 → true，显式 `false` 仍然尊重；
  新增 `AppleMDNS.EnabledOn()` 作为 nil 安全的读取口。注意 SMB 协议层的 AAPL
  create context 支持本就始终启用，不受此开关控制；开关只影响 mDNS 广播记录。
  证据档位：单测级（默认值三态 + mdns 服务定义生成两用例先红后绿）。
- **bh4 锁/读写边界语义九连修（分支 `lock/bh4-a3-a13`，merge `ff997f7`）**：对照 Samba
  brlock.c/torture 用例逐条对齐——锁冲突矩阵复刻 `brl_conflict`（同句柄独占不叠、共享/独占混合可叠）；
  零长锁改「分界点」语义（持 {10,0} 时 {9,2} 拒、{10,2} 允许，`brl_conflict` 闭区间模型）；
  回绕区间回新增的 `STATUS_INVALID_LOCK_RANGE`；多元素含阻塞元素回 INVALID_PARAMETER；
  目录句柄 LOCK 回 INVALID_DEVICE_REQUEST；阻塞锁实现为**同步有界等待**（至多 10s、并发等待者
  ≤64、释放广播唤醒 + 100ms 兜底轮询；与规范的差距——interim STATUS_PENDING 与等待期 CANCEL——
  需 server 层异步未决请求表，已在注释与板内列为移交项）；FLUSH 补访问校验（只读句柄/无 ADD
  目录拒绝）；WRITE DataOffset 精确校验、CreditCharge 覆盖校验（保守放行子 quantum，标 TODO）、
  WRITE_UNBUFFERED 在 ≥3.0.2 并入落盘。bh4-A#3（零长读）经核对已由上游 `d22fbd0` 覆盖，
  无需重复修复。证据档位：单测级先红后绿（lock_block/lock_validate/rw_hardening 等新用例组）+
  四门禁绿；真机多客户端互斥未测。
- **bh3 元数据残余四修 + READONLY 删除面补齐（分支 `meta/bh3-f4-f8`，merge `049d14c`）**：
  READONLY 目标的 DELETE_ON_OPEN/Create 携带 DELETE_ON_CLOSE 在 fs.Open **之前**拦截（回
  STATUS_CANNOT_DELETE——打开后再拒文件已被 vfs 删掉，测试专门断言文件幸存），判据与 #196 写面
  同为「打开时属性快照」，目录豁免；DOS 属性读路径改 Samba 默认的「存储值优先」合成
  （存储记录覆盖可设置位，「清除只读」不再被 POSIX 推导位静默冲掉，旧 bbolt 记录读侧过滤兼容）；
  显式设置 write time 后获得 sticky 语义（句柄粒度，写入/关闭补偿，防截断类变更冲掉所设值）；
  btime 回退口径对齐 Samba MIN(ctime,mtime,atime)；SET_INFO 落库前过滤 DIRECTORY/SPARSE/REPARSE
  客观位（对照 dosmode.c SAMBA_ATTRIBUTES_MASK）。证据档位：单测级先红后绿 +
  四门禁绿；协议级实测未做。
- **bh5 FSCTL 权限/映射与 VNI 校验六修（分支 `fsctl/bh5-f4-f9-vni`，merge `73e7fb5`）**：
  SET_ZERO_DATA 接入 strict locking 检查（复用 B1 的锁表谓词，冲突回 FILE_LOCK_CONFLICT，
  即第一波拍板的 D1 归属落地）；QUERY_ALLOCATED_RANGES 收紧为仅 ReadData；SET_SPARSE 补认
  APPEND_DATA；流句柄上的 SET_SPARSE 按Samba dosmode.c 改无操作成功（QAR/ZERO 维持
  NOT_SUPPORTED 并用例钉住，findings 原文允许）；FSCTL_VALIDATE_NEGOTIATE_INFO 方言校验从
  「整表顺序相等」改为规范的最大公共方言匹配（篡改类子用例仍全拒）；VNI 复核失败按
  MS-SMB2 §3.3.5.15.12 MUST 断开传输连接（server 层在响应写出后终止连接，忠实重放不误伤）。
  证据档位：单测级先红后绿 + go-smb2 第三方客户端对本分支构建的服务端完整验收 + 四门禁绿。
- **bh5 命名流解析与枚举三修（分支 `stream/bh5-f10-f12`，merge `eb036d8`）**：
  通用流名在不区分大小写的共享上增加兜底匹配（精确命中优先，EqualFold 一次，open/remove
  两路接入，防止 OpenIf 回访凭空建流）；「file:」尾冒号拒绝（OBJECT_NAME_INVALID）且流名类型
  后缀 VFS 与命令层统一只认 `:$DATA`（Samba streams_xattr_get_name 口径）；AFP_AfpInfo
  「创建后首次写入才进流清单」经对照 vfs_fruit netatalk 档确认为一致语义，钉测试固化并记录依据。
  证据档位：单测级先红后绿（stream_delete 回归守卫保持绿）+ 四门禁绿。
- **协议栈热路径性能调优（分支 `perf/v050-hotpath`，merge `e411ffc`）**：v0.4.0 基线显示瓶颈在
  协议栈自身（loopback 单流写 94.7 MB/s / 读 199.6 MB/s，加密反而比签名快 1.49×）。本轮先建
  可复现基准与 pprof 测量设施（test/perf + 可选 loopback 管理端口，环境变量开启、默认关闭），
  profile 定位后做四项优化：AES-CMAC 批处理化（CBC 化整段走 AES-NI 路径 + 字长 XOR + 按密钥
  缓存展开状态，64B 小消息 2.13×、1MiB 1.27×）；AES-CCM 批处理化（CTR 走标准库批量路径，
  Seal/Open ~2.3×）；TRANSFORM 报文单次分配布局（去掉整载荷二次拷贝）；ReadFrame 帧缓冲复用
  （串行性论证见 transport.go 注释）。端到端 A/B 实测：signing 单流写 +15.8%、16 流写 +25%
  （CPU −26%）、encryption 单流写 +17.7%；读方向受客户端自身软件验签钳制仅 +5~7%。
  **正确性边界**：不改任何协议语义——RFC 4493 / SP 800-38B/C 标准向量、wire golden、全仓测试、
  集成套件全绿；签名/加密默认值与协商行为零变化。分析报告与移交清单（handleRead 双拷贝、
  bbolt DOS 写放大、AES-GMAC 协商评估）见 test/reports/perf-v050-analysis-draft.md。
  证据档位：微基准 + 端到端 A/B 实测（容器 loopback）；race 门禁本容器无 gcc 未跑，
  由 CI 兜底（新增共享态均为 sync.Map/atomic/pool 设计）。

- **READ 路径单次分配（分支 `cmdio/handleread-single-alloc`，第三波）**：v0.5 分析报告移交的
  「handleRead 双拷贝」（堆分配 66% 所在）已消除——wire 新增预留式组装 API（Reserve/Commit），
  文件内容经 ReadAt 直写响应缓冲。同载 alloc_space profile 实证：旧路径
  `handleRead`(262.5MB)+`ReadResponse.Append`(250MB) 两热点归零，读路径分配字节 **−51%**；
  线上字节逐字节不变（穷举对照 + legacy 复刻对照机器证明），encryption c1 读端到端 CPU −9%。
  证据档位：allocs/op 基准 + 成对 A/B + 四门禁绿。
- **bbolt 按桶分级持久化，小文件元数据操作提速数倍（分支 `bolts/dos-nosync`，第三波）**：
  v0.5 分析报告 F4 的 bbolt 写放大已消除——旁路库按桶分类：可再生桶（创建时间/DOS 属性，
  崩溃后可按既有合成基线回落）以 NoSync 打开、关闭时显式 Sync；**命名流/xattr/稀疏区间/
  FileID 等内容与关键桶保持逐事务强制落盘，持久性与改动前一致**。进程崩溃不丢任何记录
  （数据过 write() 进页缓存），仅掉电可能丢最近的派生元数据——语义边界由 team-lead 拍板并
  写入代码注释。成对 A/B：small create **4.3~5.2×**（~700→2900–3600 ops/s）、delete
  **7.3~8.6×**、c16 写 p95 毛刺收敛 ~20%。metadata_path 文件名规则/flock/跨进程语义未动。
  证据档位：A/B 实测 + 重开存活回归用例（注明验证的是进程崩溃而非掉电）+ 四门禁绿。
- **SMB 3.1.1 签名算法协商与 AES-GMAC 支持（分支 `gmac/signing-algorithm`，第三波）**：
  新增 `server.signing_algorithm: auto/aes-cmac/aes-gmac` 配置，**默认 auto 与历史行为完全
  一致**（不回应签名算法协商，零兼容风险）。aes-gmac 按 MS-SMB2 §2.2.3.1.7 / §3.1.4.1 实现
  （nonce=MessageId 小端+方向/CANCEL 位；原语以 NIST CAVP GCMVS 官方向量钉住；GCM 硬件路径，
  微基准 1MiB 签名 **7.1×** 于 CMAC）；客户端不支持时显式协商失败、绝不静默降级；
  max_dialect<3.1.1 时启动报错。smbclient 4.22 双向签名互通为协议级实测。
   ⚠️ **尚未在 Windows/macOS 真机验收，真机验证通过前建议保持 auto**。

### 已知问题 / 未实现（v0.5.0）

- **阻塞锁是「同步有界等待」，不是规范的 interim STATUS_PENDING + CANCEL 异步模型**
  （至多等 10s、并发等待者 ≤64、释放广播唤醒 + 100ms 兜底轮询）。补齐需要 server 层
  异步未决请求表，已列为移交项。
- **AES-GMAC 签名协商尚未在 Windows/macOS 真机验收**；真机验证通过前建议保持默认 auto。
- **CreditCharge 覆盖校验保守放行子 quantum**（代码内标 TODO）。
- **README 的 `filesystem_mode` 三态措辞订正尾巴未完成**：AGENTS.md / CHANGELOG /
  configs/example.yaml 均已两态化，README 个别段落仍残留旧表述（进度板两波挂账无人认领）。
- **Time Machine 真机验收继续未做**（阶段二长期标准，项目所有者 2026-08-09 起为可选项，
  不是本版发布门槛）；Apple 扩展代码保留并有单测与协议级用例覆盖，如实表述为
  「已实现、尚未真机验收」。

---

## v0.4.0（2026-08-25，正式版）

> 写作纪律同 v0.2.0/v0.3.0：功能没合入 `main` 之前不写、打折写在句子主干里、
> 区分三档置信度（已实测 / 只交叉编译 / 仅代码推断）。本节所有条目均已合入 `main`
> （PR 号逐条核对：#176、#182–#189）。

### 发布亮点

- **修复多进程共享同一共享目录时的启动失败（v0.3.0 已知缺陷 OI-1，本版最重要的行为变化）**。
  builtin 旁路存储此前按「共享根哈希」决定库文件名，两个服务进程共享同一 share 目录时，
  后到者会卡满 flock 超时（5 秒）后退出。现在默认落点把**服务实例标识**（监听 addr:port）
  一并编进文件名：每个实例各开各的旁路库，互不阻塞；同一配置重启得到同一路径，元数据照常复用；
  显式配置 `metadata_path` 时唯一性由配置者自己负责。**证据强度分档**：组件级 helper-process
  多进程回归测试 + `go test -overlay` 变异反向对照（删掉区分逻辑 3/3 变红）；黑盒确认来自
  合并后 main 上 `scripts/acceptance.sh` 全量重跑 rc=0——该验收本身就是三个服务进程共享一个
  share 目录的场景。
- **服务端健壮性测试去抖**：`internal/server` 的 8 个时序测试从墙钟断言改为包内原子计数断言
  （拒绝/节流抑制/超长帧/畸形帧/闲置关闭/握手超时/期限解除各有确定性观测点，生产行为零改变）。
  变异验证 8/8 变红；8 忙循环 + `GOGC=5` 高负载 `-count=12` 全绿。CI 不再需要 `-skip` 隔离。
- **CI 与验收加固**：GitHub Actions windows-latest 单测的 PowerShell 解析错误已修
  （顶层 `defaults: run: shell: bash`）；`acceptance.sh` 给每个被拉起的服务实例独立
  `metadata_path`（纵深防御，不再依赖跨进程共享）；`metadata_path` 配置校验改为**全平台生效**
  并删除「仅 Windows 生效」的错误 WARN。
- **三客户端交叉验证基线报告入库**（`test/reports/client-matrix-v030-20260825.md`）：对 v0.3.0
  的黑盒实测——smbclient 七种方言协商 + 全操作集 + 8MB md5 回环、impacket 11/11、
  go-smb2 独立客户端 9 步 + 集成套件 21/21 全部通过；认证拒绝、guest 告警、只读强制正确。

### 已知问题 / 未实现（v0.4.0）

- **`native` 档在 linux / darwin / windows 上仍恒定启动失败**（与 v0.3.0 相同，未变）：
  各平台均有至少一项能力被源码硬编码为不支持。默认 `auto`，未显式写 `native` 的用户不受影响。
- **Time Machine 真机验收仍未做**（维持可选项，不阻塞发布）：Apple 扩展代码保留，
  部分经协议级实测，端到端备份/恢复从未在真实 macOS 上跑过，定级维持 C 档。
- **SMB1「SMB 2.???」升级入口只有单测档证据**：本环境三家第三方客户端均无法触达该路径
  （smbclient 已剔除 SMB1、impacket 直发 SMB2）。按项目所有者决定暂不补测，
  详见 `docs/protocol-notes.md` §4 的记录；**请勿据此宣称支持 SMB1 客户端**。
- **GitHub Actions windows runner 的修复尚未经真机验证**：代码侧已修并合入，
  但本容器无法跑 Windows，「windows-latest 单测转绿」待镜像仓下一次 GA run 回填确认。
- 认证安全专项审计（NTLMv2 校验面/常量时间比较/日志不落口令的系统盘点）列入 v0.4.x 后续，
  本版未做系统性审计（既有单测与 §8 纪律仍然有效）。

---

## v0.3.0（2026-08-10，正式版）

> **写作纪律同 v0.2.0**（详见下方 v0.2.0 段开头的说明）：功能没合入 `main` 之前不写、
> 打折写在句子主干里、区分三档置信度（已实测 / 只交叉编译 / 仅代码推断）、PR 号逐条核对。
> 本节按证据强度分档陈述，不夸大。

### 发布亮点

- **OS 能力抽象（oscap）六项能力全部接进 VFS 数据路径（6/6）**。v0.2.0 时只有扩展属性
  （xattr）与命名流两项真正生效；本版本由 vfs-sparse / vfs-attr 角色把剩余四项——稀疏文件、
  稳定 FileID、创建时间、DOS 属性位——也接进 `internal/vfs` 的真实读写路径。接线口径与
  可复算判据见下方 v0.2.0 段保留的 `BEGIN-OSCAP-WIRING-STATUS` 块（已同步更新为 6/6）。
  **证据强度分档**：xattr / 命名流 / 稀疏文件 / DOS 属性位经 impacket 低阶 SMB2 客户端
  **协议级实测**；稳定 FileID / 创建时间目前以**单测 + 代码核实**为准，真机大目录吞吐与
  FileID 稳定性验证待补，不属于「已实测」。
- **`filesystem_mode` 三档（auto / native / portable）现在对全部六项能力都有真实运行期效果**。
  `portable` 整机不碰宿主可选能力、数据落自带旁路存储；`auto` 逐项优先原生并降级；
  `native` 强制全走原生（某项不支持即启动报错）。
- **freebsd CI 缺口补上**：`test/ci/check-test-compile.sh` 的平台列表追加 `freebsd/amd64`，
  此前 6 个带 `!linux && !darwin && !windows` 约束的文件从未被任何 CI 平台编译过
  （AGENTS.md §1.2 已记录此历史缺口）。补上后 `GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go vet
  ./...` 进入 CI 门禁；手工补跑 rc=0 已确认当前能编译。
- **至少三种第三方 SMB 客户端验收**：smbclient + impacket + go-smb2 三家均通过端到端，
  满足 AGENTS.md §3 门槛（`mount.cifs` 在容器内仍 skip，rc=77，由另三家补足）。

### 已知问题 / 未实现（v0.3.0）

- **`native` 档在 linux / darwin / windows 上仍恒定启动失败**（与文件系统无关）：
  各平台均有至少一项能力被源码硬编码为不支持，而 `native` 契约要求六项全原生，三家相乘
  无任何平台可用。默认 `auto`，未显式写 `native` 的用户不受影响。报错文案归因仍有误
  （见 v0.2.0 段 OSCAP 块）。
- **Time Machine 仍未经真机验收**：Apple 扩展（AAPL / 命名流 / 稀疏文件 / `_adisk._tcp`）
  已实现，部分经协议级实测，但**端到端 Time Machine 备份与恢复尚未在真实 macOS 上跑过**，
  定级维持 C 档，请勿用于唯一备份。详见 README「Time Machine 状态」段。

---

## v0.2.0（2026-08-09，正式版）

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
- **发布渠道改为按 tag 名的 SemVer 判定**（PR #156，合入 `25e92d2`）。
  在此之前 Release 步骤**硬编码** `preRelease: true` / `latest: false`，且 `tag_push`
  对任何 tag 都触发、不按 tag 名区分 —— 也就是说 v0.2.0 本来会和 v0.1.0 一样被发成预发布。
  现在的规则：**tag 名带连字符**（`v0.2.0-rc1`）→ 预发布；**不带连字符**（`v0.2.0`）→
  正式版且标记为 latest。

  实现是**两个互斥 stage + `if:`**，而不是往 `options` 里塞变量：CNB 只对**插件任务的
  `settings`** 明文声明支持 `$VAR` 替换，`git:release` 是内置任务用 `options:`，没有这条
  声明 —— 往里塞变量是赌一个没写进文档的行为，赌输的代价就是发出一个渠道错误的 Release。

  **两条真 tag 的端到端实测**（不是推断）：

  | tag | 正式版 stage | 预发布 stage | `prerelease` | `is_latest` |
  |---|---|---|---|---|
  | `v0.0.99-probe` | skipped | success | True | False |
  | `v0.0.99` | success | skipped | False | True |

  并做过**跨配置对照**，排除「是 tag 名本身决定渠道、与本次改动无关」这个替代解释：
  `v0.1.0` 与 `v0.0.99` 同为不带连字符的形态，前者走旧配置得 `prerelease=True`、
  后者走新配置得 `False` —— 差异只能归因于这次改动。

  ⚠️ 查询时**看 `is_latest` 字段，不要看 `latest`**：后者实测恒为 `null`，照它判会得出
  「所有 Release 都不是最新版」的错误结论（与 PR 的 `merged=null` 同型）。
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

### 内部：OS 能力抽象（**六项能力的两套适配器都已建成；v0.2.0 时其中 2 项已接进数据路径，v0.3.0 起 6 项全部接进，见 v0.3.0 段**）

> **一句话结论**：port 层 + native 适配器 + builtin 适配器 + portable 模式 CI 门禁
> 四块**全部已在 `main`**，测试是真跑的、门禁是有牙的；
> **扩展属性与命名流两项已由 PR #159 接进 `internal/vfs` 的真实数据路径**，
> 其余四项在 `internal/vfs` / `internal/server` / `cmd/` 里仍无调用点，
> 对这四项而言本版本的运行行为与 v0.1.0 **逐字节等同**。
> 「代码建成」和「行为生效」在这里仍是两件事，下面分开写，判据附在每条后面；
> 接线后的准确口径以下方 `BEGIN-OSCAP-WIRING-STATUS` 块为单一真相。

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

<!-- BEGIN-OSCAP-WIRING-STATUS：oscap-wire 的接线 PR 一合入 main，整段替换本块，不要散改别处。
     ⚠️ 同类块**共四处**（本处 + README.md 已知限制第 7 条 + configs/example.yaml 的
     filesystem_mode 段 + AGENTS.md §1.2「已建成 ≠ 已生效」），四处讲的是同一件事，
     接线后**四处都要改**，只改这里会在另外三处留下过期陈述。
     一次找齐：grep -rn OSCAP-WIRING-STATUS . | grep -v '^./history/'
     该 grep 的命中共**三类**，只有第一类要改：
       ① 活断言 —— 成对的 BEGIN/END 标记（就是上面那四处），**整段替换正文**
       ② 范例引用 —— 只有孤零零一行 BEGIN，且被引在代码围栏里当写法样例，**不要动**：
          · docs/status-v0.2.0.md（PM 进度板）
          · memory/feedback_falsifiable_assertions.md
       ③ 正文交叉引用 —— **不含 BEGIN/END**，只在句子里提块名，例如 README.md:545
          「详见 CHANGELOG.md v0.2.0 段的 OSCAP-WIRING-STATUS 块」。
          正文不用改，但**块一旦改名或删除必须同步**，否则就退化成「见 X 而 X 不存在」。
     （初版只写了①②两类，漏了③。漏的原因很典型：③ 不含 BEGIN 关键字，
       按「有没有 BEGIN」去分类就永远看不见它，而 grep 照样会把它捞出来。）
     （初版写「三处」，漏了 AGENTS.md —— 漏的那处恰好是 oscap-wire 点名要改的。
       所以「一套 N 处」这个数字本身也要核，别照抄。） -->
> **⚠️ 更新注记（2026-08-25 补记，v0.5 开发版起口径）**：`filesystem_mode` 已收敛为两态
> `auto` / `portable`，`native` 整档移除（PR #191，见顶部 Unreleased 条目）。下方正文写作于
> 三态时代：其中「三档」的表述、「`native` 强制全走原生（某项不支持即启动报错、不降级）」，以及
> 两条 ⚠️（native 档恒定启动失败、报错文案归因有误）均已随档位整档移除而失去当前性——按发布时点
> 状态原样保留作历史，请勿当作现状；当前口径以 AGENTS.md §1.2 的两态表为准。四处同步进度：
> configs/example.yaml 与 AGENTS.md §1.2/§5 P7 已随 #191 改为两态；README 的对应陈述截至本注记
> 写入时仍为旧口径（README 不归本文件管辖，此处仅记录事实快照）。

**已接线 6 项 / 共 6 项**（v0.3.0 起全部接进 `internal/vfs` 数据路径；v0.2.0 时仅 2/6，
见下方「历史」段）。下面判据**命令可原样粘贴复跑**，不依赖任何时间戳：

- ✅ **oscap 已进入真实数据路径**，`filesystem_mode` 对**全部六项**能力**都有真实运行期效果**。
  三条互相独立、可一行复算的判据：
  1. `go list -deps ./cmd/stupidsamba | grep -c oscap` = **3**（自 v0.2.0 接线起即为 3，未变）——
     `internal/oscap`、`oscap/native`、`oscap/builtin` 三个包**都真被链进发布二进制**。
     这是最硬的一条：链接依赖是编译器算出来的事实，测试可以写得很漂亮却测不到真实路径，
     而这个数字伪造不了。
  2. 包外真实调用点覆盖**全部六项能力**：`CapXattr`、`CapNamedStream`（v0.2.0 PR #159）、
     `CapSparse`、`CapStableFileID`、`CapCreationTime`、`CapDOSAttributes`
     （后四项由 vfs-sparse / vfs-attr 角色在 v0.3.0 落地）。原样可粘贴核查：
     `grep -rn 'caps\.\(Xattr\|Streams\|Sparse\|StableFileID\|CreationTime\|DOSAttributes\)()' --include='*.go' . | grep -v '^./internal/oscap/' | grep -v '_test.go' | grep -v '//'`
     ⚠️ 此 grep 只证明「有调用点」，不证明「端到端跑通」；端到端验证强度见 v0.3.0 段「3 客户端验收」。
  3. 旧实现 `newXattrAccessor` / `readMetaXattrFast` 的**活调用清零**（v0.2.0 已完成，R11 双写消除），
     `internal/vfs/xattr_unix.go`(-211) 与 `xattr_other.go`(-23) **已整文件删除**；
     v0.3.0 接后四项时，各自的旧平台专属实现也一并拆除，不再与 oscap 双份实现撞车。
- ✅ **六项能力全部接进数据路径**：`CapXattr`（6 处）、`CapNamedStream`（4 处）、
  `CapSparse`（稀疏文件 FSCTL：`FSCTL_SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`）、
  `CapStableFileID`、`CapCreationTime`、`CapDOSAttributes`（SET_INFO 落 DOS 属性位）。
  **证据强度分档**（按 AGENTS.md 要求，不混为一谈）：xattr / 命名流 / 稀疏文件 / DOS 属性位
  经 impacket 低阶 SMB2 客户端**协议级实测**通过；`CapStableFileID` / `CapCreationTime`
  目前以**单测 + 代码核实**为准，真机大目录吞吐与 FileID 稳定性验证待补，不属于「已实测」。
- ✅ **`filesystem_mode` 三档（auto / native / portable）现在对全部六项能力都有真实运行期效果**：
  `portable` 整机不碰宿主可选能力（数据落自带旁路存储），`auto` 逐项优先原生并降级，
  `native` 强制全走原生（某项不支持即启动报错、不降级）。
- ⚠️ **`native` 档在 linux / darwin / windows 上仍会恒定启动失败**（与文件系统无关，v0.3.0 未修）：
  根因是**每个平台都有至少一项能力被源码硬编码为不支持**（linux/darwin 的 `dos_attributes`、
  darwin 还有 `sparse_file`、windows 的 `xattr`——`internal/oscap/probe_windows.go:24`
  无条件 `return false`），而 `native` 的契约是「有一项不支持就报错、不降级」，三家相乘即没有任何平台可用。
  **默认值是 `auto`**（`internal/oscap/mode.go` `DefaultMode = ModeAuto`），没有显式写 `native` 的
  用户不受影响；撞上的人改用 `auto`（逐项降级）或 `portable`（全部 builtin）。
- ⚠️ **当前报错文案的归因仍有误**：它说「所在的**文件系统**不支持 dos_attributes」，
  而真相是**本平台压根没有 native 实现**，换任何文件系统都无效。照这句话去换盘是白折腾。
  修复排在 v0.3.0 之后（改 `oscap.UnsupportedError` 的结构）。

> **历史（v0.2.0 时仅 2/6，v0.3.0 起已 6/6）**：本节初版写作于 v0.2.0，当时只有
> `CapXattr` 与 `CapNamedStream` 两项接进数据路径，其余四项在 `internal/vfs` /
> `internal/server` / `cmd/` 里**无调用点**，`filesystem_mode` 对那四项没有运行期效果——
> 那才是「已建成 ≠ 已生效」的真实状态。v0.3.0 由 vfs-sparse / vfs-attr 角色把剩下四项
> 也接进数据路径后，本块整体更新为 6/6。保留这段是为了让「为什么曾经 2/6」与「判据怎么
> 数出来」可追溯，**请勿把本段当成当前状态**。
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
- **`filesystem_mode` 只对已接线的两项生效（v0.2.0 时）；v0.3.0 起六项全部生效，本条已解决**。
  历史口径（v0.2.0）：当时仅 `CapXattr` 与 `CapNamedStream` 接进 `internal/vfs` 真实数据路径，
  改开关只对这两项有运行期效果，`CapSparse` / `CapStableFileID` / `CapCreationTime` /
  `CapDOSAttributes` 四项尚无产品调用点、三档跑起来完全一样，故「设成 `portable` 就整机
  不碰宿主特性」彼时**不成立**。v0.3.0 把四项全部接进后（见 v0.3.0 段与上方
  `BEGIN-OSCAP-WIRING-STATUS` 块），三档现在对全部六项能力都有真实效果。
- **D-native（待决）：接线落地后 `native` 档在任何平台都启动不了。**
  **先说清适用范围**：本条描述的是**接线之后**的行为，而接线已由 PR #159 在本版本落地 ——
  也就是说它**现在就成立**，不再是「将要发生」。（写这条时接线尚未合入，当时 `native`
  仍然启动成功，即上节表格上排三格全绿；那一排现在只剩「接线前」的历史对照价值。）
  默认值是 `auto`，**没有显式写 `native` 的用户不受影响**，所以它不是普遍性缺陷；
  登记在这里是因为撞上的人除了那句报错没有任何别的提示，而那句报错的归因还是错的。
  根因、逐平台出处与可复算判据见上节 `BEGIN-OSCAP-WIRING-STATUS` 块（单一真相，不在此重复）；
  一句话是：`native` 要求六项全走原生，而 linux / darwin / windows **各自都有**至少一项被
  硬编码成不支持。**待决的是取舍，不是事实**——三个方向都改动了合同，需要有人拍板：
  (a) 维持现状，把 `native` 明确记为「保留档位，当前无可用平台」，文档不再宣称它可用于排障；
  (b) 放宽为「只对**已接线**的能力强制 native」，代价是 `native` 不再等于「六项全原生」；
  (c) 补齐缺的那几项 native 实现（darwin `F_PUNCHHOLE`、DOS 属性的原生承载），代价最大。
  **在拍板之前，任何文档都不要写「`native` 用于测试和排障」**——那句话接线后即成假话。
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
  反向对照也已补齐（`08cfa27` / PR #160，19:08:59 CST），且是**三向**的：
  `test/ci/negative-verify.sh` 第 6 节往 `internal/oscap/native/native_other.go`
  注入一个类型错误，验 **6a** 干净树全绿、**6b** 新门禁必须变红**且报错点名
  `not an int`**（排除「因别的原因红」的假阳性）、**6c** 只用旧四平台编译同一份故障
  **变回绿**——最后这一条正是「洞确实存在过」的证据。整节无 skip 门控，默认路径直跑。
  判据（按稳定锚点定位，不用节号也不用计数）：

  ```sh
  grep -n 'ANCHOR: freebsd-fallback-files' test/ci/negative-verify.sh   # rc=0 即该节在位
  sh test/ci/negative-verify.sh                                          # 20:19 CST 实跑：40 通过 / 0 失败
  ```

  > **⚠️ 这里原本写的是「`grep -c -i freebsd …` → 8」，20:21 CST 复算时发现它在本 PR 自己
  > 的分支上已经变成 **11** —— 因为**我在同一个 PR 里给那节加了锚点注释，注释里又提了
  > 三次 freebsd**。也就是说：**我亲手把自己写的判据数改掉了，而且是在同一个 PR 内。**
  > 这是同一形态今天的第三次复发，前两次分别隔了 90 秒和几十分钟，这次间隔是**零** ——
  > 断言与破坏它的改动躺在同一个 diff 里。
  > 教训因此再收紧一层：**判据不要绑在「会随任何编辑漂移的计数」上。**
  > 计数型判据（`grep -c`、行数、文件数）只适合一次性核对，不适合写进长期文档；
  > 长期文档要绑**稳定锚点**或**命令的退出码**，它们不会因为有人多写一行注释就变。
  ⚠️ **本条曾在 `main` 上短暂写反，留下记录当教训**：`84e5d71`（19:10:29）里这段原文写的是
  「这一档目前没有反向对照…`grep -c freebsd` → **0**，实测 18:59 CST」，
  而补丁 `08cfa27` 在 **19:08:59** 就已合入——**比我的文档落地早 90 秒**。
  也就是说那句断言**在进入 `main` 的那一刻就已经是假的**，尽管它带了判据、带了时间戳、
  当时也确实实测过。教训不是「要写可证伪的断言」（那条已经做到了），而是新的一条：
  **可证伪断言必须在合入前重跑一次，写作时刻的真不等于合入时刻的真**——
  尤其当你登记的正是「某人应该去修的缺口」时，那个人很可能就在这几分钟里修好了。
  形态上它属于本仓库「写了但从未被验证过」清单的**第 10 例**（前 9 例是**代码**没被执行，
  这一例是**注释里引用了一个不存在的实体**：`check-test-compile.sh:113` 当时指向的
  `negative-verify.sh` freebsd 段尚不存在，读者会据此以为对照已有而不再去补）——
  该形态已修复，但**它派生出的「断言时效性」问题在本条身上真实复发了一次**，故一并留档。
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
