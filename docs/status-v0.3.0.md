# stupidSamba v0.3.0 — Release Plan & Progress Board

> 本文件是 v0.3.0 的**发布计划 + 进度板**（AGENTS.md §7.1，由 PM 维护）。
> 团队（7 名 agent，含 PM）并行开发，每人独立 worktree + 独立分支 + PR。
> 开工时间锚点：2026-08-10 20:49 CST。

---

## 0. 一句话目标

**把 `oscap` 六项能力里的「其余四项」真正接进 VFS 数据路径**，
让 `filesystem_mode` 的 `portable` / `native` / `auto` 三态对**全部六项**能力都生效，
而不是只对其中两项（xattr + named-stream）生效。这是 AGENTS.md §1.2 / §5 P7 明文记载的
v0.3.0 第一优先级，也是当前 doc-honesty 的最大缺口（文档写「2/6」，运行期确实如此）。

附加目标：补上 §1.2 点名的 CI 死角（freebsd 平台列表）、非 Windows 的 builtin 元数据兜底缺口，
以及把四处 OSCAP-WIRING-STATUS 陈述更新成「6/6」。

---

## 1. 验收门槛（v0.3.0 必须全部满足才算发布）

1. `go list -deps ./cmd/stupidsamba | grep -c oscap` 在四项新能力接完后**仍 ≥ 3**，
   且包外真实调用点从 9 继续增长（不止 xattr/stream 两条）。
2. 用 AGENTS.md §1.2 的 grep 判据，能数到 **4 个新能力各有调用点**（sparse / fileid /
   crtime / dosattr）。
3. `CGO_ENABLED=0 go build ./...` + `go vet ./...` + `go test ./...` 全绿（四平台）。
4. `test/ci/check-test-compile.sh` 平台列表末尾**已追加 `freebsd/amd64`**，且
   六个 `!linux && !darwin && !windows` 文件**至少被编译过一次**（手动补跑验证 rc=0）。
5. 至少 3 种第三方客户端（smbclient / impacket / go-smb2）对**新能力**做基本验证
   （创建时间回读、DOS 属性、FileID 稳定、PunchHole 后 AllocatedRanges 正确）。
6. 四处 OSCAP-WIRING-STATUS 块 + `configs/example.yaml` 的 YAML 块**同步更新为 6/6**。
7. `filesystem_mode: portable` 在 CI 真跑（已有 portable-mode.sh 门禁），且新能力在
   portable 下走 builtin 旁路而非宿主能力。

---

## 2. 团队与文件所有权（杜绝互相覆盖，见 AGENTS.md §7.1 / §7.3）

| Agent | 角色 | 拥有文件（除自己建的 worktree 外） |
|---|---|---|
| `pm` | 进度跟踪、阻塞识别、决策催办 | 本文件 `docs/status-v0.3.0.md`（进度板部分） |
| `vfs-sparse` | CapSparse 接数据路径 | `internal/vfs/sparse_*.go`、`internal/vfs/oscap_sparse.go`(新)、`internal/smb/command/ioctl.go`(sparse FSCTL) |
| `vfs-attr` | CapCreationTime + CapDOSAttributes + CapStableFileID + metadata_other 兜底 | `internal/vfs/oscap_attr.go`(新)、`internal/vfs/local.go`、`internal/vfs/attr.go`、`internal/vfs/time.go`、`internal/vfs/metadata*.go`、`internal/smb/command/query_info.go`、`internal/smb/command/set_info.go`、`internal/smb/command/aapl.go`、`internal/smb/command/create_context_qfid.go` |
| `config` | 把 FilesystemMode 逐共享正确下传到 LocalConfig | `internal/config/*.go`（不碰 `configs/example.yaml`，那是 docs-release 的） |
| `ci` | 补 freebsd 平台列表死角 + 跨平台编译校验 | `test/ci/check-test-compile.sh`、`test/ci/negative-verify.sh`（如需） |
| `qa` | 验收 / 集成测试 / 三客户端矩阵 | `test/integration/*`(新)、验收脚本 |
| `docs-release` | 四处 OSCAP-WIRING-STATUS 块 + 版本号 + 发布说明 | `CHANGELOG.md`、`README.md`、`AGENTS.md`(OSCAP-WIRING-STATUS 块)、`configs/example.yaml`(OSCAP-WIRING-STATUS-YAML 块)、版本常量 |

**铁律**：每个 agent 只改自己拥有的文件；需要改别人文件（如 `fs.go` 接口）必须发消息给
team-lead 协调，不得自行动手。公共契约（`oscap.Provider` 访问器）已存在于 `local.go` /
`oscap_xattr.go`，新能力一律仿 `oscap_xattr.go` 的 `xattrAt` 模式加**自己的**访问器方法，
写在自己拥有的新文件里，避免碰 `oscap_xattr.go`。

---

## 3. 各能力接线要点（给实现 agent 的规格）

### 3.1 CapSparse（vfs-sparse）
- 模式：`vfs.LocalFS` 通过 `l.caps.Sparse()` 拿到 `oscap.SparseFile`（仿 `xattrAt`）。
- 接线点：
  - `internal/smb/command/ioctl.go` 的 `FSCTL_SET_ZERO_DATA` → `PunchHole`；
    `FSCTL_SET_SPARSE` → `SetSparse`；`FSCTL_QUERY_ALLOCATED_RANGES` → `AllocatedRanges`；
    `FSCTL_FILE_LEVEL_TRIM`/preallocate → `Preallocate`。
  - 已有 `ioctl_sparse_test.go`，扩展它覆盖 native + portable 两条路径。
