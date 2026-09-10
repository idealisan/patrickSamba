# 测试基础设施调研（v0.2.0）

> 本文回答一个问题：**要把 stupidSamba 在 Linux / Windows / macOS 三侧都验收到位，
> 需要什么样的机器、什么样的权限、跑什么样的流水线。**
>
> 面向对象是「照着搭环境的人」，不是「了解技术全貌的人」。每条结论都标注了证据等级：
>
> | 标记 | 含义 |
> |---|---|
> | **【实测】** | 在本开发容器里跑过命令，输出贴在文中 |
> | **【查文档】** | 来自官方文档，附出处链接 |
> | **【推断】** | 从前两者推导，**未经验证**，落地前需自行确认 |
> | **【未查证】** | 试过但没查到，已说明查过哪里 |

---

## A. 本开发容器的能力边界（一切结论的地基）

先把地基测清楚，否则后面所有方案都是空谈。

### A.1 硬件与内核

**【实测】** 2026-08-09，容器 `327c94b1396b`：

```
$ nproc
2

$ free -m
               total        used        free      shared  buff/cache   available
内存：          4096        1278        1285           0        1538        2817

$ df -h /
文件系统        大小  已用  可用 已用% 挂载点
overlay         256G  3.5G  253G    2% /

$ uname -a
Linux 327c94b1396b 5.4.241-1-tlinux4-0025.10 #1 SMP Wed Jul 29 12:22:28 CST 2026 x86_64 GNU/Linux

$ cat /etc/os-release | head -3
PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
```

| 项 | 实测值 |
|---|---|
| CPU | **2 核** |
| 内存 | 4 GiB 总量，**约 2.8 GiB 可用** |
| 磁盘 | overlay，256 GiB，可用 253 GiB（充裕，不是瓶颈） |
| 内核 | 5.4.241 tlinux4 |
| 发行版 | Debian 13 trixie |
| Go | go1.25.0（**不在 PATH**，需 `export PATH=$PATH:/usr/local/go/bin`） |

### A.2 虚拟化：**不可能**，而且原因比"没装软件"更硬

**【实测】**

```
$ ls -l /dev/kvm
ls: 无法访问 '/dev/kvm': 没有那个文件或目录

$ grep -c -E 'vmx|svm' /proc/cpuinfo
0
```

`/proc/cpuinfo` 里 **一条 vmx/svm 都没有** —— 这不是"宿主没把 `/dev/kvm` 透进来"，
而是**这颗（虚拟）CPU 根本没有暴露虚拟化扩展**。即使有人把 `/dev/kvm` 挂进来也没用，
KVM 需要 CPU 的 VT-x/AMD-V 支持才能工作。

> **结论（实测）：本开发容器永远跑不了硬件加速虚拟机。**
> 这是一条**不可通过配置修复**的限制，不要在这上面花时间。

### A.3 Capability：缺的不只是 `CAP_SYS_ADMIN`

**【实测】** `CapEff = 0x00000000a80c05fb`，逐位解出：

| | capability |
|---|---|
| **有** | CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, SETGID, SETUID, SETPCAP, **NET_BIND_SERVICE**, SYS_CHROOT, SYS_PTRACE, MKNOD, AUDIT_WRITE, SETFCAP |
| **缺** | DAC_READ_SEARCH, LINUX_IMMUTABLE, NET_BROADCAST, NET_ADMIN, NET_RAW, IPC_LOCK, IPC_OWNER, SYS_MODULE, SYS_RAWIO, SYS_PACCT, **SYS_ADMIN**, SYS_BOOT, SYS_NICE, SYS_RESOURCE, SYS_TIME, SYS_TTY_CONFIG, LEASE, AUDIT_CONTROL, MAC_*, SYSLOG, WAKE_ALARM, BLOCK_SUSPEND, AUDIT_READ, PERFMON, BPF, CHECKPOINT_RESTORE |

两条有用的推论：

- **`CAP_NET_BIND_SERVICE` 是有的** —— 也就是说本容器里**可以**直接监听 445 端口，
  验收脚本用 4445 这类高位端口只是为了多 agent 并行不打架，不是权限所迫。
- **`CAP_NET_RAW` 缺失** —— 装不了 `tcpdump`/`tshark` 那种抓原始包的路子。
  这解释了为什么项目里用的是应用层中继探针 `scripts/clients/smbtap`
  而不是抓包：**不是设计偏好，是环境所迫**。这个取舍是对的，别改回去。

### A.4 Docker：**能用**，但和你想的不一样

**【实测】** Docker CLI 29.6.2 + 可用的 `/var/run/docker.sock`，容器能起：

```
$ docker run --rm alpine:latest sh -c 'echo HELLO_FROM_NESTED; grep CapEff /proc/self/status'
HELLO_FROM_NESTED
CapEff:	00000000a80405fb
```

但有三条**必须知道**的限制：

#### （1）它是 sibling daemon，**不共享文件系统**

