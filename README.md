# stupidSamba

一个**用纯粹 Go 实现的、极简易用的 SMB/CIFS 文件共享服务器**。
自带进程内 mDNS/DNS-SD 广播，支持 macOS、Windows、Linux 等各类 SMB 客户端。

> 本项目当前阶段目标（阶段一）是提供一个**可用的 SMB 文件共享服务**。
> Time Machine 备份是下一阶段（阶段二）的目标，当前已具备大部分前置能力，但端到端
> 「备份并成功恢复」的验收仍在进行，详见下方「[Time Machine 状态](#timemachine)」。

---

## 它是什么，为什么用得上

把一台机器（树莓派、旧电脑、NAS、容器）上的目录，变成局域网里所有人都能用
**Finder / 文件资源管理器 / `smbclient`** 直接打开的共享盘 —— 不需要装 Samba，
不需要 systemd 服务，不需要改宿主系统的用户。

它和 Samba 最大的区别是**极简与零外部依赖**：

| 传统 Samba | stupidSamba |
|---|---|
| 需要 `smbd` / `nmbd` 守护进程 | **单个静态二进制**，启动即可用 |
| 依赖系统用户（`/etc/passwd`、`pam`、`winbind`） | **账户完全自己管**，与宿主系统用户零关系 |
| 依赖 `avahi-daemon` 做服务发现 | **进程内 mDNS**，不依赖任何外部服务 |
| 动态链接，可能依赖系统库 | `CGO_ENABLED=0` 编译，**静态链接、单文件分发** |

换句话说：下载一个二进制、写一个配置文件、运行，就完事了。

### 硬性设计约束（也是它的承诺）

- **禁止 CGO、禁止外部动态库**：产物是单个静态链接的二进制，`ldd` 结果为
  「不是动态可执行文件」。
- **禁止外部进程**：不 fork/exec 任何 `smbd` / `avahi-daemon` / `mount` 等命令，
  运行时不需要系统预装任何服务。
- **协议栈全在仓库内**：SMB2/3、NTLMv2、SPNEGO、mDNS/DNS-SD 全部自实现。
- **认证自成体系**：用户名与口令只来自本软件的配置文件，**不读宿主系统的用户
  管理**（不读 `/etc/passwd`、不走 PAM/NSS、不要求宿主上真有这些用户）。详见
  「[已知限制与说明](#notes)」。

---

## 快速开始

### 1. 准备配置文件

最小可用配置只需要 `shares` 一段：

```yaml
server:
  name: STUPIDSAMBA
auth:
  users:
    - name: alice
      password: "changeme"        # 或改用 nt_hash（见下文）
shares:
  - name: public
    path: /srv/share/public       # 必须是已存在的绝对路径目录
    comment: "公共读写目录"
```

把上面的内容存成 `stupidsamba.yaml`。**共享目录必须事先存在**，否则启动校验会报错。

### 2. 启动

```sh
# 若 445 是特权端口且你不是 root，见下方「已知限制」
stupidsamba -config stupidsamba.yaml
```

启动后日志会打印监听地址与协商到的方言范围，例如：

```
SMB 服务已监听 addrs=127.0.0.1:445 dialects=2.0.2..3.1.1 shares="public"
```

想先只校验配置、不真正起服务，用 `-check`：

```sh
stupidsamba -config stupidsamba.yaml -check
# 配置校验通过：stupidsamba.yaml（1 个共享）
```

### 3. 连上去

**Linux（`smbclient`）**：

```sh
# 列出共享
smbclient -L //127.0.0.1 -p 445 -U alice%changeme

# 进入某个共享做文件操作
smbclient //127.0.0.1/public -p 445 -U alice%changeme -m SMB3
smb: \> ls
smb: \> put localfile.txt
smb: \> get remotefile.txt
smb: \> mkdir subdir
smb: \> rm remotefile.txt
smb: \> rmdir subdir
```

> 注意：`//主机/共享` 与端口要分开写 —— 主机部分只填 IP 或主机名，
> 端口用 `-p` 指定（如 `-p 445`）。写成 `//127.0.0.1:445/share` 会被当成
> NetBIOS 名字而解析失败。

**关于 `mount.cifs`（Linux 内核客户端）**：本项目的开发 / CI 容器缺少
`CAP_SYS_ADMIN`，因此在该容器内执行 `mount -t cifs` 会报
`Unable to apply new capability set` 而失败。这是**环境限制，不是服务端不支持
Linux 内核客户端**——在具备该能力的普通 Linux 主机上，
`mount -t cifs //host/share /mnt -o user=alice,pass=...` 可以正常挂载。
（本项目的客户端验收矩阵用 `smbclient` + `impacket` + 纯 Go `go-smb2` 三家覆盖，
见下方「[客户端测试矩阵](#matrix)」。）

**macOS（Finder）**：
`前往 ▸ 连接服务器`，输入 `smb://127.0.0.1`（默认端口 445 可省略），
输入用户名 `alice` 与口令即可。若改了端口：`smb://127.0.0.1:445`。

**Windows（资源管理器）**：
地址栏输入 `\\127.0.0.1\public`（端口非 445 时：`\\127.0.0.1@445\public`），
在弹出的凭据框里输入 `alice` 与口令。

---

## 安装

### 方式一：下载预编译二进制（推荐）

v0.1.0 发布在 CNB 仓库的 Release 页面（标记为 **prerelease**）：

- **Release 页（推荐，普通用户点这里下载）**：
  `https://cnb.cool/finalappstore/stupidSamba/-/releases/v0.1.0`
  页面里的「下载」按钮由 web 会话处理跳转，能正常拿到文件。
- **原始文件直链（给脚本 / CI 用）**：
  `https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.1.0/stupidsamba_v0.1.0_<os>_<arch>.tar.gz`
  （Windows 用 `.zip`；`SHA256SUMS` 在同目录 `.../download/v0.1.0/SHA256SUMS`）。
  注意：该直链需在请求里带 `Authorization: Bearer <token>` 且跟随重定向（`-L`），
  最终从公开 CDN `asset.cnb.cool` 取字节；浏览器在 Release 页点按不受此限。
  ⚠️ 不要把 `cnb.cool` 这个 host 的 `/-/releases/download/...` 路径当可直接
  `curl` 的链接——它不下发文件，需用上面的 `api.cnb.cool` 形态。

每个压缩包内含二进制、本 `README.md`、`CHANGELOG.md` 与 `configs/example.yaml`：

| 平台 | 文件 |
|---|---|
| Linux x86-64 | `stupidsamba_v0.1.0_linux_amd64.tar.gz` |
| Linux ARM64（树莓派 4 等） | `stupidsamba_v0.1.0_linux_arm64.tar.gz` |
| macOS Apple Silicon | `stupidsamba_v0.1.0_darwin_arm64.tar.gz` |
| Windows x86-64 | `stupidsamba_v0.1.0_windows_amd64.zip` |

下载、校验、解压、运行（**无需安装、无需任何依赖**，二进制名不带版本号）：

```sh
# 普通用户：直接去上面的 Release 页点「下载」即可，无需命令行。
# 下面给需要脚本 / CI 自动下载的场景（需 Bearer token 并跟随重定向）：
curl -L -H "Authorization: Bearer <你的 token>" \
  -O https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.1.0/stupidsamba_v0.1.0_linux_amd64.tar.gz
curl -L -H "Authorization: Bearer <你的 token>" \
  -O https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.1.0/SHA256SUMS
sha256sum -c SHA256SUMS
tar -xzf stupidsamba_v0.1.0_linux_amd64.tar.gz
./stupidsamba_v0.1.0_linux_amd64/stupidsamba -config stupidsamba_v0.1.0_linux_amd64/configs/example.yaml
# Windows 解压出的是 stupidsamba.exe
```

> 说明：Release 页面上同时保留 `v0.1.0-test` 条目，那是本版本发布流程的**验证记录**
> （含预发布与多客户端验收产物），**请勿下载使用**。正式可用的版本是 **`v0.1.0`**。

### 方式二：从源码构建

要求 Go 1.25 及以上，且**必须**关掉 CGO（否则无法通过静态链接约束）：

```sh
CGO_ENABLED=0 go build -o stupidsamba ./cmd/stupidsamba
```

如需注入版本号（让 `-version` 显示 commit 与构建时间，与发布构建一致）：

```sh
go build -trimpath \
  -ldflags "-s -w -X main.version=v0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o stupidsamba ./cmd/stupidsamba
```

### 四平台交叉编译

```sh
for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 \
    go build -o stupidsamba-${t%/*}-${t#*/} ./cmd/stupidsamba
done
```

四个平台均可交叉编译通过，产物均为静态链接的单一二进制。

---

## 配置文件

完整字段示例见 [`configs/example.yaml`](configs/example.yaml)（每一行都有注释）。
配置加载是**严格模式**：出现未知字段会直接启动失败，避免把拼写错误悄悄忽略。

下面只列出**当前真正实现并会被读取**的字段（死字段不会出现在表里）。

### `server`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `name` | NetBIOS/主机名，出现在 NTLM TargetInfo 与 mDNS 里；上限 15 字节，不能有空格和点。留空用系统主机名 | 系统主机名 |
| `domain` | 工作组名，仅作显示标签，不做域控 | `WORKGROUP` |
| `min_dialect` / `max_dialect` | 方言协商范围，取值 `2.0.2` / `2.1` / `3.0` / `3.0.2` / `3.1.1` | `2.0.2` / `3.1.1` |
| `smb1` | 是否响应 SMB1 多协议协商入口（见[支持能力](#capabilities)），默认 `true` | `true` |
| `signing_required` | 强制 SMB 签名（防中间人篡改） | `false` |
| `encryption_required` | 强制 SMB3 加密：3.0/3.0.2 用 AES-128-CCM，3.1.1 用协商出的算法。开启时 `min_dialect` 与 `max_dialect` 都必须 ≥ 3.0（否则启动直接报错）；SMB 2.0.2/2.1 无加密能力，开启后这类客户端会被拒绝连接（协商阶段无共同方言，fail-closed 兜底返回 `STATUS_ACCESS_DENIED`），而非降级为明文 | `false` |
| `max_connections` | 并发连接数上限。**`0` 或不填 = 默认上限 256，本项不支持「不限」**；超过上限的新连接会被直接关闭 | `256` |

### `listen`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `addresses` | 监听的 IP 列表（可多个）。**要监听全部地址就把整个 `addresses` 留空**，不要写 `0.0.0.0`（Go 双栈监听会冲突） | 全部地址 |
| `port` | 端口，只允许一个 | `445` |

### `auth`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `allow_guest` | 允许 guest 登录（口令错误或用户不存在时降级） | `false` |
| `users` | 静态账户表，账户只由本软件管理 | 无 |
| `users[].name` | 用户名 | — |
| `users[].password` | 明文口令；与 `nt_hash` 二选一 | — |
| `users[].nt_hash` | `MD4(UTF-16LE(口令))` 的 32 位十六进制，**推荐**，避免明文落盘 | — |

> 用明文 `password` 会在启动时有一条 `WARN` 提示改用 `nt_hash`。
> `nt_hash` 的算法举例（等价）：
> ```sh
> printf '你的口令' | iconv -t UTF-16LE | openssl md4
> ```

### `shares`（列表，可多个共享）

| 字段 | 含义 | 默认值 |
|---|---|---|
| `name` | 共享名（客户端看到的名字，如 `\\server\public`） | — |
| `path` | 本地目录**绝对路径，必须已存在** | — |
| `comment` | 共享描述 | 空 |
| `read_only` | 只读共享（写操作会被拒绝） | `false` |
| `browseable` | 是否出现在共享枚举（`smbclient -L`、Finder）中；`false` 只是不列出，知道名字照样能连 | `true` |
| `guest_ok` | 允许 guest 访问本共享 | `false` |
| `valid_users` | 限定可访问用户，留空表示所有已认证用户；名字必须在 `auth.users` 里定义过 | 所有已认证用户 |
| `time_machine` | 把本共享宣告为 Time Machine 备份目标（阶段二） | `false` |
| `quota_bytes` | 向客户端**上报的卷容量上限**（字节）；`0` = 不限（按宿主真实剩余上报）。这是限制 Time Machine 备份体积的**唯一有效手段**（见[Time Machine 状态](#timemachine)） | `0` |
| `metadata_path` | POSIX 元数据旁路存储路径，**仅 Windows 使用**；Linux/macOS 留空即可 | 空 |

### `mdns`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `enabled` | 进程内 mDNS/DNS-SD 广播总开关（关掉服务照样能用，只是不会被自动发现） | `false` |
| `instance` | 服务实例名（Finder 里显示的名字），留空用 `server.name` | `server.name` |
| `interfaces` | 限定广播网卡名，留空表示所有可用网卡 | 所有网卡 |
| `apple.enabled` | 是否携带 Apple 扩展记录（`_device-info._tcp` 等） | `false` |
| `apple.model` | `_device-info._tcp` 的 `model=` 值，决定 Finder 图标，如 `MacSamba` / `TimeCapsule8,119` | `MacSamba` |
| `apple.advertise_time_machine` | 广播 `_adisk._tcp`（Time Machine 磁盘宣告） | `false` |

### `log`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `level` | `debug` / `info` / `warn` / `error` | `info` |
| `format` | `text` / `json` | `text` |
| `file` | 日志文件绝对路径，留空输出到 stderr（所在目录必须已存在） | stderr |

---

## 支持能力 <a name="capabilities"></a>

### 协议方言

- **SMB 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1** —— 全部支持，协商范围由 `min_dialect` /
  `max_dialect` 限定。
- **SMB1 仅作为多协议协商入口**：老客户端（以及 Linux 内核 cifs、impacket 的默认
  行为）会先发 SMB1 `SMB_COM_NEGOTIATE` 并带 `"SMB 2.???"`，本服务用 SMB2 响应
  把它升级到 SMB2。**本服务不提供任何 SMB1 文件操作**，因此 EternalBlue 一类针对
  SMB1 文件操作的攻击面在这里为零。

### 已实现的 SMB2 命令

共 19 个命令（未实现的命令统一返回 `STATUS_NOT_SUPPORTED`，连接不会被打崩）：

`NEGOTIATE` · `SESSION_SETUP` · `LOGOFF` · `ECHO` · `TREE_CONNECT` · `TREE_DISCONNECT` ·
`CREATE` · `CLOSE` · `READ` · `WRITE` · `FLUSH` · `LOCK` · `QUERY_INFO` · `SET_INFO` ·
`QUERY_DIRECTORY` · `IOCTL` · `CANCEL` · `CHANGE_NOTIFY` · `OPLOCK_BREAK`

其中两点需要如实说明（客户端的真实行为你应当知晓）：

- **`CHANGE_NOTIFY`**：当前返回 `STATUS_NOT_SUPPORTED`，客户端会**降级为定时轮询**
  目录变化。后果是：**macOS Finder 与 Windows 资源管理器在别人改了文件后，不会
  自动刷新目录列表**，需要手动刷新（Finder 按 ⌘R，资源管理器按 F5）。异步变更通知
  是已知未实现项。
- **`OPLOCK_BREAK`**：本服务在 `CREATE` 时一律授予 `NONE` oplock、也不宣告 leasing
  能力，因此正常情况下客户端不会发来 oplock/lease break；万一收到则按协议返回
  `STATUS_INVALID_PARAMETER`。即「真实 oplock/lease 能力」尚未实现。

### 认证 / 签名 / 加密

- **认证**：NTLMv2 服务端校验，走 SPNEGO/NTLMSSP（**只支持 NTLM，不支持 Kerberos**）。
  账户完全来自配置文件。
- **SMB 签名**：2.x 用 HMAC-SHA256（取前 16 字节），3.x 用 AES-128-CMAC（RFC 4493）。
  可由 `signing_required` 强制。
- **SMB3 加密**：3.0 / 3.0.2 用 AES-128-CCM（经 `SMB2_GLOBAL_CAP_ENCRYPTION` 能力位
  隐式启用），3.1.1 经 `ENCRYPTION_CAPABILITIES` 协商上下文选择密码套件
  （AES-128/256-CCM 或 GCM）。可由 `encryption_required` 强制——开启后协商到
  **SMB 2.0.2 / 2.1（无加密能力）的客户端会在协商阶段被 `STATUS_ACCESS_DENIED` 拒绝**，
  而非降级为明文。
- **FSCTL**：实现了 `VALIDATE_NEGOTIATE_INFO`（防降级复核）、`SET_SPARSE`、
  `SET_ZERO_DATA`、`QUERY_ALLOCATED_RANGES`（稀疏文件三件套，Time Machine 关键路径）、
  `ENUMERATE_SNAPSHOTS`（回 0 个快照）、`QUERY_NETWORK_INTERFACE` 等。
- **DCERPC/srvsvc**：IPC$ 命名管道可用（用于 `srvsvc` 共享枚举等）。

### 服务发现（mDNS / DNS-SD）

进程内实现，在 `224.0.0.251:5353` / `[ff02::fb]:5353` 上收发报文，**不依赖**
`avahi` / Bonjour / `systemd-resolved`。广播：

- `_smb._tcp` —— 基础 SMB 服务发现；
- `_device-info._tcp` —— Apple 扩展（Finder 图标由 `apple.model` 决定）；
- `_adisk._tcp` —— Time Machine 磁盘宣告（需 `apple.advertise_time_machine: true`
  且至少有一个共享设了 `time_machine: true`）。

### Apple 扩展（AAPL）

- `AAPL` create context：server query / volume caps / model info 协商已实现；
  `readdir_attr`（目录项携带 FinderInfo / 资源派生大小）在客户端请求且后端能提供
  Apple 元数据时启用。
- 稀疏文件 FSCTL 三件套已实现（见上），支持 `.sparsebundle` 的打洞与回收。
- `FLUSH` 走强制刷盘（`F_FULLFSYNC` 语义），并在标了 `time_machine` 的共享上通过
  AAPL VolumeCapabilities 宣告 `SUPPORTS_FULL_SYNC`。

---

## Time Machine 状态 <a name="timemachine"></a>

> **本节结论待定**：Time Machine 端到端「备份并成功恢复」的验收由 `tmverify` 专门进行，
> 本 README 在拿到其报告前**不做最终结论**。

当前已具备的 Time Machine 前置能力（均已实现，并已对代码核实）：

- `AAPL` 协商与 `SUPPORTS_FULL_SYNC` 宣告；
- 稀疏文件 FSCTL 三件套（`SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`）；
- `quota_bytes` 通过 `FileFsFullSizeInformation` 上报卷容量（限制备份体积的唯一手段）；
- `_adisk._tcp` mDNS 宣告；
- `readdir_attr` 目录扩展元数据。

**暂未实现、会影响 Time Machine 体验的点**：

- `resolveID`（AAPL）未实现，但 macOS 在未被宣告该能力时不会使用，影响不大；
- 真实 oplock/lease 能力未实现（见上）。

能否作为 Time Machine 目标成功完成备份与恢复，请以 `tmverify` 的验收报告为准。
v0.1.0 即便 Time Machine 未完全验收，**普通文件共享功能不受影响**。

---

## 已知限制与说明 <a name="notes"></a>

1. **445 是特权端口**：非 root 用户直接监听 445 会被系统拒绝。解决办法三选一：
   - 以 root 运行；
   - 给二进制加能力：`setcap 'cap_net_bind_service=+ep' stupidsamba`，之后普通用户也能绑 445；
   - 改用 `>=1024` 的端口（如 4445），客户端连接时显式指定，例如
     `smbclient //host/share -p 4445`、`smb://host:4445`。
   启动若绑不上特权端口，会打印人话提示上述三种方案。

2. **guest 登录的安全含义**：`allow_guest: true` 后，**任何口令（甚至错误口令）都能
   以 guest 身份登录**（这是 SMB guest 语义本身）。默认关闭，开启会有启动 `WARN`。
   另外 Windows 10/11 默认拒绝不安全的 guest 登录，guest 共享在 Windows 上可能连不上。

3. **明文口令的含义**：配置文件里写 `password: "xxx"` 是明文落盘的。生产环境请用
   `nt_hash`（口令的 MD4 哈希，仍可被离线爆破但至少不在磁盘上暴露原口令）。用明文会有
   启动 `WARN`。

4. **目录变更不会自动刷新**：因 `CHANGE_NOTIFY` 未实现（见[支持能力](#capabilities)），
   Finder / 资源管理器的目录列表不会自动更新，需手动刷新。

5. **单文件语义**：本服务是**文件共享**，不做打印机共享、不做域控、不做 DFS。

---

## 客户端测试矩阵 <a name="matrix"></a>

本项目以至少三种第三方 SMB 客户端验证（AGENTS.md §3），并尽量覆盖各平台原生客户端：

| 客户端 | 状态 | 说明 |
|---|---|---|
| `smbclient`（Samba CLI） | ✅ 已实测 | `ls` / `put` / `get` / `mkdir` / `rm` / `rmdir`、各方言、加密、`nt_hash` 登录均通过 |
| `impacket`（Python） | ✅ 目标支持 | 客户端矩阵第 3 项；本服务保留的 SMB1 多协议协商入口正是为它（默认先发 SMB1 协商）而开 |
| `go-smb2`（纯 Go 客户端） | ✅ 目标支持 | 纯 Go 端到端集成测试，可进 CI |
| macOS Finder / `mount_smbfs` | 🎯 目标 | Apple 扩展（AAPL / `_adisk` / `readdir_attr`）为其服务；Time Machine 见[上](#timemachine) |
| Windows 资源管理器 | 🎯 目标 | 签名、`guest` 策略、属性页 |
| `mount.cifs`（Linux 内核客户端） | ✅ 服务端支持 | 见[上文](#notes)：本项目 CI / 开发容器缺 `CAP_SYS_ADMIN` 跑不通，真实 Linux 主机可正常挂载 |

> 想核实「你说支持，凭什么」？完整的多客户端验收报告见
> [`docs/acceptance-v0.1.0.md`](docs/acceptance-v0.1.0.md)（smbclient / impacket / go-smb2
> 三家客户端栈逐项实测，含加密 fail-closed 验证），基线 commit `5428bcd`。

---

## 许可证

**许可证待定。** 本项目当前处于内部测试阶段（v0.1.0 prerelease），尚未确定正式开源
许可证。在许可证明确之前，请勿将代码用于未获授权的分发或再发布。

---

## 构建与自检

发布前必须本地跑过（提交纪律，CI 也会校验）：

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

四平台发布物用 `scripts/build-release.sh` 一键产出（产物落在 `dist/`，含
`SHA256SUMS`）。详见该脚本头部注释。

---

## 相关文档

- [`configs/example.yaml`](configs/example.yaml) —— 逐字段注释的完整配置示例
- [`CHANGELOG.md`](CHANGELOG.md) —— 各版本变更记录
- [`docs/acceptance-v0.1.0.md`](docs/acceptance-v0.1.0.md) —— v0.1.0 多客户端验收报告（smbclient / impacket / go-smb2 实测矩阵、加密 fail-closed 验证方法）
- [`docs/protocol-notes.md`](docs/protocol-notes.md) —— 协议研究笔记（实现依据）
