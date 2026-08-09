# AGENTS.md — stupidSamba 项目准则

> 本文件是**所有 agent（人类与 AI）在本仓库工作时必须遵守的最高准则**。
> 与本文件冲突的任何做法一律以本文件为准。开工前必读，改动架构后必须回来更新本文件。

---

## 0. 一句话项目定义

`stupidSamba` 是一个**用纯粹 Go 语言实现的、极简易用的 SMB/CIFS 文件共享服务器**，
自带进程内 mDNS/DNS-SD 广播与 Apple SMB 扩展，最终支持 macOS Time Machine 备份。

**只做文件共享。** 不做打印机共享，不做域控，不做任何其他网络服务。

---

## 1. 硬性技术约束（不可协商）

这些是项目的立身之本，**任何情况下都不允许违反**。若某个需求似乎必须违反，
先停下来在 Issue/PR 里讨论，不要擅自破例。

| # | 约束 | 说明 |
|---|---|---|
| C1 | **禁止 CGO** | `CGO_ENABLED=0` 必须能编译通过。CI 强制校验。 |
| C2 | **禁止依赖外部动态库** | 产物必须是单个静态链接的二进制，`ldd` 结果为 "not a dynamic executable"。 |
| C3 | **禁止依赖外部进程** | 不得 fork/exec 任何外部命令（`smbd`、`avahi-daemon`、`nmbd`、`mount`、`ip`、`systemd-resolve` 等一概不行）。运行时不得要求系统预装任何服务。 |
| C4 | **禁止依赖系统网络服务做协议转换/广播** | mDNS 必须由本进程自己在 224.0.0.251:5353 / [ff02::fb]:5353 上收发报文实现，**不允许**调用 avahi/Bonjour/systemd-resolved 的 D-Bus 或 socket 接口。 |
| C5 | **所有网络协议栈都在本仓库内** | SMB1/2/3、NTLM、SPNEGO/GSS-API、DCERPC(srvsvc)、mDNS/DNS-SD、NetBIOS 全部自实现或 vendor 进来。 |
| C6 | **允许使用第三方社区库，但功能不足时必须改写或重写** | 见 §4 依赖政策。宁可 vendor 一份可控的代码，也不要迁就一个功能不够的库。 |
| C7 | **跨平台** | 至少 linux/amd64、linux/arm64、darwin/arm64、windows/amd64 能交叉编译通过。平台相关代码用 build tag 隔离。 |
| C8 | **认证与加密完全自成体系，与操作系统用户管理零关系** | 见下方 §1.1，这是一条独立的硬性约束。 |
| C9 | **操作系统只作为「文件系统 + 套接字」提供方** | OS 只提供两样东西：(a) 一块可读写的普通文件系统；(b) 网络套接字。其余一概自己实现。见下方 §1.2。 |

### 1.1 认证自成体系（C8 展开）

**账户体系完全由本软件自己管理，与宿主操作系统的用户管理没有任何关系。**

明确禁止：

- ❌ PAM（`pam_authenticate` 等，无论通过 CGO 还是外部进程）
- ❌ 读取 `/etc/passwd`、`/etc/shadow`、`/etc/group`
- ❌ NSS / `getpwnam` / `getgrnam` / `os/user` 包的**查询系语义**
      （`os/user.Lookup*` 在纯 Go 模式下会去读 `/etc/passwd`，同样禁止）
- ❌ winbind、SSSD、nslcd 等名字服务守护进程
- ❌ Windows 的 `LogonUser` / SSPI / LSA，macOS 的 OpenDirectory
- ❌ 系统 keyring / Secret Service / DPAPI
- ❌ 依赖系统的 Kerberos 配置（`/etc/krb5.conf`、系统 keytab、`KRB5CCNAME`）

必须这样做：

- ✅ 用户名与口令凭据**只来自本软件的 YAML 配置文件**（明文口令或 NT hash）。
- ✅ 所有密码学原语（MD4/MD5/HMAC/RC4/AES-CMAC/AES-CCM/AES-GCM/SHA-512/SP800-108 KDF）
      **在本仓库内用纯 Go 实现或来自 Go 标准库 / `golang.org/x/crypto`**，
      不调用 OpenSSL、GnuTLS、CommonCrypto、CNG/BCrypt 等系统密码库。
- ✅ 配置里的 `uid` / `gid` 只是**给 VFS 层用的数字标签**，用于文件属主展示与权限决策，
      **不做系统用户解析**，也不要求宿主系统上真的存在这个用户。
- ✅ 授权（谁能访问哪个 share、是否只读）完全由配置文件里的 `valid_users` / `read_only` /
      `guest_ok` 决定，**不读取宿主文件系统的 ACL 来做访问判定**
      （底层 IO 仍然受进程自身的操作系统权限约束，这是不可避免的，但不作为授权依据）。

理由：本软件要能在任意环境（容器、只读根文件系统、嵌入式、Windows）以单个二进制开箱即用，
不能要求管理员先在宿主机上建用户。同时这也让行为可预测、可测试、可移植。

**自检命令**（提交前必须本地跑过）：

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

### 1.2 操作系统只是「文件系统 + 套接字」提供方（C9 展开）

**起因**：项目所有者读到 §10.3 第 2 条（`mount.cifs` 在本容器跑不通）时提出质疑 ——
「这个项目还依赖 namespace？依赖 linux 自身的 cifs 或者 mount？这不可接受，
绝不应该依赖操作系统提供的任何相关机制。操作系统只被当作一个普通的文件系统提供方。」

**核查结论：产品代码本来就是干净的** —— `internal/` + `cmd/` 的非测试代码里
0 处 `os/exec`、0 处 `syscall.Mount` / `Setns` / `Unshare` / `Chroot` / `PivotRoot`。
§10.3 第 2 条讲的是**测试工具**跑不起来，不是项目依赖。
但那段文字写得像在讲项目依赖 —— **这就是本条约束存在的理由**：
把边界写死成硬约束并且**机器校验**，不再靠「我们记得」，也不再让读者去猜。

#### 允许向 OS 索取的（只有这两样）

- ✅ **一块可读写的普通文件系统**：open/read/write/seek/stat/rename/unlink/mkdir/readdir/fsync。
      关键词是「**普通**」—— 不假设它支持 xattr、稀疏文件、稳定 inode、创建时间、
      POSIX 属主/权限位。这些可选能力**有就用，没有就由 builtin 适配器自己补**（见下）。
- ✅ **网络套接字**：TCP listen/accept/read/write，UDP 组播收发（mDNS 用）。

#### 明确禁止依赖的 OS 机制

| 类别 | 具体禁止 |
|---|---|
| 协议实现 | 内核 cifs / smb3 驱动、`mount -t cifs`、任何内核态或系统自带的 SMB 实现 |
| 守护进程 | avahi / Bonjour(mDNSResponder) / systemd-resolved / winbind / SSSD / nmbd / smbd |
| 名字解析 | NSS、`/etc/resolv.conf`、`/etc/nsswitch.conf`、`/etc/hosts`、系统解析器语义、`os/user.Lookup*` |
| 系统状态文件 | `/proc/net/` 下的一切（要网络信息就自己从套接字拿，不要去读内核导出的文本） |
| 认证机制 | PAM / SSPI / LSA / OpenDirectory / 系统 Kerberos 配置（与 C8 重合，此处再申明一次；`/etc/passwd` 那组归 C8，不在 C9 重复） |
| 挂载与命名空间 | `mount` / `umount` / `setns` / `unshare` / `chroot` / `pivot_root`、`CLONE_NEW*` 各标志、FUSE / loop 设备 |

#### 明确**不在**禁止之列：平台 ABI 本身

调用平台的官方 ABI **不算**违反 C9，别把这条读成「不许有任何系统调用」——
那样连 `os.Open` 都写不出来。

Windows 没有稳定的系统调用号，**官方 ABI 边界就是 DLL 导出函数**：Go runtime 自身的
`os` / `net` / `time` 全部通过 `NewLazySystemDLL` 调用 `kernel32.dll` / `ntdll.dll` /
`ws2_32.dll`。这是**平台调用约定**，不是 C2 说的「依赖第三方动态库」——
把我们的代码产物整个删掉，Windows 进程照样加载 kernel32。Linux 上的 `syscall` 指令同理。
本仓库现存的这类调用点如 `internal/vfs/sys_windows.go:17`
（`NewLazySystemDLL("kernel32.dll")` 取 `GetDiskFreeSpaceW`）是**合规**的。

但豁免只给**平台调用约定本身**，不是给「凡是加载 DLL 都行」。门禁按白名单判：

- ✅ 只放行 `windows.NewLazySystemDLL("<名字>")`，且名字必须是**字面量**，
      取值限于 `kernel32` / `ntdll` / `ws2_32` / `advapi32`。
- ❌ `NewLazyDLL` / `LoadLibrary` 一律违规 —— 它们**不走 System32 安全加载路径**，
      是 DLL 劫持的经典入口。参数不是字面量的（运行时拼出来的 DLL 名）同样判红。

**明确不在禁止之列的还有：套接字操作本身。** `net.Listen` / `net.ListenMulticastUDP`
以及 `internal/mdns` 在 224.0.0.251:5353 与 `[ff02::fb]:5353` 上自己收发组播报文，
不但不违反 C9，而且正是 **C4 明确要求**的做法（C4 禁的是去调 avahi/Bonjour 的
D-Bus 或 socket 接口，不是禁组播）。别把这两件事搞混了去「修」mdns。

