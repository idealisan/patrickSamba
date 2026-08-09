# stupidSamba v0.1.0 多客户端验收报告

> 本文件是测试产物，归 QA（`scripts/acceptance.sh` 的产出）。面向用户的 README / CHANGELOG 由 docs 维护。

## 运行环境

| 项 | 值 |
|---|---|
| 基线 commit | 全量套件首次跑绿于 `5428bcd`；此后 `b7ef0ed`（audit 补齐三道加密 fail-closed + `min_dialect` 启动硬错误）落地，加密矩阵已按新行为改写并**重新验证通过**（见「加密用例详解」）。打 tag 前会按 team-lead 通知再跑一次全量，届时本表头部基线一并更新。 |
| 验收脚本 | `scripts/acceptance.sh`（与基线同提交，加密矩阵段因 `b7ef0ed` 后改写，提交晚于 `5428bcd`） |
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

"加密强制不能被绕过"在协议栈里有**三道独立的防线**，验收必须分别打中、不能互相替代：

1. **配置层 fail fast**（`internal/config/validate.go`）：`encryption_required + min_dialect < 3.0` 在启动期直接报错，错误点名 `server.min_dialect`。这是把矛盾一次性暴露给用户，而不是让他在运行期看到"连不上"去瞎猜。默认 `min_dialect` 是 `2.0.2`，所以**只写 `encryption_required: true` 不带 `min_dialect` 也会 fail fast**——这个默认值陷阱被专门覆盖。
2. **方言选择层**：合法配置（`min_dialect: 3.0`）下，客户端 `-m SMB2_02/SMB2_10` 因无公共方言被 `NT_STATUS_NOT_SUPPORTED` 拒。
3. **negotiate 层 fail-closed**（`internal/smb/command/negotiate.go`）：协商出 3.0/3.0.2 但客户端没宣告 `CAP_ENCRYPTION` 时 `ACCESS_DENIED`；以及 **SMB1 多协议协商是第二个协商出口**——方言列表仅含 `"SMB 2.002"`（无 `"SMB 2.???"` 通配）时过去会就地定型 2.0.2 明文放行、绕过 negotiate，现已直接关连接。

**诚实声明可达性**：第 3 道在当前环境的可达客户端里**无法端到端触发**——`smbclient 4.22` 即便 `--client-protection=off` 也照常宣告 `CAP_ENCRYPTION`（会话照常成功），而 `impacket` 走 SMB1 通配入口 `"SMB 2.???"` 会被放行到真正的 SMB2 协商、协商出 3.0+加密亦成功。因此第 3 道由 `internal/server` 的单元测试覆盖（audit 的 `encryption_paths_test.go`，每条都做过负向实验），本客户端矩阵不硬凑。验收脚本覆盖的是第 1、2 道确实可达的路径，外加 `cipher=` 日志断言证明第 3 道的**正常**路径（3.0/3.0.2→CCM、3.1.1→GCM）真的协商出来了。

### 实测结果

```
A. 客户端主动要求加密（服务端未强制，4495）
   SMB3_11: 加密帧 c2s=13 s2c=13，明文仅协商/认证，cipher=2(GCM) → OK
   SMB3_00: 加密帧 c2s=14 s2c=14，明文仅协商/认证，cipher=1(CCM) → OK
B. encryption_required + min_dialect < 3.0 配置层拦截（反向配置，应当启动失败）
   config-enc (min 2.0.2):           启动报错且点名 'server.min_dialect' → OK
   config-enc-default (不带 min):     启动报错且点名 'server.min_dialect' → OK
C. encryption_required + min_dialect 3.0（合法配置，6495）—— 端到端拒绝 + 加密
   SMB2_02: 被拒绝（NT_STATUS_NOT_SUPPORTED），线上无明文业务命令 → OK
   SMB2_10: 被拒绝（NT_STATUS_NOT_SUPPORTED），线上无明文业务命令 → OK
   SMB3_00: cipher=1(CCM)，加密帧 c2s=14 s2c=14 → OK
   SMB3_02: cipher=1(CCM)，加密帧 c2s=14 s2c=14 → OK
   SMB3_11: cipher=2(GCM)，加密帧 c2s=13 s2c=13 → OK
D. go-smb2 对强制加密服务 → OK
```

`cipher=` 来自服务端日志（`internal/smb/command/negotiate.go` 的 `SMB 方言协商完成` 一行），作为线级判据的交叉校验保留：`cipher=1` = AES-128-CCM（3.0/3.0.2），`cipher=2` = AES-128-GCM（3.1.1）。这正是被修复的两条协商路径都走到了的证据。

> 历史注记：早期版本（commit `623c904` 之前）`config-enc` 是常驻服务（`min_dialect: 2.0.2` 跑得起来），端到端直接打中第 3 道的 `ACCESS_DENIED`；自 `b7ef0ed` 起 `min_dialect < 3.0` 改为配置层硬错误，`config-enc` 不再能启动，于是 B 组改为断言"配置层拦截"本身。这是加固，不是覆盖率倒退——第 3 道只是从"客户端端到端"挪到了"单元测试"，且在第 1 道被更早发现。

---

## 已知遗留

- 无。原 `session_setup.go:144` 的 `&& conn.Cipher != 0` 逃生口已在 `b7ef0ed` 改为 backstop（`EncryptionRequired && Cipher == 0` → `STATUS_ACCESS_DENIED` + error 日志），第 3 道防线的正常路径与拒绝路径均有 `internal/server/encryption_paths_test.go` 覆盖。
- `validate.go` 对 `encryption_required + min_dialect < 3.0` 在 `b7ef0ed` 由 WARN 升级为**启动硬错误**（字段 `server.min_dialect`，文案直接给改法），与 `max_dialect` 那条对称；不做静默抬高 `min_dialect` 的魔法行为。

## 复跑方式

```sh
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 SMB_PORT=4445 scripts/acceptance.sh            # 全量
CGO_ENABLED=0 SMB_PORT=4445 scripts/acceptance.sh encryption  # 单用例
```

打 tag 前会按 team-lead 通知再跑一次全量，以当时 HEAD 为基线更新本报告头部的基线 commit。
