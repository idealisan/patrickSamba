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

启动后日志会打印监听地址与协商到的方言范围。上面这份最小配置没写
`listen.addresses`，等于监听全部地址，实际输出是：

```
time=2026-08-09T12:28:25.777+08:00 level=INFO msg="SMB 服务已监听" addrs=[::]:445 dialects=2.0.2..3.1.1 shares=public
```

（`addrs` 是双栈通配地址 `[::]`，IPv4 一并覆盖；显式写了 `listen.addresses` 时
会逐个列出，如 `addrs="127.0.0.1:445, [::1]:445"`。）

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

> **本仓库为私有仓库**：下载需要 **仓库访问权限** 与 **个人访问令牌（PAT）**。
> 匿名访问 Release 页或附件会返回 **404**（平台对无权限者隐藏仓库存在性，不是链接错误）。
> 先生成令牌并导出：
> ```sh
> export CNB_TOKEN=<你的个人访问令牌>
> ```

最新版本 **v0.2.0** 发布在 CNB 仓库的 Release 页面（标记为 **prerelease**）：

- **Release 页（推荐，普通用户点这里下载）**：
  `https://cnb.cool/finalappstore/stupidSamba/-/releases/v0.2.0`
  页面里的「下载」按钮由 web 会话处理跳转，能正常拿到文件。
- **原始文件直链（给脚本 / CI 用）**：
  `https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.2.0/stupidsamba_v0.2.0_<os>_<arch>.tar.gz`
  （Windows 用 `.zip`；`SHA256SUMS` 在同目录 `.../download/v0.2.0/SHA256SUMS`）。
  注意：该直链需在请求里带 `Authorization: Bearer <token>` 且跟随重定向（`-L`），
  最终从公开 CDN `asset.cnb.cool` 取字节；浏览器在 Release 页点按不受此限。
  ⚠️ 不要把 `cnb.cool` 这个 host 的 `/-/releases/download/...` 路径当可直接
  `curl` 的链接——它不下发文件，需用上面的 `api.cnb.cool` 形态。

每个压缩包内含二进制、本 `README.md`、`CHANGELOG.md` 与 `configs/example.yaml`：

| 平台 | 文件 |
|---|---|
| Linux x86-64 | `stupidsamba_v0.2.0_linux_amd64.tar.gz` |
| Linux ARM64（树莓派 4 等） | `stupidsamba_v0.2.0_linux_arm64.tar.gz` |
| macOS Apple Silicon | `stupidsamba_v0.2.0_darwin_arm64.tar.gz` |
| Windows x86-64 | `stupidsamba_v0.2.0_windows_amd64.zip` |

下载、校验、解压、运行（**无需安装、无需任何依赖**，二进制名不带版本号）：

```sh
BASE=https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.2.0

# 必须带 Bearer 令牌（-H）并跟随跳转（-fL：-f 遇错不写文件，-L 跟 302 到 CDN）
curl -fL -H "Authorization: Bearer $CNB_TOKEN" \
  -o stupidsamba_v0.2.0_linux_amd64.tar.gz \
  "$BASE/stupidsamba_v0.2.0_linux_amd64.tar.gz"

# 校验完整性
curl -fL -H "Authorization: Bearer $CNB_TOKEN" -o SHA256SUMS "$BASE/SHA256SUMS"
sha256sum -c SHA256SUMS 2>/dev/null | grep linux_amd64

tar -xzf stupidsamba_v0.2.0_linux_amd64.tar.gz
./stupidsamba_v0.2.0_linux_amd64/stupidsamba -config stupidsamba_v0.2.0_linux_amd64/configs/example.yaml
# Windows 解压出的是 stupidsamba.exe
```

> 说明：Release 页面上同时保留 `v0.1.0` 与 `v0.1.0-test` 条目。
> `v0.1.0-test` 是发布流程的**验证记录**，**请勿下载使用**；`v0.1.0` 是上一个版本。

### 方式一之二：Docker 镜像（NAS / 家庭服务器推荐）

v0.2.0 起每个版本同时发布**多架构容器镜像**（`linux/amd64` + `linux/arm64`），
基础镜像是 `scratch`——镜像里只有一个静态二进制和一份配置，没有 shell、没有包管理器。
镜像里的二进制与上面裸包里的是**同一份字节**（发布脚本从 `.tar.gz` 里解出来再 `COPY`
进镜像），所以裸包做过的那几项自检对镜像同样成立。

**先登录，镜像和本仓库一样是私密的**（实测：不登录直接 pull 会被拒，报
`pull access denied ... no basic auth credentials`）：

```sh
# 用户名是固定字面量 cnb，不是你的账号名；密码是 CNB 访问令牌
docker login docker.cnb.cool -u cnb -p "$CNB_TOKEN"
```