**【实测】** 宿主写一个标记文件，再 bind mount 同一路径进去：

```
$ echo "TESTMARKER-$$" > /tmp/probe-marker.txt
$ docker run --rm -v /tmp:/mnt alpine:latest sh -c 'echo count=$(ls /mnt | wc -l); ls /mnt/probe-marker.txt'
count=1
ls: /mnt/probe-marker.txt: No such file or directory
```

`-v /tmp:/mnt` 挂进去的是 **daemon 那一侧的 `/tmp`**，不是我们的 `/tmp`。
daemon 跑在另一个容器（`300abe6e2349`）里，与我们同内核、不同文件系统。

> **实操影响：`-v $PWD:/src` 这种惯用写法在本环境里是无声失效的 ——
> 目录会挂上，但内容是空的。** 要把文件送进容器只能用 `docker cp`
> 或 `docker build`（把文件打进镜像）。本文后面的实验都用 `docker cp`。

#### （2）`--privileged` 起不来，但 `--cap-add` 可以

**【实测】**

```
$ docker run --rm --privileged alpine:latest true
docker: Error response from daemon: failed to create task for container: ...
error mounting "sysfs" to rootfs at "/sys": ... operation not permitted

$ docker run --rm --cap-add SYS_ADMIN alpine:latest sh -c 'grep CapEff /proc/self/status'
CapEff:	00000000a82405fb     # bit21 SYS_ADMIN 已置位
```

`--privileged` 因为要重挂 `/sys` 而失败；单独 `--cap-add SYS_ADMIN` 反而成功。
**但下一条说明这个 SYS_ADMIN 并没有用。**

#### （3）整个环境在 **user namespace** 里 —— 这才是 mount.cifs 的真正死因

AGENTS.md §10.3 第 2 条把 `mount.cifs` 跑不通归因于「缺 `CAP_SYS_ADMIN`」。
**这个归因不完整。** 本轮做了一次决定性实验：

在一个 `--cap-add SYS_ADMIN` 的 Debian 12 容器里装好 `cifs-utils`，
把 stupidsamba 二进制 `docker cp` 进去，服务端和客户端都在同一个容器内（走 127.0.0.1）：

```
--- server log ---
level=INFO msg="SMB 服务已监听" addrs=127.0.0.1:4459 dialects=2.0.2..3.1.1 shares=public
--- mount attempt ---
mount error(1): Operation not permitted
mount_rc=32
```

继续排查根因：

```
$ grep -i -E "cifs|smb" /proc/filesystems
nodev	cifs
nodev	smb3                      # ← 内核**有** cifs，模块已加载，不是缺模块

$ mount -t tmpfs none /mnt/t && echo tmpfs_mount_OK
tmpfs_mount_OK                    # ← mount() 系统调用本身**是通的**

$ cat /proc/self/uid_map
         0       1000          1  # ← 我们在一个 user namespace 里，root 被映射到宿主 uid 1000
         1       1001      65536
```

三条放在一起，结论就唯一了：

> **【实测 + 内核语义推断】** 在非初始 user namespace 里，内核只允许挂载带
> `FS_USERNS_MOUNT` 标志的文件系统（tmpfs / proc / sysfs / devpts / ramfs / fuse 等）。
> **`cifs` 没有这个标志**，所以 `mount -t cifs` 返回 `EPERM`
> —— 与是否持有 `CAP_SYS_ADMIN` **无关**。
>
> 这意味着：`--cap-add SYS_ADMIN`、`--privileged`、给容器加任何 capability，
> **都不可能让 mount.cifs 在本环境跑通**。这条路是死的，不要再试了。

对照实验（tmpfs 挂载成功）排除了"mount 被 seccomp 全局禁掉"这个替代解释——
**探针有反向对照，不是恒红的摆设**。

#### （4）连 FUSE 逃生通道也被堵死

`fuse` 自内核 4.18 起带 `FS_USERNS_MOUNT`，理论上是 user namespace 里唯一能挂
网络文件系统的路子（例如 `rclone mount` 的原生 Go SMB 后端）。实测不行：

```
$ grep -w fuse /proc/filesystems
nodev	fuse                      # ← 文件系统注册了

$ ls -l /dev/fuse
ls: 无法访问 '/dev/fuse': 没有那个文件或目录     # ← 但设备节点不存在

$ mknod /tmp/fusedev c 10 229
mknod: /tmp/fusedev: 不允许的操作                # ← 且造不出来（device cgroup 拦截）
```

**【推断】** 在一台**普通 Linux 主机**上（不在 user namespace 里），
`docker run --device /dev/fuse --cap-add SYS_ADMIN` 就能让 FUSE 客户端跑起来。
本环境不行是本环境的问题。见 §D 的「标准档」。

### A.5 已装 / 未装工具清单

**【实测】**

