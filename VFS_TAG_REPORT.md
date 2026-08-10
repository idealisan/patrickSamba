# VFS / OSCAP 发布质量核验报告 — v0.2.0 tag

- 验证者：`vfs` agent（独立核验）
- 验证对象：**tag `v0.2.0` = commit `ccc4302987242781a10b2611aa41f980c04ba3a7`**（与 team-lead 交代一致）
- 工作目录：`/work/vfs-v020`（分支 `vfs/v020-tag-verify`；核验时 `git checkout v0.2.0` 至 detached HEAD 上跑）
- 验证时间：2026-08-10 10:22–10:27 CST
- 工具链：`CGO_ENABLED=0`，`go`（/usr/local/go）

---

## 1. 构建与测试（在 v0.2.0 tag 上）

| 判据 | 期望 | 实际 | 一致? |
|---|---|---|---|
| `go test ./internal/vfs/... ./internal/oscap/...` 结果 | 全 PASS | **全 PASS**（4 pkg：`vfs` 3.171s、`oscap` 0.008s、`oscap/builtin` 0.436s、`oscap/native` 0.030s） | ✅ |
| 子测试统计（`-v`） | 0 FAIL / 0 SKIP | **304 PASS / 0 SKIP / 0 FAIL** | ✅ |
| `go build ./...` rc | 0 | **rc=0**（无输出） | ✅ |

pkg 数：**4**（vfs + oscap + oscap/builtin + oscap/native），无 FAIL、无 SKIP。

## 2. CHANGELOG「OSCAP-WIRING-STATUS」块「2/6 接线」口径复算（v0.2.0 tag）

CHANGELOG 块原文声明「**已接线 2 项 / 共 6 项**」（`CHANGELOG.md:252`），并给出三条可复算判据。原样复跑：

| # | 判据（原样命令） | 期望 | 实际 | 一致? |
|---|---|---|---|---|
| 1 | `go list -deps ./cmd/stupidsamba \| grep -c oscap` | 3 | **3**（oscap / oscap/native / oscap/builtin 三包均链进二进制） | ✅ |
| 2 | 块内 grep（oscap 包外真实调用点） | 9 | **9**（assemble.go×1、config/validate.go×1、vfs/oscap_xattr.go×3、vfs/stream_xattr.go×4） | ✅ |
| 3a | `grep -rn 'newXattrAccessor\|readMetaXattrFast' --include='*.go' .`（裸跑） | 1 | **1**（`oscap_xattr.go:131` 注释一处，非调用） | ✅ |
| 3b | 同上 `\| grep -v '^[^:]*:[0-9]*://'`（去注释） | 0 | **0** | ✅ |

已接线两项与代码一致：调用点里 `caps.Xattr()`（CapXattr）+ `caps.Streams()`（CapNamedStream）实际被用；
`CapSparse` / `CapStableFileID` / `CapCreationTime` / `CapDOSAttributes` 四项无产品调用点 → 对这四项
`filesystem_mode` 无运行期效果。即口径确为 **2/6**，与 CHANGELOG 一致，无夸大。

**结论：CHANGELOG 的「2/6 接线」口径与 v0.2.0 代码完全一致（3 条判据 3/3 相符）。**

## 3. portable 门禁在位

| 判据 | 实际 | 一致? |
|---|---|---|
| `grep -n gate_portable .cnb.yml` | **命中 3 行**：191 定义 `&gate_portable`、217 引用、261 引用（push + pull_request 两条路径均挂） | ✅ |
| `test/ci/portable-mode.sh` 存在 | **存在**，26586 字节，可执行（`-rwxr-xr-x`） | ✅ |

## 4. 总结论

- **v0.2.0（ccc4302）在 vfs / oscap 模块上为发布质量**：4 个包全部 PASS（304/0/0），`go build ./...` rc=0，
  portable CI 门禁在位（定义 + push/PR 双引用 + 脚本存在）。
- **CHANGELOG 关于 oscap 的「2/6 接线」口径与代码一致**：三条可复算判据实测 3 / 9 / 1→0，与 CHANGELOG 声明逐条相符。

以上均为在 v0.2.0 tag 上的实测事实，无未证实表述。
