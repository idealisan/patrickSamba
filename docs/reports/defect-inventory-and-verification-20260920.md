# 缺陷盘点与本机（Windows）可验证性分档

- 日期：2026-09-20 14:59 CST（`date` 实测）
- 范围：**只做盘点与分档，不改任何代码**；本文记录现状与「哪些问题在本机能解能验」
- 来源：仓库自带文档（CHANGELOG / README / docs / AGENTS.md）、非测试 Go 里的
  TODO 标记、以及 2026-09-20 这轮 CI 全绿过程中的实际观察
- 证据标注沿用仓库惯例：【实测】＝本次跑过命令或读过具名文件行；
  【未查证】＝没拿到一手证据，不能当结论用

---

## 0. 结论速览

1. 缺陷分五类：**产品能力缺口**、**验证强度不足**、**性能/实现短板**、
   **仓库与工程卫生**、**本轮修复新引入的代价**（最后一类容易被忽略，单列）。
2. 本机能验的能力边界是**三通道**，都已实测：
   Windows 原生（Windows 宿主语义）、**WSL/Ubuntu-22.04+ext4**（Linux 宿主语义）、
   **WSL 里的 smbclient 4.15**（真实第三方客户端）。
3. 因此「平台无关的协议逻辑」与「Windows 专属代码」**都能在本机解并验**；
   Linux 专属也能在 WSL 里验；**只有 macOS 专属与真机验收本机做不了**。
4. 两个不利前提（都实测）：本机**非管理员**，且 **445 被系统 `LanmanServer` 占着**
   → Windows 真机验收这条路封死（系统 SMB 客户端还不支持自定义端口）。
5. `history/` 与 `conversation-*.txt`：**本报告初版（14:59）按当时约定将其列为
   「不是缺陷、不动」；同日 15:0x 所有者推翻该约定，拍板「从工作树排除（磁盘保留）、
   加 `.gitignore`、并从全部历史提交中清除」**——实测 `history/` 工作树为
   **138 MB**（初版写 23 MB 是只数了部分文件），另 `.cnb.yml:66-72` 记着它是
   「目前最大的一处浪费」。见文末「更新（2026-09-20 15:0x）」。

---

## 1. 盘点口径

| 来源 | 覆盖什么 |
|---|---|
| `CHANGELOG.md` 各版「已知问题 / 未实现」 | 产品能力缺口（作者自认） |
| `README.md`「已知限制与说明」1–8 条 | 使用侧限制 |
| `docs/timemachine-status.md`、`docs/tm-prereview-20260907.md` | TM 相关缺口 |
| `docs/status-v050.md` 与 `docs/bughunt-20260825/` | 历史进度板的遗留移交 |
| `grep -rn "TODO\|FIXME\|未实现\|尚未接线\|没有调用点" --include=*.go`（非测试） | 代码里就地记录的缺口 |
| 2026-09-20 CI 全绿过程（4 次提交 + run `35494605067`） | 实际踩到/新引入的问题 |

---

## 2. 缺陷与不足清单

### 2.1 产品能力缺口（文档已自认）

| 缺口 | 影响面 | 证据 |
|---|---|---|
| 目录 lease 未实现 | macOS 目录级缓存协商拿不到 | CHANGELOG v0.7.3「已知问题」 |
| CHANGE_NOTIFY 只在命令层记账 | 宿主上别的进程改共享目录不触发通知，客户端少一次自动刷新（有轮询兜底） | README 已知限制第 4 条 |
| 无内核层 oplock 协调 | 有人在宿主上直接写文件时，持有写缓存许可的客户端会**静默读到旧内容** | README 已知限制第 6 条 |
| `ackLease` / `ackOplock` 不收敛 | 直接采信客户端回报的状态，未对「高于服务端要求」收敛 | CHANGELOG v0.7.3 |
| AAPL 三项未实现 | `OSX_COPYFILE` / `NFSAce` / `RESOLVE_ID` 恒宣告 false；`aapl.go:492` 另缺 hasMeta 区分 | `internal/smb/command/aapl.go:81,85,91,215,243,492` |
| lock sequence 重放抑制未实现 | 多通道重传场景（MS-SMB2 §3.3.5.14） | `internal/smb/command/lock.go:474` |
| SUPERSEDE 严格语义未实现 | 未做 unlink+create（会破坏其它已打开句柄，故从宽） | `internal/vfs/local.go:382,438` |
| macOS 上 native 打洞未实现 | `F_PUNCHHOLE` 需 unsafe 且无法验证 → 降级 builtin 写零，**TM bundle 只涨不缩** | `internal/oscap/native/native_darwin.go:12,34`、`sys_darwin.go` 的 TODO |
| Windows 重解析点「第二层」未接线 | 拿句柄反查真路径那层没接 → 换名/改名存在 TOCTOU 窗口（**安全**） | `internal/vfs/winreparse_windows.go` 末尾「现状说明」+ Issue #110 |
| 未实现的 SMB2 命令 / InfoClass / FSCTL / srvsvc 方法 | 一律返回 NOT_SUPPORTED 或 fault（这是**设计**：返回人话错误而非 panic），但能力确实缺 | `dispatch.go:74`、`ioctl.go:21,71,74`、`query_info.go:169,242`、`set_info.go:119`、`srvsvc.go:113` |