判据是**语义**不是形式：向 OS 要「一个字节区间的读写」「一个能收发的套接字」是允许的，
向 OS 要「一个 SMB 客户端」「一次身份认证」「一次挂载」「一份现成的名字解析结果」是禁止的。
前者绕不过去，后者我们本来就该自己做。

#### 架构：能力抽象 port + native/builtin 双适配器

项目所有者给的方向：「这个项目要做好对操作系统能力的抽象模块，并提供**依赖系统调用的
模块实现版本**和**本项目在自己内部实现的另一个版本**。」落成结构：

- `internal/oscap` —— 定义**能力 port**（接口）。它描述「我需要什么语义」，
  不描述「谁来做」。
- `internal/oscap/native` —— **借助 OS 能力**的适配器：有 xattr 就用 xattr，
  有 `FALLOC_FL_PUNCH_HOLE` 就打洞，有 NTFS ADS 就直接写 ADS。快、省、
  且行为与宿主上的本地工具一致（`getfattr` / `ls -l@` / `du --apparent-size`
  看到的和 SMB 客户端看到的是同一份东西）。
- `internal/oscap/builtin` —— **本项目自己实现**的适配器：只用 C9 允许的那两样东西
  （普通文件 + 套接字）把**同一份语义**做出来。慢一些，但任何能跑 Go 的地方都能跑。

两条铁律：

1. **逐能力矩阵降级，不是整体二选一。**
   每一项能力**独立**决定走 native 还是 builtin。真实场景本来就是混合的：
   ext4 有 xattr 但拿不到可靠的「创建时间」，于是命名流走 native、创建时间走 builtin。
   所谓「窄档」只是**所有项都指向 builtin 的极限情况**，不是一个单独的实现分支 ——
   不要写出「if 窄平台 { 走另一套代码 }」这种结构，那会变成第二份永远没人测的实现。
2. **builtin 版必须完整。**
   每一项能力都必须有 builtin 实现，不允许出现「这项只有 native 有」。
   理由：将来移植到未知系统环境（嵌入式、只读根文件系统、我们没见过的 NAS 固件）时，
   **builtin 是唯一底座** —— 缺一项就等于那个平台整个不可用，而且往往到现场才发现。
   因此：**新增一项能力的 native 实现时，必须同时给出 builtin 实现**，不许赊账。

配置三态 `filesystem_mode`：

| 取值 | 含义 |
|---|---|
| `auto`（默认） | 逐项探测宿主能力，能 native 就 native，不能就自动落到 builtin |
| `native` | 强制全部走 native；探测到某项不支持就**启动即报错**，不静默降级 |
| `portable` | 强制全部走 builtin，完全不碰 OS 的可选能力。可移植性/可预测性最高，性能最低 |

`native` 为什么要「不支持就报错」而不是降级：它的用途是**在测试里钉死走的是哪条路**。
一个会偷偷降级的 `native` 等于没有 —— 这个亏本项目已经吃过：
某个策略开关只测了「允许」这条路径，全绿，而「拒绝」那条路径压根没接线，
测试从头到尾都在验证同一条路。

**实现进度（2026-08-09 更新，原文写的「都还不存在」已过时）**：
本节最初写就时，`internal/oscap` 与 `filesystem_mode` 都只是规划，排期 v0.3.0。
**v0.2.0 期间提前落地了，现在它们是真代码，可以照着去找**：

| 部件 | 位置 | 状态 |
|---|---|---|
| port 层（6 接口 + 逐能力矩阵 + 三态 Mode + 平台探测） | `internal/oscap/*.go` | 已在 main（PR #123，`6413e1d`），31 PASS / 0 SKIP |
| `filesystem_mode` 三态配置 | `internal/config`、`configs/example.yaml` | 已在 main（PR #126，`212d6f9`），取值判定委托 `oscap.ParseMode` 保持单一真源 |
| native 适配器 | `internal/oscap/native/` | **已在 main**（PR #138，`d351683`），六项齐全，37 PASS / **0 SKIP** / 0 FAIL |
| builtin 适配器 | `internal/oscap/builtin/` | **已在 main**（PR #129，`e4f0f80`），六项齐全，bbolt 旁路存储，49 PASS / 0 SKIP / 0 FAIL |
| portable 模式 CI 门禁 | `test/ci/portable-mode.sh` | **已在 main**（PR #135，`ff77acb`），挂 push + pull_request 两条路径（`.cnb.yml` 的 `&gate_portable`），4 个变异体反向对照 4/4 变红 |
| **运行期消费方** | `internal/vfs`、`internal/server`、`cmd/` | ❌ **无**（截至 2026-08-09 18:35 CST 的当下事实；`CapXattr` 接线 PR 合入 `main` 后此格改写）。全仓唯一引用 oscap 的产品代码是 `internal/config/validate.go`，且只用于 `oscap.ParseMode` 校验配置字符串。见下方「已建成 ≠ 已生效」 |

上面那张三态表因此已经是**对现有代码的描述**，不再是「将要建成的东西」。
2026-08-09 的一轮文档审计核实：**双适配器与 portable CI 门禁已于 v0.2.0 期间全部合入**，
上一版这里写的「开发中 / 未完成」已过期，故就地订正。

**⚠️ 已建成 ≠ 已生效（v0.2.0 的实际边界，落笔前务必知道）**：
六项能力的两套适配器都造好了、都有测试、portable 门禁也在 CI 里真跑，
但**整个 oscap 子系统至今没有任何产品调用点**，
所以 `filesystem_mode` 三个取值在 v0.2.0 运行期**行为完全一样**。
四条互相独立的判据（2026-08-09 18:07~18:15 CST 实测，可自行复算）：

1. `go list -f '{{.ImportPath}} {{.Imports}}' ./... | grep oscap` —— 全仓只有
   `internal/config` import 了 `internal/oscap`（为了 `ParseMode`），
   **没有任何包** import `oscap/native` 或 `oscap/builtin`。
2. `go list -deps ./cmd/stupidsamba | grep -c oscap` = **1** —— 两个适配器
   **根本没被链进发布二进制**。
3. `grep -rn 'oscap\.Open\|oscap\.New\|SelectMatrix\|ProbeNative\|native\.New\|builtin\.New' --include='*.go' . | grep -v '^./internal/oscap/'`
   —— 包外唯一命中是 `internal/config/config.go:25` 的一句**注释**，零个真实调用。
4. `FilesystemMode` 在产品代码里只出现在 `internal/config/defaults.go:48-49`（填默认值）
   与 `internal/config/validate.go:238-245`（校验取值），**没有运行期消费者**。

真实数据路径仍走各自那份旧实现，与 oscap 并存但互不相干（例如 xattr 在数据路径上是
`internal/vfs/xattr_unix.go`，`internal/oscap/native/xattr_posix.go` 是另一份、无人调用）。
**接线时必须一并拆掉旧的那份**，否则会重演 `internal/meta` 与
`internal/vfs/metadata_windows.go` 的双实现撞车（风险 R11）。

顺带一条给写文档的人：`configs/example.yaml:21-35` 对 `filesystem_mode` 的注释是**按设计意图**
写的（「逐项探测宿主支持情况」），没有提示它当前无运行期效果。改动那里之前先读本段。

**前置要求（portable 必须在 CI 里真跑）—— 已满足，原文保留作为判据说明**：
`portable` 模式必须**在 CI 里真跑一遍**，不能只是配置项里多一个取值。
理由：builtin 是「将来移植到未知系统」的唯一底座，而一条在 CI 里从未被执行过的路径，
到需要它的那天一定是坏的 —— 那时既没有原始作者在场，也没有可对照的正确行为。
**没有 CI 覆盖的 builtin 就是一份薛定谔的实现**，写了等于没写。
本仓库已有同型前科：挂在特定 build tag 下的代码，默认 CI 一行都不会编译执行，
直到有人专门补一关才被真正看见（`test/ci/check-test-compile.sh` 的注释里记了两例）。

现由 `test/ci/portable-mode.sh` 满足。**它自带反向对照，这才是它算数的原因**：
四个变异体分别模拟「portable 偷偷调 native factory」「用例被改名导致门禁空转」
「子测试全 SKIP 但父测试仍 PASS」「子测试整体不执行且不留 SKIP 痕迹」，
**4/4 都让门禁变红，且红在四个不同的判据上**（业务断言 / 用例清单 / SKIP 计数 /
结果行下界）。后两个变异体值得单独记住：它们是「报绿但什么都没跑」的两种形态，
只统计 FAIL 数的门禁对它们完全无感。

**⚠️ 同型缺口仍在，不要以为门禁已经没有死角**：
`test/ci/check-test-compile.sh:104` 的平台列表是
`linux/amd64 linux/arm64 darwin/arm64 windows/amd64`，**没有一个满足
`!linux && !darwin && !windows`**，于是本仓库 6 个带该约束的文件
（`internal/oscap/native/native_other.go`、`internal/oscap/probe_other.go`、
`internal/vfs/attr_other.go`、`internal/vfs/sparse_other.go`、`internal/vfs/sys_other.go`、
`internal/oscap/probe_helper_other_test.go`）**从进仓库起一行都没被编译过**。
手工补跑 `GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go vet -tags <全部已注册 tag> ./...` → rc=0，
说明它们**当前**能编译；但 CI 不看，就随时会在无人察觉时坏掉。
修法是往那份平台列表末尾**追加**一项 `freebsd/amd64`（照 §7.1 的共享文件规矩，只追加、不重排）。

