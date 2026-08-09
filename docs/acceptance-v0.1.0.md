# stupidSamba v0.1.0 多客户端验收报告

> 本文件是测试产物，归 QA（`scripts/acceptance.sh` 的产出）。面向用户的 README / CHANGELOG 由 docs 维护。

## 运行环境

| 项 | 值 |
|---|---|
| 基线 commit | `5428bcd`（`main`，即首次跑全量时 HEAD；该 commit 之后无产品代码改动） |
| 验收脚本 | `scripts/acceptance.sh`（commit 同基线） |
| 测试容器 | Linux amd64，Go `go1.25.0 linux/amd64`（与 CI 镜像 `golang:1.25` 同版本） |
| 主服务端口 | `SMB_PORT=4481`；派生端口 `GUEST=+1000 / STRICT=+2000 / ENC=+3000 / TAP=+4000`（偏移取 1000 起是为了避开多 agent 并行时连号分配的调试端口，已实测会撞） |
| smbclient | `Version 4.22.10-Debian-4.22.10+dfsg-0+deb13u2` |
| impacket | `python3` + `python3-impacket`（apt 安装，非 pip；pip 会与已有 cryptography 冲突） |
| go-smb2 客户端 | `scripts/clients/gosmb2`，依赖 `github.com/hirochachacha/go-smb2 v1.1.0` |
| `CGO_ENABLED` | `0`（`go vet` / `go build` 均在纯 Go 下执行） |

## 客户端版本与角色（AGENTS.md §3 矩阵）

| # | 客户端 | 命令 / 入口 | 在矩阵中的角色 |
|---|---|---|---|
| 1 | `smbclient`（Samba 官方 CLI） | `smbclient //127.0.0.1/<share> -p <port> -U <user>%<pass> -m <dialect> -c <cmds>` | 必测项，主力 |
| 3 | `impacket`（Python 独立栈） | `scripts/clients/impacket_test.py` / `impacket_dialects.py` | 必测项，第三方协议栈 |
| 6 | `hirochachacha/go-smb2`（Go 独立栈） | `scripts/clients/gosmb2` | 必测项，可进 CI、无 root |
| 2 | `mount.cifs`（Linux 内核客户端） | **SKIP**，见下 | 环境能力限制 |

**硬门槛（≥3 家第三方客户端栈）由 smbclient + impacket + go-smb2 满足**，三家均为与 Samba / Windows 无关的独立实现。

## `mountcifs` SKIP 原因（非漏测）

`scripts/acceptance.sh` 的 `t_mountcifs` 返回 `rc=77`（GNU 惯例的"环境不满足"），本次实测：

```
Unable to apply new capability set.
```

容器缺少 `CAP_SYS_ADMIN`，`mount.cifs` 在本环境**永远跑不通**，与 stupidSamba 服务端无关。AGENTS.md §10.3 第 2 条已将此定性为环境限制，并明确「§3 要求的三种第三方客户端由 smbclient + impacket + go-smb2 满足，不要因为它 fail 就判定验收不通过」。判定门禁只统计 smbclient / impacket / go-smb2 三家，不含 mountcifs。

## 结果汇总

| 用例 | 结果 | 要点 |
|---|---|---|
| smbclient | **PASS** | ls/get/put/mkdir/cd/rename/rm/rmdir 全功能 |
| impacket | **PASS** | 含删除文件 |
| gosmb2 | **PASS** | ReadDir/ReadFile/写回读/1MiB 大文件/Stat/子目录/Mkdir-Rename-Remove/稀疏偏移/Statfs |
| mountcifs | **SKIP** | 容器缺 CAP_SYS_ADMIN（见上） |
| dialects | **PASS** | 2.0.2/2.1/3.0/3.0.2/3.1.1 各自 ls+get+put+rm，并断言服务端日志真实协商出的方言号；impacket 跨栈复核 4 档 |
| signing | **PASS** | 客户端强制签名 / SMB 2.0.2 强制签名（HMAC-SHA256 路径）/ 服务端 `signing_required` |
| encryption | **PASS** | 见下，12 项线级断言 |
| readonly | **PASS** | 可读 / 写被拒 `NT_STATUS_MEDIA_WRITE_PROTECTED` / 建目录被拒 |
| authfail | **PASS** | 错误口令 / 不存在用户 / 匿名（`allow_guest=false` 时）/ 不存在共享 |
| guest | **PASS** | 匿名读取 / 具名用户 / 启动日志含 guest 告警 |

**结论：第三方客户端栈 3 家全绿 + 附加用例全绿。**

---

## 加密用例详解（本轮重点）

### 为什么不用「smbclient 能读到内容」做判据

该判据对**明文旁路**完全无感：服务端不加密、客户端也不加密，内容照样读得到，用例照样绿。本次发现的缺陷——`encryption_required: true` 的服务被 `smbclient -m SMB2_10` 把方言压到 3.1.1 以下，加密算法协商不出来、加密强制静默失效、全程明文——正是被这个弱判据漏掉的。所以加密判据改成**看网线上的字节**。

### 线级探针 `scripts/clients/smbtap`