### 2.2 验证强度不足（「跑绿」≠「验过」）

| 项 | 说明 | 证据 |
|---|---|---|
| oplock/lease 与三套发现协议无 Windows/macOS **真机**验收 | 只到协议级（手写客户端 / nmblookup） | README 已知限制第 6、7 条 |
| Time Machine 端到端「备份并成功恢复」**一次都没跑过** | 2026-08-09 起被降为可选项，非发布阻塞项 | README 开头、`docs/timemachine-status.md` |
| macOS native ctime 路径未在真 macOS 上跑过 | CI 只做交叉编译 | `internal/oscap/native/ctime_darwin.go:60` |
| v0.7.3 两个 bug 修复无真机证据 | 容器内单测 + `-race` | CHANGELOG v0.7.3「实测」 |

### 2.3 性能与实现短板

| 项 | 说明 | 证据 |
|---|---|---|
| 末级名字**不存在**时仍是全目录 O(n) 扫描 | `.sparsebundle/bands/` 十万条时是 O(n²) 风险；作者标注为「未修热点」 | `internal/vfs/path_perf_test.go` 文件头 + `path.go` |
| 用量统计非增量 | 最多滞后 30 s–10 min；`quota_bytes` 只上报不拦截 | `internal/vfs/usage.go` 的「失效场景（已知取舍）」 |
| perf 报告 §8 遗留 | `command.handleRead` 双拷贝、bbolt DOS 落盘写放大、AES-GMAC 协商评估 | `docs/status-v050.md:283` |
| 共享/文件夹的元数据写放大 | builtin 旁路库按桶分级持久化的既有取舍 | perf 分析报告（历史） |

### 2.4 仓库与工程卫生

| 项 | 说明 | 证据【实测】 |
|---|---|---|
| **模块路径与仓库对不上** | `go.mod` 是 `github.com/finalappstore/stupidsamba`，仓库现在是 `idealisan/patrickSamba`；import 指向的不是托管它的仓库 | `go.mod:1`；`git remote -v` |
| **两个 5 MB ELF 产物入库** | `scripts/clients/gosmb2/gosmb2client`(4 989 537 B)、`test/e2e/client_gosmb2/gosmb2client`(4 989 613 B)，静态链接、**未 strip**、带 debug_info；`.gitignore` 覆盖了 `/bin/`、`/dist/`、`/perf` 却漏了它们 | `file` / `du -b`；`.gitignore` |
| 根目录疑似调试残留 | `a.txt`(37 B)＝一个 **HTTP 404 响应体**；`t.txt`(14 B)＝一段 sha 片段 | `head -c` |
| **版本号与 tag 脱节** | CHANGELOG 已写 v0.7.3（2026-09-07 正式版），但最新 tag 是 **v0.7.2**；**新 GitHub 仓库一个 tag 都没有** | `git tag`、`git ls-remote --tags origin`（空） |
| 文档滞后 | `docs/status-*.md` 最新停在 **v0.5**（项目已 v0.7.3）；仓库内 `memory/MEMORY.md` 只有 **5 行**索引，而 `memory/` 下有 **51** 个文件 | `ls docs/`、`wc -l memory/MEMORY.md`、`ls memory/*.md \| wc -l` |
| CI 双轨 + 历史分叉 | 主 CI 在 CNB（`.cnb.yml`），GitHub 是镜像；GitHub 上的 main 是**重写过的单支历史**（作者被统一），与 CNB 原始历史已分家 | `.github/workflows/ci.yml` 文件头、本轮 4 次提交 |
| 死代码 / 空壳（`AGENTS.md §7.5` 禁删，故留置） | `internal/vfs/sparse_{unix,other,windows}.go` 的 `platformAllocatedRanges`/`platformSetSparse` 无调用点；`internal/smb/crypto/zz_gmac_bench_178740_.go` 空壳；`grantIfDeferrable` 死函数 | 各文件头注释、CHANGELOG v0.7.3 |