**门禁**：C9 由 `scripts/check-constraints.sh` 的 C9 段做机器校验（扫描禁用符号与 import），
配 `test/ci/negative-verify.sh` 做**反向对照**（故意塞一段违规代码，确认门禁真的会红）。
反向对照不是可选项：**一个从来没红过的门禁，和没有门禁是一回事。**

**但门禁覆盖的只是上面那张清单的「可机检子集」，清单本身仍然是完整的约束。**
有些条目落不进正则：FUSE 是 `open("/dev/fuse")` 加 ioctl，loop 设备是 `/dev/loop*` 加
`LOOP_SET_FD`，都没有稳定的符号特征；`smbd` / `avahi-daemon` 这类**守护进程依赖**
归 C3（禁止 fork/exec）管，不在 C9 段重复扫。
**机器扫不到的部分，靠 code review 和 §9 研究准则兜**：评审时问一句「这个能力是我们自己
实现的，还是问 OS 要来的」；拿不准就按 §9 查规范、查真实客户端行为，别猜。
写这一段是因为反过来更危险 —— 读者若默认「凡是写进清单的都被机器兜住了」，
评审时就会放松警惕，而这恰好是本项目栽过的那类坑的完整形态。

---

## 2. 功能目标与阶段划分

### 阶段一：可用的 SMB 文件共享（当前阶段）

- **必须**：SMB 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1 协议协商与文件共享。
  **SMB2 是必须支持的底线**，SMB3 是本阶段目标。
- **尽量**：SMB1 (NT LM 0.12) —— 至少实现 SMB1 多协议协商入口
  （客户端先发 SMB1 `SMB_COM_NEGOTIATE` 带 `"SMB 2.???"` 时能正确升级到 SMB2）。
  完整 SMB1 文件操作为可选加分项，优先级低于 SMB2/3 的正确性。
- 多目录共享：配置文件里可写**多个**共享路径。
- 监听配置：**IP 地址列表**（多个）+ **单个端口**（默认 445）。
- NTLMv2 认证（服务端校验）、SMB 签名、SMB3 加密。
- 进程内 mDNS/DNS-SD 广播，可开关、可配置扩展字段。
- YAML 配置文件。

### 阶段二：Apple 生态扩展与 Time Machine

- `AAPL` create context（server query / resolve id / readdir_attr）。
- Alternate Data Stream（`file:AFP_AfpInfo`、`file:AFP_Resource`、`com.apple.*` xattr）。
- mDNS 广播 `_smb._tcp` / `_device-info._tcp` / `_adisk._tcp`（Time Machine 磁盘宣告）。
- `F_FULLFSYNC` 语义、稀疏文件、卷容量正确上报（`.sparsebundle` band 目录的大目录性能）。
- **验收标准：macOS 能把 Time Machine 备份到本服务并成功恢复。**

> **⚠️ 上面这条验收标准是「阶段二」的长期标准，不是任何单个版本的发布门槛。**
>
> **2026-08-09 项目所有者决定**：Time Machine 真机验收**从 v0.2.0 的强制项降为可选项**，
> 不再是发布阻塞项。原话：「time machine 暂时没有时间真机测试，这个需求先放一放，
> 先做其他的需求，这个需求从 0.2.0 也作为可选项，不强制要求了。」
> 原因是**没有可用的真机环境与时间**（开发容器无 macOS），不是这块做不动。
>
> **降的是验收要求，不是功能**：已实现的 Apple 扩展代码（AAPL create context、ADS、
> `_adisk._tcp` 广播、稀疏文件 FSCTL 等）**全部保留**，单测与协议级用例继续跑，
> 上面四条阶段二目标一条都不删。
>
> **对文档口径的硬性要求**：正因为标准是被主动降下来的，就更不许在文档里把它抬回去。
> `README.md` / `CHANGELOG.md` 里**不得出现**「支持 Time Machine」「TM 验收通过」
> 这类未经真机证实的表述；如实写法是「已实现 Apple 扩展 X/Y/Z，**尚未在真机上验收**」，
> 并按证据强度分档（真机实测 > 第三方客户端协议级实测 > 单测 > 仅代码存在）。

---

## 3. 测试标准（硬性验收门槛）

> **至少使用三种不同的第三方 SMB 客户端工具测试通过。**

> **以下均为「测试对端客户端」——用来连接我们的服务端做验证，不是本项目的运行依赖。**
> 本服务端不依赖其中任何一个即可独立运行：这台机器上一个都没装，
> `stupidsamba` 照样启动、照样服务真实客户端（这正是 C9 要求的，见 §1.2）。
> 这句话放在表格最前面，是因为这张表曾被读成「项目依赖 cifs-utils / 内核 cifs 驱动」。

必测客户端矩阵（至少覆盖 3 种，优先前 4 项）：

| # | 客户端 | 测试方式 |
|---|---|---|
| 1 | `smbclient`（Samba 官方 CLI） | `smbclient //127.0.0.1/share -U user%pass -m SMB3` — ls/get/put/mkdir/rm/rename |
| 2 | `mount.cifs` / `cifs-utils`（Linux 内核客户端）**（可选）** | 真实挂载后跑 POSIX 文件操作与 `fio`/`dd` 吞吐。**在非初始 user namespace 的容器里内核不放行 cifs 挂载**（详见 §10.3 第 2 条），本项直接跳过（`acceptance.sh` 记 skip/rc=77），由 smbclient + impacket + go-smb2 三家满足「至少三种客户端」的门槛 |
| 3 | `pysmb` 或 `impacket`（Python 实现，第三方栈） | 脚本化回归测试，便于 CI |
| 4 | macOS Finder / `mount_smbfs` | Apple 扩展与 Time Machine 验收 |
| 5 | Windows 10/11 资源管理器 | 签名、guest 策略、属性页 |
| 6 | `hirochachacha/go-smb2`（Go 客户端库） | 纯 Go 端到端集成测试，可进 CI |

**要求**：
- 每个协议模块都要有**单元测试**（报文编解码用固定字节向量做 golden test）。
- 关键路径要有**抓包比对**：与真实 Samba 的响应做字段级 diff（可用 tshark 离线 pcap，不要求 CI 装）。
- 集成测试放 `test/integration/`，用 build tag `//go:build integration` 隔离。
- **测试用例中的字节向量优先来自真实抓包或 MS-SMB2 规范示例，不要凭空编造。**

---

## 4. 依赖政策

### 允许直接依赖

- 纯 Go、License 兼容（MIT / BSD / Apache-2.0）、维护活跃。
- **禁止引入 AGPL 依赖**（本项目是网络服务，AGPL 会传染到整个服务）。
  特别注意：`macos-fuse-t/go-smb2` 是 AGPL-3.0，**只可作为设计参考阅读，不得 import、不得复制其代码**。

### 必须自己写 / vendor 的部分（调研已确认无可用现成库）

| 组件 | 结论 |
|---|---|
| SMB2/3 **服务端**协议状态机 | 生态中无可用的宽松许可实现，**自己写** |
| **服务端** NTLMv2 校验 | 所有社区库都是 client 侧，**自己写** |
| SPNEGO token 编解码 | 无现成库，**自己写**（`negHints` 的 `GeneralString` 标准库编不出来，需硬编码 DER 字节） |
| AES-CMAC | 标准库无，**自己写**（RFC 4493，约 100 行） |
| AES-CCM | 标准库只有 GCM，**自己写**（SP800-38C） |
| 可写 VFS 抽象 | `io/fs` 只读，`afero` 是路径语义不匹配 SMB 的句柄语义，**自己定义接口** |

### vendor 规则

第三方代码复制进 `internal/` 时：
1. 保留原始版权头与 License 全文，放在该目录的 `LICENSE` 文件。
2. 在目录内写 `ORIGIN.md`：来源 URL、commit hash、License、**改了什么**。
3. 在根目录 `THIRD_PARTY.md` 登记一行。

---

## 5. 架构准则

### 分层（依赖只能从上往下，禁止反向依赖）

```
cmd/stupidsamba          启动、信号处理、优雅退出
    ↓
internal/config          YAML 配置解析与校验（无业务逻辑）
    ↓
internal/server          连接接入、生命周期编排（Server → Connection → Session → Tree → Open）
    ↓
internal/smb/…           SMB 协议层
      ├── wire/          纯报文编解码（无状态、无 IO、可单测）
      ├── dialect/       方言协商策略
      ├── command/       每个 SMB2 命令一个 handler（策略模式）
      └── crypto/        签名、加密、KDF、preauth hash
    ↓
internal/auth            SPNEGO / NTLM / 账户后端（接口化）
    ↓
internal/vfs             可写虚拟文件系统抽象（接口 + 本地磁盘实现）
    ↓
internal/oscap           OS 能力抽象（port + 双适配器，见 §1.2 C9 / §5 P7；
                         三层都已落地，但**尚未被 vfs/server 调用**，见 §1.2「已建成 ≠ 已生效」）
      ├── native/        借助 OS 能力：xattr / 稀疏文件 / NTFS ADS / 平台 stat 扩展
      └── builtin/       只用「普通文件 + 套接字」自实现同一份语义
    ↓
internal/mdns            进程内 mDNS/DNS-SD responder（与 SMB 层无耦合）
```

### 设计原则