| 工具 | 状态 | 备注 |
|---|---|---|
| `smbclient` | ✅ 已装 | Samba 官方 CLI，验收矩阵第 1 家 |
| `python3` | ✅ 已装 | impacket 用，第 3 家 |
| `go` | ✅ `/usr/local/go/bin/go` 1.25.0 | **不在 PATH** |
| `gcc` / `make` / `git` | ✅ 已装 | |
| `docker` | ✅ 29.6.2 | 限制见 §A.4 |
| `apt-get` | ✅ 可用，能联网装包 | |
| `tshark` / `tcpdump` | ❌ 未装 | 装了也没用，缺 `CAP_NET_RAW` |
| `qemu-*` | ❌ 未装 | 见 §A.6 |
| `smbtorture` | ❌ 未装 | 见 §B |
| `wine` | ❌ 未装 | 见 §B |
| `mount.cifs` | ❌ 未装，**且装了也跑不通** | 见 §A.4(3) |

### A.6 QEMU TCG（纯软件模拟）：**能跑，但只够跑 Linux 客机**

**【实测】** `apt-get install -y --no-install-recommends qemu-system-x86` 装得上
（约 12 MB deb，10 秒装完），版本 **QEMU 10.0.11**。TCG 不需要 `/dev/kvm`、
不需要 `CAP_SYS_ADMIN`，纯用户态跑，**在本容器里是可用的**。

用 Alpine 3.21 virt ISO（66 MB）测启动到 login 提示符的耗时：

```
$ qemu-system-x86_64 -accel tcg -m 1024 -smp N -cdrom alpine.iso -nographic -display none
accel=tcg smp=2 boot_to_login=34s
accel=tcg smp=1 boot_to_login=22s
```

（日志里能看到 `Welcome to Alpine Linux 3.21` / `localhost login:`，确认真的起来了。）

两点值得记下来：

- **`-smp 2` 比 `-smp 1` 慢**（34s vs 22s）。TCG 下多核要做跨 vCPU 的内存序同步，
  在只有 2 个物理核的宿主上是净亏。**跑 TCG 一律用 `-smp 1`。**
- 同样的 Alpine virt ISO 在 KVM 上通常 2~3 秒起来 → **TCG 约慢 8~11 倍**。

**【推断】** 按 8~11 倍外推到 Windows（**未实测，下面的数字不要当承诺**）：

| 操作 | KVM 参考值 | TCG 外推 |
|---|---|---|
| Windows 10 安装 | ~20 分钟 | **3~7 小时** |
| Windows 10 冷启动到桌面 | ~30 秒 | **5~15 分钟** |

而且外推还偏乐观 —— 本容器只有 **2 核、约 2.8 GiB 可用内存**：
Windows 10 官方最低 2 GiB（实际体验需 4 GiB），Windows 11 硬性要求 4 GiB + TPM 2.0，
**内存这一关就过不去**。

> **结论：本容器可以用 QEMU TCG 跑 Linux 客机，但跑 Windows 客机做 SMB 验证不现实。**
> 若确实要用 TCG 跑 Windows，应当在一台内存 ≥ 16 GiB 的机器上做，并且只用于
> **一次性录制**（装好后存成 qcow2 快照，之后每次从快照恢复，别每次重装）。
>
> **TCG 的真正用武之地是 Linux 客机**：需要一个"能 `mount -t cifs` 的干净内核"时，
> 用 TCG 起一个 Alpine 客机，`-netdev user` 让客机通过 `10.0.2.2` 访问宿主上的
> stupidsamba，就能绕开 §A.4(3) 的 user namespace 限制。
> **【推断，未实测】** 这条路子在本容器里理论上可行（TCG 客机有自己的完整内核，
> 不在宿主的 user namespace 里），但需要给 Alpine 做自动登录 + 自动执行脚本，
> 属于后续可做的工作，本轮未验证。

---

## A 组小结（一句话版）

| 问题 | 答案 |
|---|---|
| 能跑硬件加速 VM 吗？ | **不能，且不可修复**（CPU 无 vmx/svm，无 `/dev/kvm`） |
| 能跑 Docker-in-Docker 吗？ | **能**，但 daemon 是 sibling，**bind mount 宿主路径无效**，只能 `docker cp` |
| 能跑 mount.cifs 吗？ | **不能，且不可修复**（在 user namespace 里，cifs 无 `FS_USERNS_MOUNT`） |
| 能跑 FUSE 客户端吗？ | **不能**（无 `/dev/fuse`，且 `mknod` 被拦） |
| 能监听 445 吗？ | **能**（有 `CAP_NET_BIND_SERVICE`） |
| 能抓包吗？ | **不能**（无 `CAP_NET_RAW`），只能用应用层中继探针 `smbtap` |

---

## B. Windows 客户端怎么测

### B.0 先说结论：两张清单

这是本节的产出，也是"要不要买机器"的决策依据。

#### 清单一：**必须真 Windows** 才能测的（买机器的理由）