### 2.5 本轮修复**新引入**的代价（容易被忽略，单列）

| 项 | 说明 |
|---|---|
| Windows 路径式 Stat 多一次句柄 open | 为拿真实分配长度/硬链接数/文件索引（`hostid_windows.go`）；对 QUERY_INFO 热路径是净增开销；**句柄路径已复用现有句柄，不受影响** |
| Windows 上目录句柄的 FLUSH 实际是 no-op | `FlushFileBuffers` 对目录不可用，现按「无可刷」返回成功（SMB 语义上必须成功）。代价：**Windows 上目录元数据的持久性没有真保障**，不如 Linux（fsync 目录有效） |

---

## 3. 本机（Windows）的能力边界【全部实测】

| 通道 | 能验什么 | 证据 |
|---|---|---|
| **Windows 原生** | 所有 `//go:build windows` 代码 + 平台无关逻辑，按 **Windows 宿主语义** | `go test -short ./internal/vfs/ ./internal/smb/command/ ./internal/server/` 在本机全绿 |
| **WSL（Ubuntu 22.04 / ext4）** | 所有 `//go:build linux` 代码 + 平台无关逻辑，按 **Linux 宿主语义**（真正的大小写敏感 FS） | 交叉编译出 linux 测试二进制后丢进 WSL 运行：`TestLinkBasic`/`TestColonPathCreatesBaseNotEscape`/`TestCaseInsensitiveLookup` 全 PASS |
| **WSL + smbclient** | **真实第三方客户端**验收（GitHub `acceptance` job 的本地复现） | WSL 内 `smbclient --version` → `Version 4.15.13-Ubuntu` |
| **`-race`** | 项目目前只在 CNB 跑的那一档 | WSL 内 `gcc` 11.4.0 已有；**缺 Go**，但 WSL 有网（`curl -sI https://go.dev/dl/` → HTTP/2 405）→ 装一次即可 |
| **macOS** | ❌ 只能交叉 vet + 交给 CI | 本机无 macOS |

**两个不利前提（决定「哪些做不了」）**：

1. 本机**非管理员**（`net session` 失败）。
2. `445` 被系统 **`LanmanServer`（RUNNING）** 占用并 `LISTENING`。
   → 用 Windows 自带 SMB 客户端连本机服务这条真机验收路走不通；且 Windows 客户端
   不支持自定义端口（`net use` 只能 445）。**除非提权停掉系统文件共享服务。**
3. WSL 里 **impacket 未安装**（`import impacket` 失败），需 `pip install`（有网，装得动）。

**本机可用的验证手段（可直接复制执行）**：

```bash
# 1) Windows 原生
go test -count=1 -short ./internal/vfs/ ./internal/smb/command/ ./internal/server/

# 2) Linux 侧：交叉编译测试二进制 → WSL 里跑（ext4，真 Linux 语义）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/x.test ./internal/vfs/
wsl -e sh -c 'cd /mnt/c/Users/songcheng1/AppData/Local/Temp && chmod +x x.test && ./x.test -test.run ...'

# 3) 真实客户端验收（WSL 内）：smbclient 已装；impacket 需先 pip install
```

---

## 4. 分档：哪些能解、能不能在本机验

### 档 A — 本机可解 + 可验（建议优先）

