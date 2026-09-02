# stupidSamba

一个**用纯粹 Go 实现的、极简易用的 SMB/CIFS 文件共享服务器**。
自带进程内 mDNS/DNS-SD 广播，支持 macOS、Windows、Linux 等各类 SMB 客户端。

> 本项目当前阶段目标（阶段一）是提供一个**可用的 SMB 文件共享服务**。
> Time Machine 备份是下一阶段（阶段二）的目标，当前已具备大部分前置能力，但端到端
> 「备份并成功恢复」**一次都没有跑过**，且真机验收已于 2026-08-09 被项目所有者
> **降为可选项**（无可用 macOS 真机环境），不再是版本发布的阻塞项。
> 详见下方「[Time Machine 状态](#timemachine)」。

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
- **操作系统只当「文件系统 + 套接字」用**：不依赖内核 cifs 驱动、不依赖 `mount`、
  不依赖 namespace，也不假设宿主文件系统支持扩展属性、稀疏文件、稳定 inode 或创建时间。
  这条由 `scripts/check-constraints.sh` 的 C9 段机器校验。
  承诺的后半句自 v0.3.0 起完整兑现：不依赖可选文件系统能力的自带实现
  （`internal/oscap/builtin`）已接进真实数据路径（6 项能力全部生效），
  宿主不支持某项能力时由旁路存储兜底，不再静默丢失；
  取用策略由 `filesystem_mode` 控制（`auto` / `portable` 两态）。
  详见「[已知限制与说明](#notes)」第 7 条与 [`CHANGELOG.md`](CHANGELOG.md) 的 v0.3.0 段。

---

## 快速开始

### 1. 准备配置文件

最小可用配置只需要 `shares` 加一个用户（与官方镜像内置的演示账号是同一套）：

```yaml
server:
  name: STUPIDSAMBA
auth:
  users:
    - name: stupidsamba
      password: "stupidsamba"     # 演示口令，正式使用请换掉；或改用 nt_hash（见下文）
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
smbclient -L //127.0.0.1 -p 445 -U stupidsamba%stupidsamba

# 进入某个共享做文件操作
smbclient //127.0.0.1/public -p 445 -U stupidsamba%stupidsamba -m SMB3
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

**关于 `mount.cifs`（Linux 内核客户端）**：本项目的开发 / CI 容器跑在
**非初始 user namespace** 里（`cat /proc/self/uid_map` = `0 1000 1`），内核只放行带
`FS_USERNS_MOUNT` 标志的文件系统，而 cifs 没有这个标志，于是 `mount -t cifs` 报
`mount error(1): Operation not permitted`。这是**环境限制，不是服务端不支持
Linux 内核客户端**——注意它与 `CAP_SYS_ADMIN` 无关（加 capability 重跑仍然失败，
同容器 `mount -t tmpfs` 却成功），在具备初始 namespace 的普通 Linux 主机上，
`mount -t cifs //host/share /mnt -o user=alice,pass=...` 可以正常挂载。
（本项目的客户端验收矩阵用 `smbclient` + `impacket` + 纯 Go `go-smb2` 三家覆盖，
见下方「[客户端测试矩阵](#matrix)」。）

**macOS（Finder）**：
`前往 ▸ 连接服务器`，输入 `smb://127.0.0.1`（默认端口 445 可省略），
输入用户名 `alice` 与口令即可。若改了端口：`smb://127.0.0.1:445`。

**Windows（资源管理器）**：
地址栏输入 `\\127.0.0.1\public`，在弹出的凭据框里输入用户名与口令。
**Windows 10/11 默认拒绝 guest 匿名登录**，所以服务端要连得上，配置里必须有
真实账号（默认配置内置了演示账号，见下文 Docker 一节）。
非 445 端口也能直连：地址带端口即可，如 `\\172.26.0.217:4445\public`
（Windows 11 实测可用，2026-08-26；445 是默认端口，可省略）。

---

## 安装

### 方式一：下载预编译二进制（推荐）

> **本仓库为私有仓库**：下载需要 **仓库访问权限** 与 **个人访问令牌（PAT）**。
> 匿名访问 Release 页或附件会返回 **404**（平台对无权限者隐藏仓库存在性，不是链接错误）。
> 先生成令牌并导出：
> ```sh
> export CNB_TOKEN=<你的个人访问令牌>
> ```

最新版本 **v0.4.0** 发布在 CNB 仓库的 Release 页面（正式版通道；自 PR #156（`25e92d2`）起
发布渠道按 **tag 名的 SemVer** 判定 —— 带连字符的 tag（`v0.2.0-rc1`）才走预发布。
**「正式版通道」说的是发布渠道，不是成熟度背书**，成熟度以下文各节的实测证据为准）：

- **Release 页（推荐，普通用户点这里下载）**：
  `https://cnb.cool/finalappstore/stupidSamba/-/releases/v0.4.0`
  页面里的「下载」按钮由 web 会话处理跳转，能正常拿到文件。
- **原始文件直链（给脚本 / CI 用）**：
  `https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.4.0/stupidsamba_v0.4.0_<os>_<arch>.tar.gz`
  （Windows 用 `.zip`；`SHA256SUMS` 在同目录 `.../download/v0.4.0/SHA256SUMS`）。
  注意：该直链需在请求里带 `Authorization: Bearer <token>` 且跟随重定向（`-L`），
  最终从公开 CDN `asset.cnb.cool` 取字节；浏览器在 Release 页点按不受此限。
  ⚠️ 不要把 `cnb.cool` 这个 host 的 `/-/releases/download/...` 路径当可直接
  `curl` 的链接——它不下发文件，需用上面的 `api.cnb.cool` 形态。

每个压缩包内含二进制、本 `README.md`、`CHANGELOG.md` 与 `configs/example.yaml`：

| 平台 | 文件 |
|---|---|
| Linux x86-64 | `stupidsamba_v0.4.0_linux_amd64.tar.gz` |
| Linux ARM64（树莓派 4 等） | `stupidsamba_v0.4.0_linux_arm64.tar.gz` |
| macOS Apple Silicon | `stupidsamba_v0.4.0_darwin_arm64.tar.gz` |
| Windows x86-64 | `stupidsamba_v0.4.0_windows_amd64.zip` |

下载、校验、解压、运行（**无需安装、无需任何依赖**，二进制名不带版本号）：

```sh
BASE=https://api.cnb.cool/finalappstore/stupidSamba/-/releases/download/v0.4.0

# 必须带 Bearer 令牌（-H）并跟随跳转（-fL：-f 遇错不写文件，-L 跟 302 到 CDN）
curl -fL -H "Authorization: Bearer $CNB_TOKEN" \
  -o stupidsamba_v0.4.0_linux_amd64.tar.gz \
  "$BASE/stupidsamba_v0.4.0_linux_amd64.tar.gz"

# 校验完整性
curl -fL -H "Authorization: Bearer $CNB_TOKEN" -o SHA256SUMS "$BASE/SHA256SUMS"
sha256sum -c SHA256SUMS 2>/dev/null | grep linux_amd64

tar -xzf stupidsamba_v0.4.0_linux_amd64.tar.gz
./stupidsamba_v0.4.0_linux_amd64/stupidsamba -config stupidsamba_v0.4.0_linux_amd64/configs/example.yaml
# Windows 解压出的是 stupidsamba.exe
```

> 说明：Release 页面上保留着历史条目 `v0.1.0` ~ `v0.3.0`，以及发布流程验证用的
> `v0.1.0-test` / `v0.0.99-probe` / `v0.2.0-rc0`。后三个是**流程验证记录**，
> **请勿下载使用**；`v0.1.0` ~ `v0.3.0` 是旧版本，新部署请用 v0.4.0。

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
# 试跑：一条命令起服务，内置配置自带演示账号（stupidsamba/stupidsamba），
# Windows 11 / macOS / Linux 客户端都能直接连。
docker run -d --name stupidsamba \
  -p 445:445 \
  -v /你的目录:/data \
  docker.cnb.cool/finalappstore/stupidsamba:v0.4.0

smbclient //127.0.0.1/public -U stupidsamba%stupidsamba -m SMB3 -c ls
```

> 注：已发布的 v0.4.0 镜像内置的还是旧版试用配置（guest 匿名 + 回环端口示例）。
> 上面的用法（演示账号 + `-p 445:445`）自**包含本版新内置配置的下一个镜像**起生效。

`docker pull` 会按当前平台自动挑架构，不需要指定。要在 amd64 机器上核对 arm64
那一份，用 `docker pull --platform linux/arm64 ...`。

### 从 Windows 访问

1. 资源管理器地址栏输入 `\\<运行 Docker 的机器 IP>\public`
   （例如 `\\172.26.0.217\public`；也可以在「添加网络位置」向导里填同样的路径）。
2. 弹出凭据框时输入用户名 `stupidsamba`、口令 `stupidsamba`
   （可勾选"记住我的凭据"，之后就不再询问）。
3. 想映射成网络驱动器：「此电脑 ▸ 映射网络驱动器」，填同一个 UNC 路径并勾选
   "使用其他凭据连接"即可。

两个最常见的坑：

- **地址里的端口写法**：`\\IP:端口\share` 可以直接带端口号
  （Windows 11 实测可用，2026-08-26），所以宿主侧映射成 `-p 4445:445` 这类
  非默认端口时资源管理器照样能连；省略端口则只走默认的 445/tcp。
- **guest 匿名连不上不是 bug**：Windows 10/11 默认拒绝不安全的 guest 登录，
  这就是默认配置改用真实账号的原因。

⚠️ **内置配置 [`configs/docker.yaml`](configs/docker.yaml) 是开箱即用的试用配置，
不要直接用于生产**：演示账号是公开凭据（启动日志会打出 WARN 提醒）。正式使用请
挂载自己的配置覆盖它：

```sh
docker run -d --name stupidsamba \
  -p 445:445 \
  -v /你的目录:/data \
  -v ./my-config.yaml:/etc/stupidsamba/config.yaml:ro \
  docker.cnb.cool/finalappstore/stupidsamba:v0.4.0
```

两点须知：

- **mDNS 在默认桥接网络下无效，因此内置配置里是关的。** 组播报文出不了 docker0 网桥，
  就算出得去，广播的也是容器内部的 172.17.x.x 地址，客户端照着连必然失败。
  要让 Finder / 资源管理器自动发现，用 `--network host` 起容器，
  并挂载一份把 `mdns.enabled` 改成 `true` 的配置。
- 容器内固定监听 445。要用非特权端口就改**宿主侧**的映射（`-p 4445:445`），
  不要改容器内端口。宿主侧换了端口后 Windows 资源管理器仍可直连：
  地址写成 `\\IP:4445\public` 带上端口号即可（Windows 11 实测可用）。

### 方式二：从源码构建

要求 Go 1.25 及以上，且**必须**关掉 CGO（否则无法通过静态链接约束）：

```sh
CGO_ENABLED=0 go build -o stupidsamba ./cmd/stupidsamba
```

如需注入版本号（让 `-version` 显示 commit 与构建时间，与发布构建一致）：

```sh
go build -trimpath \
  -ldflags "-s -w -X main.version=v0.4.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
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

### 顶层

| 字段 | 含义 | 默认值 |
|---|---|---|
| `filesystem_mode` | OS 可选能力（扩展属性 / 稀疏文件 / 命名流 / 稳定 FileID / 创建时间 / DOS 属性）的取用策略，`auto` / `portable` 两态，**大小写敏感**。六项能力自 v0.3.0 起**全部接进数据路径**，两态对全部六项都有真实运行期效果。⚠️ 旧的第三态 `native` 已在 v0.5 开发版移除：旧配置写了 `filesystem_mode: native` 会启动报错，改 `auto` 或 `portable`，详见[已知限制](#notes)第 7 条 | `auto` |

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
| `oplocks` | 允许授予 oplock / lease（客户端本地缓存，能显著提高小文件密集读写吞吐）。打开后宣告 `SMB2_GLOBAL_CAP_LEASING`。**默认关闭** —— 未做 Windows/macOS 真机验收，而这类实现错误的代价是**静默的脏数据**。已知边界见[已知限制](#notes) | `false` |

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
| `metadata_path` | 旁路元数据库落盘路径。⚠️ **所有平台都生效**——旁路库位置在非 Windows 上自 v0.3.0 接线起就真实生效，v0.4.0 起**配置校验**也改为全平台（不再是「仅 Windows」的字段）：它既决定 Windows 上 POSIX 属主/权限位旁路库的位置，也决定 oscap builtin 六项能力旁路库（`.stupidsamba-oscap-*.db`）的位置——后者在 `auto` 与 `portable` 两档、任何平台上都会真实创建。校验在**所有平台**做：必须是**当前运行平台**意义上的绝对路径且父目录已存在；把 Windows 路径写进 Linux 配置会**直接启动失败**（报错会点明「另一个平台的绝对路径」；这是 v0.4.0 的行为变化，此前仅 WARN 放行）。留空时落点由程序自己决定：oscap 旁路库落在**共享根目录的兄弟位置**（文件名编入根路径哈希与服务实例标识，多进程各开各的库不互抢文件锁），Windows 的 POSIX 库落在 `%AppData%\stupidsamba\` 下。填在共享目录内部不会报错但会有一条 WARN（客户端能看见这个数据库文件） | 空（按上述默认规则落点） |

### `mdns`

| 字段 | 含义 | 默认值 |
|---|---|---|
| `enabled` | 进程内 mDNS/DNS-SD 广播总开关（关掉服务照样能用，只是不会被自动发现） | `false` |
| `instance` | 服务实例名（Finder 里显示的名字），留空用 `server.name` | `server.name` |
| `interfaces` | 限定广播网卡名，留空表示所有可用网卡 | 所有网卡 |
| `apple.enabled` | 是否携带 Apple 扩展记录（`_device-info._tcp` 等） | `false` |
| `apple.model` | `_device-info._tcp` 的 `model=` 值，决定 Finder 图标，如 `MacSamba` / `TimeCapsule8,119` | `MacSamba` |
| `apple.advertise_time_machine` | 广播 `_adisk._tcp`（Time Machine 磁盘宣告） | `false` |

### `ws_discovery`（v0.7.0 新增）

让 **Windows** 的「网络」自动发现本机。与 mDNS 是并列的两套协议，
各覆盖各的客户端（Windows 10 之后也能听 mDNS，但「网络」列表主要靠这个）。

| 字段 | 含义 | 默认值 |
|---|---|---|
| `enabled` | WS-Discovery 开关 | `false` |
| `interfaces` | 限定网卡名，留空表示所有支持组播的网卡 | 所有网卡 |
| `uuid` | 设备标识。**留空则按名字派生一个稳定的 UUIDv5**（跨重启不变，Windows 网络列表才不会攒同名僵尸条目）。同一台机器跑多个实例时需要各自钉一个 | 按名字派生 |
| `metadata_port` | WS-Transfer `Get` 的 HTTP 端口；Windows 列出主机后会来这里取详情。填 `-1` 表示不启用（主机仍能列出，但点开无信息） | `5357` |

### `netbios`（v0.7.0 新增）

让 `\\NAME` 能被解析，并让本机出现在网上邻居的浏览列表里。与 WS-Discovery
互补，缺一个都会让 Windows 用户别扭（一个管"看得见"，一个管"叫得出名"）。

| 字段 | 含义 | 默认值 |
|---|---|---|
| `enabled` | NetBIOS 名称服务开关 | `false` |
| `interfaces` | 限定网卡名，留空表示所有支持广播的网卡 | 所有网卡 |
| `name` | 本机 NetBIOS 名，留空用 `server.name`（自动大写、超 15 字节截断） | `server.name` |
| `workgroup` | 工作组名，留空用 `server.domain` | `server.domain` |
| `comment` | 网上邻居里显示的说明文字 | `stupidSamba file server` |
| `announce_interval_seconds` | 主机宣告间隔（保活：太稀疏客户端要等很久才看见我们，太密则刷局域网） | `240` |

> ⚠️ 137 / 138 是**特权端口（<1024）**，非 root 时绑不上。三种解决办法与
> SMB 的 445 完全一样：以 root 运行 / `sudo setcap 'cap_net_bind_service=+ep'`
> / 关掉本段。绑 138 失败只告警并退化到临时端口；绑 137 失败则本组件起不来。

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
- **SMB1 仅作为多协议协商入口**：老客户端会先发 SMB1 `SMB_COM_NEGOTIATE` 并带
  `"SMB 2.??"`，本服务用 SMB2 响应把它升级到 SMB2。**本服务不提供任何 SMB1 文件操作**，
  因此 EternalBlue 一类针对 SMB1 文件操作的攻击面在这里为零。
  （如实说明：2026-08-25 的三方黑盒复测里没有任何客户端真的走到这条入口——
  新版 smbclient 已剔除 SMB1、impacket 0.12 默认直发 SMB2——所以该路径目前只有
  单元测试档证据；纯 SMB1 请求会被正确拒绝并记 WARN。见 `docs/protocol-notes.md` §4。）

### 已实现的 SMB2 命令

共 19 个命令（未实现的命令统一返回 `STATUS_NOT_SUPPORTED`，连接不会被打崩）：

`NEGOTIATE` · `SESSION_SETUP` · `LOGOFF` · `ECHO` · `TREE_CONNECT` · `TREE_DISCONNECT` ·
`CREATE` · `CLOSE` · `READ` · `WRITE` · `FLUSH` · `LOCK` · `QUERY_INFO` · `SET_INFO` ·
`QUERY_DIRECTORY` · `IOCTL` · `CANCEL` · `CHANGE_NOTIFY` · `OPLOCK_BREAK`

- **`CHANGE_NOTIFY`（v0.6.0 起真正实现）**：请求挂起，被监视目录发生变化时补发
  带 `FILE_NOTIFY_INFORMATION` 的响应，Finder / 资源管理器会**自动刷新**目录列表。
  变更来自本服务自己的写路径（CREATE / WRITE / SET_INFO / CLOSE 删除），
  **宿主上其它进程直接改动共享目录不会触发通知** —— 客户端会因此少一次自动刷新，
  但不会出错（它本来就有轮询兜底）。详见「[已知限制与说明](#notes)」。
- **`OPLOCK_BREAK`（v0.6.0 起真正实现）**：`server.oplocks: true` 时 `CREATE` 会按
  请求授予 oplock / lease，`NEGOTIATE` 宣告 `SMB2_GLOBAL_CAP_LEASING`；别的客户端要
  动这个文件时先发 break 通知**并等确认**，确认到达后才放行被推迟的打开。
  默认关闭，理由与已知边界见「[已知限制与说明](#notes)」。

### 文件行为语义（v0.4.x 对照 Samba 的修正）

以下行为自 v0.4.x 起生效（对照真实 Samba 行为逐项核对后的修复波），此前的版本
在这些点上是空壳或语义错误：

- **字节范围锁是真的**（v0.4.x 起）：`LOCK` 授予的范围锁现在会真正阻挡**其他句柄**
  对重叠区间的 READ/WRITE——读只被外句柄的独占锁阻挡，写被任何重叠的外句柄锁阻挡；
  同一句柄自己的锁不妨碍自己（与 Samba 的 STRICT_LOCK_CHECK 一致），冲突回
  `STATUS_FILE_LOCK_CONFLICT`。句柄关闭/断连时其全部锁随之释放。
- **READONLY 属性拒绝写入**（v0.4.x 起）：带 `FILE_ATTRIBUTE_READONLY` 的目标，
  WRITE 按**打开时的属性快照**拒绝（`STATUS_ACCESS_DENIED`）。判据是配置与属性位，
  不读宿主 ACL。注意这与共享级 `read_only: true` 是两层独立的只读：前者按对象属性，
  后者按共享配置。
- **零长度读成功回 0 字节**（v0.4.x 起）：`READ` Length=0 不再回 `END_OF_FILE`
  而是正常成功——它是客户端的合法探测手法；越界非零读才回 `END_OF_FILE`。
  同时鉴权检查先于一切长度判定，无权句柄不再能借零长读探测文件是否存在。
- **CREATE 携带的 FileAttributes 生效**（v0.4.x 起）：创建性打开携带的属性位按 Samba
  语义落地——`DIRECTORY` 位静默剥掉（目录与否由操作本身决定）、自动叠上 `ARCHIVE`、
  只对 created/overwritten/superseded 生效（opened 不动既有属性）；新建对象的
  创建时间同样会持久化到旁路库，改名/删除会把旁路元数据一并迁移或清理，
  不再残留孤儿记录。
- **流的删除粒度是单个流**（v0.4.x 起）：对 `file.txt:stream` 句柄做删除
  （delete-on-close 或 SET_INFO FileDisposition）只删那一个流，不再连带删掉基础文件
  与其他流。配套语义：带创建意图（OPEN_IF 等）打开流时若基础文件不存在会自动建出
  空基础文件；SUPERSEDE/OVERWRITE* 打开时会清掉残留的 Apple 元数据流
  （FinderInfo 等），避免截断后的「新」文件仍显示旧的颜色标签。

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

### 缓存与变更通知（v0.6.0 新增）

- **异步变更通知（`CHANGE_NOTIFY`）**：请求挂起、目录变更时补发
  `FILE_NOTIFY_INFORMATION`，支持 `SMB2_WATCH_TREE` 递归与 `CompletionFilter` 过滤。
  变更由**命令层记账**（自己的写路径），不用 inotify —— 理由见
  [已知限制](#notes) 第 4 条。
- **oplock / lease**：`server.oplocks: true` 时授予 oplock（II / EXCLUSIVE / BATCH）
  与租约（RqLs v1 / v2），并在冲突时发 break 通知**等确认**后才放行新的打开。
  **默认关闭**，理由与边界见[已知限制](#notes) 第 6 条。
- **异步未决请求表**：`CHANGE_NOTIFY` 与阻塞锁共用。请求挂起后读循环照常收帧，
  因此 `CANCEL` 能真正取消一个正在等待的请求（此前 `CANCEL` 只能静默丢弃 ——
  不存在可被取消的未决请求）。

### 服务发现

三套协议**全部进程内实现**，不 fork/exec 任何外部守护进程，也不依赖
`avahi` / Bonjour / `systemd-resolved` / `nmbd` / `wsdd`（AGENTS.md C3 / C4）。
它们覆盖不同的客户端，各开各的：

| 协议 | 端口 | 让谁能发现 | 配置段 | 默认 |
|---|---|---|---|---|
| mDNS / DNS-SD | UDP 5353 | macOS / Linux（Finder、文件管理器） | `mdns.enabled` | `true` |
| **WS-Discovery**（v0.7.0 新增） | UDP 3702 + TCP 5357 | **Windows「网络」** | `ws_discovery.enabled` | `false` |
| **NetBIOS**（v0.7.0 新增） | UDP 137 / 138 | **`\\NAME` 解析、网上邻居浏览列表** | `netbios.enabled` | `false` |

后两者默认关闭，理由见「[已知限制与说明](#notes)」。

#### mDNS / DNS-SD

在 `224.0.0.251:5353` / `[ff02::fb]:5353` 上收发报文，广播：

- `_smb._tcp` —— 基础 SMB 服务发现；
- `_device-info._tcp` —— Apple 扩展（Finder 图标由 `apple.model` 决定）；
- `_adisk._tcp` —— Time Machine 磁盘宣告（需 `apple.advertise_time_machine: true`
  且至少有一个共享设了 `time_machine: true`）。

#### WS-Discovery（Windows）

在 `239.255.255.250:3702` / `[ff02::c]:3702` 上收发组播，应答 `Probe` →
`ProbeMatch`、`Resolve` → `ResolveMatch`，把自己声明为 `wsdp:Device` +
`pub:Computer`（后者才是 Windows 显示成"一台电脑"的依据）。
另在 TCP 5357 提供 WS-Transfer `Get` 端点，供 Windows 拉取设备详情。

设备 UUID 按名字派生（UUIDv5），**跨重启稳定** —— 否则每次重启都会在
Windows 网络列表里留下一个新的同名条目。

#### NetBIOS（nmbd 的核心子集）

在 UDP 137 应答名字查询（`\\NAME` 解析）与节点状态（相当于 `nbtstat -A`），
在 UDP 138 周期性发主机宣告，把自己加进浏览列表。

**刻意不做** Samba nmbd 的全量功能：WINS 服务器、浏览主控选举、域主控浏览、
名字注册冲突仲裁。那些属于"参与一个 NetBIOS 工作组的治理"，范围远超
"让别人能看见我"。也因此**不声明自己是主控浏览器** —— 谎报会让别的机器
真的来问我们要浏览列表。

### Apple 扩展（AAPL）

- `AAPL` create context：server query / volume caps / model info 协商已实现；
  `readdir_attr`（目录项携带 FinderInfo / 资源派生大小）在客户端请求且后端能提供
  Apple 元数据时启用。
- 稀疏文件 FSCTL 三件套已实现（见上），支持 `.sparsebundle` 的打洞与回收。
- `FLUSH` 走强制刷盘（`F_FULLFSYNC` 语义），并在标了 `time_machine` 的共享上通过
  AAPL VolumeCapabilities 宣告 `SUPPORTS_FULL_SYNC`。

---

## Time Machine 状态 <a name="timemachine"></a>

> **截至 v0.4.0（含），本项目从未跑过一次真实的 Time Machine 备份，更没有做过恢复**——
> 开发环境里没有 macOS，「备份并成功恢复」一次都没有发生过（v0.2.0 时如此，
> 此后三个版本也没有补上这个验证）。
> 下面列的是「服务端前置能力已实现并实测」，**不等于「Time Machine 能用」**：前者是
> 对服务端行为的探针，后者需要真机端到端验证。
>
> **2026-08-09：项目所有者已把 TM 真机验收降为可选项**，不再是发布阻塞项
> （原因是没有真机环境与时间）。**降的是验收要求，不是功能**——下面列的 Apple 扩展
> 代码全部保留并继续跑测试。这也意味着**这一节的结论短期内不会有新证据**，
> 请按下面标注的证据强度自行判断，不要因为版本号往前走就推断它变可靠了。

**证据强度分档**（本节所有条目按此标注，避免把不同强度的东西读成一回事）：

| 档 | 含义 |
|---|---|
| **A** | macOS 真机端到端实测 —— **本项目目前一条都没有** |
| **B** | 第三方 SMB 客户端（impacket / smbclient / go-smb2）协议级实测通过 |
| **C** | 仅本仓库单元测试覆盖 |
| **D** | 代码存在，但没有任何一关执行过它 |

服务端侧的多项 Time Machine 前置能力已实现，并用 impacket 低阶 SMB2 客户端逐项实测通过
（下列均为 **B 档**）：

- `AAPL` 协商（server query / readdir_attr）；
- 命名流与 Alternate Data Stream（含目录上的流，`.sparsebundle` 依赖）；
- 稀疏文件 FSCTL 三件套（`SET_SPARSE` / `SET_ZERO_DATA` / `QUERY_ALLOCATED_RANGES`）；
- 卷容量与 `quota_bytes` 上报；
- 大目录枚举性能（v0.4.0 基线：5 万条**热**枚举约 0.34 s、约 6–7 µs/条且线性扩展；
  更早的 v0.2 实测约为 0.28 s。见 `test/reports/perf-v040-20260825.md`，loopback 口径）；
- `_adisk._tcp` mDNS 广播。

`F_FULLFSYNC`：Linux / Windows 分支实测通过（**B 档**）；**Darwin 的 `F_FULLFSYNC` 分支
在本容器里编不了也跑不了，只有交叉编译通过做保证**（**D 档**）。

**v0.2.0 新增：durable / persistent handle v1 / v2 已实现**（授予、断线后重连认领、
超时回收；PR #20，缺陷修复 PR #40）。这是上一版这里列为「对 TM 影响最大的缺口」的那一项，
**现已不再是缺口**。
证据强度 **B 档**：单元测试 + impacket 线级用例（手工拼 create context）验证了
授予 / 重连 / 超时三条路径；**没有** macOS 真机长跑断线恢复的证据。
也就是说已验证的是「握手与重连协议正确」，**不是**「真实备份过程不会中断」。

但以下能力仍缺，可能导致备份不稳定甚至失败（按对 Time Machine 的实际影响排序）：

- **oplock / lease** —— v0.6.0 已实现，但**默认关闭**（`server.oplocks: false`）。
  保持默认时客户端仍退化为不缓存，`.sparsebundle` 的 band 目录那种小文件密集写
  吞吐明显低于 Samba；不影响正确性。打开能改善吞吐，代价与三条边界见
  「[已知限制与说明](#notes)」第 6 条。
  **注意：这是这一节的结论里唯一在近期发生过实质变化的项，但变化的是"有了开关"，
  不是"验证过了" —— 本项目至今没有 macOS 真机数据，打开后的真实表现未知。**
- **AAPL resolveID** —— 对 TM 本身**无实际影响**（我们不宣告 `kAAPL_SUPPORT_RESOLVE_ID`，
  客户端就不会使用），仅 Finder 的别名 / 最近项目按 64 位 file id 反查路径会退化为按路径查找。

### 使用须知

- **不要拿真实备份数据试。** 请仅用测试数据（或一台可随时清空的机器）试用，确认能完成
  一轮完整备份并成功浏览快照后，再考虑放真实数据。**请勿把它作为唯一一份备份的目的地。**
- **最可能的失败模式是稳定性，不是连不上。** oplock / lease 默认关闭，客户端退化为不缓存，
  长时间大体量备份（`.sparsebundle` band 目录海量小文件）下的表现未知——可能慢，
  也可能中途报错。
  **所有 macOS 版本均未经实测**，不要写成「某版本有点抖」这类暗示我们试过的口吻。
- **备份共享务必显式设 `quota_bytes`。** 不设时上报宿主真实剩余空间，Time Machine 会一路
  写满磁盘。注意：`quota_bytes` 上报的**可用空间 = 配额 − 宿主卷已用空间**（出于性能不
  递归统计共享自身占用），因此 **`quota_bytes` 必须大于「宿主卷已用空间 + 期望备份体积」**，
  否则即便共享是空的，客户端也会看到可用空间为 0 而**直接拒绝开始备份**。（`quota_bytes`
  小于 1 GiB 时启动会告警；在过小的卷上 TM 会反复失败。）
- 若备份失败，请提供服务端 `log.level: debug` 的日志（含每个 SMB 命令与 NTSTATUS），
  这比 macOS 侧报错更有用。

逐项验证证据见 [Time Machine 支持状态](docs/timemachine-status.md)
（⚠️ 该文档的 A/B/C 定级依据未被本轮文档审计反向核验，引用时请自行复核）。

Time Machine 未通过验收**不影响普通文件共享功能**——后者是阶段一的目标，
已由 smbclient / impacket / go-smb2 三家客户端实测覆盖。

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

4. **目录变更的自动刷新只覆盖本服务自己的写（v0.6.0 起）**：`CHANGE_NOTIFY` 已实现，
   但变更是**在命令层记账**的 —— 只有经本服务写路径（CREATE / WRITE / SET_INFO /
   CLOSE 删除）产生的变化会通知客户端。**宿主上其它进程直接改动共享目录**（`touch`、
   `rm`、另一个服务实例）不会触发通知，客户端会因此少一次自动刷新，但不会出错
   （它本来就有轮询兜底）。
   不用 inotify / kqueue / ReadDirectoryChangesW 的理由见[支持能力](#capabilities)
   与 `internal/smb/command/notify_hub.go` 的文件头注释。

5. **单文件语义**：本服务是**文件共享**，不做打印机共享、不做域控、不做 DFS。

6. **oplock / lease 默认关闭，且有三条已知边界**：`server.oplocks`（默认 `false`）。
   打开后 `CREATE` 会授予 oplock / lease，`NEGOTIATE` 宣告 `SMB2_GLOBAL_CAP_LEASING`。

   - **为什么默认关**：授予缓存许可等于许可客户端把读写缓存在本地，服务端必须在
     别的客户端动这个文件时先打破它**并等确认**。这类实现错误的表现是另一个客户端
     读到旧内容 —— **静默的脏数据**，没有任何一方会报错。本特性尚未在 Windows /
     macOS 真机上验收，在此之前保守一侧是正确的默认。
   - **边界一**：复合链中间的 `CREATE` 不授予（它需要挂起通路才能在冲突时等确认，
     而异步响应是单发的）。现实里 Windows 资源管理器的 `[CREATE, QUERY_INFO, CLOSE]`
     这类短链拿不到缓存许可，不影响正确性。
   - **边界二**：极少数「第二个客户端用**复合链**打开一个已被缓存许可的文件」的情形
     会收到 `STATUS_SHARING_VIOLATION`。这是刻意的选择：不能等确认却放行，就是放行
     一次会读到脏数据的访问，那比一次可重试的失败糟得多；重试即可成功。
   - **边界三**：等确认的上限是 30 秒（与 Samba 的超时后强行推进同款），超时按
     「已打破」处理并放行被推迟的打开。

7. **WS-Discovery 与 NetBIOS 都默认关闭，且没有 Windows 真机验收**（v0.7.0 新增）。

   - **为什么默认关**：这两套协议都是"在局域网里大声报出自己"的行为，
     默认打开等于替所有用户决定了要占 3702 / 5357 / 137 / 138 这些端口、
     并向网段内广播主机信息。开发容器里没有 Windows，这两项**没有真机验收**，
     所以保守一侧是正确的默认。
   - **验证强度**：只到**协议级**（`nmblookup` 4.22 实测名字查询/组名/节点状态
     全部正确；手写 WS-Discovery 客户端实测 `Probe`→`ProbeMatch`、`Resolve`→
     `ResolveMatch`、WS-Transfer `Get` 全部正确）。**没有**在任何真实 Windows
     或 macOS 上确认过"网络里确实能看见"。
   - **137 / 138 是特权端口**，非 root 时绑不上（处置见上面「服务发现」小节的提示）。
   - **NetBIOS 只实现了 nmbd 的子集**：不做 WINS 服务器、浏览主控选举、
     域主控浏览、名字注册冲突仲裁。若你的网络依赖 NetBIOS 浏览主控选举来
     汇总列表，本服务**不参与**选举 —— 它只会宣告自己，不会替别人维护列表。

<!-- BEGIN-OSCAP-WIRING-STATUS-README：本条与 CHANGELOG 的同名块、configs/example.yaml 的
     同名段、AGENTS.md §1.2 的同名块是**一套四处**，接线 PR 合入后四处都要改，
     别只改 CHANGELOG 那一处。
     全部落点一次找齐：grep -rn OSCAP-WIRING-STATUS . | grep -v '^./history/' -->
8. **`filesystem_mode` 对 6 项 OS 能力全部生效（v0.3.0 起 6/6；v0.2.0 时仅 2/6），
   取值为 `auto` / `portable` 两态**：
   **六项能力（扩展属性 xattr / 命名流 / 稀疏文件 / 稳定 FileID / 创建时间 / DOS 属性）
   都已接进 VFS 数据路径**，两态行为对全部六项**确实不同**：
   `portable` 整机不碰宿主可选能力（数据落自带旁路存储），`auto` 在支持的宿主上逐项优先走原生并降级。
   自行复算：`go list -deps ./cmd/stupidsamba | grep -c oscap` = `3`
   —— `internal/oscap`、`oscap/native`、`oscap/builtin` 都被链进了二进制（v0.2.0 接线前是 `1`）。
   六项调用点核查：`grep -rn 'caps\.\(Xattr\|Streams\|Sparse\|StableFileID\|CreationTime\|DOSAttributes\)()' --include='*.go' . | grep -v '^./internal/oscap/' | grep -v '_test.go' | grep -v '//'`
   （只证明有调用点，不证明端到端跑通；端到端验证强度见 CHANGELOG v0.3.0 段）。

   ⚠️ **第三态 `native` 已在 v0.5 开发版移除**（项目所有者 2026-08-25 拍板）：旧配置写了
   `filesystem_mode: native` 会启动报错并建议改用 `auto` 或 `portable`。移除原因是它的契约
   「全部能力强制走原生、缺一项启动即报错」在任何平台都无法满足——每个平台都至少有一项能力被
   源码硬编码为不支持（linux/darwin 的 DOS 属性位、darwin 另缺稀疏文件、windows 缺 xattr、
   其余平台六项全无），三平台恒定启动失败，「钉死原生路径」的测试需求由 `auto` +
   生效矩阵断言覆盖（见 vfs 测试的 requireKind 做法）。默认值本来就是 `auto`，
   未显式写过 `native` 的用户不受影响。
   详见 [`CHANGELOG.md`](CHANGELOG.md) 的 Unreleased 段与 `BEGIN-OSCAP-WIRING-STATUS` 块。

   > **历史（v0.2.0 时仅 2/6）：** 彼时只有扩展属性（xattr）与命名流两项接进数据路径，
   > 其余四项（稀疏文件 / 稳定 FileID / 创建时间 / DOS 属性）各取值跑起来行为完全一样，
   > 在 FAT32/exFAT 外置盘、`nouser_xattr` 挂载、只读根这类宿主上照旧静默丢失。
   > v0.3.0 由 vfs-sparse / vfs-attr 角色把四项也接进后，本段更新为 6/6；请勿把本历史段
   > 当成当前状态。
<!-- END-OSCAP-WIRING-STATUS-README -->

---

## 客户端测试矩阵 <a name="matrix"></a>

本项目以至少三种第三方 SMB 客户端验证（AGENTS.md §3），并尽量覆盖各平台原生客户端：

| 客户端 | 状态 | 说明 |
|---|---|---|
| `smbclient`（Samba CLI） | ✅ 已实测 | `ls` / `put` / `get` / `mkdir` / `rm` / `rmdir`、各方言、加密、`nt_hash` 登录均通过 |
| `impacket`（Python） | ✅ 已实测 | 客户端矩阵第 3 项；对 v0.3.0 黑盒复测 11/11 通过（认证拒绝、全操作集、8MB md5 回环、只读强制） |
| `go-smb2`（纯 Go 客户端） | ✅ 已实测 | 对 v0.3.0 独立客户端 9 步 + 仓库集成套件 21/21 全部通过（含加密与只读强制） |
| macOS Finder / `mount_smbfs` | 🎯 目标 | Apple 扩展（AAPL / `_adisk` / `readdir_attr`）为其服务；Time Machine 见[上](#timemachine) |
| Windows 资源管理器 | 🎯 目标 | 签名、`guest` 策略、属性页 |
| `mount.cifs`（Linux 内核客户端） | ⏭️ 本环境跳过，未实测 | 开发容器跑在**非初始 user namespace** 里（`cat /proc/self/uid_map` = `0 1000 1`），内核只放行带 `FS_USERNS_MOUNT` 标志的文件系统，而 cifs 没有这个标志，于是 `mount error(1): Operation not permitted`。**与 `CAP_SYS_ADMIN` 无关**——`--cap-add SYS_ADMIN` 下重跑仍然失败，同容器 `mount -t tmpfs` 却成功（反向对照）。这是环境限制不是服务端缺陷，但我们**没有**在真实 Linux 主机上验证过，所以这里不写「✅ 支持」。`scripts/acceptance.sh` 把它记为 skip(rc=77) |

> 想核实「你说支持，凭什么」？完整的多客户端验收报告见
> [`docs/acceptance-v0.1.0.md`](docs/acceptance-v0.1.0.md)（smbclient / impacket / go-smb2
> 三家客户端栈逐项实测，含加密 fail-closed 验证），基线 commit `5428bcd`。
> 更近的黑盒复测见 [`test/reports/client-matrix-v030-20260825.md`](test/reports/client-matrix-v030-20260825.md)
> （对 v0.3.0：smbclient 七种方言 + 全操作集、impacket 11/11、go-smb2 集成套件 21/21）；
> 性能基线见 [`test/reports/perf-v040-20260825.md`](test/reports/perf-v040-20260825.md)
> ——注意那是 **loopback 协议栈基线，不是真实网络吞吐**：4 vCPU 容器里单流大文件约
> 写 95 MB/s / 读 200 MB/s（SMB3.1.1+签名），并发 16 流聚合读约 716 MB/s，
> `auto` 与 `portable` 两档在本机负载下性能几乎无差。

---

## 许可证

**许可证待定。** 本项目仍处于内部测试阶段，尚未确定正式开源许可证。
在许可证明确之前，请勿将代码用于未获授权的分发或再发布。

（v0.2.0 起 Release 渠道按 tag 名的 SemVer 判定，`v0.2.0` 这样不带连字符的 tag 走正式版
通道。**渠道不等于成熟度**：内部测试阶段这一判断没有变。）

---

## 构建与自检

发布前必须本地跑过（提交纪律，CI 也会校验）：

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...

# 推送前还要跑这一关：go build **不编译 `_test.go`**，只跑上面三条会漏掉
# 「测试文件写坏了但四平台交叉编译照样绿」的情况（本仓库真出过一次）。
# 它对全部已注册 build tag × 四个目标平台跑 go vet，约 18 秒。
sh test/ci/check-test-compile.sh
```

四平台发布物用 `scripts/build-release.sh` 一键产出（产物落在 `dist/`，含
`SHA256SUMS`）。详见该脚本头部注释。

---

## 相关文档
- [`codebuddy-files/readme.md`](codebuddy-files/readme.md) —— 必须执行落实的操作，否则一定会带来巨大的代价
- [`configs/example.yaml`](configs/example.yaml) —— 逐字段注释的完整配置示例
- [`CHANGELOG.md`](CHANGELOG.md) —— 各版本变更记录
- [`docs/acceptance-v0.1.0.md`](docs/acceptance-v0.1.0.md) —— v0.1.0 多客户端验收报告（smbclient / impacket / go-smb2 实测矩阵、加密 fail-closed 验证方法）
- [`test/reports/client-matrix-v030-20260825.md`](test/reports/client-matrix-v030-20260825.md) —— 对 v0.3.0 的三客户端黑盒复测报告
- [`test/reports/perf-v040-20260825.md`](test/reports/perf-v040-20260825.md) —— v0.4.0 性能基线（loopback，非真实网络吞吐）
- [`docs/timemachine-status.md`](docs/timemachine-status.md) —— Time Machine 逐项验证证据（A/B/C 定级）
- [`docs/protocol-notes.md`](docs/protocol-notes.md) —— 协议研究笔记（实现依据）
- [`docs/dev-workflow.md`](docs/dev-workflow.md) —— 开发工作流 SOP（独立 worktree + 独立分支 + PR），参与开发前必读