| # | 事项 | 为什么替代不了 | 严重度 |
|---|---|---|---|
| B-W1 | **stupidSamba 自身在 Windows 上运行** | `internal/vfs/*_windows.go`、`internal/mdns/sockopt_windows.go`、win-meta 的 MetadataStore 全是 Windows-only 代码路径。交叉编译只证明"能编译"，不证明"能跑"。见 §B.1 —— **仓库里已经有 8 个 Windows-only 测试函数从未被执行过** | **最高** |
| B-W2 | Windows 自带 SMB 重定向器（`mrxsmb.sys`）的真实报文行为 | 它是内核驱动，Wine 没有（§B.4 实测），QEMU 无 KVM 跑不动（§A.6）。Samba 的 `libsmbclient` 是**另一套独立实现**，报文模式（compound 组合、create context 顺序、DFS 探测、lease key 复用）与微软实现不同 | 高 |
| B-W3 | Windows 11 24H2 的**客户端侧默认策略** | 24H2 默认要求 SMB 签名、默认封禁 guest 回退。我们只能在 Linux 侧模拟"客户端要求签名"（§B.3 已实测通过），但**模拟的是 Samba 对该策略的理解，不是微软的实现** | 中 |
| B-W4 | 资源管理器 UI 层行为 | 属性页、"以前的版本"标签、缩略图（会去读 `desktop.ini` / 生成 `Thumbs.db`）、右键菜单、离线文件。这些是 Explorer 而非协议栈的行为，无第三方等价物 | 中 |
| B-W5 | `robocopy` / `xcopy` 的服务端卸载路径 | 当服务端宣告支持时 robocopy 会走 `FSCTL_SRV_COPYCHUNK`。我们**目前没实现**（§B.2 smbtorture 实测 21 个用例挂在这上面），修完后需要真 robocopy 验一次 | 中 |
| B-W6 | 真 SSPI 的 NTLMv2（含 MIC、通道绑定 EPA） | LSA 生成的 NTLM token 与 Samba 客户端生成的不完全一样（尤其 AV_PAIR 集合、MsvAvChannelBindings）。这是**认证被拒**类问题的高发区 | 中 |
| B-W7 | 驱动器映射持久化 / `net use` / UNC 直接访问 | `\\host\share` 路径解析、凭据管理器交互 | 低 |

**B-W1 是唯一一条"没有它就交付不了"的**。其余 6 条都是"没有它质量有风险"。
如果只肯为一件事买机器，那就是为 B-W1 —— 因为 win-vfs（#12）和 win-meta（#11）
两个任务正在写的代码，**现在没有任何手段能证明它跑得起来**。

#### 清单二：**不需要真 Windows** 就能覆盖的（省钱的部分）

| # | 事项 | 用什么替代 | 证据 |
|---|---|---|---|
| B-L1 | SMB2/3 协议合规性（331 个用例） | `smbtorture`（Samba 官方一致性套件） | §B.2 **【实测】** |
| B-L2 | 签名协商与校验 | `smbclient --option="client signing=required"` | §B.3 **【实测】** |
| B-L3 | SMB3 加密 | `smbclient --client-protection=encrypt` + `smbtap` 线级探针 | §B.3 **【实测】** |
| B-L4 | guest 策略（Win11 默认拒绝 guest） | `smbclient -N` 断言 `LOGON_FAILURE` | §B.3 **【实测】** |
| B-L5 | 全部 5 个方言的协商 | `smbclient -m SMB2_02/SMB2_10/SMB3_00/SMB3_02/SMB3_11` | §B.3 **【实测】** |
| B-L6 | NTLMv2 报文级正确性 | impacket（独立 Python 实现，第三家栈） | 现有 `scripts/acceptance.sh` |
| B-L7 | Windows 版**编译期**正确性（含 `_test.go`） | `GOOS=windows go vet ./...` | §B.1 **【实测】**；**2026-08-25 起 CI 已做**（CNB 测试代码编译校验 stage + GA cross-vet job） |
| B-L8 | 文件属性 / DOS attribute 往返 | smbtorture `smb2.getinfo` / `smb2.setinfo` | §B.2 |
| B-L9 | Alternate Data Stream 语义 | smbtorture `smb2.streams` | §B.2 |

> **关键判断：清单二覆盖的东西，买 Windows 机器也买不回来 —— 反过来也一样。**
> smbtorture 打的是协议一致性（Windows 客户端本身不会去测这些边界），
> 真 Windows 打的是"微软实现到底怎么发报文"。两者**不重叠、不可互相替代**，
> 不要用"我们有 smbtorture 了所以不需要 Windows"或反过来的说法。

---

### B.1 **仓库里已经有从未被执行过的 Windows 测试代码**