| # | 问题 | 怎么解 | 怎么验 | 风险 |
|---|---|---|---|---|
| 1 | Windows 重解析点「第二层」未接线（**安全**） | `finalPathOf` 原语已备好，缺「拿到句柄后立刻反查、不合规就关掉」 | 本机造 junction/symlink 做穿越用例 | 低（新增，不改既有路径） |
| 2 | Windows 目录句柄 FLUSH 实际 no-op | 试「以 `GENERIC_WRITE` 打开目录句柄再 FlushFileBuffers」；不行则如实写进已知限制 | 本机 Windows 用例 | 中（可能真做不到） |
| 3 | Windows 路径式 Stat 多一次 open | 改用 `GetCompressedFileSizeW`（免句柄拿 Alloc）；NLink 仅在需要时补 | 本机 benchmark 对比 | 低 |
| 4 | `go.mod` 模块路径 ≠ 仓库名 | 二选一：改模块路径（机械替换）或把仓库改回原名 | `go build/vet` + 三包全量；WSL 再验 Linux 侧 | 中（面广但机械） |
| 5 | 两个 ELF 产物 + 根目录残留 | `git rm --cached` + 扩 `.gitignore` | `git status` + CI | 低（**需所有者点头**，涉及删东西） |
| 6 | tag 与版本脱节 | `git tag v0.7.3` + `git push --tags` | `gh` 查 | 低 |
| 7 | 文档滞后（status / memory 索引） | 补写 | 人读 | 低 |
| 8 | 死代码 / 空壳 | 需先解禁 `AGENTS §7.5` 禁删令 | 编译 + 全量用例 | 低（**需所有者决定**） |
| 9 | CI 双轨 + 历史分叉 | 策略决定（保留并写清 / 统一） | `gh` | 低（需所有者定方向） |

### 档 B — 平台无关的逻辑缺口：本机可写可验（不依赖宿主语义）

- 目录 lease；`ackLease`/`ackOplock` 收敛；lock sequence 重放抑制；
  AAPL 三项 + hasMeta；SUPERSEDE 严格语义；
  `path.go` 末级不存在时的 O(n) 全扫；
  用量统计非增量；bbolt DOS 写放大；`handleRead` 双拷贝；AES-GMAC 协商。

### 档 C — 平台相关：可在本机对应通道验

- **Linux 专属**（`//go:build linux`）：在 WSL 里验。
- **CHANGE_NOTIFY 的宿主进程触发**：Windows 用 `ReadDirectoryChangesW`、Linux 用 inotify，
  **两侧都能在本机验**（Windows 原生 + WSL）。⚠️ 但这是一次**推翻既有决定**的改动
  （README 现写明「不用 inotify/kqueue/ReadDirectoryChangesW」，理由见 `notify_hub.go` 文件头），
  需所有者先拍板。

### 档 D — 本机解不了

| 问题 | 为什么 |
|---|---|
| macOS native 打洞（F_PUNCHHOLE）、darwin ctime | `//go:build darwin` + 需真 macOS 跑行为（含 unsafe 结构体），只能交叉 vet + CI 验 |
| Time Machine 端到端「备份并成功恢复」 | 需真 macOS 客户端 |
| Windows 真机验收（系统 SMB 客户端连本机服务） | 445 被 `LanmanServer` 占 + 非管理员 + 客户端不支持自定义端口（除非提权停服务） |
| 无内核层 oplock 协调 | Linux `F_SETLEASE` 与四平台约束冲突（项目已论证不可移植）；Windows 无对应物 → 本质不可解，只能文档化 |

---

## 5. 建议顺序（待所有者确认）

1. **档 A #1**：安全缺口，价值最高，且本机可完整验证。
2. **档 A #5、#6**：低风险、立竿见影（需点头）。
3. **档 A #2、#3**：把本轮修复新引入的两个代价收尾。
4. **档 A #4**：等所有者定「改模块路径还是改仓库名」。
5. **档 B**：按版本排（目录 lease / 记账类修复优先）。
6. **档 C**：`ReadDirectoryChangesW` / inotify 需要先推翻既有决定。

---

## 6. 未查证 / 待决

1. **macOS 侧的一切行为**：本机无法验，本文对 macOS 的判断均来自交叉编译 + CI 结果。
2. **Windows 目录 FLUSH 是否真有办法**（是否可用 `GENERIC_WRITE` 打开目录句柄再刷）：
   未查一手资料，见档 A #2，需实测。
3. **`git tag v0.7.3` 该指向哪个提交**：CHANGELOG 记 2026-09-07，但需确认当时的发布提交
   （本地历史被重写过，tag 与提交的对应关系要重新确认）。