- **P1 报文层与状态层严格分离**：`wire/` 包只做 `[]byte ↔ struct`，不碰任何状态、不做 IO。
  这样每个结构体都能用固定字节向量做 golden test。
- **P2 命令用策略模式分发**：`map[Command]Handler`，一个命令一个文件，
  新增命令不改分发器。未实现的命令统一走默认 handler 返回 `STATUS_NOT_SUPPORTED`。
- **P3 VFS 是接口，不是实现**：SMB 层只依赖 `vfs.FileSystem` 接口，
  本地磁盘、只读、内存实现都是可替换的。**句柄语义**（Open→Handle→操作→Close），不是路径语义。
- **P4 认证后端接口化**：`auth.Provider`，允许 static（配置文件里的用户表）、guest、
  未来的 LDAP/AD 等实现。
- **P5 错误就是 NTSTATUS**：内部错误类型统一能映射到 NTSTATUS，
  在 `internal/smb/status` 集中定义，禁止在 handler 里裸写魔数。
- **P6 不要过早抽象**：只在已经有第二个实现或明确即将有时才抽接口。
- **P7 OS 能力抽象成 port，配 native/builtin 两套 adapter；平台差异仍用 build tag 隔离**：
  SMB 语义需要一批「宿主文件系统不一定表达得了」的元数据 —— POSIX 属主/权限位、
  命名流（ADS）、稀疏区间、稳定 FileID、真实创建时间、DOS 属性。
  对**每一项**能力，`internal/oscap` 定义 port，`native/` 借助 OS 能力实现，
  `builtin/` 用「普通文件 + 一份旁路存储」自己实现同一份语义（详见 §1.2 C9 展开）。
  旁路存储用纯 Go 的嵌入式 KV，**禁止 `mattn/go-sqlite3` 这类需要 CGO 的方案**（C1）。
  平台特有代码依然用 build tag 隔离，**不要让兼容层污染主路径**。

  **逐项选择，不是整体二选一**：同一次运行里命名流可以走 native、创建时间走 builtin。
  `filesystem_mode: portable` 时全部指向 builtin；`native` 时某项不支持就启动报错，
  不静默降级。

  NTFS 原生就支持 alternate data stream、稀疏文件、稳定 FileID、真实创建时间和 DOS 属性，
  这些在 Windows 上**走 native、不需要旁路存储** —— Windows 上真正缺的只有 POSIX
  属主/权限位。这个事实判断依然正确，变的只是它的定位：现在它是**能力矩阵里的几行**，
  而不是「Windows 特例」。

  > **当前缺口（v0.3.0 待补，不要以为已经做完了）**：
  > 2026-08-09 复核仍然成立，只是原因变了一半 —— `internal/oscap/builtin` 现在**有**
  > 完整的六项旁路实现，但**数据路径压根不调用它**（见 §1.2「已建成 ≠ 已生效」的四条判据），
  > 所以对使用者而言缺口一点没变小。
  > `internal/vfs/metadata_other.go:8-10` 在非 Windows 平台仍直接 `return nil, nil` ——
  > 也就是说**非 Windows 上根本没有旁路兜底**。这在旧的「Linux/macOS 原生能力足够」
  > 假设下成立，但在 C9 下不成立：宿主文件系统不支持 xattr 的场景是真实存在的
  > （FAT32/exFAT 外置盘、部分 NAS 导出、`nouser_xattr` 挂载、只读根），
  > 那时这些元数据会**静默丢失**，连报错都没有。
  > 这是 builtin 完整化最大的一个缺口，v0.3.0 的第一优先级。

### 编码规范

- Go 官方 `gofmt` + `go vet`，不引入 lint 之外的花活。
- 常量必须用命名常量并注明规范出处，例如：

  ```go
  // MS-SMB2 §2.2.3 SMB2 NEGOTIATE Request — Dialects
  const (
      SMB202 Dialect = 0x0202
      SMB210 Dialect = 0x0210
  )
  ```
- **所有涉及网络字节的代码必须显式写明字节序**（SMB2 报文体是**小端**，
  Direct TCP 长度前缀是**大端**，DNS/mDNS 是**大端**）。这是最容易出错的地方。
- 二进制解析**必须先校验长度再切片**，禁止裸切片导致 panic。
  外部输入解析失败一律返回错误，**不允许 panic 打崩服务**。
- 每个 handler 入口都要防御路径穿越（`..`、绝对路径、符号链接逃逸）。

---

## 6. 配置文件

YAML，尽量简单，能跑起来只需几行。示例见 `configs/example.yaml`。

要点：
- `listen.addresses` 是**列表**（多个 IP），`listen.port` 是**单个**端口。
- `shares` 是**列表**，每项至少有 `name` + `path`，可选只读、guest 等。
- `mdns.enabled` 开关 + `mdns.apple` 子项控制是否携带 Apple 扩展 TXT 字段。
- 配置校验要在启动时一次性做完并给出**人话**错误信息（指出是哪一行/哪个字段）。

---

## 7. 协作准则（多 agent 并行开发）

### 7.1 并行分工（**强制**，不是可选项）

**任何非平凡的推进都必须组建子 agent 团队并行做，禁止 team-lead 一个人串行写。**
这是项目所有者反复强调过的要求（原话：「现在的工作速度太慢了，你必须使用 3~5 个子 agent，
分工合作形成团队去完成工作」）。串行开发在本项目被明确判定为不可接受。

**团队规模：下限 3 人，上限 10 人**（上限由项目所有者于 v0.2.0 期间从 5 人上调至 10 人）。
超过 5 人时 §7.1 末尾的文件所有权表**必须**先落到文件级再开工，否则人越多互相覆盖越狠。

**必须有一名专职 PM**（角色名 `pm`，项目所有者要求：「添加一个项目团队成员，专职做项目管理，
从而避免项目进度问题」）。PM **不写产品代码**，只做进度跟踪、阻塞识别、决策催办，
维护 `docs/status-<版本>.md` 进度板。设立原因：team-lead 在并行 5+ 人时会被技术评审吃满，
进度问题（谁没开工、谁在等谁、哪个决策卡着）无人盯，实际发生过 5 人零分支而无人察觉。

分工按 §5 的分层切分，模块之间**通过接口契约解耦**，先定接口再并行实现。

初始按分层划分的角色：

| Agent 角色 | 负责范围 |
|---|---|
| `wire` | `internal/smb/wire`、`internal/smb/status` — 报文编解码与 NTSTATUS |
| `auth` | `internal/auth` — SPNEGO / NTLMv2 服务端 / `internal/smb/crypto`（CMAC/CCM/KDF/签名/加密） |
| `vfs` | `internal/vfs` — 可写 VFS 接口与本地磁盘实现、路径安全、属性映射 |
| `server` | `internal/server`、`internal/smb/command` — 连接/会话/树/句柄状态机与命令分发 |
| `mdns` | `internal/mdns`、`internal/config`、`cmd/` — mDNS responder、配置、启动装配 |

随着 `internal/smb/command` 变大，该目录内部要**按文件再切分**，否则三个 agent 会互相覆盖。
实际使用中验证有效的切法（可按当期重点调整，但必须落到**文件级**）：

| Agent | `internal/smb/command` 下拥有的文件 | 另外拥有 |
|---|---|---|
| `server` | `dispatch/context/conn/session/tree/close/open/echo/negotiate/session_setup/tree_connect/smb1/settings/read_write/cancel` | `internal/server`、`internal/smb/dialect` |
| `info` | `query_info/set_info/ioctl/pipe` | `internal/smb/wire/query_info.go` 等对应报文文件 |
| `apple` | `aapl/create/query_directory` | `internal/mdns` 的 Time Machine 广播部分 |
| `qa` | （不改产品代码） | `test/`、`scripts/` |

配套规则：

- **每个 agent 的 prompt 里必须写死「能改哪些文件 / 明确不能改哪些」**。
  5 个 agent 共用同一个工作树 `/workspace`，不做文件级隔离一定会互相破坏。
- 公共文件（如 `internal/smb/wire/const.go`）规定为**只追加、加完立刻单独提交**。
  **两个全队都会碰的 CI 文件同样适用，且要求更严**：
  - `test/ci/check-test-compile.sh` 的 `TAGS=` 行（逗号列表）
  - `.cnb.yml` 的 `&gate_*` 锚点及其在各 stage 列表里的引用

  这两处**只允许在列表末尾追加一项**，**不要顺手重排既有条目、也不要重写周围的注释**。
  理由：追加造成的冲突是**单行、语义无歧义**的，谁都能在一分钟内解掉；
  而重排或重写注释会把同一处变成大段冲突，解的人还得逐行判断哪些是别人的意图。
  实测：`metabolt`（PR #26）与 `qadefect`（PR #29）先后往 `TAGS=` 追加，
  两次冲突都只是一行，合并成 `TAGS=integration,smoke,metabolt,qadefect` 即可。

  **不要因此给这两个文件指定「单一 owner」代为登记。** `TAGS=` 守卫是
  **故意做成自执行**的：谁引入新 tag 却不登记，谁的 PR 当场就红（见该脚本 §0 自检）。
  改成排队等 owner 代登记，等于把「谁引入谁负责」变成「谁引入谁提需求」，
  即时反馈作废，还给全队加了一个串行点。
