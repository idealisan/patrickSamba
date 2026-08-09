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

### A.6 QEMU（TCG 纯软件模拟）实测

见本节末尾「§A.6 实测记录」。

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