4. **CNB 与 GitHub 的历史分叉如何处理**：涉及发布流水线归属，属策略问题。
5. 本文是**快照**：2026-09-20 14:59 CST 的现状。此后 CI、tag、产物状态都可能变。

---

## 更新（2026-09-20 15:0x）—— 所有者拍板清除 history / codebuddy 相关入库

本文 §0 第 5 条与 §2.4 的口径在同日被所有者**推翻**（「把仓库里不该出现的 history、
codebuddy 相关的目录排除掉，并且历史 commit 里也清理掉」）。处置如下【实测】：

| 项 | 处置 |
|---|---|
| `history/`（138 MB、115 个文件）、`codebuddy-files/`（7 KB）、根目录 `conversation-2026-08-08.txt` / `conversation-2026-08-09-00-01.txt` | `git rm -r --cached`（**磁盘保留**）+ `.gitignore` 新增 `/history/`、`/codebuddy-files/`、`/conversation-*.txt`；并用 `git filter-branch --index-filter` 从 `--branches --tags` 的**全部历史提交**中清除（`--prune-empty`） |
| `scripts/env/patch-codebuddy.sh` | **保留**：`.cnb.yml:48-50` 在直接跑它，`AGENTS.md:1067` 称其为修 TUI 卡死的根治脚本，删了会打挂 CNB CI |
| `docs/troubleshooting-codebuddy.md` | **保留**：AGENTS.md:700 引用；属文档不是目录 |
| `memory/project_dual_codebuddy_session.md` | **保留**：只是名字含 codebuddy，本质是项目记忆 |
| 旧规矩 | `memory/feedback_conversation_exports.md` 的「永远不要…加入 `.gitignore`」**作废**，文件已改写并标注反转日期 |

安全网（清理前已落）：

- `refs/backups/pre-purge-8f199ac` → 旧 HEAD `8f199ac`（`--branches --tags` 之外的
  ref，不会被重写波及）；
- `../pre-purge-8f199ac.bundle`（63.5 MB，`--all` 全量打包）。

**待办（本次未动，需后续处理）**：

1. `scripts/save-history.sh` 的默认落点仍是仓库内 `history/`，与 `.gitignore` 冲突，
   需改为仓库之外的持久位置；`AGENTS.md:837-838` 的「归档入仓」条目需同步修订。
2. `.cnb.yml:70-97` 里 `"!(history/**)"` 的排除规则已成死规则（无害，但注释过时）。
3. `AGENTS.md:771`「崩溃后 `history/` 一旦入库就是这个证据」的表述需改口径：
   归档仍在，只是**不再入仓**。
4. 强推后 **GitHub 上的 SHA 会第三次全变**（第一次作者统一、第二次本次清理），
   CNB 侧的原始历史与 GitHub 的分叉进一步扩大。

---

## 更新（2026-09-20 16:35）—— 模块路径迁移完成

§5 建议顺序里第 4 条（模块路径 vs 仓库名）已按所有者的选择落地：**改模块路径**
（不动仓库名）。

- `go.mod`：`module github.com/finalappstore/stupidsamba` →
  `module github.com/idealisan/patrickSamba`；180 个 `.go` 文件的 import 同步
  机械替换（含测试）。
- 刻意**未动**的两类引用：`docker.cnb.cool/finalappstore/stupidsamba` 是 CNB
  镜像仓库名，与 Go 模块路径是两回事；`docs/status-v0.2.0.md` 等历史快照里
  引用旧路径的测试输出属于时间锚定记录，不改。
- 验证【实测】：`go mod tidy` 无意外改动（go.sum 未变）；`go build ./...` 通过；
  GOOS=linux/darwin/windows vet 全过；`internal/config` 与 `internal/smb/command`
  全量 `-short` 全绿，测试输出已是新路径。
- 本条落定后，§4 档 A 的 #4 从待办中移除。

---

## 更新（2026-09-20 17:00）—— CNB 全面退役

所有者拍板「以后不再使用 CNB」。处置【实测】：

**删除**（CNB 专属死物）：

- `.cnb.yml`（CNB 流水线，20 处引用）；
- `scripts/ci-status.sh`（查 CNB API 的 CI 状态诊断，18 处）——由 `gh run list/view` 取代；
- `scripts/publish-release.sh`（CNB Open API 发版，24 处）——由 `gh release create` 取代；
- `docs/docker-registry.md`（CNB 制品库文档，37 处）。