透明 TCP 中继：客户端连 `127.0.0.1:PORT_TAP`，它原样转发到真实服务端，同时在转发路径上按 **Direct TCP 传输层**（4 字节长度前缀，**大端**）切帧，只读每帧开头的 `ProtocolId` 与 SMB2 头里的 `Command`（MS-SMB2 §2.2.1.2）。

判据两条（均必须满足，任一条不成立即判失败）：

1. **存在性**：出现 `SMB2 TRANSFORM_HEADER`，`ProtocolId = 0xFD 'S' 'M' 'B'`（MS-SMB2 §2.2.41）。
2. **全称否定（关键）**：明文帧里除 `NEGOTIATE(0x00)` / `SESSION_SETUP(0x01)` 外**不许出现任何命令**。按 MS-SMB2 §3.3.4.1.4 这两个本来就不能加密；会话建立之后若加密生效，`TREE_CONNECT` 及其后的所有请求都必须是 TRANSFORM 帧。第 2 条比第 1 条严格一个量级——只加密了一部分流量的实现（恰好能通过"有加密帧"这种存在性断言）会被抓出来。

### 探针本身的反向对照（验证工具也要被验证）

为防止探针是个恒绿的摆设，对**不加密**的会话（服务端 `encryption_required: false`、客户端也不要求加密）跑过一遍，探针报出：

```
c2s_plain_postauth_cmds=0x3,0x4,0x5,0x6,0x8,0xe,0x10
```

即它能确实抓得住明文（TREE_CONNECT / TREE_DISCONNECT / CREATE / CLOSE / READ / QUERY_DIRECTORY / QUERY_INFO）。若此反向对照为空，则上面所有"无明文业务命令"的断言都不可信。

### 测试配置的可得性（关键洞察）

`config-strict` 的 `min_dialect: "3.0"` 会让低方言在**方言选择**那一层就被挡掉，**根本走不到**本次修复的 negotiate 阶段 fail-closed 代码。因此新增第 4 个服务 **`config-enc`**：`encryption_required: true` + **`min_dialect: "2.0.2"`**，故意把门开到最低，让客户端真能协商到 2.0.2/2.1，才能验证"加密强制自己"会不会 fail closed。B 组返 `NT_STATUS_ACCESS_DENIED`、C 组返 `NT_STATUS_NOT_SUPPORTED` 正好证明**方言层拦截**与**加密层拒绝**是两道独立的防线，都在。

### 实测结果

```
A. 客户端主动要求加密（服务端未强制，4481）
   SMB3_11: 加密帧 c2s=13 s2c=13，明文仅协商/认证，cipher=2(GCM) → OK
   SMB3_00: 加密帧 c2s=14 s2c=14，明文仅协商/认证，cipher=1(CCM) → OK
B. encryption_required + 允许低方言接入（7481）—— 降级绕过回归
   SMB2_02: 被拒绝（NT_STATUS_ACCESS_DENIED），线上无明文业务命令 → OK
   SMB2_10: 被拒绝（NT_STATUS_ACCESS_DENIED），线上无明文业务命令 → OK
   SMB3_00: cipher=1(CCM)，加密帧 c2s=14 s2c=14 → OK
   SMB3_02: cipher=1(CCM)，加密帧 c2s=14 s2c=14 → OK
   SMB3_11: cipher=2(GCM)，加密帧 c2s=13 s2c=13 → OK
   服务端为被拒的低方言留下了告警日志 → OK
C. encryption_required + min_dialect 3.0（6481）
   SMB2_10: 被拒绝（NT_STATUS_NOT_SUPPORTED），线上无明文业务命令 → OK
   SMB3_11: cipher=2(GCM) → OK
D. go-smb2 对强制加密服务 → OK
```

`cipher=` 来自服务端日志（`internal/smb/command/negotiate.go` 的 `SMB 方言协商完成` 一行），作为线级判据的交叉校验保留：`cipher=1` = AES-128-CCM（3.0/3.0.2），`cipher=2` = AES-128-GCM（3.1.1）。这正是被修复的两条协商路径都走到了的证据。

---

## 已知遗留（不阻断 v0.1.0）

- `internal/smb/command/session_setup.go:144` 的 `if conn.Settings.EncryptionRequired && conn.Cipher != 0` 逃生口：negotiate 层已 fail closed，走到这里时 `Cipher != 0` 恒成立，当前不构成现实风险。team-lead 已要求 audit 改为 backstop（`EncryptionRequired && Cipher == 0` → `STATUS_ACCESS_DENIED` + WARN），作为纵深防御，本次验收后归集进 v0.1.0 发布。
- `validate.go` 对 `encryption_required + min_dialect < 3.0` 的处置是**启动 WARN 而非硬错误**（commit `656bff5`），是有意为之：保留 `config-enc` 这种配置的可验证性，重于早期静态拒绝。

## 复跑方式

```sh
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 SMB_PORT=4445 scripts/acceptance.sh            # 全量
CGO_ENABLED=0 SMB_PORT=4445 scripts/acceptance.sh encryption  # 单用例
```

打 tag 前会按 team-lead 通知再跑一次全量，以当时 HEAD 为基线更新本报告头部的基线 commit。