- 每个 agent 分配**专属调试端口**（例：4451~4455），否则起服务互相抢 445/4445。
- agent 需要改别人的文件时，**发消息给 team-lead 协调**，不要自己动手。
- team-lead 收到 agent 的调研结论后要**自己消化再下发具体规格**（文件路径 + 行号 + 改什么），
  不要把「based on your findings, go fix it」这种话丢回去 —— 那等于没做分工。
- 每个 agent 的 prompt 必须是**自包含**的：它看不到 team-lead 的对话历史，
  项目背景、环境坑（见 §10.3）、提交纪律、文件所有权都要重复写一遍。

### 7.2 **尽快提交，尽快推送**（重要）

> **运行环境不稳定，工作机器随时可能消失。任何未推送的工作都可能永久丢失。**

**所有 agent 必须遵守**：

1. **完成任何一个可编译的最小单元就立刻 `commit`。** 不要攒大提交。
   宁可提交 20 个小 commit，也不要憋一个大 commit 然后丢掉。
2. **commit 之后立刻 `push`。** 不要等"做完再一起推"。
3. 判断标准：**只要 `go build ./...` 能过，就可以提交推送**，
   哪怕功能还没做完（用 `TODO` 标注，或让未实现路径返回 `STATUS_NOT_SUPPORTED`）。
4. 大致每完成一个文件、或每 10~15 分钟有产出，就提交推送一次。
5. 如果发现自己已经改了很多但还没提交 —— **立刻停下来提交**。

推荐提交命令（**必须显式写出分支名**，理由见下）：

```sh
sh test/ci/check-test-compile.sh && git add -A \
  && git commit -m "<模块>: <做了什么>" \
  && git push origin "$(git branch --show-current)"
```

> **⚠️ 血泪教训（R15）：不要用 `go build ./...` 当推送前的自检 —— 它不编译 `_test.go`。**
> 真实事故：某 agent 照着旧版本条做，`CGO_ENABLED=0 go build ./...` 给了绿灯，推送成功，
> 结果远端那个 commit `go vet` 直接失败
> （`create_context_durable_test.go:166:42`：`cannot use intent (*wire.DurableIntent) as *Tree value`）。
> **关键路径上的分支在远端是坏的，而本人以为已经安全推送了** —— 又一例「成功回显 ≠ 事情真的发生」。
> `test/ci/check-test-compile.sh` 用**全部已注册的 build tag** 在**四个平台**上跑 vet，
> 包含 `_test.go`，正好堵住这个洞（实测约 18 秒，值这个钱）。
> 顺带：**新增任何 build tag，必须同步登记进该脚本的 `TAGS=`**，否则带该 tag 的文件
> 没有任何一关会编译它 —— 脚本自己会检查这件事并报错，别把它当成误报绕过去。

> **⚠️ bisect 约定（同源血的延伸）：** R15 那个洞**已经进了 main**，所以它不再只是历史故事 ——
> 它是一颗埋在主干历史里、将来一定会被 `git bisect` 踩到的地雷。
> `git bisect` 走到这类提交时，撞上的是与被查 bug **完全无关**的 `_test.go` 编译错误。
> 而 `git bisect run` 的判据是**退出码**：`0`=good、`1..124`=bad、**`125`=skip**、`126/127/>127`=中止。
> 编译失败时 `go vet` / `go test` 退出 `1`，于是**被判成 `bad`** —— 二分会一路收敛到错误的提交，
> 且全程不报任何异常。**这是「成功回显 ≠ 事情真的发生」在 bisect 上的同型变体。**
>
> 修正：**先编译自检，不过就返回 125 让 bisect 跳过**，别让它参与二分：
>
> ```sh
> git bisect run sh -c 'sh test/ci/check-test-compile.sh || exit 125; <真正的判定命令>'
> ```
>
> **实测证据**（2026-08-09，用临时 worktree 逐个提交跑出来的，不是推断）：
> 洞的范围是主干上**恰好一个提交** `0ad6441`（`server: durable 登记表修复…`）——
> 在它上面 `CGO_ENABLED=0 go build ./...` **通过**、`go vet ./...` **失败**于
> `create_context_durable_test.go:166:42`；紧随其后的 `68b35ad`（`test: 适配 reconnect 新签名`）
> 已修复，之后的 `83ff755` / `4844647` 均正常。四个提交上 `test/ci/check-test-compile.sh` 都**存在**，
> 所以上面这条 `run` 命令在整段区间里都是可用的，不会因为脚本缺失而自己变成 `126/127` 中止。
>
> **顺带一条 SHA 引用纪律**：本条最初引用的是 `8199288`。那个 SHA 与 `0ad6441` 的 `patch-id` 完全相同
> （同一份改动的 rebase 前版本），但它**不被任何 ref 引用**，`git for-each-ref --contains` 返回空 ——
> 也就是说它随时会被 gc 掉，将来读者 `git show 8199288` 只会得到 `unknown revision`。
> **文档里引用提交必须引用主干可达的那个 SHA**，落笔前先 `git merge-base --is-ancestor <sha> origin/main` 验一次。

> **⚠️ 血泪教训：推送的 refspec 必须是自己的分支，不能写死也不能省略。**
> 多个 worktree **共享同一份 `.git`**，所以在自己 worktree 里执行 `git push origin main`
> 推的是**本地 main 分支**（别人的），不是你的工作。它会照常打印成功、
> **不报任何错**，而你的提交一个都没出去，等容器崩了才发现。
> `scripts/save.sh` 曾长期写死 `git push -q origin main`，实际就是这个坑（已修）。
> 自查命令：`git log --oneline origin/$(git branch --show-current)..HEAD`，有输出就是还没推。

### 7.3 独立工作目录 + 独立分支 + PR（**v0.1.0 之后强制**）

> 项目所有者要求（原话）：「从下个版本的开发开始，团队中使用自己独立的分支开发，
> 不要共用一个分支了，走提 PR，合并的流程。」
> 以及：「每个人创建自己的工作目录，隔离工作就好了。目的是为了各自工作互不干扰。」

**适用范围**：v0.1.0 发布之后的所有开发。v0.1.0 的收尾仍沿用共用 `main` 的旧模式。

**三件套缺一不可**：独立工作目录（隔离文件）+ 独立分支（隔离历史）+ PR（把关进主干）。

#### 7.3.1 每个 agent 一个 git worktree（**根治互相干扰**）

开工第一件事，先给自己开一个工作目录，**之后所有操作都在里面做**：

```sh
export PATH=$PATH:/usr/local/go/bin
git -C /workspace worktree add /work/<agent 角色> -b <agent 角色>/<主题> origin/main
cd /work/<agent 角色>
git push -u origin "$(git branch --show-current)"   # ← 不要跳过，理由见下
```

例：`git -C /workspace worktree add /work/auth -b auth/smb30-encryption origin/main`

> **第 4 行「先把空分支推上去」是必做的，不是可选的。**
> `worktree add -b <分支> origin/main` 会把新分支的 **upstream 设成 `origin/main`**——
> 也就是说你开工那一刻，分支的默认推送目标就是**别人的 main**。
> 在这个状态下裸跑 `git push` 会被 `push.default=simple` 拒绝（响雷，不算致命），
> 但它正是 §7.2 里那个「refspec 静默丢提交」事故的土壤。
> 立刻推一次空分支，upstream 当场纠正为你自己的分支，之后 `git push` 才是安全的。
> 附带好处：分支在远端立刻可见，PM 盘点进度时能第一时间看到你已开工。

要点：

- **`/workspace` 是 team-lead 的目录，停在 `main` 上。其他 agent 一律不在 `/workspace` 里改文件。**
- 各 worktree 文件系统上完全独立，**别人的未提交中间态再也砸不到你**
  （§10.3 第 6 条那类"莫名其妙的 `undefined: xxx`"从根上消失）。
- 它们共享同一个 `.git` 对象库，所以 `git fetch` / 分支 / 提交历史都是互通的，不占额外下载。
- 一个分支只能被一个 worktree 检出，**这天然强制了「一人一分支」**。
- **完事之后不要清理，把 worktree 留在原地。** `git worktree remove` / `git worktree prune`
  会命中 CodeBuddy 的 HIGH 风险判定并弹出确认面板，而该面板存在缺陷会卡死主 TUI（见 §7.5）。
  留着的 worktree 除了占点磁盘没有任何害处，悬空记录也无害。确实需要清理时由人类操作者手动执行。
- 起服务调试仍按 §7.1 用**自己的专属端口**，worktree 隔离的是文件，不是端口。

#### 7.3.2 分支与 PR 规则

1. **分支名 `<agent 角色>/<简短主题>`**，例如 `auth/smb30-encryption`、`vfs/named-streams`。
2. **禁止直接向 `main` 推送**。所有改动一律经 PR 合入。
3. §7.2「尽快提交尽快推送」**依然有效，且更重要**——只是推的目标从 `main`
   变成自己的分支。环境随时崩溃，未推送的工作依旧会永久丢失。
4. PR 要小、要能独立编译通过。一个 PR 只做一件事，不要攒。
5. PR 由 team-lead 或另一个 agent 评审后合并；合并前 CI 必须绿。
6. 分支落后时用 `git pull --rebase origin main` 跟进，
   **禁止** `git push --force` 到 `main`；对**自己的**分支可以 `--force-with-lease`
   （注意是 `--force-with-lease` 不是 `--force`）。