**新增**：

- `.github/workflows/docker.yml`：打 `v*` tag 时用 `scripts/docker-build.sh` 构建双架构
  镜像并推送 **ghcr.io/idealisan/patricksamba**，同时把发布裸包挂到 GitHub Release
  （顺带补上「方式一」里「资产尚未上传」的缺口）。

**改**：

- README：发版下载通道 → GitHub Releases；Docker 章节 → ghcr 拉取（`read:packages`
  PAT）；`$CNB_BRANCH` → `$GITHUB_REF_NAME`；`docker.cnb.cool` 引用清零。
- Dockerfile：`org.opencontainers.image.source` → GitHub 仓库地址。
- `.github/workflows/ci.yml` 头注释：不再自称「CNB 之外的更多测试层」。
- `internal/wsd/metadata.go`：WS-Discovery 的 `ManufacturerUrl` 原来广播的是
  cnb.cool 链接（**真产品行为**），已改为 GitHub 仓库地址。
- AGENTS.md（7 处）、`docs/dev-workflow.md`（CNB API 建/合 PR 教程 → `gh pr create/merge`）、
  `docs/test-infra.md`、`test/ci/negative-verify.sh`、`scripts/docker-build.sh`、
  `scripts/env/patch-codebuddy.sh`、`.gitignore`、`internal/vfs` 两处注释。

**保留**（时间锚定的历史记录，不属于「CNB 描述」）：CHANGELOG 的历史条目、
`docs/status-*`、`docs/v020-*`、`docs/security-audit-v0.1.0.md`、`RELEASE_*`、
`VFS_TAG_REPORT.md`、`memory/`（23 个文件）、`docs/troubleshooting-codebuddy.md`
里的口述引用。

**⚠️ 迁移留下的真实缺口**：旧 CNB 门禁里的 build/vet/gofmt/race/静态链接/portable
实测等 stage 已随 `.cnb.yml` 移除，**GitHub Actions 侧是否都有对应物需要逐项核对**
（`dev-workflow.md` §5.5 与 `test-infra.md` 已标注）。`-race` 档尤其可疑 ——
GitHub Actions 的三档 unit 都没带 race。

---

## 更新（2026-09-20 17:20）—— 缺口已补：Gates 门禁工作流

上面的迁移缺口已落地：新增 **`.github/workflows/gates.yml`**（push/PR 触发），
等价迁移旧 CNB 门禁的七道检查：

| job | 内容 |
|---|---|
| `gofmt` | `gofmt -l` 必须为空 |
| `constraints` | C1/C3/C4/C6/C8/C9（`scripts/check-constraints.sh`） |
| `test-compile` | 测试代码编译校验：全部 build tag × 全部平台（`test/ci/check-test-compile.sh`） |
| `race` | `CGO_ENABLED=1 go test -race ./...`（race 依赖 cgo，`=0` 时 0.1 秒假绿） |
| `static-link` | linux 产物 `LC_ALL=C ldd` 必须静态链接 |
| `portable` | `test/ci/portable-mode.sh`（builtin 底座必须在 CI 里真跑） |
| `gate-integrity` | `test/ci/negative-verify.sh`（注入故障证明每关有牙） |

分工：`gates.yml` = 门禁（push/PR 必须绿）；`ci.yml` = 扩展测试面。

本机（Windows）能验的部分已验【实测】：`check-constraints.sh` 全绿；
`check-test-compile.sh` 全绿（4 平台 × 全部 tag 类型检查）；`portable-mode.sh`
在本机因 `TestPortablePunchHoleLeavesExistingHoleAlone` 被 SKIP 而红 —— 根因是
`fileBlocks` 没有 Windows 实现（`blocks_other_test.go` 恒返回 ok=false），
**CI 的 ubuntu runner 上会真跑**（CNB 时代即如此）。曾考虑给 Windows 补
`fileBlocks`（用 FILE_STANDARD_INFO.AllocationSize），核实后**放弃**：builtin 的
打洞在 NTFS 上是写零实现，会让「本来就是洞」的区间真的变胖，用例会由 SKIP 变
FAIL —— 这暴露的是 builtin 路径在 NTFS 上的真实平台事实，归入「语义类另立」，
不用改测试口径去迁就。
