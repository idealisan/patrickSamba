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

---

## 3. 测试标准（硬性验收门槛）

> **至少使用三种不同的第三方 SMB 客户端工具测试通过。**

必测客户端矩阵（至少覆盖 3 种，优先前 4 项）：

| # | 客户端 | 测试方式 |
|---|---|---|
| 1 | `smbclient`（Samba 官方 CLI） | `smbclient //127.0.0.1/share -U user%pass -m SMB3` — ls/get/put/mkdir/rm/rename |
| 2 | `mount.cifs` / `cifs-utils`（Linux 内核客户端） | 真实挂载后跑 POSIX 文件操作与 `fio`/`dd` 吞吐 |
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
- **P7 平台差异用 build tag 隔离，不要让兼容层污染主路径**：
  Windows 上要能作为 Time Machine 的存储后端，但 Windows 没有 POSIX 的 uid/gid/mode。
  这类"宿主文件系统表达不了的元数据"通过一个 `MetadataStore` 旁路存储解决
  （纯 Go 的嵌入式 KV，**禁止 `mattn/go-sqlite3` 这类需要 CGO 的方案**）。
  **该兼容层只在 Windows 编译进来**；Linux/macOS 原生能力足够，使用 noop 实现，零开销。
  注意 NTFS 原生就支持 alternate data stream、稀疏文件、稳定 FileID、真实创建时间和
  DOS 属性，这些**不需要**旁路存储 —— 只有 POSIX 属主/权限位才需要。

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
CGO_ENABLED=0 go build ./... && git add -A \
  && git commit -m "<模块>: <做了什么>" \
  && git push origin "$(git branch --show-current)"
```

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
```

例：`git -C /workspace worktree add /work/auth -b auth/smb30-encryption origin/main`

要点：

- **`/workspace` 是 team-lead 的目录，停在 `main` 上。其他 agent 一律不在 `/workspace` 里改文件。**
- 各 worktree 文件系统上完全独立，**别人的未提交中间态再也砸不到你**
  （§10.3 第 6 条那类"莫名其妙的 `undefined: xxx`"从根上消失）。
- 它们共享同一个 `.git` 对象库，所以 `git fetch` / 分支 / 提交历史都是互通的，不占额外下载。
- 一个分支只能被一个 worktree 检出，**这天然强制了「一人一分支」**。
- 完事清理：`git -C /workspace worktree remove /work/<agent 角色>`；
  强行删目录会留下悬空记录，用 `git worktree prune` 收拾。
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

### 7.4 提交信息规范

```
<模块>: <一句话说明>

模块取值：wire / auth / vfs / server / mdns / config / cmd / test / docs / ci
```

例：`wire: 实现 SMB2 Packet Header 编解码与 golden test`

---

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
- 子 agent 的 jsonl 单独存在 `<uuid>/subagents/` 下。团队并行时这里往往比主线还大
  （实测主线 1.8 MB、5 个子 agent 合计 1.9 MB），排查「某个 agent 当时到底做了什么」
  只能看这些文件，务必一起备份。

### 10.2 记忆必须入库

agent 记忆的**权威副本是仓库里的 `memory/`**，`~/.codebuddy/.../memory` 只是当次会话的
工作副本，重启即失。任何记忆的新增/修改都要写进 `/workspace/memory/` 并 git 提交。

### 10.3 环境固有限制与避坑清单

1. **Go 不在 PATH**：每个新 shell 都要 `export PATH=$PATH:/usr/local/go/bin`。
   真没了就重装 `go1.25.0.linux-amd64.tar.gz` 到 `/usr/local/go`。
2. **`mount.cifs` 在本容器永远跑不通** —— **注意：真死因不是缺 `CAP_SYS_ADMIN`**，
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
   实测有效（tmverify 在 uid/gid 删除窗口期就是这么绕过去的）。用完 `git worktree remove`。
   注意这只是**应急**手段；常态应当按 §7.3.1 一开始就待在自己的 worktree 里。
8. **致命坑二：smbclient 4.22 的 `-c` 不按换行分割命令**。多条命令必须用**分号**分隔。
   写成多行会产生 `NT_STATUS_NO_SUCH_FILE listing \get` 这种**假故障**，
   看起来像服务端 bug，其实是测试脚本的问题。
