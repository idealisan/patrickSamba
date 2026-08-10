---
name: metadata_path 真实行为（v0.2.0 发版后发现的过期文档）
description: metadata_path 在 PR #159 之后于所有平台被消费（oscap builtin 旁路），但配置层注释/WARN 仍写"仅 Windows/会被忽略"——这是一组过期文档，v0.2.1 修
type: project
---

**事实（2026-08-10 在 ccc4302 上由 team-lead 实读源码确认，非转述）：**

`metadata_path` 现在**在所有平台都被消费**，不只是 Windows。两条并存的元数据机制：

1. `internal/meta`（POSIX 属主/权限旁路）—— Windows-only。`internal/vfs/metadata_other.go:8-10` 在非 Windows `return nil, nil`。配置层注释说的就是它。
2. `internal/oscap/builtin`（六项能力旁路，PR #159 已接 xattr + 命名流 2/6）—— **无 build tag，全平台跑**。`internal/vfs/oscap_xattr.go:50` `openCaps()` 调 `oscap.Open(..., oscap.Options{MetadataPath: oscapMetadataDir(l.cfg.MetadataPath), ...}, native.New, builtin.New)`；`internal/oscap/builtin/store.go:204` 用 `MetadataPath` 决定 bbolt sidecar 落点。

**最硬证据**：qa 的 acceptance 红（多实例 bbolt 跨进程 flock 超时）**只在 Linux 上 sidecar 真打开时才成立**——证明非 Windows 上 metadata_path 是活的。

**结论**：真实行为是「**消费全平台、校验仅 Windows**」（`validate.go:394-396` 仅 Windows 校验）。

**因此以下四处是过期假话（v0.2.0 已随包发出，不可逆，v0.2.1 修）：**
- `internal/config/config.go:128` 注释「仅 Windows 使用…Linux/macOS 留空即可，填了也会被忽略」→ 假（Linux 也消费）。
- `internal/vfs/local.go:51` 注释「仅 Windows 使用」→ 假。
- `internal/config/validate.go:601` WARN「该字段仅在 Windows 上生效，当前平台会忽略它」→ 假（Linux 消费）。**这条 team-lead 曾误判为"准确行为、#11 不该删"——复核后确认应改为"全平台消费"措辞，不是保留。**
- `configs/example.yaml:178`（「仅 Windows 使用，Linux 留空即可」）与 `:184-189`（「所有平台都参与启动校验 / Linux 填 Windows 路径会启动失败」）→ 两处都错：178 低报（实际全平台消费），184-189 高报（校验仅 Windows）。

**Why:** PR #159 把 xattr/命名流接进 vfs 数据路径后，oscap builtin 旁路在所有平台都启用了，但配置层的注释与 WARN 还停留在只有 `internal/meta`（Windows-only）的旧认知，没人同步。

**How to apply:** v0.2.1 把它作为一个**连贯决策**整体修，别东补一句西补一句：
- 三处文本（config.go:128 / local.go:51 / validate.go:601）统一改成「所有平台消费，控制旁路库落点」。
- example.yaml 两处对齐：消费全平台；校验仍仅 Windows（这是 PR #18 刻意设计，为让一份 Windows 配置能在 Linux 复用不硬报错）。
- **#176 应拒/重定**：它把 `validate.go` 校验门从 Windows-only 改成 all-platforms，等于**回退 PR #18**——会重新引入跨平台配置不可复用的硬报错 bug。除非有强理由，保持「仅 Windows 校验」。
- 已发布的 v0.2.0 tag 与 tarball 不可回补（§7.5 不能删 tag，本环境无 CNB API 凭据 PATCH Release 正文），记为已知缺陷。