- 错误一律过 `mapOscapError`（已在 `oscap_xattr.go`，只读复用）。
- 注意 `oscap.SparseFile` 的**非对称契约**：`SetSparse(false)` 必须返回 `ErrNotSupported`
  而非假装成功（见 ports.go 注释）。

### 3.2 CapStableFileID（vfs-attr）
- `l.caps.FileID()` 拿到 `oscap.StableFileID`。
- 接线点：
  - `internal/smb/command/query_info.go` 的 `FileInternalInformation` 返回 `caps.FileID()`。
  - `internal/smb/command/create_context_qfid.go` 的 QFid create context（AAPL）。
  - `internal/smb/command/aapl.go` 的 `QFID` 查询。
- 拿不到稳定值时 native 返回 `ErrNotSupported`（让上层走 volumeSerial 兜底），**绝不**用
  「差不多的」值冒充（见 ports.go 契约：会变 FileID 比没有更糟）。

### 3.3 CapCreationTime（vfs-attr）—— 含 metadata_other 兜底缺口
- `l.caps.CreationTime()` / `SetCreationTime()`。
- 接线点：`local.go` 的 `statHost` 现在走 `statCreateTime(host)`（POSIX ctime 误用），
  改为走 `caps.CreationTime()`；拿不到真值返回 `ErrNotSupported` 时回落到原逻辑（不要冒充）。
- **关键缺口（§5 P7 / §1.2）**：`internal/vfs/metadata_other.go`（非 Windows）现在
  `return nil, nil`，导致非 Windows 上**完全没有 builtin 兜底**。必须补一个 builtin 旁路
  存储（仿 `metadata_windows.go` 用 `internal/oscap/builtin` 的 store 或独立 KV），让
  `filesystem_mode: portable` 下创建时间能存能取。**builtin 版必须完整**（§1.2 铁律 2）。
- set_info 的 `FileBasicInformation` 创建时间 → `caps.SetCreationTime()`。

### 3.4 CapDOSAttributes（vfs-attr）
- `l.caps.DOSAttributes()` / `SetDOSAttributes()`。
- 接线点：`attr.go` 的 `attrFromFileInfo` 现在**合成** DIRECTORY/SPARSE/READONLY 等位；
  **显式设置过**的位（HIDDEN/SYSTEM/ARCHIVE 等）改从 `caps.DOSAttributes()` 取，
  没有记录时走合成逻辑（`ErrNotFound` 回落）。
- set_info 的 `FileBasicInformation` / `FileAttributesInformation` 写位 → `caps.SetDOSAttributes()`。
- 边界：DIRECTORY/SPARSE/REPARSE_POINT 等客观事实位**不**归本能力管，由 vfs 合成层剔除
  （仿现有 `settableDOSAttributes`）。

---

## 4. 进度板（PM 维护，每项填 ✅/🚧/❌ + PR 链接）

| 工作项 | 负责 | 状态 | PR |
|---|---|---|---|
| 发布计划 / 进度板 | pm | ✅ 已建 | — |
| CapSparse 接线 | vfs-sparse | 🚧 进行中 | |
| CapStableFileID 接线 | vfs-attr | 🚧 进行中 | |
| CapCreationTime 接线 + metadata_other 兜底 | vfs-attr | 🚧 进行中 | |
| CapDOSAttributes 接线 | vfs-attr | 🚧 进行中 | |
| FilesystemMode 逐共享下传 | config | 🚧 进行中 | |
| freebsd 平台列表死角 | ci | 🚧 进行中 | |
| 三客户端 / 集成验收 | qa | 🚧 进行中 | |
| 四处 WIRING-STATUS 块 + 版本号 | docs-release | 🚧 进行中 | |
| 合并主干 + 打 tag | team-lead | ⬜ 待各 PR 合入 | |

---

## 5. 发布流程（team-lead 执行，合入后）

1. 各 agent PR 合入 `main`（每个 PR 必须 CI 绿，含 `check-test-compile.sh` 四平台）。
2. `config` 的 FilesystemMode 下传 + `vfs-attr` 的四项接线合入后，跑一次全量
   `CGO_ENABLED=0 go test ./...`（四平台）。
3. `qa` 的三客户端验收脚本在 `portable` 模式真跑一遍。
4. `docs-release` 更新 CHANGELOG / README / AGENTS.md / example.yaml 的 6/6 陈述。
5. 打 `v0.3.0` tag，写发布说明（按证据强度分档，不写未经证实的 TM 验收表述）。

---

## 6. 环境坑（全员必读，见 AGENTS.md §10.3）

- 每条命令先 `date`。Go 路径须 `export PATH=$PATH:/usr/local/go/bin`（刚重装，1.25.0）。
- **禁止一切删除/清理命令**（`rm`/`git restore`/`git worktree remove`/`reset --hard`/`clean` 等），
  会触发高危确认面板卡死 TUI；要回滚用 `git checkout HEAD -- <file>`，要干净基线新开 worktree。
- 每个可编译最小单元立刻 `commit` + `push` 到自己分支；自检用 `test/ci/check-test-compile.sh`
  （不是裸 `go build ./...`，后者不编 `_test.go`，曾栽过，见 R15）。
- 分支名 `<角色>/<主题>`，禁止推 `main`。worktree 用完留在原地不删。
