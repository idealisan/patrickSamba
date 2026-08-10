# SERVER_TAG_REPORT — v0.2.0 server/smb/auth 发布质量独立核验

- 核验人：`server` agent（SMB 协议/服务端模块）
- 工作树：`/work/server-v020`，分支 `server/v020-tag-verify`
- 被核验对象：**tag `v0.2.0` = commit `ccc4302`**（`git rev-list -n1 v0.2.0` 实测确认）
- v0.1.0 基线：commit `d80d257`
- 核验时间：2026-08-10 10:22–10:27 CST
- 方法：仅在 checkout `v0.2.0` 的工作树上做只读核验（test / build / grep / git diff）。未运行发布脚本、未推 tag、未触发流水线。

---

## 1. 编译与测试（在 v0.2.0 checkout 上实测）

| 项 | 命令 | 期望 | 实际 | 一致? |
|---|---|---|---|---|
| 单元测试 | `CGO_ENABLED=0 go test ./internal/server/... ./internal/smb/... ./internal/auth/...` | 全 PASS | **7/7 pkg PASS，0 FAIL，rc=0** | ✅ |
| 全量构建 | `CGO_ENABLED=0 go build ./...` | rc=0 | **rc=0，无输出** | ✅ |

测试覆盖的 7 个包（`go list` 实测）：
`internal/server`、`internal/smb/command`、`internal/smb/crypto`、`internal/smb/dialect`、`internal/smb/status`、`internal/smb/wire`、`internal/auth` —— 全部 `ok`。

---

## 2. CHANGELOG v0.2.0「协议与功能」服务端声明 × 代码抽样核对

每项：判据（grep 关键符号的实际位置）/ 期望 / 实际 / 一致?

### 2.1 durable / persistent handle v1/v2（PR #20，修复 #40，验证 #29）

- **判据**：`internal/smb/command` 下存在 durable handler 与 persistent 分配器。
- **期望**：授予 / 重连认领 / 超时回收 三条路径有实现符号。
- **实际（FOUND）**：
  - `command/durable.go` — `durableTable` 的 `register`(:205) / `disconnect`(:246) / `reconnect`(:324) / `reap`(:300) / `remove`(:283)；`newPersistentID`(:181) 全进程唯一分配器；v1(DHnQ/DHnC) 与 v2(DH2Q/DH2C) 区分（:59）。
  - `command/create_context_durable.go`、`command/durable_defect_test.go`（#40 缺陷回归）、`command/durable_qa_test.go`（#29 独立验证）均在。
- **一致?** ✅（符号齐全，与「授予/重连/超时回收」声明相符；置信度分档与真机备份未验的表述亦与代码状态相符）

### 2.2 ShareAccess 冲突判定（PR #34）→ STATUS_SHARING_VIOLATION

- **判据**：`ShareAccess` / `STATUS_SHARING_VIOLATION` 的**实际返回点**，且被 CREATE 路径调用。
- **期望**：不再是「定义了从不返回」，而是有活返回点并接线进 create。
- **实际（FOUND）**：
  - 常量：`status/status.go:69` `SharingViolation = 0xC0000043`。
  - 活返回点：`command/share_access.go:141` `return status.SharingViolation`（双向判定，见 :14 注释）。
  - 接线：`command/create.go:95` `shareModes.check(...)`、`create.go:158` `shareModes.add(...)` —— CREATE 预检真实调用。
  - 端到端用例：`share_access_test.go` 含正/反方向、按身份(inode)而非路径、硬链接、ADS 独立等多例。
- **一致?** ✅

### 2.3 oplock / lease break 主动推送通道（PR #12；NTSTATUS 常量 PR #4）

- **判据**：`OPLOCK_BREAK` 命令注册 + `STATUS_INVALID_OPLOCK_PROTOCOL` 返回点 + break 通知 MessageId 固定值。
- **期望**：通道存在；非法确认回 `STATUS_INVALID_OPLOCK_PROTOCOL`；MessageId=0xFFFFFFFFFFFFFFFF。
- **实际（FOUND）**：
  - 注册：`command/oplock.go:9` `register(wire.CommandOplockBreak, true, true, handleOplockBreak)`。
  - 返回点：`command/oplock.go:107` 与 `:113` `return status.InvalidOplockProtocol`（常量 `status/status.go:121` = 0xC00000E3）。
  - wire：`wire/oplock_break.go:41` 注释「服务端发通知时 MessageId 必须是 0xFFFFFFFFFFFFFFFF」。
- **一致?** ✅（并与 CHANGELOG「仅通道、不宣告 LEASING、对客户端可观察行为无变化」的克制表述相符）

### 2.4 配额按共享粒度（PR #14；启动自检 PR #15）

- **判据**：`quota_bytes` 配置字段 + 生产路径上的 `applyQuota` + 启动自检。
- **期望**：可用空间按本共享用量计算；启动时对过小/已超配额告警。
- **实际（FOUND）**：
  - 配置字段 `QuotaBytes`：`vfs/local.go:78`，`NewLocalFS` 用它（:159）。
  - 生产路径接线：`vfs/local.go:680` `l.applyQuota(info)`（StatFS 内），实现于 `:700`。
  - 启动自检：`config/validate.go:590` 过小告警（<1GiB），`:634` `quotaUsageWarnings`（已小于现有用量告警）。
- **一致?** ✅
- **备注（非缺陷）**：实现落在 `internal/vfs` 而非 server/smb/auth，属跨模块功能；CHANGELOG 将其归入「协议与功能」，与代码位置不冲突。

### 2.5 加密 / 签名：crypto、auth、dialect 自 v0.1.0 零改动

- **判据**：`git diff --stat v0.1.0..v0.2.0 -- internal/smb/crypto internal/auth internal/smb/dialect`。
- **期望**：输出为空（无改动）。
- **实际**：**输出为空，rc=0** —— 三个目录自 v0.1.0 起零改动。
- **一致?** ✅（与 CHANGELOG 第 37–39 行「三目录零改动、本版本无加密/签名修复可写」完全一致）

---

## 3. 结论

- **发布质量**：被发布的 `v0.2.0`（`ccc4302`）在 server/smb/auth 三模块上 **7/7 包测试 PASS、全量 build rc=0**，达发布质量，未发现回归。
- **CHANGELOG 一致性**：v0.2.0「协议与功能」段的 5 项服务端声明（durable/persistent、ShareAccess 冲突、oplock/lease break 通道、按共享配额、crypto/auth/dialect 零改动）**逐项与代码一致（5/5 FOUND / 一致）**，且置信度分档与克制表述（仅通道无客户端可观察变化、durable 未做真机长跑）与代码实际状态相符，无夸大。
- 未发现 MISSING 项，未发现「声明有、代码无」的悬空陈述。