> **2026-08-25 更新**：本节写作时的两条 CI 缺口此后都已闭合，原文保留作历史调研记录：
> ① CNB 门禁已新增「测试代码编译校验（全部 build tag × 全部平台）」stage
> （`test/ci/check-test-compile.sh`，含 windows/darwin/freebsd 的 `_test.go`）；
> ② main 上已有 GitHub Actions 工作流 `.github/workflows/ci.yml`——多 OS 单测
> （ubuntu/macos/windows 真实执行 `go test ./...`）+ 跨平台 `go vet`（含 `_test.go`，
> linux/darwin/windows/freebsd × amd64 及 linux/darwin arm64）。下文「给 qa 的具体建议」
> （交叉编译门禁改用/补上 `go vet`）即由该 GA job 落实。**Windows 测试代码从未被真实
> *运行* 这一点仍然成立**（GA 只在 ubuntu 上跑单测；windows-latest 单测的修复见
> CHANGELOG v0.4.0 已知问题），真机运行仍缺。

这是本节最硬的一条事实，也是 B-W1 的直接证据。

**【实测】**

```
$ ls internal/vfs/*_windows*.go internal/mdns/*_windows*.go
internal/vfs/attr_windows.go
internal/vfs/errmap_windows.go
internal/vfs/metadata_windows.go
internal/vfs/metadata_windows_test.go     ← 注意这个
internal/vfs/sparse_windows.go
internal/vfs/sys_windows.go
internal/mdns/sockopt_windows.go

$ grep -c '^func Test' internal/vfs/metadata_windows_test.go
8
```

这 8 个测试函数是：`TestMetadataPutGet`、`TestMetadataDeleteSubtree`、
`TestMetadataRenameSubtree`、`TestMetadataRenameOverwrite`、`TestMetadataRenameMissing`、
`TestMetadataPersistsAcrossReopen`、`TestMetadataCodec`、`TestDefaultMetadataPathOutsideShare`。

**它们从来没有运行过，一次都没有。** 原因：

1. CI 的交叉编译门禁跑的是 `GOOS=windows ... go build ./...`（`.cnb.yml:63`）。
   **`go build` 根本不编译 `_test.go` 文件**，所以它连"能不能编过"都没检查。
2. CI 的 `go test ./...` 只在 Linux 上跑，`//go:build windows` 的文件被直接跳过。

我手工补测了一次，好消息是**当前是绿的**：

```
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
build_rc=0
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...
vet_rc=0
```

`go vet` 会连 `_test.go` 一起做类型检查，所以这一跑至少证明了"Windows 测试代码目前能编译"。

> **给 `qa` 的具体建议（我不改 `.cnb.yml`，请 team-lead 转达）**：
> 在交叉编译门禁里，把每个目标的 `go build ./...` **改成或补上** `go vet ./...`。
> 成本几乎为零（本机实测两条命令都是秒级），收益是把 Windows/darwin 的
> 测试代码纳入编译期校验。这不能替代真机运行，但能挡住"win-meta 改了接口、
> Windows 测试文件编不过、谁都不知道"这类事故。
>
> ```yaml
> # 现在（只查产品代码）
> - GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build ./...
> # 建议（连 _test.go 一起类型检查）
> - GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go vet ./...
> ```
>
> **【实测】** 这条改动在 `GOOS=windows`、`GOOS=darwin` 下当前都通过，不会把 CI 变红。

---

### B.2 `smbtorture`：本容器里可用的最强武器

**【实测】** 装得上、跑得动、能发现真问题。

```
$ apt-get install -y -qq samba-testsuite
$ smbtorture --version
Version 4.22.4-Debian-4.22.4+dfsg-1

$ smbtorture --list | grep -c '^smb2\.'
331
```

#### 跑法（有一个坑）

```sh
smbtorture "//127.0.0.1/public" -U "testuser%testpass123" \
  --option="smb ports=4459" smb2.ioctl.sparse_file_flag
```

> ⚠️ **端口必须写成 `--option="smb ports=4459"`。**
> 我先试的 `--option="libsmb:client_port=4459"` 会返回 `NT_STATUS_NO_MEMORY` ——
> 一个**看起来像服务端崩了的假故障**，实际只是选项名不对。
> 这个坑与 AGENTS.md §10.3 第 8 条（smbclient `-c` 必须用分号）同类，
> 都是"测试工具的问题伪装成服务端 bug"。

耗时：**331 个用例全扫一遍 62 秒**（2 核容器，每个用例套 `timeout 25`）。
完全能进 CI。个别用例会挂住，**必须套 timeout**。

#### 基线（打在 `main` 的 `d80d257`，即 v0.1.0 tag）

**【实测】** 331 个用例：**52 PASS / 186 FAIL / 88 SKIP / 5 OTHER**。

186 个 FAIL 里绝大多数是"功能还没做"，不是回归。按根因聚类：