```sh
# 试跑：内置配置开着 guest 匿名读写，所以刻意只发布在回环地址上
docker run -d --name stupidsamba \
  -p 127.0.0.1:4445:445 \
  -v /你的目录:/data \
  docker.cnb.cool/finalappstore/stupidsamba:v0.2.0

smbclient //127.0.0.1/public -p 4445 -N -m SMB3 -c ls
```

`docker pull` 会按当前平台自动挑架构，不需要指定。要在 amd64 机器上核对 arm64
那一份，用 `docker pull --platform linux/arm64 ...`。

⚠️ **内置配置 [`configs/docker.yaml`](configs/docker.yaml) 是试用配置，不要直接用于生产**：
它开着 guest 匿名读写。正式使用请挂载自己的配置覆盖它：

```sh
docker run -d --name stupidsamba \
  -p 445:445 \
  -v /你的目录:/data \
  -v ./my-config.yaml:/etc/stupidsamba/config.yaml:ro \
  docker.cnb.cool/finalappstore/stupidsamba:v0.2.0
```

两点须知：

- **mDNS 在默认桥接网络下无效，因此内置配置里是关的。** 组播报文出不了 docker0 网桥，
  就算出得去，广播的也是容器内部的 172.17.x.x 地址，客户端照着连必然失败。
  要让 Finder / 资源管理器自动发现，用 `--network host` 起容器，
  并挂载一份把 `mdns.enabled` 改成 `true` 的配置。
- 容器内固定监听 445。要用非特权端口就改**宿主侧**的映射（`-p 4445:445`），
  不要改容器内端口。

### 方式二：从源码构建

要求 Go 1.25 及以上，且**必须**关掉 CGO（否则无法通过静态链接约束）：

```sh
CGO_ENABLED=0 go build -o stupidsamba ./cmd/stupidsamba
```

如需注入版本号（让 `-version` 显示 commit 与构建时间，与发布构建一致）：

```sh
go build -trimpath \
  -ldflags "-s -w -X main.version=v0.2.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o stupidsamba ./cmd/stupidsamba
```