**为什么改**：共用一个工作树 + 共用 `main` 已经反复造成两类真实事故——
一是别人未提交的中间态让 `go build` 报出根本不存在的错误（详见 §10.3 第 6/7 条），
二是不合规/半成品代码直接落进 `main`，直到发布前才被发现
（gofmt 门禁在 HEAD 就是红的、`test/` 因 build tag 从未被真正校验）。
**worktree 消灭第一类，PR 拦住第二类。**

#### 7.3.3 冲突避免（worktree 之外仍需遵守）

- **每个 agent 只改自己负责目录下的文件**。worktree 让你不会*物理上*破坏别人，
  但两个人在各自分支里改同一个文件，合并时照样冲突。文件所有权（§7.1 表格）依然有效。
- 需要别人改接口时，**发消息沟通**，不要自己动手改。
- 共享的接口定义文件（如 `internal/vfs/fs.go` 的接口部分）一旦定稿，
  修改前必须先通知所有相关 agent。
- push 前先 `git pull --rebase`；遇到冲突**解决冲突**，
  **禁止** `git push --force`、`git reset --hard` 丢弃别人的提交。

#### 7.3.4 指控别人动了你的工作树之前，**必须先排除你自己**

> **⚠️ 在指控别人动了你的工作树之前，必须先排除你自己。** 本项目已发生 **3 起**误报，
> 全部由举报人自己造成，全部广播给了全队（2026-08-09 的 16:38:48 / 16:38:52 / release 的 `60620c3`）。
> **误报的代价不比真事故小**：一次广播 fan-out 9 人，两次就是 18 条入站消息，全队掉头。
>
> 广播前**必须**跑完这三条，并把结果贴进消息里：
>
> 1. **查自己的工具调用历史** —— 那个「我没写过」的文件，十有八九是你自己 90 秒前写的。
>    尤其当你正在做「先写大文件、再拆成小文件」这类两步改动时（§10.3 第 6 条的单人版本）。
> 2. **查自己的后台任务** —— 长命令会被**自动转入后台**继续改你的树（`Auto-backgrounded
>    after hitting foreground timeout`）。用 `/tasks` 或 TaskOutput 确认没有自己的残留任务在跑。
>    探针/变异类脚本一律用**每轮唯一**的临时文件名，不要复用固定名字。详见 §10.3 第 11 条。
> 3. **查是不是自己 rebase 出来的孤儿 SHA** —— `git reflog` 里找那个「陌生提交」的 patch 等价物。
>    `git branch --contains <sha>` 为空 = 孤儿 = 极可能是你 rebase 前的旧版本，**内容并没有丢**。
>
> 三条都排除干净了，再广播。**证据不足时发给 team-lead 单独核，不要 broadcast。**

**三起误报的实际形态**（都写在这里，是因为它们看上去毫无共同点，实则同一个错误）：

| 举报内容 | 真相 |
|---|---|
| 「我的树里出现了非我所写的 Windows 文件」 | 自己 10 秒前写的。编译错误的行号（`winIDs redeclared`）精确指向他自己刚写的那两个文件 |
| 「有别人的进程在我树里跑 C9 探针」 | 自己被自动后台化的那个 shell 任务（§10.3 第 11 条）。他自己 30 秒前扫进程只扫出**一个** pid |
| 「release 分支里混进了外来提交」 | 自己 `pull --rebase` 前的旧 SHA，内容原封不动地在新 SHA 里 |

**同一个错误是**：把「我不记得做过」当成了「不是我做的」。在并行团队里，**你自己的历史比你的记忆长**。

### 7.4 提交信息规范

```
<模块>: <一句话说明>

模块取值：wire / auth / vfs / server / mdns / config / cmd / test / docs / ci
```

例：`wire: 实现 SMB2 Packet Header 编解码与 golden test`

### 7.5 禁止使用会触发 CodeBuddy 高危确认面板的命令形态

> **实证**：CodeBuddy 会用一组正则把 Bash 命令分档（SAFE/LOW/MEDIUM/HIGH/CRITICAL），
> 命中 HIGH/CRITICAL 会弹出「requires confirmation every time」确认面板。
> 该面板存在缺陷：一旦超时就再也无法关闭，**任何按键都消不掉，主 TUI 就此卡死**，
> 而后台 agent 仍在运行 —— 表现为「界面死了但活还在干」，极难判断。
> 判定规则的实测复现器见 `scripts/diag/risk-replica.js`，详情见 `docs/troubleshooting-codebuddy.md`。

**以下命令形态一律禁止在 agent 工作流中使用**（多段命令按 `;`/`&&`/`|` 拆开逐段判定并取最高档，
所以把它藏在一长串命令的末尾同样会触发）：

| 禁用 | 档位 | 替代做法 |
|---|---|---|
| `git worktree remove` / `git worktree prune` | HIGH | **不清理**，留在原地（§7.3.1） |
| `rm -r` / `rm -rf` / `rm *` | HIGH | 留着不删；确需删除交由人类操作者 |
| `git restore <file>` | HIGH | `git checkout HEAD -- <file>`（实测 SAFE） |
| `git checkout -- <path>` | HIGH | 同上 |
| `git branch -D` / `git rm -r` | HIGH | `git branch -d`；或留着不删 |
| `git push -f` / `--force` / `--delete` | HIGH | `--force-with-lease`（实测 SAFE，且 §7.3.2 本就要求用它） |
| `git reset --hard` / `git clean -fd` | CRITICAL | 用临时 worktree 取干净基线（§10.3 第 7 条） |
| `sudo` / `chmod 777` / `find -delete` / `find -exec rm` / `\| xargs rm` | HIGH | 视情况改写；一般本项目用不到 |

**验证代码时不要靠"改一下再改回来"**（那需要 `git restore`）。
用 `go test -overlay=<json>` 注入变异体，工作树全程零修改 —— 这也是本项目做变异测试的标准做法。

### 7.6 每个命令前先 `date` 看时间（环境不稳，时间戳是证据）

> **项目所有者硬性要求（2026-08-09 口述）**：开发环境会不定期崩溃并清空 git 仓库以外的一切。
> 崩溃后复盘时，最缺的就是「这件事到底发生在崩溃前还是崩溃后、隔了多久」。
> 时间戳是唯一的客观证据，所以**每一条 Bash 命令都必须以 `date` 开头**。

**规则**：

- 每个 agent 在 Bash 工具里执行的**每一条命令**，都必须先 `date` 再干活，例如：
  ```sh
  date; cd /work/<role> && CGO_ENABLED=0 go build ./...
  ```
- 多段命令用 `;` / `&&` 串联时，`date` 放在**最前面**即可（一次时间戳覆盖整条命令链）。
- 目的不是给人看，而是给崩溃后的复盘当时间锚点。不要嫌啰嗦——它只往 stdout 多打一行当前时间，
  不占用任何额外资源、不触发任何风险档位。
- 提交信息、PR 描述里也尽量带上关键动作的发生时间（用 `date` 的输出），方便 PM 盘点时序。

**为什么**：环境崩溃后，`~/.codebuddy` 的会话历史、记忆工作副本、工具缓存全部丢失，
只剩 git 仓库里的时间戳（commit time / push time）。命令前打 `date` 等于把「崩溃前最后在做什么」
这件事实时写进工具输出日志，崩溃后 `history/` 一旦入库就是这个证据。没有它，复盘只能靠猜。

## 8. 安全准则

- 路径穿越：所有客户端传入的路径必须经过统一的 `vfs` 层规范化与根目录约束校验，
  `..`、绝对路径、符号链接逃逸一律拒绝（`STATUS_OBJECT_PATH_INVALID` / `STATUS_ACCESS_DENIED`）。
- 长度校验：任何来自网络的 offset/length 字段在使用前必须校验边界，防止越界读与整数溢出。
- 资源限制：单帧大小、并发连接数、每连接打开句柄数、目录枚举缓冲都要有上限。
- 认证：密码不落日志。NTLM 比较用**常量时间比较**（`crypto/subtle`）。
- 默认不开 guest；开启 guest 必须在日志里明确警告。
- 不要为了让某个客户端连上就关闭签名校验 —— 先查原因。

---

## 9. 研究准则

> 网络协议是极其复杂且精确的事情。**先查清楚，再动手。**

- 权威来源优先级：
  1. Microsoft Open Specifications（MS-SMB2 / MS-FSCC / MS-NLMP / MS-DTYP / MS-ERREF）
  2. RFC（1001/1002 NetBIOS、4178 SPNEGO、4493 CMAC、6762 mDNS、6763 DNS-SD）
  3. Samba 源码与文档（尤其 `vfs_fruit` 之于 Apple 扩展）
  4. 真实抓包
- **不确定的字段值不要猜**。查不到就抓包验证，或在代码注释里明确标注 `// TODO: 待验证`。
- 写死的常量必须在注释里注明规范章节号。
- 发现规范与真实客户端行为不一致时，**以真实客户端行为为准**，并在注释里记录这个差异和原因。

---

## 10. 会话历史、记忆与开发环境（**血泪教训，必读**）

> **本项目的开发容器会不定期崩溃并重启，清空 git 仓库以外的一切。**
> 已经真实发生过多次：Go 工具链消失、python3/smbclient 消失、agent 记忆被清空、
> 整个 CodeBuddy 会话历史丢失。**凡是不在 git 里的东西都不可信。**

**⚠️ 订正（2026-08-09 实证）：上面那句是「最坏情况」，不是「每次都这样」。
把两种故障分清楚，直接决定崩溃后你该做什么。**