| 数量 | 失败根因 | 归属 |
|---:|---|---|
| 21 | `setup copy chunk error` —— `FSCTL_SRV_COPYCHUNK` 未实现 | **此前未登记的缺口**（对应 B-W5） |
| 19 | `oplock_level was 0, expected 9` | 任务 #4 tm-lease 正在做 |
| 17 | CHANGE_NOTIFY 返回 `NOT_SUPPORTED` | 未登记 |
| 12 | `durable_open was 0, expected 1` | 任务 #3 tm-handle 正在做 |
| 8 | byte-range lock 语义（该 `LOCK_NOT_GRANTED` 却返回 `OK`） | **此前未登记** |
| 7 | streams `OBJECT_NAME_NOT_FOUND` | 待查 |
| 6 | credits：`cur_credits was 512, expected 514` | **此前未登记，疑似真 bug** |
| 6 | 稀疏属性位 `FILE_ATTRIBUTE_SPARSE_FILE`(0x200) 从不上报 | 已 DM `tm-vfs`，见下 |
| 5 | session `LOGON_FAILURE`（疑似 reauth 路径） | 待查 |
| 3 | rename 该 `SHARING_VIOLATION` 却返回 `OK` | **此前未登记** |
| 3 | compound `INVALID_HANDLE` vs `FILE_CLOSED` | 状态码偏差 |

#### 顺带发现的一个文档失准（已 DM tm-vfs）

**【实测】** 同一套里这两个用例结果相反：

```
$ smbtorture ... smb2.ioctl.sparse_file_attr
success: sparse_file_attr

$ smbtorture ... smb2.ioctl.sparse_file_flag
failure: source4/torture/smb2/ioctl.c:3328:
  Expression `is_sparse' failed: no sparse attr after set
```

根因在 `internal/vfs/attr.go:103` —— 稀疏位是由 `Alloc < Size` **现算**的，
不是 `FSCTL_SET_SPARSE` 之后置上的粘性标志。torture 的流程是
「建空文件 → SET_SPARSE(TRUE) → 立刻 QUERY_INFO」，此时还没打洞，现算逻辑必然算不出 0x200。

这说明 `docs/timemachine-status.md` §5「稀疏文件已实现且实测通过」
**只覆盖了 FSCTL 行为侧**（真打洞、QAR 正确、空间真回收），属性位侧从未验证。
**【未查证】** macOS 建 `.sparsebundle` band 时到底读不读 0x200 —— 查不到权威说法，
已在 §C 的人工清单里做成一个可证伪的步骤。

#### 给 qa 的接法建议

1. **不要一上来做成阻塞门禁**，186 FAIL 会让门禁永远红着，等于没有门禁。
2. 做成**白名单基线**：把当前 52 个 PASS 固定成列表，CI 只跑这 52 个并要求全绿。
   新增 PASS 随时往里加；**已有的 PASS 变 FAIL 就是回归、就该红**。
3. 全量扫描做成 **allow-fail 的信息性任务**，只输出计数，用来看"这次改动顺手修好了几个"。

---

### B.3 Linux 侧能模拟到什么程度（实测）

服务端：本地构建的 `main` 二进制，监听 127.0.0.1:4459，`allow_guest: false`。

**【实测】**

```
=== 1) 强制签名 required（对标 Win11 24H2 客户端默认） ===
$ smbclient //127.0.0.1/public -U testuser%testpass123 -p 4459 -m SMB3 \
    --option="client signing=required" -c 'ls'
  testsmb2_dir      D    10  ...
  testsmb2_file.dat A     7  ...
        67108864 blocks of size 4096. 65875178 blocks available      ← 通过

=== 2) 强制加密 ===
$ smbclient ... --client-protection=encrypt -c 'ls'
        67108864 blocks of size 4096. 65875178 blocks available      ← 通过

=== 3) 匿名/guest（Win11 默认拒绝 guest，我们也应拒绝） ===
$ smbclient //127.0.0.1/public -N -p 4459 -m SMB3 -c 'ls'
session setup failed: NT_STATUS_LOGON_FAILURE                        ← 符合预期

=== 4) 五个方言逐个协商 ===
SMB2_02 -> ok    SMB2_10 -> ok    SMB3_00 -> ok    SMB3_02 -> ok    SMB3_11 -> ok
```

**这四条足以证明"Windows 客户端连不上"的常见原因（签名、加密、guest、方言）
在我们这边都是通的。** 但它证明不了 B-W2/B-W3/B-W6 —— 因为发报文的是 Samba，不是微软。

---

### B.4 Wine：**实测无效，可以彻底排除**

这条本来是"看起来最省钱"的方案，值得把否定证据摆清楚，免得以后有人再花时间试。

**【实测】** 装了 Debian 13 的 wine 10.0（`--no-install-recommends` 约 344 KB deb，
装完 1 分钟内完成）：

```
$ wine --version
wine-10.0 (Debian 10.0~repack-6)

$ WINEDEBUG=-all wine cmd /c 'dir \\127.0.0.1\public'
 Directory of Z:\127.0.0.1          ← ★ 关键：UNC 被解析成了本地 Unix 路径
 找不到文件