> 版本号**没有硬编码在任何源文件里**：`cmd/stupidsamba/main.go` 里的默认值刻意是
> `version = "dev"`，发布构建由 `scripts/build-release.sh` 经 `-ldflags -X` 注入，
> 值来自 git tag（CI 里是 tag_push 事件的 `$CNB_BRANCH`）。
> 所以「发新版本」= 打新 tag，不需要改代码；反过来，直接 `go build` 出来的二进制
> `-version` 会显示 `dev`，一眼就能看出它不是发布产物。

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
| `encryption_required` | 强制 SMB3 加密：3.0/3.0.2 用 AES-128-CCM，3.1.1 用协商出的算法。开启时 `min_dialect` 与 `max_dialect` **都必须 ≥ 3.0，否则启动直接报错**（SMB 2.x 没有加密能力，这类客户端会被拒绝连接，是预期行为而非 bug）；开启后协商到 2.0.2/2.1 的客户端会在协商阶段被拒绝，而非降级为明文 | `false` |
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
| `path` | 本地目录绝对路径，**必须已存在**。「绝对」按**运行平台**判定：Windows 上 `/srv/share` 不算绝对路径，要写 `C:\srv\share` | — |
| `comment` | 共享描述 | 空 |
| `read_only` | 只读共享（写操作会被拒绝） | `false` |
| `browseable` | 是否出现在共享枚举（`smbclient -L`、Finder）中；`false` 只是不列出，知道名字照样能连 | `true` |
| `guest_ok` | 允许 guest 访问本共享 | `false` |
| `valid_users` | 限定可访问用户，留空表示所有已认证用户；名字必须在 `auth.users` 里定义过 | 所有已认证用户 |
| `time_machine` | 把本共享宣告为 Time Machine 备份目标（阶段二） | `false` |
| `quota_bytes` | 向客户端**上报的卷容量上限**（字节）；`0` = 不限（按宿主真实剩余上报）。这是限制 Time Machine 备份体积的**唯一有效手段**（见[Time Machine 状态](#timemachine)）。⚠️ 上报的**可用空间 = `quota_bytes` − 宿主卷已用空间**（出于性能不递归统计本共享自身占用），因此 **`quota_bytes` 必须大于「宿主卷已用空间 + 期望备份体积」**，否则即使共享是空的，客户端也会看到可用空间为 0 而拒绝开始备份 | `0` |
| `metadata_path` | POSIX 元数据旁路存储路径，**仅 Windows 使用**；Linux/macOS **留空**即可。⚠️「非 Windows 会忽略它」是**运行时**行为，但**校验在所有平台都做**：填的必须是当前运行平台意义上的绝对路径，否则启动直接失败。所以在 Linux 上填 `C:\...` 会起不来 —— 跨平台复用同一份配置时这项要么留空、要么按平台分开写 | 空（落在 `%AppData%\stupidsamba\` 下，按共享根路径哈希命名） |

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
  **SMB 2.0.2 / 2.1（无加密能力）的客户端会被拒绝连接（返回 `STATUS_NOT_SUPPORTED`，
  因为无共同方言），而非降级为明文**。
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

> **v0.1.0 尚未通过 Time Machine 端到端验收。** 本项目**从未跑过一次真实的 Time Machine
> 备份，更没有做过恢复**——开发环境里没有 macOS，「备份并成功恢复」一次都没有发生过。
> 下面列的是「服务端前置能力已实现并实测」，**不等于「Time Machine 能用」**：前者是
> 对服务端行为的探针，后者需要真机端到端验证。

服务端侧的多项 Time Machine 前置能力已实现，并用 impacket 低阶 SMB2 客户端逐项实测通过：

- `AAPL` 协商（server query / readdir_attr）；
- 命名流与 Alternate Data Stream（含目录上的流，`.sparsebundle` 依赖）；
- 稀疏文件 FSCTL 三件套（`SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`）；
- 卷容量与 `quota_bytes` 上报；
- 大目录枚举性能（5 万 band 文件全量枚举约 0.28 s，内存不增长）；
- `_adisk._tcp` mDNS 广播。

`F_FULLFSYNC`：Linux / Windows 分支实测通过；**Darwin 的 `F_FULLFSYNC` 分支在本容器里
编不了也跑不了，只有交叉编译通过做保证**。

但以下能力**未实现**，可能导致备份不稳定甚至失败（按对 Time Machine 的实际影响排序）：

- **durable / persistent handle** —— **对 TM 影响最大**。一次备份动辄数小时，没有它，
  网络抖动会导致句柄丢失、备份中断重来。服务端在客户端请求 `DHnQ` / `DH2Q` 时**明确
  不予授予**（不返回对应响应 context），客户端因此知道没拿到、不会去做断线 reclaim——
  行为可预期，但意味着 Wi-Fi 一抖整次备份就失败重来。
- **oplock / lease** —— 服务端不声明 `SMB2_GLOBAL_CAP_LEASING`、不授予任何 oplock
  （`create.go` 恒回 `OplockLevelNone`），客户端退化为不缓存，`.sparsebundle` 的 band
  目录那种小文件密集写吞吐明显低于 Samba；不影响正确性。
- **AAPL resolveID** —— 对 TM 本身**无实际影响**（我们不宣告 `kAAPL_SUPPORT_RESOLVE_ID`，
  客户端就不会使用），仅 Finder 的别名 / 最近项目按 64 位 file id 反查路径会退化为按路径查找。

### 使用须知

- **不要拿真实备份数据试。** 请仅用测试数据（或一台可随时清空的机器）试用，确认能完成
  一轮完整备份并成功浏览快照后，再考虑放真实数据。**请勿把它作为唯一一份备份的目的地。**
- **最可能的失败模式是稳定性，不是连不上。** 服务端不授予任何 oplock / lease，长时间大体量
  备份（`.sparsebundle` band 目录海量小文件）下的表现未知——可能慢，也可能中途报错。
  **所有 macOS 版本均未经实测**，不要写成「某版本有点抖」这类暗示我们试过的口吻。
- **备份共享务必显式设 `quota_bytes`。** 不设时上报宿主真实剩余空间，Time Machine 会一路
  写满磁盘。注意：`quota_bytes` 上报的**可用空间 = 配额 − 宿主卷已用空间**（出于性能不
  递归统计共享自身占用），因此 **`quota_bytes` 必须大于「宿主卷已用空间 + 期望备份体积」**，
  否则即便共享是空的，客户端也会看到可用空间为 0 而**直接拒绝开始备份**。（`quota_bytes`
  小于 1 GiB 时启动会告警；在过小的卷上 TM 会反复失败。）
- 若备份失败，请提供服务端 `log.level: debug` 的日志（含每个 SMB 命令与 NTSTATUS），
  这比 macOS 侧报错更有用。

逐项验证证据见 [Time Machine 支持状态](docs/timemachine-status.md)。

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

6. **不授予任何 oplock / lease**：`CREATE` 恒回 `OplockLevelNone`，且 `tree_connect` 虽宣告了
   `SMB2_GLOBAL_CAP_LEASING` 之外的 `FORCE_LEVELII_OPLOCK` 能力位，但这是**有意为之**
   （约束客户端别申请独占 oplock，语义上不是虚假宣告）。对普通文件共享几乎无影响；
   对 Time Machine 的影响是客户端退化为不缓存，见[上](#timemachine)。

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
- [`docs/dev-workflow.md`](docs/dev-workflow.md) —— 开发工作流 SOP（独立 worktree + 独立分支 + PR），参与开发前必读