| | 容器重启 | **单个 node 进程死亡（今日 6 次全是这一种）** |
|---|---|---|
| PID 1 | 换新 | **连续存活**（当日实测 8h59m 未变） |
| `/work` 下的 worktree | 没了 | **全在** |
| git 对象库 | 没了 | **全在** |
| `~/.codebuddy` 会话 / subagent jsonl / 记忆工作副本 | 没了 | **全在** |
| 丢的是什么 | 一切 | **只有内存态**：各 agent 的活上下文，以及约 98 秒未落盘的日志缓冲 |
| 正确的恢复动作 | `restore-history.sh` + `--resume` | **`--resume=<uuid>` + 各人回自己的 worktree** |

**队友不是独立进程，是同一个 node 进程内的任务，所以宿主一死 10 个人同时归零** ——
这会让人误以为「整个环境没了」。**判定「丢了什么」之前先跑 `uptime` 和 `ls /work`，不要默认清空。**

**所以：进程死亡后不要重建团队。** 重建会白白丢掉全部上下文（本次丢了 10 个人的），
而磁盘上的成果一件都没少。**「凡是不在 git 里的都不可信」仍然成立**（它讲的是最坏情况下的下限），
但它不是「盘点前不必核实磁盘」的理由。

### 10.1 CodeBuddy 会话历史存在哪、怎么救回来

**存放位置**（全部在仓库外，重启即失）：

| 路径 | 内容 |
|---|---|
| `~/.codebuddy/projects/workspace/<uuid>.jsonl` | 会话主线完整记录（每行一条消息，含 reasoning 与工具调用） |
| `~/.codebuddy/projects/workspace/<uuid>/subagents/agent-*.jsonl` | 每个子 agent 的完整分支记录 |
| `~/.codebuddy/projects/workspace/<uuid>/tool-results/` | 大块工具输出缓存（可从 jsonl 推出，不必备份） |
| `~/.codebuddy/projects/workspace/memory/`、`MEMORY.md` | agent 记忆的**工作副本**（权威副本在仓库 `memory/`，见 §10.2） |
| `~/.codebuddy/history.jsonl` | 跨项目的历史 prompt 列表（只是输入框历史，不含回答） |

**入库**：`scripts/save-history.sh` —— 把上面的 jsonl 快照进仓库 `history/`，
自动生成 `history/INDEX.md`（列出每个会话的行数、大小、时间、首条用户消息），并提交推送。
**team-lead 应当在每个里程碑、以及任何一次长时间工作之后主动跑一次。**

**恢复**（重启后）：

```sh
scripts/restore-history.sh          # 看仓库里存了哪些会话
scripts/restore-history.sh 3dfd1b74 # 把该会话（支持 uuid 前缀）放回 ~/.codebuddy
codebuddy --resume=3dfd1b74-3275-4b3f-9728-7234708c415e
```

**关键事实（已实测验证）**：

- `codebuddy --resume=<uuid>` 是**可用**的，前提是那个 uuid 的 jsonl 在
  `~/.codebuddy/projects/workspace/` 下。恢复只是把历史上下文读回来，
  **不会重跑任何工作**，代价远低于从头再来。
- **`codebuddy -c` / `--continue` 是陷阱**：它接的是「最近一次」会话。崩溃后你新开的
  那个空会话就是最近的，于是恢复出一个空壳 —— 这就是「resume 不管用」的真相，
  不是 resume 坏了，是接错了会话。**永远用 `--resume=<uuid>` 指名道姓。**
- uuid 从 `history/INDEX.md` 里挑：**挑行数最多的那个，不是时间最新的那个**。
- **`--resume` 不检查这个 session 是否已经被一个活着的进程挂着。**
  **resume 之前先跑一次自查**：

  ```sh
  grep -h sessionId /root/.codebuddy/sessions/*.json | sort | uniq -c
  ```

  **同一个 sessionId 计数 ≥ 2 = 有另一个进程还挂在上面。**
  先确认那个旧进程确实是死的或空转的，再 resume；拿不准就问项目所有者，
  **不要擅自 kill 别人的进程**。

  这是一条**已观测到的危险，不是已证实的事故成因**——措辞请照抄，不要写成
  「会导致冲突」。实测数据：2026-08-09 15:28 与 15:29:39 起的两个进程确实同挂
  session `2d8786c5`，但按 pid 统计工具类日志行是 **24 行 vs 6627 行**，
  旧进程**全程空转**（日志构成全是 `[Startup]`/`[MCP]`/`[PluginManager]` 启动期噪声，
  没有一条任务执行流）。

  **论证的要害是这条不变量**：把 `agent-*.jsonl` 按角色名归并后，
  **事故窗口内每个角色恰好 1 个实例**，没有出现过第二套同名 agent。
  也就是说：双挂载**发生了**、浪费了内存、共写 `file-history` 撞出 158 处 ENOENT，
  但**没有**造成任何跨 agent 写入冲突。**记录危险，不要给它安一个未经证实的因果。**

  > **佐证用的文件总数是快照，会随会话继续增长，不要把它当常量引用。**
  > 该 session 下的 `agent-*.jsonl` 个数：**2026-08-09 18:25 实测 75 个**
  > （全 workspace 80 个，另一 session `3dfd1b74` 占 5 个）。
  > 同一个数在 18:07 数还是 72 / 全量 77，**18 分钟涨了 3** —— 本条初稿写的「73」
  > 和复核时说的「68」之所以对不上，就是各自数在了不同时刻，谁都没错。
  > 复核请重新数，不要沿用文中的数：
  >
  > ```sh
  > ls /root/.codebuddy/projects/workspace/<uuid>/subagents/agent-*.jsonl | wc -l
  > ```
  >
  > **⚠️ 重数那条不变量时必须按时间窗口过滤，否则会得到假阳性。**
  > 崩溃重启后同名角色会被**重新拉起**，于是同一角色在整个 session 里合法地存在
  > 第二个 jsonl。实测（18:25）：按角色名归并，`oscap-gate` 全局计数为 **2**，
  > 但两个文件的 mtime 分别是 **15:27** 与 **17:55** —— 后者是两个多小时后的重启实例，
  > 根本不在事故窗口内。**只看全局计数会误判成「不变量已被推翻」。**
  > 正确做法是先按窗口筛文件再归并：
  >
  > ```sh
  > find <subagents 目录> -name 'agent-*.jsonl' \
  >      -newermt "2026-08-09 15:20" ! -newermt "2026-08-09 15:40"   # 窗口内 9 个
  > ```
  >
  > 另注：角色名不在文件名里（文件名是随机 hash），要从首行 prompt 文本里抠，
  > 且**并非所有 agent 的 prompt 都用同一句式**——18:25 实测 75 个文件里有 59 个
  > 匹配不上 `你是 stupidSamba 项目的 \`<角色>\` agent` 这个句式。
  > 也就是说这条不变量**只在能识别出角色名的那部分文件上被验证过**，
  > 不要把它当成对全部 agent 的普遍断言。
- 子 agent 的 jsonl 单独存在 `<uuid>/subagents/` 下。团队并行时这里往往比主线还大
  （实测主线 1.8 MB、5 个子 agent 合计 1.9 MB），排查「某个 agent 当时到底做了什么」
  只能看这些文件，务必一起备份。

### 10.2 记忆必须入库

agent 记忆的**权威副本是仓库里的 `memory/`**，`~/.codebuddy/.../memory` 只是当次会话的
工作副本，重启即失。任何记忆的新增/修改都要写进 `/workspace/memory/` 并 git 提交。

### 10.3 环境固有限制与避坑清单

1. **Go 不在 PATH**：每个新 shell 都要 `export PATH=$PATH:/usr/local/go/bin`。
   真没了就重装 `go1.25.0.linux-amd64.tar.gz` 到 `/usr/local/go`。
2. **`mount.cifs` 在本容器永远跑不通。**

   > **先划清边界（这段别删）**：本条只影响**测试手段**。
   > **产品自身不依赖任何 mount / namespace / 内核文件系统驱动机制** ——
   > 见 §1 C9 与 §1.2，由 `scripts/check-constraints.sh` 的 C9 段**机器校验**。
   > 下面讲的全是「我们拿什么工具去连它」，不是「我们的服务需要什么」。
   > 之所以要专门写这一句：本条原文曾被读成「这个项目依赖 linux 的 cifs 和 namespace」，
   > 直接触发了 C9 这条约束的确立。

   **注意：真死因不是缺 `CAP_SYS_ADMIN`**，
   本条曾长期归因错误，2026-08-09 由 r-infra 用决定性实验推翻，现修正如下。

   实验：`docker run --cap-add SYS_ADMIN` 起 Debian 12，装 cifs-utils，
   `docker cp` 二进制进去，服务端客户端同容器走 127.0.0.1 —— **仍然**
   `mount error(1): Operation not permitted`。继续排除：
   - `/proc/filesystems` 里**有** `cifs` 和 `smb3` → 不是缺内核模块；
   - `mount -t tmpfs` **成功** → mount() 系统调用本身通的（这是反向对照，
     排除了「seccomp 全局禁 mount」这个假设）；
   - `cat /proc/self/uid_map` = `0 1000 1` → **我们在一个非初始 user namespace 里**。

   结论：非初始 user namespace 内，内核只放行带 `FS_USERNS_MOUNT` 标志的文件系统
   （tmpfs / proc / sysfs / devpts / fuse…），**cifs 没有这个标志**，所以返回 EPERM，
   **与持不持有 `CAP_SYS_ADMIN` 无关**。`--privileged`、任何 `--cap-add` 都救不了。
   FUSE 逃生通道也堵死：`fuse` 已注册但 `/dev/fuse` 设备节点不存在，
   `mknod` 被 device cgroup 拦。

   实践含义不变：这是环境限制不是服务端 bug，`scripts/acceptance.sh` 已把它做成
   skip(rc=77)，不要因为它 fail 就判定验收不通过。**但不要再浪费时间去加 capability
   或换 docker 参数** —— 那条路在数学上就是死的。§3 要求的「至少三种第三方客户端通过」
   由 **smbclient + impacket + go-smb2** 三家满足（第四家见第 10 条 smbtorture）。