```

`Z:` 在 Wine 里是 Unix 根 `/`。也就是说 Wine 把 `\\127.0.0.1\public`
当成了**宿主 Linux 文件系统上的 `/127.0.0.1/public`**，
**根本没有发出任何 SMB 报文**。服务端日志在这次尝试期间零新增（最后一条还停在
上一轮 smbclient 测试的时间戳）—— 这是反向对照，不是只看客户端报错。

根因也很清楚：

```
$ ls /usr/lib/x86_64-linux-gnu/wine/x86_64-windows/*.sys | wc -l
20        # 有 ndis.sys / netio.sys / tdi.sys / http.sys …
$ ls /usr/lib/x86_64-linux-gnu/wine/x86_64-windows/ | grep -iE 'mrxsmb|rdbss'
（无输出）
```

**Wine 没有 `mrxsmb.sys`，也没有 `rdbss.sys`** —— Windows 的 SMB 重定向器是内核驱动，
Wine 没有内核，所有文件 IO 一律委托给宿主 Linux。所以哪怕 Wine 里真能访问 UNC，
底层用的也会是 **Linux 的 `cifs.ko`**，测的还是 Linux 栈，
**与"微软的 SMB 客户端怎么发报文"无关**。而 `cifs.ko` 在本容器还挂不上（§A.4）。

> **结论：Wine 在任何情况下都不能替代真 Windows 做 SMB 客户端测试。**
> 这不是"配置一下也许行"，是架构上不存在这个能力。**不要再试了。**

---

### B.5 CI 上跑 Windows 的三条路

#### 路线 1：CNB 自托管 Windows 构建节点

**【查文档】** CNB 文档（`build-node.md`、`grammar.md`）说明：CNB **没有托管的
Windows/macOS 云主机**，但支持把自己的机器注册成构建节点，在 `.cnb.yml` 里用
`runner.namespace` + `runner.tags` 选中。自托管节点**不计费**。

**【未查证 / 有矛盾】** `grammar.md` 写 `runner.namespace` "仅企业版有效"，
而 `build-node.md` 把它当作 SAAS 根组织的功能介绍。两处口径不一致，
**落地前必须在组织设置页面里人工确认本组织有没有这个入口**，别照着文档就下单买机器。

- 优点：与现有 `.cnb.yml` 同一套流水线，不用把代码镜像到别处。
- 缺点：需要一台常开的 Windows 机器 + 网络可达 CNB；矛盾未澄清前有落空风险。

#### 路线 2：镜像到 GitHub 用 `windows-latest`

> **2026-08-25 更新**：这条路线**已被采纳落地** —— main 上已有
> `.github/workflows/ci.yml`（多 OS 单测 + cross-vet + 三客户端验收），
> 仓库已镜像到 GitHub 并由 GA 执行（CNB 上不触发）。原文保留作决策过程记录。

**【查文档】** GitHub Actions 标准托管 runner（[官方规格表](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)）：

| 标签 | CPU | 内存 | 磁盘 | 架构 |
|---|---|---|---|---|
| `windows-latest` / `windows-2025` / `windows-2022` | 4 | 16 GB | 14 GB | x64 |
| `windows-11-arm`（公开预览） | 4 | 16 GB | 14 GB | arm64 |
| `macos-latest` / `macos-14/15/26` | 3 (M1) | 7 GB | 14 GB | arm64 |
| `macos-15-intel` / `macos-26-intel` | 4 | 14 GB | 14 GB | Intel |

（上表是**公开仓库**的规格；私有仓库的 Linux/Windows 会降到 2 核 8 GB，macOS 规格不变。）

**关键的一句原文**：
> "Use of the standard GitHub-hosted runners is **free and unlimited on public repositories**."

**这条决定了成本模型，也决定了这条路值不值得走：**

- **【实测】** 本仓库 `https://cnb.cool/finalappstore/stupidSamba.git`
  匿名 `curl` 返回 **404 → 目前是私有仓库**。
- 若愿意把代码**以公开仓库形式镜像到 GitHub**，Windows **和 macOS** 的 CI
  就都是**免费且无限量**的 —— 这是所有方案里成本最低的一条，没有之一。
- 若坚持私有，则走 GitHub 计费：Windows 是 Linux 的 2 倍费率、macOS 是 10 倍，
  Free 计划每月 2000 分钟额度换算下来只有约 **200 分钟 macOS**，很快就不够用。
  **【推断】** 费率倍数来自 GitHub 计费文档的常识性数字，本轮**未逐条核对当前价目表**，
  真要按私有仓库付费前请自行复核。

> 这是一个**需要项目所有者拍板的产品决策**（开源 or 不开源），不是技术选型，
> 我不替他做决定。但必须指出：**开源与否直接决定了 Windows/macOS CI 是 0 元还是要买机器。**

#### 路线 3：QEMU 跑 Windows 客机

**【实测 + 外推】** 见 §A.6：本容器无 KVM，TCG 纯软件模拟比 KVM 慢 8~11 倍，
Windows 10 冷启动外推 **5~15 分钟**，且内存只有 2.8 GiB 可用（Win10 实际需 4 GiB，
Win11 硬性 4 GiB + TPM 2.0）。**本容器直接排除。**

即便换一台有 KVM 的机器，这条路也只在"不想暴露代码、又不想买 Windows 授权硬件"时才有意义。
另外注意 GitHub 官方对自家 runner 的表态：
> "While nested virtualization is technically possible while using runners,
> **it is not officially supported**."

所以"在 GitHub Linux runner 里套 KVM 跑 Windows"也不是一条稳的路。

#### 三条路对比

| | 路线 1 CNB 自托管 | 路线 2 GitHub 托管 | 路线 3 QEMU |
|---|---|---|---|
| 金钱成本 | 一台 Windows 机器 + 电 | **公开仓库 = 0**；私有 = 计费 | 一台有 KVM 的机器 |
| 前置条件 | 组织有自托管入口（**待确认**） | **愿意开源** | 宿主有 vmx/svm |
| 与现有流水线 | 同一份 `.cnb.yml` | 需维护第二份 workflow + 镜像同步 | 自己搭 |
| 稳定性 | 自己维护 | 高 | 低（镜像/快照维护累） |
| 能否覆盖 B-W1 | ✅ | ✅ | ✅ |
| 能否覆盖 B-W4（Explorer UI） | ✅（可远程桌面人工看） | ❌（无头，只能跑命令行） | ✅ |

> **我的建议**：先问"愿不愿意开源"。
> 愿意 → 路线 2，零成本拿下 Windows **和 macOS** 两侧的自动化部分，
> 真机只留给 B-W4（Explorer UI）和 Time Machine 人工验收。
> 不愿意 → 路线 1，但先去组织设置页确认自托管入口存在，再买机器。

---

## 三种第三方客户端实测记录（AGENTS.md §3 硬性门槛）

日期：2026-09-10
执行环境：本开发容器（Linux/amd64，非初始 user namespace）

跑法（三家一起跑，或只跑其中一家）：

```sh
sh test/e2e/smoke.sh              # 三家全跑
sh test/e2e/smoke.sh impacket     # 只跑 impacket（smbclient / gosmb2 同理）
```

判据不是「客户端说成功了」，而是驱动脚本在**服务端磁盘上**独立核对七个操作的结果，
内容比对一律 `cmp` 整字节（原理与踩过的坑见 `test/e2e/smoke.sh` 文件头）。

### 三家客户端版本与实测结果

| 客户端 | 版本 | 实测 | 命令 |
|---|---|---|---|
| smbclient（Samba） | 4.22.10-Debian | ✅ 9/9 判据通过 | `sh test/e2e/client_smbclient.sh` |
| impacket（Python 独立协议栈） | 0.12.0 | ✅ 9/9 判据通过 | `/usr/bin/python3 test/e2e/client_impacket.py` |
| go-smb2（纯 Go） | v1.1.0 | ✅ 9/9 判据通过 | `sh test/e2e/client_gosmb2.sh` |

外加服务端侧两条（优雅退出、无 panic）与七条磁盘判据，单次全跑共 29 项全绿。

`mount.cifs` 是**第四种**客户端，在本容器**永远跑不通**：非初始 user namespace
下内核只放行带 `FS_USERNS_MOUNT` 标志的文件系统，cifs 没有这个标志，与特权无关。
这是环境限制，不是产品缺陷，不要再花时间试参数。

### impacket 的两个坑（都会表现为「静默跳过」）

1. **PATH 上第一个 python3 未必是装了 impacket 的那个。** 本容器
   `/usr/local/bin/python3`（uv 装的 3.12）没有 impacket，系统
   `/usr/bin/python3` 才有。裸写 `python3` 会让这一家整条被 SKIP，
   而日志只说一句「环境缺少该客户端」——看不出是被 PATH 骗了。
   `test/e2e/smoke.sh` 现在会逐个候选解释器实测 `import impacket`，取第一个能导入的。
2. **不要用 pip 装 impacket**：会和已装的 cryptography 冲突。用
   `apt-get install python3-impacket`。

### 判据是否真的会红（不是只证明了「现在是绿的」）

`test/e2e/reverse-control.sh` 用 `go build -overlay` 把服务端**故意改坏**
（工作树零改动），断言套件定点红在预期的判据上。六个变异全部被抓住：

| 变异 | 改坏了什么 | 被哪些判据抓住（三家各一份） |
|---|---|---|
| write-corrupt | 写进磁盘的字节翻一位 | `*/put-bytes` |
| read-corrupt | 读出的字节翻一位 | `*/get-bytes` |
| rename-noop | 回成功但没搬 | `*/rename-old-gone`、`*/rename-new-disk` |
| remove-noop | `LocalFS.Remove` 回成功但没删 | `*/rmdir-disk` 等 |
| doc-noop | delete-on-close 路径被掐断 | `*/rm-disk` |
| readdir-hide | 文件在但不出现在目录列表 | `*/client-run` |

客户端缺席时该脚本降级为 `[WARN]` 而不是判红 —— 让结论取决于构建机装没装
python 包，只会掩盖真正该报警的信号。