3. **impacket 用 apt 装，不要用 pip**：`apt-get install python3-impacket`
   （pip 装会和已有的 cryptography 版本冲突）。
4. **致命坑一：`pkill -f <路径>` 会自杀**。该模式会匹配到执行它的 shell 自己的命令行，
   把父 shell 一起杀掉，表现为「命令无输出 / 被 SIGTERM / 服务起不来」，极难排查。
   一律用 `pkill -x stupidsamba`。起后台服务用
   `setsid nohup ... > log 2>&1 < /dev/null & disown`。
   **但 `pkill -x` 也有坑**：进程名超过 15 字符时内核 `comm` 字段被截断，
   pkill 报 `pattern that searches for process name longer than 15 characters
   will result in zero matches` 然后**静默匹配不到** —— 旧进程还活着占着端口，
   新进程绑不上，表现和「服务起不来」一模一样。**调试二进制名必须 ≤ 15 字符**
   （`stupidsamba4462` 正好 15，再长就废了）。
5. **`setsid nohup ... &` 之后 `$!` 不是监听进程**：`$!` 是 setsid 包装进程的 PID，
   真正 listen 的是它的子进程，`kill $!` 杀不掉。找真实 PID 用 **`fuser <port>/tcp`**
   （本容器 `ss -ltnp` 拿不到 pid 列）。
6. **多 agent 共用工作树时，未提交的中间态会砸到别人**。真实发生过：某 agent 分两步做
   重命名（先改声明、再改引用），中间约 1 分钟窗口里工作树是 `undefined: xxx`，
   另一个 agent 正好在那时 `go build ./...` 撞上，排查了一个根本不存在的 bug。
   **跨文件改动必须一次原子改完再落盘**；报编译错误前先重跑一次确认不是瞬时态。
   **根治办法是 §7.3.1 的「每人一个 git worktree」**——只要还共用一个工作目录，
   光靠分支是没用的（分支隔离提交历史，不隔离文件）。
7. **临时被别人的中间态挡住时，用临时 worktree 从已推送的 HEAD 构建，不要干等**：

   ```sh
   git worktree add /tmp/clean-$$ origin/main && cd /tmp/clean-$$
   ```

   这样拿到一个干净且可编译的基线继续干活，既不用等别人落盘，也不用去动别人的文件。
   实测有效（tmverify 在 uid/gid 删除窗口期就是这么绕过去的）。**用完留在原地不要删**（见 §7.5）。
   注意这只是**应急**手段；常态应当按 §7.3.1 一开始就待在自己的 worktree 里。
8. **致命坑二：smbclient 4.22 的 `-c` 不按换行分割命令**。多条命令必须用**分号**分隔。
   写成多行会产生 `NT_STATUS_NO_SUCH_FILE listing \get` 这种**假故障**，
   看起来像服务端 bug，其实是测试脚本的问题。
9. **worktree 里 `.git` 是文件不是目录**，任何 `$REPO/.git/xxx` 的写法都会静默失效。
   真实事故：`scripts/save.sh` 用 `mkdir "$REPO/.git/xxx.lock"` 做互斥锁，在 worktree 下
   `mkdir` 必然 `ENOTDIR`，而代码不区分失败原因，当成「锁被别人占着」空转 **120 秒**后
   报「等待推送锁超时」退出 —— **提交已落地、推送从未发生**。也就是说在 §7.3 强制的
   worktree 工作流下，那个脚本 100% 推不出去。同理 `[ -d "$REPO/.git/rebase-merge" ]` 恒为
   false，冲突检测形同虚设。
   **一律用 `git rev-parse --git-common-dir`（跨 worktree 共享的真 .git）与
   `git rev-parse --git-path <名字>`（当前 worktree 的私有路径），不要自己拼 `.git/`。**
10. **CI 的红绿要看事件类型**：`push` 事件与 `pull_request` 事件跑的是不同版本的 `.cnb.yml`，
    同一个 commit 可以 push 红、PR 绿。已实测：`tm-handle/durable` 与 `r-infra/test-infra`
    开 PR 前只有 push 记录且全红，开 PR 后 PR 事件立刻 success。
    **合并前要看的是 PR 事件的构建**，不要被 push 的「假红」吓住。
    查询用 `scripts/ci-status.sh <分支名>`（退出码 0=绿 1=红 2=跑着 3=无记录 4=调用失败）。
11. **自动后台化会让你误以为别人在动你的树。**

    **长命令超时后不会被杀，而是转入后台继续跑**（工具会回一句
    `Auto-backgrounded after hitting foreground timeout. The command is still running —
    no SIGTERM was sent.`，很容易被当成普通的超时提示读过去）。
    你若在前台又开了第二轮，两轮会在同一棵树上用同一个文件名互相踩，
    **现象与「另一个 agent 在写我的树」完全一致**。

    真实事故：win-meta 的 C9 探针（2026-08-09 16:33–16:38）。他自己的后台任务
    `0OGWya` 一直在循环改写 `zz_c9_probe_scratch.go`，前台第二轮读到的内容不断变化，
    据此**向全队广播了一起不存在的冲突**。讽刺的是他在广播前 30 秒（16:37:50）
    自己扫过一遍进程，结果**只有一个 pid=2948668**，输出里那段 `probe()` 函数体
    **正是他自己的脚本** —— 证据当时就在手里，只是没被当成证据。

    **判据**：扫一遍 `cwd` 落在你树里的进程，**把命令行看完** —— 那多半就是你自己的脚本；
    再用 `/tasks` / TaskOutput 确认没有自己的后台任务在跑。
    **预防**：探针、变异体、临时校验脚本一律用**每轮唯一**的文件名
    （`zz_probe_$$_$RANDOM.go`），不要复用固定名字 —— 固定名字是这类自伤的必要条件。
12. **CodeBuddy 的高危命令授权面板：补丁已在仓库里，容器重建会自动重打，不要再手工改包。**
    症状是即便权限模式已是 `bypassPermissions`，`rm -r` / `git restore` /
    `git worktree remove` 这类命令仍会弹「requires confirmation every time」，
    而那个面板一旦孤儿化就**任何按键都关不掉**（实测卡死主 TUI 56 分钟，只能重启进程）。
    根治脚本是 `scripts/env/patch-codebuddy.sh`（**已在 `main`**）。
    设计上它由 `.cnb.yml` 在 `vscode` 事件自动执行，但**那处挂接截至 2026-08-09 19:03 CST
    还没进 `main`**（在 PR #152 / ci-trigger 分支上；判据：`grep -c patch-codebuddy .cnb.yml`
    → **0**）。所以**现在开工第一件事仍是自己手工跑一次**：
    `sh scripts/env/patch-codebuddy.sh`。等上面那条 grep 变成非 0，本句即可删掉。
    **怀疑没生效时不要看构建日志的绿灯** —— CodeBuddy 实测是在容器启动约 2 分钟**之后**
    才装上的，stage 跑早了会「优雅跳过退出 0」，日志全绿而补丁没打上。要判断就跑：

    ```sh
    sh scripts/env/patch-codebuddy.sh --check; echo "rc=$?"   # 0=已就位 3=没打 4=没装
    ```

    没打就直接再跑一次（幂等，随便跑）。CodeBuddy 升级后正则可能失配 ——
    脚本会**报错退出而不是静默跳过**，重新定位手册见
    `docs/troubleshooting-codebuddy.md` §6.4。
    ⚠️ 补丁拆掉的只是「问一句」，**§7.5 那张禁用命令表依然全部有效**：
    以前是面板挡着不让你误删，现在只剩纪律挡着，所以纪律反而更要紧。

    > **备注（docs-honesty 于 2026-08-09 18:48 追加、19:03 复核时记录）**：本条原本被要求写作
    > 「已挂进 `.cnb.yml` 的 `vscode` 事件」，但落笔与复核时该 YAML 改动（ci-trigger 分支）
    > **都还没合入 `main`**（`grep -c patch-codebuddy .cnb.yml` → 0；`main` 的 `.cnb.yml:19`
    > 确实有 `vscode:` 段，但里面只有一句 `echo "云原生开发环境已就绪"`——
    > **「有 vscode 事件」不等于「补丁挂上去了」，别只看事件名就下结论**）。
    > 故按本仓库「按 `main` 实际内容写、不预写未来态」的规矩改为上面的措辞，
    > 待 ci-trigger 合入后由后续 PR 收敛成完成态。这本身就是本轮诚实性审计要立的规矩，
    > 不在此条上破例。另：`setsid nohup ... &` 的后台进程能否活过 stage 结束，env-patch **未实测**，
    > 本条未就此做出「已验证」表述。
