# stupidSamba v0.2.0 进度看板

> 维护者：`pm`（专职项目管理 agent），分支 `pm/status`。
> **本文件的判断只采信客观证据**：分支上的 commit、已合并的 PR、可复现的实测记录。
> agent 的自我汇报不作为进度依据。
>
> 最近盘点：**2026-08-09 07:20Z（第 13 轮 · 全量盘点 main/PR/分支/孤儿，`main` 转绿、#26 为唯一阻塞）**
> 上轮：第 12 轮（实测复现 CI 全链，查明 `main` 红灯有两道）、第 11 轮（拍板 posix.v2 / CI 归属）、13:22（第 4 轮）、13:14（第 3 轮）、13:06（第 2 轮）、12:15（第 1 轮 · 基线）
> 本期核对基线：`origin/main` = `0d60c68`（`push` CI = `success`，绿；相对第 12 轮 `4a1bee2` 前进 22 个提交）
>
> **第 12 轮头条**：合并 fix-ci 的 PR #35 **不足以**让 `main` 变绿——它后面还压着一个
> 真实的产品级 data race，而修它的代码正未提交地躺在 `qa-proto` 的磁盘上。详见 §12。
>
> **✅ 该头条已闭环（见第 13 轮）**：#35 与 qa-proto 的 R12 修复（含 data race，PR #40）均已合入，
> `origin/main` 的 `push` CI 现 = `success`（绿）。第 12 轮「两道红灯」是合并顺序未完成的瞬时态，非结构性故障。

---

## 第 4 轮：**堵塞已解除**（13:18 一次性合入 9 个 PR）

第 2、3 轮的头号问题「评审吞吐为零、主干静止 40 分钟」**已解决**。

| 指标 | 第 1 轮 12:15 | 第 3 轮 13:14 | 第 4 轮 13:22 |
|---|---|---|---|
| 开着的 PR | 0 | **9** | **4** |
| 已合入 `main` 的 PR | 1 | 9 | **18** |
| 主干最后前进 | — | 40 分钟前 | **3 分钟前** |
| 无主/无 PR 的在写代码 | 约 1500 行 | 约 3900 行 | **约 1611 行（仅剩 win-meta）** |

13:18 一次合入：#17 → #11 → #19 → #10 → #12 → #14 → #15 → #13 → #16，
**顺序与本看板 §2.1 的建议一致**（#17 第 0 批打头、零冲突批在前、`vfs/local.go` 串行链在后）。

### 本轮三项跟踪事项全部闭环

| 事项 | 第 2/3 轮记录 | 结果 |
|---|---|---|
| §2.2 PR #7 「被架空的逻辑」 | 764 行零产品调用点 | ✅ **已修复并合入**（见下方核实） |
| §4.3 `tm-handle` 无主分支 +1677 | 无人认领 | ✅ 已开 **PR #20** |
| §4.4 `r-infra` 无主分支 +600 | 无人认领 | ✅ 已开 **PR #21** |

**PR #19 接线核实**（在 `origin/main` 上重新验证，不采信 PR 描述）：

```
internal/vfs/path.go:203  return validateWindowsName(name, hostTrimsTrailingDotSpace)
                          ↑ ValidateComponent 内部，真实调用
path.go 中的旧 reservedNames  → 已删除，grep 无匹配
winOpenParamsFor              → 非测试引用 7 处
isNameSurrogateTag            → 非测试引用 2 处
```

三个函数从「零产品调用点」变成全部接线。**R9 关闭。**

### R10「假红」的预测得到验证

第 3 轮我据 §2.4 预测：*「一旦开了 PR，PR 事件的构建大概率立刻变绿。」*
`tm-handle/durable` 与 `r-infra/test-infra` 此前只有 push 记录且全红，
开 PR（#20/#21）后 **PR 事件构建均为 `success`，push 仍为 `error`，`.cnb.yml` 仍是旧版**——
与 §2.4 的因果解释完全吻合。**这条预测是可证伪的，且没有被证伪。**

### 当前 4 个 PR（第 4 轮快照；**第 6 轮这 4 个已全合入，现开 PR 见 §6.4**）

| PR | 分支 | 主题 | push 构建 | 说明 |
|---|---|---|---|---|
| #18 | `win-meta/metadata-path` | `metadata_path` 非 Windows 平台跳过校验 | ✅ | `.cnb.yml` 新 |
| #20 | `tm-handle/durable` | durable handle v1/v2 + create context 注册表重构 | ❌ **假红** | `.cnb.yml` 旧，见 §2.4 |
| #21 | `r-infra/test-infra` | v0.2.0 A 块调研（纯 Markdown 一个文件） | ❌ **假红** | 纯文档不可能编译失败 |
| #22 | `qa/save-sh-residual` | 记忆更新（worktree 里 `.git` 是文件等） | ✅ | `.cnb.yml` 新 |

⚠️ **`memory/MEMORY.md` 三方冲突预警**：#18、#22 **和本看板分支 `pm/status`** 都在追加
`memory/MEMORY.md`。三方都是「在文件尾部追加一行」，git 一定判冲突。
建议合并顺序 **#22 → #18 → `pm/status`**（我排最后，冲突由我来解，不占用他人时间）。

> **后续（13:28）**：该冲突已如预告发生，并由我在 rebase `pm/status` 时自行解决完毕
> （三方条目全保留），#18 / #22 无需为此做任何事。看板已提 **PR #24**。

---

## 第 6 轮：三方回函后的订正（**win-vfs / win-meta / qa**）

本轮回函让第 2–5 轮里三处判断需要**撤回或更正**。以下结论全部用 `git fetch` 后的
`origin/main` / `origin/<分支>` 实物复核过，不采信回函自述。

### 6.1 撤回「`reservedNames` 不是死变量」的更正（我的误判）

第 2 轮 §2.2 我给 team-lead 的附正是**错的**——当时基于 PR #7 刚提出、接线未做的快照。
现已复核 `origin/main:internal/vfs/path.go`：

```
path.go:203   return validateWindowsName(name, hostTrimsTrailingDotSpace)   ← 接线已在 PR #19 落地
grep "reservedNames" path.go  →  NONE（已删除）
```

`reservedNames` 是接线的**因变量**：一旦 `ValidateComponent` 改调 `validateWindowsName`，
旧表即无引用，删它是接线收尾动作。我当初看到 `:200` 在用，是因为那时接线还没做。
**更正已撤销，team-lead 无需据此做任何反向操作。**（win-vfs 在 PR #19 留了
`winpath_test.go:189` 的 `legacyReservedNames` 冻结快照防回退，断言新表是旧表超集。）

### 6.2 PR #7 的「被架空逻辑」判定要拆成两半

第 2 轮 §2.2 说 #7 三个函数全零产品调用点。复核后**不能混为一谈**：

| 函数 | 文件 | 现状（origin/main） | 结论 |
|---|---|---|---|
| `validateWindowsName` | `winpath.go` | `path.go:203` 真实调用；`reservedNames` 已删 | ✅ **PR #19 已接线并合入**，R9 此处关闭 |
| `isNameSurrogateTag` | `winopen.go` | 仅 `winopen_test.go` 调用；`open_windows.go` 在 main 中**不存在** | ❌ **真缺口，无生产消费方** |
| `winOpenParamsFor` | `winopen.go` | 同上 | ❌ **真缺口，无生产消费方** |

`winOpenParamsFor` / `isNameSurrogateTag` 是 `os.OpenFile` flag → `CreateFileW` 入参的**纯翻译层**，
其消费方是**尚未编写的 Windows-only 后端 `open_windows.go`**。这个缺口靠读 diff 完全看不出来，
正是本项目「被架空逻辑」bug 类的真身——**只靠查非测试调用点抓得住**。`winpath` 那半已被我
自己的复核查实已补，所以这次净抓到一个真缺口（winopen），不是两个。

> **owner 已定 = win-vfs**（第 8/9 轮）：原设想的消费方 `open_windows.go` 由 win-vfs 以单点 build-tag 分裂接缝 `openhost_windows.go` 取代（PR #30，分支 `win-vfs/openhost-seam`），与 unix 侧 `openhost_unix.go` 一起构成 `openHostFile` 接缝。win-meta 明确不碰 `internal/vfs`（team-lead 边界）。第一步接缝已完成：7 处宿主打开收口到 `openHostFile`，`winOpenParamsFor`/`isNameSurrogateTag` 调用点以 TODO 标出；第二步真接 `CreateFileW` 被 team-lead 暂缓（无 Windows runner 验 `os.NewFile(handle)` 的 Seek/ReadAt/WriteAt/Truncate 互操作——不可静态验的存亡假设，不赌）。整条链都在 vfs，win-meta 无需另起一份，无重复造风险。

### 6.3 win-meta 的 `bolt.go`「消失」是误报——已澄清

第 5 轮 §4.1 我记「`internal/meta/bolt.go` 那处 modified 改动既不在 commit 也不在工作树」。
win-meta 澄清：那 1 行是把 `subtreeSkipByte` 由 `"0"` 改成 `"/"` 的**变异测试残留**，
提交它会引入 bug；他在还原前跑完对照（`TestGetDir` 在 `bolt.go:178` 死循环 90s 超时），
确认该常量被测试真实覆盖，已 `git checkout` 还原，**工作树干净**。
两个 commit（`20ade80`、`66da303`）在 12:20 就已 push，`git log origin/win-meta/metadata-store..HEAD` 为空。
**所以不是丢失、不是违规，是我的快照误读。R2 在 win-meta 身上并未兑现。** 记一笔方法论：
未提交改动可能是变异测试残留，下次先问「是不是测试残留」再记风险。

### 6.4 当前开着的 PR（#26 / #30 / #33 开着；#24/#25/#27/#28/#29 第 11 轮同步已合入）

#18 / #20 / #21 / #22 / #23 已合入 `main`（`ef73b549`）。第 11 轮再合入 #24/#25/#27/#28/#29（main → `1acae3f`）。**现开着：#26（conflict）、#30（win-vfs openhost 接缝）、#33（win-meta 分隔符收尾）**（见 §11）：

| PR | 分支 | 主题 | 档 | 备注 |
|---|---|---|---|---|
| #24 | `pm/status` | 本看板 | ✅ **已合入**（sha `986b1e0` → main） | 我的 |
| #25 | `team-lead` 系 | AGENTS.md §7.5 高危命令禁令 | ✅ **已合入** | 团队纪律 |
| #26 | `win-meta/metadata-store` | `internal/meta` bbolt/noop + 单测 | 🟡 **draft，现 conflict** | ⚠️ R11 处置已决（D6，`posix.v2` 不迁移）；现 `mergeable_state: conflict`（仅 `memory/MEMORY.md` + `memory/project_config_path_platform_semantics.md` 两文件），非 CI；team-lead 已令 win-meta merge main 按并集解。见 §11 |
| #27 | `win-vfs` 系 | 抽 `IsWindowsSlash` 为分隔符唯一真源 | ✅ **已合入** | 名字词法相关 |
| #28 | `qa-vfs` 系 | 变异测试（mutation testing） | ✅ **已合入** | team-lead 代开 |
| #29 | `qa-proto` 系 | durable 缺陷复现用例 | ✅ **已合入** | team-lead 代开；另见 R12 |
| #30 | `win-vfs/openhost-seam` | `openhost_windows.go` 接缝（R9 收口第一步） | 🟡 | owner=win-vfs，第二步 CreateFileW 暂缓 |
| #33 | `win-meta` 系 | 分隔符收尾小 PR | 🟡 | win-meta 队列新增 |

### 6.5 qa 的 save.sh 修复已随 PR #22 合入

qa 担心「`gh` 不可用、PR 需 team-lead 开」。复核：`qa/save-sh-residual` 的 head `fda24e2`
正是 **PR #22 的合并 head**，已合入 `main`。即 retry rebase 目标修正 + worktree `.git` 锁失效
修复 + 死分支删除（commit `37befb4`，qa 把死分支删除与 retry 修复并成一提交）**已全部进 main**。
qa 曾问「拆成两个 PR 还是保持合并」——现已无所谓，合入即定论，无需再拆。
17 个 save.sh 场景测试（`test/ci/save-sh-scenarios.sh`）已随 PR #22 进 `main`，可当回归基线。

---

## 三大块总览（第 6 轮重算）

v0.2.0 范围由项目所有者定义为三件事。下表**逐条用 `origin/main` 上的实际文件核对过**，
不采信 PR 描述，也不采信任何人的自我汇报。

| 块 | 完成度 | 在 `main` 上的实物证据 |
|---|---|---|
| **A. 测试基础设施调研** | 🟡 **已合入，待实测** | `docs/test-infra.md`（600 行，PR #21）：容器能力边界、`mount.cifs` 真死因是 user namespace、QEMU TCG 实测、Windows 测试路线 |
| **B. Time Machine 兼容功能** | 🟡 **主体已合入，待实测** | durable handle v1/v2（`command/durable.go`、`create_context_durable.go`、`wire/durable_context.go`，PR #20）；oplock/lease break 推送通道（`server/oplock_send.go`，PR #12，**仍不授予**）；配额按本共享用量（`vfs/usage.go`，#14/#15）；旁路元数据 mode 修复（#13）；大目录查找 4.46ms→2.07us（#10） |
| **C. Windows 后端** | 🟠 **一半合入，一半卡住** | ✅ 已合：`vfs/winpath.go` + `vfs/winopen.go`，`validateWindowsName` **已接线 `path.go:203`**（PR #19）；`metadata_path` 跨平台判定（PR #18）已合<br>❌ **`winopen.go` 的 `winOpenParamsFor`/`isNameSurrogateTag` 仍零生产调用点**——消费方 `open_windows.go`（Windows 后端）尚未编写，owner 待定（见 §6.2）<br>⚠️ `internal/meta`（bbolt 旁路存储，PR #26，**draft**）已进队列但**因与 `vfs/metadata_windows.go` 撞车被阻断，勿合**（见 §4.1） |

**整体判断**（对比第 1 轮的「尚无一行合入」）：**v0.2.0 已合入 20 个 PR，
三大块都有实物落在 `main` 上；唯一缺口是 C 块的 `internal/meta`。**

> ⚠️ 两处**刻意不给 ✅**，避免重演 v0.1.0「文档说已实现、实际是 0」的教训：
> - B 块的 oplock/lease **只打通了推送通道，仍不授予**（PR #12 标题自己写明了）。
>   在真正授予之前，不能说「Time Machine 前置能力已具备」。
> - A 块是**调研文档**，不是能跑起来的测试设施。
>   AGENTS.md §3 要求的「至少三种第三方客户端实测」并未因这份文档而完成。

---

## 0. 完成度分档定义（不许含糊）

v0.1.0 吃过一次亏：Time Machine 文档第一版写「已实现全部前置能力」，
实际上 oplock/lease 是 0。为杜绝再犯，本看板一切条目只用下面四档，**不许出现第五种说法**：

| 档 | 含义 | 准入证据 |
|---|---|---|
| ✅ **已合入并实测** | 代码在 `main` 上，且有可复现的验证记录 | PR 号 + 验证方式（含**反向对照**） |
| 🟡 **已提 PR 待评审** | PR 开着，未合入 | PR 号 |
| 🔵 **在写** | 分支上有 commit，未提 PR | 分支名 + 最近 commit |
| ⚪ **没开始** | 分支未建，或建了但无 commit | — |

「已合入但没实测」记 🟡 不记 ✅。**判据不可证伪等于没验**
（`encryption_required` 就这么假阳性过一次：只测了默认路径，没测拒绝路径）。

---

## 1. 本轮头条：瓶颈已经换人了

第 1 轮的瓶颈是「没人开分支」。**该问题已完全解决，且反转成了相反的问题。**

```
origin/main 最后一次前进：12:34:06（PR #8）
现在：                    13:06:43
──────────────────────────────────────────
主干静止 32 分钟，同期新增 8 个开着的 PR
```

| 指标 | 第 1 轮 12:15 | 第 2 轮 13:06 |
|---|---|---|
| 有 commit 的分支 | 4 | 12 |
| 开着的 PR | 0 | **8** |
| 已合入 `main` 的 PR | 1（#1） | 9（#1~#6、#8、#9 等） |
| 主干最后前进 | — | 32 分钟前 |
| 未提 PR 的在写代码 | 约 1500 行 | **约 4000 行（5 条分支）** |

**结论：评审/合并吞吐已成为唯一的全局瓶颈。**
写代码的人没有闲着，成品堆在门口进不来。堆积越久，`internal/vfs/local.go`
这类热点文件的 rebase 冲突代价越高（现在已有 3 个 PR 同时改它）。

---

## 2. 开着的 PR（8 个，全部 mergeable）

| PR | 分支 | 规模 | 主题 | 冲突面 |
|---|---|---|---|---|
| #7 | `win-vfs/platform-split` | +764 | Windows 名字词法规则（设备名表 + 平台条件化结尾点/空格） | **纯新增文件，零冲突** |
| #10 | `tm-vfs/path-exact-first` | +288/-10 | `ResolveParent` 先精确匹配（5 万条目录 4.46ms→2.07us） | `vfs/path.go`，独占 |
| #11 | `docs/changelog-unreleased` | +93/-2 | CHANGELOG 补 #3/#4/#5；dev-workflow 补 CNB 坑；`memory/` 入库 | 纯文档，零冲突 |
| #12 | `tm-lease/oplock` | +1436/-35 | oplock/lease break 主动推送通道（仍不授予） | `server/`+`command/`，独占 |
| #13 | `tm-vfs/metadata-mode` | +466/-9 | 修 Mode 归零 / 纯 mode 变更丢失 / Rename 别名数据丢失 | ⚠️ `vfs/local.go` |
| #14 | `tm-vfs/quota` | +819/-26 | 配额可用空间按本共享用量算，修 TM 被「0 可用」阻断 | ⚠️ `vfs/local.go` |
| #15 | `tm-vfs/quota-startup-check` | +1002/-26 | 启动自检 `quota_bytes` 是否已小于现有用量 | ⚠️ 含 #14；`config/validate.go` |
| #16 | `audit/cred-scan` | +307 | v0.1.0 凭据泄漏审计报告 | ⚠️ `config/validate.go` |
| #17 | `qa/save-sh-residual` | +611/-17 | `save.sh` 三处推送 bug（死分支 / rebase 目标 / worktree 锁）+ 反向验证 | 只碰 `scripts/`+`test/ci/`，零冲突 |

### 2.1 建议的合并顺序（**照此顺序不会产生冲突**）

```
第 0 批（**建议插队最先合**）：
    #17 qa/save-sh-residual
    ↑ 它修的是全队的提交推送入口。当前 9 个 PR 同时开着，save.sh 的 retry 会拿 main
      去 rebase 当前分支并改写历史——任何人跑到它都可能搅乱自己在评审中的 PR。
      早一分钟合，少一分钟暴露。只碰 scripts/ 与 test/ci/，与所有人零冲突。

第 1 批（互不相干，可任意顺序直接合）：
    #11 docs  →  #7 win-vfs  →  #10 tm-vfs/path  →  #12 tm-lease
    ↑ 这 4 个改的文件两两不相交，合完不需要任何人 rebase

第 2 批（vfs/local.go 串行链）：
    #14 quota  →  #15 quota-startup-check（是 #14 的叠加分支，#14 合完它自动瘦身）
    →  #13 metadata-mode（改同一文件，最后合，作者需 rebase 一次）

第 3 批：
    #16 audit（与 #15 同改 config/validate.go，放 #15 之后）
```

> 依据：`git diff --name-only origin/main...origin/<branch>` 逐个比对得出，非推测。
> **只要按批次走，8 个 PR 里只有 #13 一个人需要 rebase。**
> 若乱序合并，`vfs/local.go` 三方混战会让 tm-vfs 连做三次 rebase。

### 2.2 ⚠️ PR #7 的「被架空逻辑」——**第 6 轮已订正，见 §6.2**

> 第 6 轮复核结论：`validateWindowsName` 已在 PR #19 接线（`path.go:203`），`reservedNames` 已删；
> 仅 `winopen.go` 的 `winOpenParamsFor`/`isNameSurrogateTag` 仍零生产调用点（消费方 `open_windows.go` 未写）。
> 原 §2.2 基于 PR #7 刚提出的快照，已过时，保留如下仅作复盘。

逐个函数查调用点（排除 `_test.go`，**原始快照，已过期**）：

```
internal/vfs/winpath.go : validateWindowsName   → 非测试调用点 0
internal/vfs/winopen.go : isNameSurrogateTag    → 非测试调用点 0
internal/vfs/winopen.go : winOpenParamsFor      → 非测试调用点 0
```

`ValidateComponent`（`path.go:178`）仍在用旧的、按作者自己注释「不如 winpath.go 全」的
`reservedNames`（`path.go:200`）。作者知情——`winpath_test.go:186` 注释原话：
「这条是为接线（`ValidateComponent` 改调本函数）准备的回归网」。

这命中本项目反复出现的 bug 形态**「被架空的逻辑」**：代码好、测试全、CI 绿，
但生产路径一次都不走；合进 `main` 后从 diff 完全看不出来。

**处置**：`#7` 可以合（纯新增文件、零冲突），但**不得据此判定「Windows 名字词法规则已完成」**，
CHANGELOG 不许写成「已支持」。已 DM `win-vfs` 确认是刻意拆两步还是漏做；
若是拆两步，「接线 PR」将作为独立条目进入关键路径。

> ~~附带更正：team-lead 任务描述里的「删除死变量 `reservedNames`」**不成立**——
> 它在 `path.go:200` 有真实使用，不是死变量。~~
> **第 6 轮已撤回此更正**：基于过期快照，见 §6.1。

### 2.3 全量复扫：「零产品调用点」只有 #7 一例

对其余 6 个含 Go 改动的 PR，逐个提取 diff 里新增的 `func` 再查非测试调用点，
命中 2 个疑似，**人工复核后两个都是假阳性**：

| 疑似 | 实际 | 结论 |
|---|---|---|
| `tm-lease/oplock` :: `handleOplockBreak` | `oplock.go:9` 用 `register(wire.CommandOplockBreak, …)` 注册 | ✅ 已接线（策略模式分发，AGENTS.md P2，不是裸调用所以扫不到） |
| `tm-vfs/metadata-mode` :: `recordPOSIXMetadata` | `local_handle.go:257` 有真实调用 | ✅ 已接线 |

**所以「被架空的逻辑」不是本轮的系统性问题，只有 #7 一处。**
记下扫描方法本身的盲区：**策略模式注册（`register(...)`）不是函数调用，
机械扫描一定会误报**，任何命中都必须人工看一眼注册表。

---

## 2.4 CI 状态：**9 个 PR 全绿，但 5 条分支的 push 构建是「假红」**（合并前必读）

用 qa 在 PR #17 里新写的 `scripts/ci-status.sh` 同款端点
（`GET /-/build/logs?sourceRef=<分支>`）逐分支查，发现**同一个 commit 会有两条构建记录、
结论相反**：

| 分支 | tip | `pull_request` 事件 | `push` 事件 | 分支内 `.cnb.yml` |
|---|---|---|---|---|
| `qa/save-sh-residual` | 37befb4c | ✅ success | ✅ success | 新 |
| `docs/changelog-unreleased` | fe2165e2 | ✅ success | ✅ success | 新 |
| `tm-vfs/path-exact-first` | 09c8927b | ✅ success | ✅ success | 新 |
| `tm-vfs/metadata-mode` | 68dae1ed | ✅ success | ✅ success | 新 |
| `win-vfs/platform-split` | 9d6b1111 | ✅ success | ❌ **error** | 旧 |
| `tm-lease/oplock` | a66fc8e2 | ✅ success | ❌ **error** | 旧 |
| `tm-vfs/quota` | b9b7a2bd | ✅ success | ❌ **error** | 旧 |
| `tm-vfs/quota-startup-check` | 75330ed9 | ✅ success | ❌ **error** | 旧 |
| `audit/cred-scan` | ef8e4e49 | ✅ success | ❌ **error** | 旧 |

**相关性 100%，零例外：`.cnb.yml` 是旧版 → push 构建必红；是新版 → 必绿。
与分支上的代码毫无关系。**

成因链条完整可查：

1. 旧 `.cnb.yml`（blob `28c9a48d`）里 `gate_test` 写的是 `CGO_ENABLED=0 go test -race`；
2. **race 检测器依赖 cgo**，go 直接拒绝执行，0.1 秒退出、返回码 2；
3. 它是流水线第一个失败的 stage，后面四关全被 skip —— 这正是 PR #9 修掉的死结；
4. `push` 事件用**分支自己那份** `.cnb.yml` 跑，旧分支拿到的还是坏的那份 → 必红；
5. `pull_request` 事件跑的是**与 main 的合并预览**，拿到的是修好的 `.cnb.yml` → 绿。

### 结论与处置

- **没有任何一个 PR 因 CI 被卡住。9 个 PR 的 PR-事件构建全绿，可以合。**
- **不要看 push 构建的红去拦 PR** —— 那是历史包袱的回声，不是代码的问题。
- 想让 push 也变绿：分支上 `git pull --rebase origin main` 拿到新 `.cnb.yml` 即可，
  不必为此改一行代码。

> ⚠️ **这才是真正危险的地方**：`tm-handle/durable`（+1677）、`win-meta/metadata-store`
> （+1611）、`r-infra/test-infra`（+600）这三条**没有 PR**，因此只有 push 事件的构建记录，
> 三条全是红的。**任何人扫一眼都会得出「这些代码是坏的」**——
> 而 `r-infra/test-infra` 改的是**一个纯 Markdown 文件**，不可能编译失败。
> 这三条红灯 100% 来自旧 `.cnb.yml`，与代码无关。§4.3 / §4.4 代开 PR 时不要被它误导。

**记一笔方法论**：R1 是「假绿」（CI 没真跑却显示通过），这次是**「假红」**——
同一个坑的镜像。两者的共同教训是：**CI 的颜色本身不是证据，
「这个颜色是哪条流水线、在哪份配置下、对哪个 commit 得出的」才是证据。**

---

## 3. 逐人盘点（第 2 轮）

哑火线：**分支已建但 >25 分钟无新 commit，且手上没有开着的 PR。**

| 成员 | 分支 | 产出 | 最后动作 | 静默 | 档 | 判定 |
|---|---|---|---|---|---|---|
| `tui-diag` | `tui-hang` | +135 | 13:00 | 6 min | 🔵 | ✅ 活跃 |
| `win-vfs` | `platform-split` | +764 | 12:41 | 25 min | 🟡 #7 | ✅ 正常（在等评审） |
| `win-meta` | `metadata-store` | +1611 | 12:20 | **46 min** | 🔵 | 🔴 **见 §4.1** |
| `qa` | `save-sh-residual` | +611 | 13:11 | 3 min | 🟡 #17 | ✅ **已解除哑火（§4.2）** |
| `tm-handle` | `durable` | +1677 | 12:40 | 26 min | 🔵 | 🔴 **见 §4.3（不在册）** |
| `r-infra` | `test-infra` | +600 | 12:30 | 36 min | 🔵 | 🟠 **见 §4.4（不在册）** |
| `tm-vfs` | 4 条分支 | +2575 | 12:47 | — | 🟡 #10/#13/#14/#15 | ✅ 产出最高，已交付 |
| `tm-lease` | `oplock` | +1436 | 12:44 | — | 🟡 #12 | ✅ 已交付 |
| `audit` | `cred-scan` | +307 | 12:49 | — | 🟡 #16 | ✅ 已交付 |
| `docs` | `changelog-unreleased` | +93 | 12:43 | — | 🟡 #11 | ✅ 已交付 |
| `pm` | `pm/status` | 本文件 | 13:06 | — | 🔵 | — |

**在册成员**（`pm` 的 teammate 名单）：`team-lead`、`tui-diag`、`win-vfs`、`win-meta`、`qa`、`pm`。
`tm-vfs`/`tm-lease`/`audit`/`docs`/`tm-handle`/`r-infra` 已不在册 —— 前四位交付完 PR 后退出，
**后两位带着未提 PR 的代码退出**，见 §4.3 / §4.4。

---

## 4. 需要立刻处理的四件事

### 4.1 🟡 `win-meta`：PR #26 已提（draft），但**发现撞车阻断项，勿合**

第 6 轮复核（`origin/main` = `ef73b549`，`origin/win-meta/metadata-store` = `4fa81a4`）：

**进度**：
- `metadata_path` 跨平台判定 bug 修复（原 PR #18）**已合入 main** ✅。
- `internal/meta`（Store 接口 + bbolt + noop + 单测）**已提 PR #26（draft）**，head `4fa81a4`
  （`6c222d5` 建 store + `4fa81a4` 补单测：崩溃恢复 / meta 页撕裂 / 并发 / Reap 分批续扫）。
- 早先我记的「`bolt.go` 改动消失、未提交」是**误报**：那是变异测试残留，win-meta 还原前
  已跑对照确认常量被测试真实覆盖，工作树干净。两个 commit 12:20 已 push。**R2 未兑现。**

**R4 bbolt 合规：结论通过，但必须带 `-tags metabolt` 才验得到（否则假绿）**：
- `bolt.go` 挂在 `//go:build windows || metabolt`；不带 tag 编的是 `noop.go`，bbolt 不在依赖图，
  `go list -deps` / `go test` 全绿但什么都没验。带 tag 后：纯 Go（4 个非标包 CgoFiles 全空，
  linux+windows 均验过）、License（bbolt MIT / x/sys BSD-3 在白名单、无 AGPL）、
  `CGO_ENABLED=0` 五平台交叉编译全过。
- `go.mod` 里 bbolt 标 `// indirect`（build tag 让 linux 视角看不见直接引用），`go mod tidy`
  按 GOOS=windows 保留、不会误删，但标记不准——仅供参考。

**⚠️ 新阻断项（比 R4 严重，已记入风险 R11）：`internal/meta` 与 `internal/vfs/metadata_windows.go`
是两套会互相破坏数据的并存 bbolt 实现，PR #26 在定下来前不应合入 main。**

我用 `origin/main` + `origin/win-meta/metadata-store` 实物复核，win-meta 所述成立：

```
vfs/metadata_windows.go : bucket "posix"，记录 12 字节 = UID[0:4] GID[4:8] Mode[8:12]，无版本字节
                          local.go:117 已调用 openMetadataStore（已接线，是生产代码）
internal/meta/store.go  : bucket "posix"（同名！），记录 21 字节 = version[0]=1 UID[1:5] GID[5:9] Mode[9:13] FileID[13:21]
                          internal/meta 当前 0 import（死代码）
```

撞车机制（静默、不报错）：两者若指向同一 db 文件，vfs 解码器读 `buf[0:4]` 当 UID，
拿到的是 meta 的 `version` 字节（=1）；meta 解码器读 vfs 的 12 字节记录会因
`buf[0]!=recordVersion` 判「无记录」——**一个方向吐垃圾 uid/gid/mode，另一个方向静默丢记录，
都不报错**（21>12 长度检查拦不住）。`internal/meta` 还多出 `GetDir`（整目录批量取，
Time Machine `.sparsebundle` band 目录数万文件的性能命门）和 `Reap`（陈旧回收），能力是超集。

**win-meta 建议处置**（涉及 win-vfs 的文件，按 §7.3.3 他不擅自动手，需 team-lead 拍板）：
`internal/vfs` 改为依赖 `internal/meta`，删掉 `metadata_windows.go` 的 bbolt 实现，
并将 bucket 由 `posix` 改名为一个不同于 `posix` 的新名字（**实装为 `posix.v2`，带点**，见 D6）——**不写任何迁移代码**：`vfs/metadata_windows.go` 的 build tag 是 `//go:build windows`，本项目从未在真实 Windows 上运行过，那份 12 字节记录的库文件在地球上任何机器上都未被创建；v0.1.0 是私密 prerelease、无外部用户；迁移代码无样本可测、属静默失败形态——见 D6。PR #26 描述里已写明保持 draft。

<details>
<summary>第 5 轮误报原文（保留，作复盘：未提交改动可能是变异测试残留）</summary>

第 5 轮（13:27）：`/work/win-meta` 工作树已变干净，`HEAD` = `origin/win-meta/metadata-store`
（`66da303`），`internal/meta/bolt.go` 那处改动既不在 commit 也不在工作树。第 6 轮 win-meta 澄清：
那 1 行是变异测试残留（把 `subtreeSkipByte` 由 `"0"` 改成 `"/"`），提交会引入 bug；还原前跑完
对照（`TestGetDir` 在 `bolt.go:178` 死循环 90s 超时）确认常量被测试真实覆盖，已 `git checkout`
还原。**非丢失、非违规，是快照误读。**

</details>

### 4.2 ✅ `qa`：已解除（save.sh 修复随 PR #22 合入 `main`）

> **第 6 轮订正**：qa 担心的「`gh` 不可用、PR 需 team-lead 开」**已解决**——
> 复核 `qa/save-sh-residual` 的 head `fda24e2` 正是 **PR #22 的合并 head**，已合入 `main`。
> 即 retry rebase 目标修正（`main`→`origin/$BRANCH`）+ worktree `.git` 锁失效修复 +
> 死分支删除（commit `37befb4`，qa 把死分支与 retry 修复并成一提交）**全部进 main**。
> qa 曾问「拆两个 PR 还是保持合并」——现已无所谓，合入即定论。17 个 save.sh 场景测试
> （`test/ci/save-sh-scenarios.sh` + `testdata/save-legacy.sh` 反向基线）随 PR #22 进 `main`，
> 可作回归基线。qa 接下来正式开动 `qa/acceptance-v020`。

原判定（第 2 轮 13:06）：`qa/acceptance-v020` 建了分支但 32 分钟零 commit。
**qa 在 `qa/save-sh-residual`（`/work/qa-save`）上交付 save.sh 修复**，内容超出原任务范围：

- 删死代码分支（`BASE` 两支同值）——原任务项；
- retry 的 rebase 目标 `main` → `origin/<当前分支>`——原任务项，且 qa 指出它是**双重错误**
  （治不了病：push 被拒是因 `origin/$BRANCH` 有本地没有的提交，从 main 拉多少次都拿不到，
  6 次重试全部空转；还添新病：改写本分支历史）；
- **额外发现第三处、之前无人知晓的阻断性 bug：worktree 锁失效**；
- 配套 `test/ci/testdata/save-legacy.sh`（+236）做**反向对照**——符合本看板 ✅ 档准入要求。

> 记一笔：qa 的 `qa/acceptance-v020` 分支**至今没有远端**（`git branch -r` 无此分支），
> 违反 AGENTS.md §7.3.1「worktree 建完立刻推空分支」。当前它 upstream 指向 `origin/main`，
> 在这个状态下裸跑 `git push` 就是那个「静默推错目标」事故的土壤。已提醒。
>
> 讽刺的是 PR #17 修的正是同一类「打印已推送但一个 commit 都没出去」的静默失败。

<details>
<summary>第 2 轮原文（保留，用于复盘判定准确性）</summary>

### 4.2-旧 🔴 `qa`：`qa/acceptance-v020` 建了分支但 32 分钟零 commit

- 分支 tip 就是 `origin/main`，工作树干净，没有任何在写的痕迹。
- team-lead 指派的任务是「`scripts/save.sh` 残留 bug（死代码分支 + retry 用 main rebase
  改写分支历史）」。这个任务**危险度被低估了**：`save.sh` 的 retry 路径会拿 `main`
  去 rebase 当前分支并改写历史，在现在这个「8 个 PR 同时开着」的局面下，
  任何一个人跑到它都可能改写自己 PR 的历史，把评审中的 PR 搅乱。
- **建议**：确认 qa 是否还活着；若活着，把这条 PR 的优先级提到第 1 批一起合。

**结果：判定正确，处置有效**——DM 发出约 5 分钟后 qa 交付 PR #17，
且确实按建议「单独切最小 PR、不与 acceptance 攒在一起」。

</details>

### 4.3 🔴 `tm-handle`：1677 行 durable handle 无 PR，且该 agent 已不在册

- 6 个 commit，含 create context 注册表重构、durable v1/v2 授予/重连/超时/校验、
  可证伪单测、以及一个**死锁修复**（RemoveTree 与 reconnect 加锁顺序反转）。
- 这是 v0.2.0 Time Machine 块里**单笔价值最高的未交付资产**，而作者已不在名单里。
- 好消息：代码在 `origin/tm-handle/durable` 上，已推送，**不会随容器崩溃丢失**。
- **建议**：team-lead 确认 tm-handle 是否已被关闭。若是，指派任何一位在册成员
  （或 team-lead 自己）代为开 PR —— 分支已在远端，`POST /-/pulls` 一条命令的事，
  不需要原作者在场。放着不管等于白写 1677 行。

### 4.4 🟠 `r-infra`：600 行调研文档无 PR，agent 已不在册

- test-infra A/B 两组结论（容器能力边界、`mount.cifs` 真死因是 user namespace、
  QEMU TCG 实测、Windows 测试路线）—— 这是 v0.2.0 三大块里 **A 块的全部内容**。
- 同 §4.3，代码已推送不会丢，但需要有人代为开 PR 才能进 `main`。
- 优先级低于 §4.3（纯文档，不阻塞任何人写代码），但同样不该烂在分支上。

---

## 5. 关键路径与瓶颈

```
【最高】team-lead 评审吞吐 ──► 8 个 PR、约 5100 行全部卡在这里
                              主干已静止 32 分钟

tm-handle: create context 注册表重构（+1677，无 PR）──► tm-lease PR #12 的后续
                                                        （#12 本身不依赖它，可先合）

#14 quota ──► #15 quota-startup-check ──► #13 metadata-mode ──► #16 audit
              （vfs/local.go + config/validate.go 串行链，见 §2.1）

win-meta: bbolt 合规✅(带-tags metabolt) ──► internal/meta PR #26(draft)
          ──► ⚠️ 与 vfs/metadata_windows.go 同 bucket "posix" 撞车(R11)
          ──► 处置已定（D6）：vfs 依赖 meta、删 vfs bbolt、bucket 改名为不同于 posix 的新名（实装 posix.v2）不迁移、CI 归 qa-e2e（实装随 #26 合入，见 §10.3/§11）
win-vfs: winopen.go 的 winOpenParamsFor/isNameSurrogateTag 由 `openhost_windows.go` 接缝消费（owner=win-vfs，PR #30，第二步 CreateFileW 暂缓）
```

**当前瓶颈（按紧迫度，第 6 轮）**：

1. **⚠️ R11 双 bbolt 撞车**：`internal/meta`(PR #26) 与 `vfs/metadata_windows.go` 并存会静默损坏数据，
   PR #26 必须保持 draft 直到 team-lead 定处置（vfs 依赖 meta / 删 metadata_windows.go bbolt / **bucket 改名为不同于 posix 的新名（实装 posix.v2）、不迁移**）。
   这是当前唯一会「合了反而更糟」的 PR。
2. **评审吞吐**：现仅 4 个开 PR（#24/#25/#26/#27）。建议先合零冲突的 #25（团队纪律）、#27（名字词法相关），
   #24（本看板）我排最后。#26 在 R11 解决前勿合。
3. **winopen 缺口**：`winOpenParamsFor`/`isNameSurrogateTag` 已由 win-vfs 的 `openhost_windows.go` 接缝收口（owner=win-vfs，PR #30），第二步 `CreateFileW` 暂缓。
4. **qa / acceptance 正式开动**：save.sh 修复已随 #22 进 main，qa 接下来跑 `qa/acceptance-v020`（v0.2.0 验收）。

---

## 6. 风险登记

| # | 风险 | 现状 | 应对 |
|---|---|---|---|
| R1 | **CI 假绿**：`test/` 因 build tag 从未被真正编译 | ✅ **已解除**（PR #9 修复流水线死结，已合入 `main`） | 保持关注 |
| R2 | **憋大提交 / 未推送即丢失** | ✅ **已解除（第 6 轮）**：win-meta 工作树「未提交改动」是变异测试残留，已还原、工作树干净、12:20 两 commit 早 push | 记一笔：未提交改动先问「是不是测试残留」再记风险 |
| R3 | **单点文件冲突**：`internal/vfs/local.go` | 🟠 **已发生**：#13/#14/#15 三方同改 | §2.1 给出串行合并顺序，可只让 1 人 rebase |
| R4 | **bbolt 依赖合规**（纯 Go / License / 零 CGO） | ✅ **通过（第 6 轮）**：带 `-tags metabolt` 验过（不带 tag 假绿）；结论见 §4.1 | 验收请用 `go build -tags metabolt` / `go vet -tags metabolt` |
| R5 | **「已验证」无反向对照** | 待防 | 任何 ✅ 档准入必须写明失败用例 |
| R6 | 容器 2C4G，V8 堆上限触发 SIGABRT | 已有 `scripts/devenv.sh` 削峰（PR #1 已合） | 全员 `. scripts/devenv.sh`、输出加 `\| head -N` |
| **R7** | **无主分支**：作者已退出、代码未提 PR（tm-handle +1677、r-infra +600） | 🟠 已代开 PR（#20/#21 已合） | §4.3 / §4.4 已闭环 |
| **R8** | **PR 堆积期越长，热点文件 rebase 代价越高** | 🟢 已收敛：现仅 4 个开 PR | 按 §6.4 顺序合 |
| **R9** | **「被架空的逻辑」**：PR #7 新增 764 行 | 🟢 **已闭环（第 9 轮）**：`validateWindowsName` 已接线（`path.go:203`，PR #19）；`winOpenParamsFor`/`isNameSurrogateTag` 已由 win-vfs `openhost_windows.go` 接缝预留调用点（TODO），待第二步 `CreateFileW` 接线（owner=win-vfs，PR #30），非死代码失控 | §6.2；**今后凡新增函数零调用点，合并前必问一句** |
| **R10** | **CI「假红」**：旧 `.cnb.yml` 致 push 构建必红，与代码无关 | 🟢 已不阻塞：各分支已 rebase 拿到新 `.cnb.yml` | §2.4。**别拿 push 的红拦 PR** |
| **R11** | **🔴 新增·双 bbolt 实现撞车**：`internal/meta`（PR #26）与 `internal/vfs/metadata_windows.go` 同写 bucket `posix`、记录布局不同 → 静默数据损坏（见 §4.1） | 🟢 **已决（第 11 轮）**：vfs 依赖 meta、删 vfs bbolt、bucket 改名为不同于 `posix` 的新名（**实装 `posix.v2`，带点**）、不迁移；PR #26 待合（现 conflict 非 CI，见 §11） | 处置：rename posix→posix.v2、不迁移、CI 实装随 #26 合入（今后归 qa-e2e） |
| **R12** | **🔴 新增·durable handle 端到端是坏的**：重连「成功」但拿回的句柄一用就 `STATUS_INVALID_PARAMETER`——`reconnect()` 只改 `open.Session` 不改 `open.Tree`，被 `close.go:42` 跨树防护挡掉，READ/WRITE/CLOSE 全废、连关都关不掉 | 🔴 **头号技术风险（第 11 轮立）**：PR #20 已合入 main → **主干上该特性完成度实际 0%**；Time Machine 强依赖它。修复归 `qa-proto`；team-lead 裁决 Persistent 用**进程级全局计数器**，原「SessionId 入键」方案被 qa-proto 实测推翻（durable 的存在意义即跨会话重连，塞 SessionId 等于结构性废掉）。见 §11 | 单独立项跟踪；修完前 Time Machine 验收判据不可标 ✅ |
| **R13** | **🟠 新增·`test/integration` 在 main 上是红的**：qa-e2e 实测 3 个失败，但都是测试侧问题、产品没坏 | 🟠 **已定位（第 11 轮）**：该目录从未在 CI 运行（CI 只编译不执行），红了无人知。是「build tag 后代码从未真校验」洞的又一例 | 见 §11；纳入 CI 实际运行（归 qa-e2e） |

---

## 7. 待决事项（需 `team-lead` 拍板）

| # | 事项 | 谁在等 |
|---|---|---|
| D1 | 是否采纳 §2.1 的分批合并顺序？（照此只需 1 人 rebase，乱序则 tm-vfs 要 rebase 3 次） | 全队 | **已历史解决**：#7/#10/#11/#12/#13/#14/#15/#16/#17/#18/#20/#21/#22/#23 全合入 |
| D2 | bbolt 依赖是否批准（纯 Go / License / 零 CGO 三项核验由谁出结论） | `win-meta`、`win-vfs` | **第 6 轮已核验通过（带 `-tags metabolt`；不带 tag 假绿）**，无需再批；但引出 R11 |
| D3 | `tm-handle` / `r-infra` 是否已关闭？其分支由谁代为开 PR？ | `pm`、Time Machine 块整体 | ✅ 已闭环：#20 / #21 已合入 |
| D4 | `qa` 是否还活着？`save.sh` 历史改写 bug 要不要插队进第 1 批？ | 全队（它会改写别人 PR 的历史） | ✅ 已闭环：save.sh 修复随 PR #22 合入 main |
| D5 | PR #7 的接线是刻意拆两步还是漏做？合并时如何措辞才不误判为「已完成」？ | `win-vfs`、CHANGELOG | **第 6 轮已答**：`validateWindowsName` 漏了、已 PR #19 接线；`winOpenParamsFor`/`isNameSurrogateTag` 刻意拆，由 `openhost_windows.go` 接缝收口（owner=win-vfs，PR #30） |
| **D6** | ⚠️ **R11 双 bbolt 撞车如何处置？** `internal/meta`(PR #26) 与 `vfs/metadata_windows.go` 谁留谁删、bucket 改名、CI 归属 | `win-meta`、`win-vfs`、`qa-e2e`、`team-lead` | **已决（第 11 轮订正）**：vfs 依赖 meta、删 vfs bbolt；**bucket 改名为一个不同于 `posix` 的新名字（实装 `posix.v2`，带点），不写迁移代码**。`posix.v2` 而非 `posix2` 是实装中已 CI 绿的字面量，team-lead 第 11 轮推翻 PM「改回 posix2」建议、采纳 `posix.v2`（理由升级见下）。**改名核心价值 = 反方向安全降级：用户先跑 v0.2.0（21 字节记录，bucket=posix.v2）再回退 v0.1.0 旧二进制时，旧代码看不到 `posix` 这个 bucket → 判「无记录」→ 走安全默认，而绝不会把 21 字节新记录按 12 字节旧格式解出垃圾 uid/gid/mode**。将来谁想再改名须先满足这条（理由比名字字面量重要）。另：**CI（`.cnb.yml`/`test/ci`）归 qa-e2e**，但本次解阻塞实装由 win-meta 在 #26 内完成（已绿）随 #26 合入；vfs 适配层归 win-vfs。PR #26 待合（现 conflict 非 CI，见 §11）。 |

> D1 的对照实验已做完（`git diff --name-only` 逐分支比对），不是推测，可直接执行。

---

## 8. 纪律检查（每轮必查）

| 项 | 第 2 轮结果 | 第 6 轮复核 |
|---|---|---|
| 有无向 `main` 直推 | ❎ 无。`main` 的每一次前进都是 Merge PR 提交，合规 | ✅ 仍合规；本期新合 #18/#20/#21/#22/#23 全是 Merge |
| 有无久不 push | ⚠️ **有**：`win-meta` 工作树 `internal/meta/bolt.go` 未提交，静默 46 分钟 | ✅ **误报已撤**（第 6 轮 §4.1）：那是变异测试残留，已还原，工作树干净，12:20 两 commit 早 push |
| 有无在 `/workspace` 改文件 | ❎ 无。`/workspace` 干净且停在 `main` | ❎ 无（pm 只在 `/work/pm` 改看板） |
| 有无「已验证」缺反向对照 | 本轮无新增 ✅ 档条目，暂不适用 | ⚠️ **R4 假绿陷阱**：bbolt 合规不带 `-tags metabolt` 全绿但没验，已记入 §4.1 |
| worktree 隔离是否生效 | ✅ 12 个 worktree 各自独立，本轮未发生 §10.3 第 6 条那类串扰 | ✅ 仍生效 |

---

## 9. 第 7 轮（PM 收 win-meta 三报，2026-08-09 ~13:40）

win-meta 三条报告（bolt.go 主动 revert / R4 通过 / PR #26 已 open 且 rebase）已逐条核对仓库真实状态，结论：

- **R2 反证 + 误报撤除确认**：bolt.go 的 modified 是 win-meta 主动做的变异实验（subtreeSkipByte `0x30`→`0x2F`），被 `TestGetDir` 在 `-tags metabolt` 下杀出死循环 90s 超时后已 revert，工作树干净。非崩溃吞没，是刻意丢弃。R2 风险表维持「已解除」。
- **R4（bbolt 合规）：通过，建议关闭**。结论见 §4.1 / PR #26 描述（纯 Go/零 CGO、MIT+BSD-3 非 AGPL、四平台零 CGO 交叉编译 OK）。
- **PR #18 已 closed/merged**（13:18 那批 9 个 PR 之一），从待合清单移除。
- **记忆同步已进 main**：team-lead 把 win-meta 那批记忆直接提交进 main（commit `8481aae`，HEAD），并新增 `memory/project_metadata_store_consolidation.md`。win-meta 提议的 `win-meta/memory-sync` 快车道分支**不必开**——记忆没卡在 PR #26 后面。
- **D6（R11 双 bbolt 撞车）：决策已下** → 以 `internal/meta` 为唯一真源，vfs 依赖它、删 `metadata_windows.go` 的 bbolt（已写入 `project_metadata_store_consolidation.md`；task #17 completed，task #16 实施中）。
  - 分工：win-meta 拥有 `internal/meta`（PR #26，1611 行，draft 维持不合）；**win-vfs 拥有 vfs 适配层 + 删除**（task #16）；**CI（`.cnb.yml` / `test/ci/`）归 qa-e2e**（§7.1 文件级分工：qa 角色拥有 `test/`+`scripts/`+CI 配置）。
  - 对接要点：vfs 新建 `windows||metabolt` 适配文件调 `meta.Open`，`metaAdapter` 互转并记录 `GetDir`/`Reap`；删 vfs bbolt；**rename bucket `posix` → `posix2`，不写任何迁移代码**（理由见 D6 / §4.1）；`metadata_other.go` 改 `!windows&&!metabolt`。

**CI 假绿洞（责任人已订正为 qa-e2e）**：
- **CI `.cnb.yml` 从不带 `-tags metabolt`**（`grep metabol .cnb.yml` 为空）→ `internal/meta` 的 393 行 bolt.go（占 PR #26 约 64%）与 vfs 适配层在 Linux CI 从不编译/测试，是假绿洞。PM 原指派 **win-vfs**，经 team-lead 订正，**责任人应为 qa-e2e**（§7.1：`.cnb.yml` 与 `test/ci/` 是 qa-e2e 专属所有权）。
- **重要事实更新**：win-meta 已在 PR #26 上**自行修好此洞**（head `18d8d2b`，已 push）并报告 CI 真红转绿——根因是 `test/ci/check-test-compile.sh` 的 build-tag 自检遇到 `!metabolt` 反向约束直接 `exit 1`，他把 `metabolt` 加进该脚本的 KNOWN/TAGS、并在 `.cnb.yml` 加了 `gate_metabolt`（三处锚点 push/pull_request/tag_push：`go build -tags metabolt ./...` + `go test -tags metabolt ./internal/meta/...`）。他称是「按 team-lead 授权」改的 CI 公共件。
- **待 team-lead 拍板的所有权冲突**：D6 把 CI 归 qa-e2e，但 win-meta 已在同一 PR #26 里实装了同样的修复。建议：**接受 win-meta 在 #26 里的实装作为本次解阻塞的权威改动**（它已绿、且和 qa-e2e 的既定分工不冲突——这部分随 #26 合入），**今后 `.cnb.yml`/`test/ci/` 的改动归 qa-e2e**。若 team-lead 要求 qa-e2e 另起 PR 重做，请明示，我转达。

**进度总览**：完成 12 项；进行中 1 项（#16 双实现对接，win-meta 已实装 CI 修复 + bucket 改名，待 team-lead 合 #26）；pending 4 项（#11 winopen 消费方 / #13 durable 验证 / #14 ValidateComponent 绕过核查 / #15 e2e 冒烟）——均与原计划一致，无新增阻塞。新增待合 PR：**qa-vfs/verify（`db03c4d`）**，team-lead 即开。

---

## 10. 第 8 轮（PM 收 team-lead 两处订正 + win-meta #26 真红转绿，~13:45）

team-lead 就我转给 win-vfs 的对接要点下发两处订正，且 win-meta 报告 #26 已修好并 push。已据「以 team-lead 这条为准」更新全板（D6 / R11 / §4.1 / §5 / §9 均已同步）。

### 10.1 订正 1：`-tags metabolt` CI 步骤归 **qa-e2e**，不是 win-vfs
- `.cnb.yml` 与 `test/ci/` 是 qa-e2e 专属所有权（§7.1 文件级分工，qa 角色不治产品代码、拥有 `test/`+`scripts/`+CI 配置）。team-lead 已直接指派 qa-e2e 两件：
  - **优先级 A**：修 `test/ci/check-test-compile.sh` 支持反向 build 约束——现 `case "!*"` 直接 `exit 1`，是 PR #26 在 push/pull_request 两条流水线都红的**唯一真因**（team-lead 在 `/tmp/mbchk` 复现过：`错误: 发现反向 build 约束 '!metabolt'`）。修法：遇 `!tag` 时带 tag 与不带 tag 各跑一次 `go vet`，两次都要过；未知 tag 守卫保留。
  - **优先级 B**：`.cnb.yml` 加 `gate_metabolt`（push/pull_request/tag_push 三处锚点）：`CGO_ENABLED=0 go build -tags metabolt ./...` + `go test -tags metabolt ./internal/meta/...`。
- **win-vfs 不要动 `.cnb.yml`**；若已动则 revert 那部分再推。
- **已转 win-meta**：R4 关闭同意，CI 假绿洞已受理，责任人 qa-e2e。

### 10.2 订正 2：**不做旧 posix bucket 的迁移 shim**
- D6 原文决定是 **rename bucket `posix` → `posix2`，且明确不写任何迁移代码**。理由（已记入全板）：
  1. `vfs/metadata_windows.go` build tag 是 `//go:build windows`，本项目从未在真实 Windows 上运行过——CI 只交叉编译不跑 Windows 二进制，那份 12 字节记录的库**在地球上任何机器上都没被创建过**。
  2. v0.1.0 是私密 prerelease，无外部用户，不存在需保数据的现场。
  3. 迁移代码不可测试（无旧库样本可喂），写出来的 shim 只能靠臆想，等于往仓库塞一段永不被执行、永不被证伪的分支——即 `memory/project_silent_success_failures.md` 六起事故的同形态。
- win-meta 只需在 `internal/meta/bolt.go:57` 把 `bucketName` 由 `posix` 改为 `posix2`，并在上方写「为何换名、为何不迁移」注释。**不新增任何 migrate 函数。**

### 10.3 ⚠️ 两处需 team-lead 拍板的不一致（PM 已查出）
1. **bucket 名不一致（已解决 · team-lead 第 11 轮推翻 PM 建议）**：PM 在 §10.3 建议「以 team-lead 的 `posix2` 为准、让 win-meta 改回」——**此建议已撤回并转达撤销**。team-lead 第 11 轮明确采纳 win-meta 实装的 **`posix.v2`**（带点），理由比 PM 原「避免撞库」更强：反方向安全降级（见 D6 新记理由）。已实装且 CI 已绿的字面量即为正确，为此再跑一轮 CI 换名是纯浪费。**PM 早先发给 win-meta 的「改回 posix2」指令作废。**
2. **CI 实装者 vs 责任人不一致（已解决 · 归 #26）**：team-lead 第 11 轮直接对 win-meta 确认「`check-test-compile.sh` 的改法我采纳你的…这件事就**归你这个 PR 了**，我会通知 qa-e2e 别重复做」，且确认 qa-e2e 对 `check-test-compile.sh`/`.cnb.yml` **零 diff**。所以 #26 里的两处修复（`metabolt` 进 KNOWN/TAGS + `gate_metabolt`）**随 #26 合入、不 revert**；win-meta 以 team-lead 直接发给他的口径为准（与经 PM 中转的「`.cnb.yml`/`test/ci` 归 qa-e2e 专属」表面冲突，但 team-lead 已明确本次归 #26、今后变更归 qa-e2e，无实质矛盾）。板上记为「#26 内实装、team-lead 采纳、qa-e2e 不重复」。若 team-lead 改主意要挪给 qa-e2e 重做，会令 win-meta 从 #26 撤出（代价：metabolt 覆盖回退到零直到 qa-e2e 补上，team-lead 知情）。

### 10.4 环境观察（与 R2 相关）：编辑疑似被自动提交
- win-meta 报告：他做完编辑后，git 里已是一个**已提交并 push 的 commit（`18d8d2b`）**——环境里似乎有自动提交/落盘机制，提交动作不在他显式控制下。内容是他 intended 的那批（8 文件已逐项核对），非丢失、非串扰。
- PM 启示：今后盘点「谁提交了什么」时，若发现 commit 作者/时间对不上，**优先怀疑此自动提交机制，而非越权改文件**。R2 风险表维持「已解除」，但把这条加进 R2 备注，避免误判。

### 10.5 当前待合 PR（新增）
- **qa-vfs/verify（`db03c4d`）**：team-lead 即开 PR，加进待合清单。
- **已合入（第 11 轮同步）**：PR #24（本看板，sha `986b1e0` → main）、PR #25（AGENTS.md §7.5）、PR #27（win-vfs `IsWindowsSlash`）、PR #28（qa-vfs 变异测试）、PR #29（qa-proto durable 缺陷复现，team-lead 代开）。main 现 = `1acae3f`。**仅余 #26 开着（conflict 非 CI，见 §11）。**

---

## 11. 第 11 轮（PM 收 team-lead 拍板 posix.v2 / CI 归属 + 同步已合 PR + 两条重大风险，2026-08-09）

team-lead 下发两处拍板、同步 5 个已合 PR、并新报两条重大风险 + 两名新成员。全板已据「以 team-lead 为准」更新（D6 / R11 / §4.1 / §5 / §10.3 / §6.4）。**PM 只动本文件，MEMORY.md 条目由 team-lead 写**（新约定，§10 末确认）。

### 11.1 两处拍板（均推翻 PM 第 10 轮建议，原指令已撤回）

1. **bucket 名用 `posix.v2`（带点），不退 `posix2`**——PM 第 10 轮建议「让 win-meta 改回 posix2」**已作废并转达撤销**。理由升级：原只为防止撞库；win-meta 补的「**反方向安全降级**」才是改名真正的价值——用户先跑 v0.2.0（21 字节记录，bucket=posix.v2）再回退 v0.1.0 旧二进制时，旧代码看不到 `posix` 这个 bucket → 判「无记录」→ 走安全默认，而**不是**把新记录按 12 字节旧格式解出垃圾 uid/gid/mode。**理由比名字字面量重要**，将来再改名须先满足这条。板上 D6 已改写：改名 = 不同于 `posix` 的新名（实装 posix.v2）、不迁移、记反方向降级理由。
2. **CI 实装归 #26 内（team-lead 直接口径）**——team-lead 直接对 win-meta：「`check-test-compile.sh` 的改法我采纳你的…这件事就**归你这个 PR 了**，我会通知 qa-e2e 别重复做」。已核 `origin/qa-e2e/ci` 对 `test/ci/check-test-compile.sh` 与 `.cnb.yml` **零 diff**，无撞车只有一份实装；win-meta 把 `metabolt` 并进脚本 `KNOWN`/`TAGS`、与 `!windows` 同套。故 #26 内修复随 #26 合入、不 revert；**今后 `.cnb.yml`/`test/ci/` 改动归 qa-e2e**（§7.1）——与「本次归 #26」无矛盾。详见 §10.3-2。

### 11.2 PR 同步（main → `1acae3f`）

- ✅ #24（本看板，sha `986b1e0`）、#25（AGENTS.md §7.5）、#27（win-vfs `IsWindowsSlash`）、#28（qa-vfs 变异测试）、#29（qa-proto durable 缺陷复现，team-lead 代开）**全合入**。
- ⚠️ **#26 现 `mergeable_state: conflict`（非 CI，CI 已绿）**：冲突仅两文件 `memory/MEMORY.md` 与 `memory/project_config_path_platform_semantics.md`。team-lead 已令 win-meta merge main 后按并集解掉（MEMORY.md 由 team-lead 维护，PM 不碰）。解完即可合。
- 🟡 **#30（win-vfs/openhost-seam，`openhost_windows.go` 接缝，R9 收口第一步）**、**#33（win-meta 分隔符收尾小 PR）** 现开着，不阻塞主线。详见 §6.4。

### 11.3 🔴 头号技术风险 R12：durable handle 端到端是坏的

- qa-proto **线级实测**：重连「成功」，但拿回的句柄一用就 `STATUS_INVALID_PARAMETER`。
- 根因：`reconnect()` 只改 `open.Session` 不改 `open.Tree`，被 `close.go:42` 的**跨树防护**挡掉 → READ/WRITE/CLOSE 全废，连 `CLOSE` 都发不出去（关不掉）。
- **严重度**：PR #20 已合入 main，所以**主干上 durable 特性完成度实际是 0%**；而 Time Machine 强依赖它。**这是 v0.2.0 头号技术风险，单独立项（R12）。**
- 修复归 `qa-proto`。team-lead 裁决 Persistent 改用**进程级全局计数器**；原「SessionId 入键」方案被 qa-proto 实测推翻——durable 的存在意义就是跨会话重连，键里塞 SessionId 等于结构性废掉。
- ⚠️ 这直接否定 §「三大块总览」B 块（Time Machine）的「主体已合入」判断——durable 合了但坏，完成度回落到 0%。B 块 Time Machine 验收判据在 R12 修复前不可标 ✅。

### 11.4 🟠 R13：`test/integration` 在 main 上是红的

- qa-e2e 实测 3 个失败，但**都是测试侧问题、产品没坏**。
- 根因：该目录**从没在 CI 里跑过**（CI 只编译不执行 `//go:build integration` 后的代码），所以红了无人知。
- 是「build tag 后面的代码从未被真正校验」那类洞的又一例（同 R1 假绿、R4 假绿）。**归 qa-e2e 纳入 CI 实际运行**。

### 11.5 两名新成员（容器化发布，项目所有者今日新提要求）

要求原文：「以后的版本，制作 docker 镜像，同时发布裸包和 docker 镜像」+「添加一个成员专门把上一个版本做一个 docker 镜像，托管在 cnb 平台上」。

| 成员 | 负责 | 交付物 | 档 |
|---|---|---|---|
| `rel-docker` | 从 v0.2.0 起「裸包 + 镜像」双轨发布接进 `.cnb.yml` | Dockerfile + `.cnb.yml` 双轨发布流水线 | ⚪ 新建跟踪 |
| `rel-v010` | 给**已发布的 v0.1.0** 补一个镜像，托管到 CNB 制品库 `docker.cnb.cool/finalappstore/stupidsamba:v0.1.0` | v0.1.0 镜像（retroactive） | ⚪ 新建跟踪 |

> 发布物形态（裸包 + 镜像双轨）从 v0.2.0 起为硬性要求；v0.1.0 镜像为补做。

### 11.6 资源控制（全员）

项目所有者强调「注意控制资源用量，别崩溃了」。容器 **4 GB 内存、零 swap**。
- team-lead 已关掉两名完工成员（`tui-diag`、`qa`）。**在册成员更新**：tui-diag、qa 已关闭（完工）；新增 `rel-docker`、`rel-v010`。
- 全员**不要跑整包 `go test ./...`**。
- PM 盘点也**不重复跑构建**——优先读分支 diff 与别人报的结果；确需跑只跑最小范围。
- 本看板本轮所有更新均为读档 + 编辑，未触发任何 `go build` / `go test`。


### 10.6 `open_windows.go` 归属落定 = win-vfs（第 9 轮）
- win-meta 明确**不接** `open_windows.go`（team-lead 两封「元数据库归属裁决 D」「拍板」明令 win-meta 不得动 `internal/vfs/` 任何文件；他已在 #11 描述记「win-meta 不得动 internal/vfs」边界）。早先「等拍板期间去写它」的话在边界划定前说的，现已撤回。
- win-vfs 以**单点 build-tag 分裂接缝**方案取代原 `open_windows.go`：新增 `openHostFile(host, flag, perm)`，Unix 侧 `openhost_unix.go` 薄包 `os.OpenFile`，Windows 侧 `openhost_windows.go` 最终接 `winOpenParamsFor`+`CreateFileW`+`os.NewFile`。整条链都在 vfs，全归 win-vfs，**win-meta 无需另起一份，无重复造风险**（符合 AGENTS §7.3.3）。
- **进度**：接缝第一步完成并开 **PR #30（`win-vfs/openhost-seam`）**——7 处宿主文件打开全收口到 `openHostFile`，`openhost_windows.go` 当前原样转调 `os.OpenFile`，`winOpenParamsFor` 调用点以 TODO 标出。第二步真接 `CreateFileW` 被 team-lead **暂缓**：无 Windows runner 验 `os.NewFile(handle)` 的 `Seek/ReadAt/WriteAt/Truncate` 互操作——属不可静态验的存亡假设，不赌。故 `winOpenParamsFor`/`isNameSurrogateTag` 现处「接缝里预留、待第二步接线」状态，**非死代码失控**（R9 由 🟠 半关闭改为 🟢 已闭环）。
- 看板 `owner 待定` 三条（§6.2 / §5 / §9 / D5）已全部改为 win-vfs；task #11 已更新：第一步接缝完成，转为跟踪第二步 CreateFileW 接入。
- 附注：win-vfs 确认我早先对 team-lead 的「reservedNames 不是死变量」误报已撤回并经其复核无误，此条无需再跟；「先查调用点再动手」方法论本次又抓到 winopen 真缺口，沿用。

---

## 12. 第 12 轮（2026-08-09 06:50Z）：**`main` 的红灯是两道，不是一道**

本轮我没有采信任何人的自述，把 CI 的 11 道关卡在干净工作树上逐条重跑了一遍。
结论推翻了本轮开工时的普遍认知（「合了 #35 就绿了」）。

### 12.1 头号结论：关键路径上串着两块，缺一不可

CI 的 stage **顺序执行、失败即中断**。这意味着**第一道红灯会把它后面所有关卡全部掩盖**
（这正是 R1 那个「CI 假绿」老洞的同构变体，第三次出现了）。

`pull_request` 流水线的 11 关顺序：
`build → vet → gofmt → **test_compile** → constraints → test → **race** → cross → static → example_config → smoke`

| 关卡 | `origin/main` `4a1bee2` | fix-ci `20af87d`（PR #35） | qa-proto 本地未提交树 |
|---|---|---|---|
| ④ 测试代码编译校验 | ❌ **红** `qadefect` tag 未注册 | ✅ 过 | ❌（缺 #35） |
| ⑤ 约束 C1/C3/C4/C8 | 被掩盖 | ✅ 过 | — |
| ⑥ 单元测试 `CGO_ENABLED=0` | 被掩盖 | ✅ 过 | — |
| ⑦ **竞态检测** | **被掩盖** | ❌ **红·真 bug** | ✅ **过** |
| ⑧ 四平台交叉编译 | 被掩盖 | ✅ 过 | — |

**所以：`#35` 只解开第 ④ 关，解开后立刻暴露第 ⑦ 关的真 bug。两块都落地，`main` 才可能绿。**

### 12.2 第 ⑦ 关是一个真实的产品级 data race（不是测试问题）

```
WARNING: DATA RACE
Read  at internal/smb/command/durable.go:172  (*durableTable).remove()
        ← (*Open).close()            open.go:222
Write at internal/smb/command/durable.go:248  (*DurableState).disconnectedAtZero()
        ← (*durableTable).reconnect() durable.go:242
--- FAIL: TestQADurableSessionCloseReconnectNoDeadlock (0.05s)
        testing.go:1617: race detected during execution of test
```

两处都在 **产品代码** `durable.go`，触发者是 PR #29 合入的缺陷复现用例。
**归属 R12（durable handle），owner `qa-proto`。**

> 反向对照（满足 R5 要求）：同一命令在 `qa-proto` 本地工作树上
> `CGO_ENABLED=1 go test -race ./internal/smb/command/` → `ok  1.420s`。
> 即「有修复则绿、无修复则红」双向成立，不是环境噪声。

### 12.3 🔴 关键路径上的修复**尚未提交**（本轮最高丢失风险）

| 事实 | 证据 |
|---|---|
| `origin/qa-proto/durable-fix` @ `8199288` **编译不过** | `vet: create_context_durable_test.go:166:42: cannot use intent (*wire.DurableIntent) as *Tree value in argument to durableRegistry.reconnect` |
| 其**本地**工作树（+3 个未提交 `_test.go`）编译干净且 race 已消除 | `go vet` rc=0；`go test -race` → `ok` |

也就是说，**当前全队关键路径上的唯一一块，只存在于容器磁盘上，一次重启即永久蒸发**。
已于 06:49Z 直接致信 `qa-proto` 要求立刻 `commit && push`（其状态早已满足 §7.2 的提交门槛）。

附带教训：作者推送前只跑了 `go build`，而 **`go build` 不编译 `_test.go`** ——
AGENTS.md 已记过这个洞，本轮它又咬了一次。**推送前请跑 `sh test/ci/check-test-compile.sh`。**

### 12.4 五个开着的 PR：全红，且全红在同一条链上

| PR | 分支 | push | PR 事件 | 实测红因 |
|---|---|---|---|---|
| #35 | `fix-ci/qadefect-tag` | ❌ | ❌ | 第 ⑦ 关 race（其目标第 ④ 关**已过**） |
| #34 | `srv-share/share-access` | ❌ | ❌ | 第 ④ 关 `qadefect`（已复现） |
| #33 | `win-meta/validate-slash-source` | ❌ | ❌ | 第 ④ 关 `qadefect`（已复现） |
| #31 | `win-vfs/open-seam` | ✅ | ❌ | 第 ④ 关：push 建的是旧基线故绿，PR 与 `main` 合并后拿到 `qadefect` 才红 |
| #26 | `win-meta/metadata-store` | ❌ | ❌ | 同链（另有 R11 历史包袱） |

> **#31 是本项目「假红/假绿」现象的第三种形态**，与 §2.4 记的两种都不同：
> 这次是 **push 绿而 PR 红**，且 **PR 的红才是真的**。
> 老经验「别拿 push 的红拦 PR」依然成立，但**反过来「push 绿就没事」是错的**。
> 判据统一为：**只看 `pull_request` 事件。**

### 12.5 逐人盘点（客观证据，非自述）

| agent | 分支 | 领先 main | 远端 | 状态 |
|---|---|---|---|---|
| `fix-ci` | `fix-ci/qadefect-tag` | +1 | ✅ 已推 | 🟢 目标关卡已达成，PR #35 待合 |
| `oscap-gate` | `oscap-gate/c9-constraint` | +1 | ✅ 已推（3 分钟前） | 🟢 `check-constraints.sh` C9 段已落地 |
| `oscap-audit` | `oscap-audit/capabilities-doc` | +1 | ✅ 已推（60 秒前） | 🟢 `docs/os-capabilities.md` 首版已落地 |
| `oscap-rules` | `oscap-rules/c9-agents-md` | 0 | 分支已建、无提交 | 🟠 本地 `AGENTS.md` 改动**未提交**，需催 |
| `qa-proto` | `qa-proto/durable-fix` | +1 | ⚠️ 已推但**编译不过** | 🔴 见 §12.3，关键路径 |
| `win-backend` | `win-backend/symlink-gap` | 0 | ✅ 空分支已推 | ⚪ 已开工建仓，暂无产出 |
| `qa-verify` | `qa-verify/e2e-ci` | 0 | ✅ 空分支已推 | ⚪ 已开工建仓，暂无产出 |

**✅ 「5 个 agent 零分支而无人察觉」的历史事故本轮没有复发**——在册 7 人全部已建远端分支，
其中 5 人已有实质提交。§7.3.1「开工先推空分支」这条纪律是有效的，继续保持。

### 12.6 🔴 R7 复发：`qa-e2e` 的成果成了孤儿

`/work/qa-e2e` 工作树相对 `origin/qa-e2e/ci` 领先 **29** 个提交，逐条比对后：
**27 个是已在 `main` 里的合并祖先，真正未推送的原创提交是 2 个**——

```
265bcaf  test: 失败对照实验 reverse-control.sh，6 个变异全部被定点抓住
ad27691  test: 三客户端端到端冒烟套件 test/e2e/smoke.sh + 变异生成器
```

这**正是 v0.2.0 A 块（测试设施）的核心交付物**，而且自带反向对照（恰好满足 R5）。
作者 `qa-e2e` 因容器重启已退出，**这两个提交只存在于本容器磁盘上**。
处置同 R7 旧例（#20/#21 代开 PR），**需 team-lead 指派接手人**——见 D-新1。

### 12.7 其他工作树残留（已核实，均不阻塞）

| 工作树 | 残留 | 判定 |
|---|---|---|
| `/work/win-meta` | +1 提交（仅 merge main）、未提交改 `test/ci/check-test-compile.sh` | 与 fix-ci 同文件但**未提交未推送**，不会冲突；作者已退出，属残留 |
| `/work/qa`、`/work/tm-vfs` | 本地分支无远端、0 领先 | 空壳，无内容丢失风险 |
| `/work/rel-docker`、`/work/rel-v010`、`/work/tui-diag` | 各 1 个未提交文件 | 作者已退出，0 未推送提交，无原创成果损失 |

### 12.8 风险登记增补

| # | 风险 | 现状 | 应对 |
|---|---|---|---|
| **R12** | durable handle（含本轮新查明的 data race） | 🔴 **仍是头号**，且已确证**卡住全队 CI**，影响面比第 11 轮判断的更大 | 修复已存在但未提交，见 §12.3 |
| **R14** | **🔴 新增·门禁串行掩盖后续关卡**：第一道红灯让后面 7 关从未执行，「修好第一道」被误当成「全绿」 | 🔴 本轮实证：`main` 掩盖了真 race。这是 R1 的第三次同构复发 | 判据改为：**修红灯必须把整条链跑到底**，不能只验自己那一关。本看板今后每轮实跑全链 |
| **R15** | **🟠 新增·「已推送」不等于「可编译」**：`go build` 不编译 `_test.go`，推上去的分支可以是坏的 | 🟠 本轮命中 `qa-proto` | 推送前跑 `sh test/ci/check-test-compile.sh`；建议 team-lead 定为全员纪律 |
| **R7** | 无主分支/孤儿成果 | 🔴 **复发**：`qa-e2e` 2 个 A 块提交无人认领，仅存于磁盘 | 见 §12.6，待指派 |

### 12.9 待决事项（需 `team-lead` 拍板）

| # | 事项 | 建议 | 谁在等 |
|---|---|---|---|
| **D-新1** | `qa-e2e` 那 2 个孤儿提交谁接手代开 PR？ | 建议指派 `qa-verify`（其职责本就是端到端验证与 CI 接入，且分支空着正好承接） | A 块进度 |
| **D-新2** | PR #35 是否**先合**？（合了 `main` 仍红在 race，但它让红灯前移到真问题） | 建议**先合**：它本身已过目标关卡且经独立复现；留着不合只会让后续所有 PR 继续红在一个已知已修的点上 | 全部 5 个 PR |
| **D-新3** | 是否把「推送前跑 `check-test-compile.sh`」写进 AGENTS.md §7.2？ | 建议写入，可交正在改 AGENTS.md 的 `oscap-rules` 顺带落笔（避免与其冲突） | 全队纪律 |
| **D-新6** | **#35 把 `TestQADefectDurableReconnectRebindsTree` 等 3 条留在 `qadefect` tag 后面，与你此前「根因已修就该去 tag 挪回默认路径」的拍板相抵触，是否要求 fix-ci 调整？** | 建议：**不必单独返工**——qa-proto 的分支已把这 3 条提升到默认路径，按「先合 #35、再合 qa-proto（测试文件取 qa-proto 侧）」的顺序合完，终态自然正确。但 #35 的「归位」措辞与 D-新4 的语义问题相关，值得你复核一句 | fix-ci / qa-proto 合并顺序 |
| **D-新4** | `qadefect` 里那些**故意失败**的用例，语义上应「只编译」还是「要执行并断言其失败」？ | 现状只编译不执行。若本意是后者，则归位工作还差一步 | fix-ci / qa 口径 |

### 12.10 本轮资源用量说明

第 11 轮定的「PM 不重复跑构建」在本轮**被我有意突破**，因为「合了 #35 就绿」这个判断
无法靠读 diff 证伪，而 CNB 的 build API **不返回 stage 级明细**（`pipelines[].stages` 为空数组），
不实跑就查不出第二道红灯。实跑均限定最小范围、串行、并发为 1，单次最长 24s，未触发内存告警。

**结论：这条自律应当修订为「不重复跑*别人已经报过结果*的构建；涉及关键路径判定时必须自己实跑」。**
本轮如果继续遵守旧口径，交付给 team-lead 的会是一个错误的「合并 #35 即解锁」的结论。

---

## 12.11 追加（07:00Z）：两块修复都已进远端，但**它们互相冲突**

### 好消息：§12.3 的丢失风险已解除

| agent | 新状态 | 核实 |
|---|---|---|
| `qa-proto` | ✅ 已推 `a094a43`（+2） | `CGO_ENABLED=1 go test -race ./internal/smb/command/` → **`ok 1.263s`**，编译已修好 |
| `oscap-rules` | ✅ 已推（+2） | §12.5 的 🟠 转 🟢 |
| `qa-verify` | ✅ 已推（+2） | 已开工产出 |

**催办有效**：qa-proto 与 oscap-rules 收到催办后均在数分钟内推送。关键路径上那份
「只存在于磁盘」的修复现已安全落在远端。

### 🔴 新问题：#35 与 `qa-proto/durable-fix` 存在真实合并冲突

```
冲突（内容）：合并冲突于 internal/smb/command/durable_defect_test.go   （1 个冲突块）
冲突（内容）：合并冲突于 internal/smb/command/durable_qa_test.go       （1 个冲突块）
```

两人**同时改同一对测试文件**：`fix-ci` 往里「归位漏搬的失败用例」，`qa-proto` 则为适配
`reconnect` 新签名改写同一批用例。这是 §7.1 文件所有权表没覆盖到的交叉——
`internal/smb/command/durable_*_test.go` **没有唯一 owner**。

### 冲突解决后整链可绿（可行性已验证）

以 `-X theirs` 强制解冲突做**可行性探针**：

| 关卡 | 结果 |
|---|---|
| ④ 测试代码编译校验 | ✅ 过 |
| ⑥ 单元测试 `CGO_ENABLED=0` | ✅ 过 |
| ⑦ 竞态检测 | ✅ **过** |

**即：`#35` + `qa-proto` 修复 = `main` 可以绿。这是本轮最重要的正面结论。**

### ~~⚠️ 但探针本身丢了东西~~ → **PM 误报，已撤回（见下）**

我最初按 `func Test` 计数发现 `durable_defect_test.go` 从 7 掉到 4，判定
「`-X theirs` 静默删掉了 fix-ci 归位的 3 个用例」，并据此要求 qa-proto 做并集。
**这个判断是错的，已向 qa-proto、fix-ci、team-lead 全部更正。**

### ✅ 更正后的事实：那 3 个用例是被**提升**，不是被删

只数了单个文件的数字就报警，没做**去向核对**。查清后三条一条不少：

| fix-ci 侧：`durable_defect_test.go`（`qadefect` tag 后，**不执行**） | qa-proto 侧：`durable_qa_test.go`（默认路径，**每次 CI 都跑**） |
|---|---|
| `TestQADefectDurableReconnectRebindsTree` | `TestQADurableReconnectRebindsTree` |
| `TestQADefectExpiredEntryClosesHandle` | `TestQADurableExpiredEntryClosesHandle` |
| `TestQADefectEvictionRequiresAuthorization` | `TestQADurableEvictionRequiresAuthorization` |

qa-proto 侧另有新增的 `TestQADurableExpiredEntriesReclaimedByNewRegistrations`。

**总用例数：qa-proto 侧 4+9 = 13，fix-ci 侧 7+5 = 12。qa-proto 比对方多一个，不是少三个。**

### 而且 qa-proto 的方向才是项目已拍板的那个方向

`durable_defect_test.go` 文件头的规矩（team-lead 此前已就同一条用例拍过板）：

> tag 后面放的是**根因未修**的复现用例；**每修好一个就去掉 tag 挪回** `durable_qa_test.go`
> 当回归保护。「不要靠删除用例来『修复』失败」——**加 tag 藏起来和删除是同一类动作。**

qa-proto 修好了 5 个根因（归属校验 / data race / 超时关句柄 / 先授权后驱逐 / 重连改绑树），
随即把对应用例从 tag 后挪回默认路径——**完全合规**。

反过来，**#35 把 `TestQADefectDurableReconnectRebindsTree` 留在 `qadefect` tag 后面，
正是 team-lead 此前明确要求回退过的做法**（当时点名的就是这条用例）。**这需要 team-lead 复核**——见 D-新6。

### 建议合并顺序（据上更正）

1. **先合 #35**（其 `test/ci/check-test-compile.sh` 的 tag 注册是必需的，且该文件无冲突）
2. `qa-proto` rebase → 两个测试文件**取自己那一侧**（不是并集）→ 核对计数应为 4 / 9
   → 确认 `grep qadefect test/ci/check-test-compile.sh` 仍在 → 开 PR
3. 该 PR 绿后合入，`main` 恢复绿；届时 #26/#31/#33/#34 逐个 rebase 即可脱红

### 📌 PM 自查：本轮我犯了自己刚警告别人的那个错

我在 §12.11 里刚写完「整链变绿不能当依据、必须做反向核对」，
紧接着就用「计数变少」直接推出「用例被删」，**没做去向核对**——
同一封信里既提出判据又违反判据。

**教训（已写进记忆）：「数量减少」≠「成果丢失」。发现计数下降时，
必须先 `grep` 用例名去别处找一遍，确认是「删除」还是「搬迁/改名」，再下结论。**
这与 R5「判据必须可证伪」是同一条纪律。

### 12.12 建议补进 §7.1 文件所有权表

本轮冲突的根因是所有权表**只切到目录级**，`internal/smb/command/durable_*_test.go`
同时被 `fix-ci`（CI 归位）和 `qa-proto`（R12 修复）改动。建议明确：

| 文件 | 建议 owner |
|---|---|
| `internal/smb/command/durable.go`、`durable_*_test.go` | `qa-proto`（R12 期间独占） |
| `test/ci/*.sh`、`.cnb.yml` | `fix-ci` |

AGENTS.md §7.1 原话已经预警过这件事：「随着 `internal/smb/command` 变大，
该目录内部要**按文件再切分**，否则三个 agent 会互相覆盖。」本轮就是它的又一次应验。

---

## 第 13 轮：**`main` 已转绿，第 12 轮「两道红灯」预测闭环**（07:20Z 起）

> 最近盘点：**2026-08-09 07:20Z（第 13 轮 · 全量盘点 main / PR / 分支 / 孤儿）**
> 上轮：第 12 轮（实测复现 CI 全链，查明 `main` 红灯有两道）
> 本期核对基线：`origin/main` = **`0d60c68`**（`push` 事件 CI = **`success`**，绿）
> 第 12 轮基线 `4a1bee2` 之后 `main` 又前进 **22 个提交**。

### 13.1 头条：第 12 轮的头号顾虑已消散

第 12 轮我的核心判断是「合并 fix-ci 的 #35 **不足以**让 `main` 变绿，后面还压着一个真实 data race」。
实测结论现在被实践推翻且方向变好：

- **#35 已合入**（`aae12b2` Merge PR #35）；
- **qa-proto 的 R12 修复（6 个 durable 缺陷，含那个 data race）已合入**（`0d60c68` Merge PR #40）；
- `origin/main` 的 `push` 事件 CI = **`success`**。

**`main` 现在是绿的。** 第 12 轮「两道红灯」只是合并顺序未完成的瞬时态，不是结构性故障。
原 §12 建议的合并顺序（先 #35 → qa-proto rebase 取自己侧 → 合入恢复绿）已按建议发生，结论成立。

### 13.2 自第 12 轮起合入的 PR（共 7 个，主干高速前进）

| PR | 分支 | 主题 | 核实 |
|---|---|---|---|
| #31 | win-vfs/open-seam | vfs: 收口宿主文件打开点到 openHostFile 接缝 | ✅ tip 已是 main 祖先 |
| #33 | win-meta/validate-slash-source | config: isAbsWindowsPath 委托 internal/vfs 唯一真源 | ✅ 同上 |
| #35 | fix-ci/qadefect-tag | ci: 注册 qadefect build tag 进 check-test-compile 白名单 | ✅ 同上 |
| #36 | oscap-rules/c9-agents-md | docs: 新增 C9 操作系统边界约束，改写 P7 为 native/builtin 双适配器 | ✅ 同上 |
| #37 | oscap-gate/c9-constraint | ci: 新增 C9 门禁（禁挂载与命名空间机制）+ 反向对照 | ✅ 同上 |
| #40 | qa-proto/durable-fix | server: 修复 durable handle 6 个缺陷（R12 / 含 data race） | ✅ 同上 |
| #41 | fix-ci/c9-gate-name | ci: .cnb.yml 关卡名补上 C9 | ✅ 同上（fix-ci/c9-gate-name 现已 +0，并入 main） |

> 这 7 个 PR 在 round-12 盘点时或处于「开但未合」或尚未出现；现在全部合入。
> **round-12 的 D-新2（先合 #35）已自然闭环。**

### 13.3 当前开着的 PR（仅 3 个）

| PR | 分支 | 主题 | 状态 | 说明 |
|---|---|---|---|---|
| #38 | qa-verify/e2e-ci | test: 修正集成测试假故障 + 修掉验收脚本里说谎的成功信息 | **mergeable** | 见 §13.6 关注点 |
| #34 | srv-share/share-access | server: 实现 SMB ShareAccess 共享模式冲突判定 | **mergeable** | 可直接合 |
| #26 | win-meta/metadata-store | windows: MetadataStore 旁路存储（P7） | **⚠️ conflict** | 唯一阻塞项，见 §13.4 |

> 开 PR 数从 round-12 的 8 个降到 3 个，且下降**全部来自正常合入**（#31/#33/#36/#37 经核实 tip 已是 main 祖先），**不是丢工作**。这正是 round-12 §12.11「数量减少≠成果丢失」纪律的正面印证。

### 13.4 唯一阻塞项：#26 冲突——且是**低工作量**冲突

`win-meta/metadata-store` 从旧 `main`（`1acae3f`，83 分钟前）切出，期间 main 落了 22 个提交。
与当前 main 做三路合并，冲突文件**只有 3 个，且全是共享的配置/记忆文件，不含 win-meta 的功能代码**：

| 冲突文件 | 为何冲突 | 建议处置 |
|---|---|---|
| `.cnb.yml` | main 经 #37/#41 加了 C9 关卡；win-meta 也动了它 | **取 main 版**（C9 门禁是主线共识） |
| `memory/MEMORY.md` | 两边都往索引加指针 | **取 main 版**后由 win-meta 补自己那行 |
| `test/ci/check-test-compile.sh` | main 经 #35/#37 注册了 qadefect / C9 反向对照 | **取 main 版** |

**关键**：win-meta 的真实功能代码 `internal/meta/{store,bolt,noop}.go(+_test)`、`ORIGIN.md`、`THIRD_PARTY.md`
在合并里是「仅 win-meta 侧改动」，**不与 main 冲突**。所以这不是功能冲突，只是 3 个共享文件被 main 的 CI/记忆 PR 抢改了。

**建议动作（给 win-meta owner，疑似 `win-backend`）**：
`git pull --rebase origin main` → 三处冲突全选 `ours` 之外的「main 版」（即 `git checkout --theirs` 那 3 个文件）→ 保留 `internal/meta/*` → 重新 push → PR 转 `mergeable`。预计 < 10 分钟。

### 13.5 无主/无 PR 但在写的分支：全部「活跃」，无停滞

以下分支领先 main 但**未开 PR**，逐一核对最后提交时间（全部在 83 分钟内），**没有停滞分支**：

| 分支 | 领先 | 最后提交 | 主题 | 判读 |
|---|---|---|---|---|
| win-backend/symlink-gap | +3 | 5 分钟前 | vfs: 链接逃逸判据平台化（堵 Windows junction 末级逃逸） | 进行中 |
| tm-dev/timemachine | +2 | 3 分钟前 | docs: v0.2.0 Time Machine 缺口清单 | 进行中 |
| rel-v010/cnb-image | +4 | 69 分钟前 | docs: README 增加 CNB 制品库拉取镜像小节 | 进行中（发布物） |
| qa-verify/negative-matrix | +3 | 8 分钟前 | test: 修掉 acceptance.sh 说谎的成功信息 + 探针残留 | 进行中（见 §13.6） |
| oscap-audit/capabilities-doc | +2 | 34 秒前 | docs: OS 能力审计 §2.8 反直觉结论 | 进行中 |
| rel-docker/image | +1 | 76 分钟前 | docker: scratch + COPY-only 多架构 Dockerfile | 进行中（发布物） |
| qa-vfs/verify | +2 | 69 分钟前 | test: 变异测试全量成绩（39 条 38 杀） | 进行中 |
| qa-e2e/ci | +2 | 83 分钟前 | test: 失败对照 reverse-control.sh，6 变异全抓 | **孤儿抢救中**（见 §13.7） |
| win-vfs/openhost-seam | +1 | 79 分钟前 | vfs: 抽出 openHostFile 接缝（build tag 分裂） | 进行中 |
| win-meta/is-windows-slash | +1 | 71 分钟前 | config: isWindowsSlash 委托 vfs（单点真源，PR #27） | 进行中 |
| pm/status | +7 | — | 本看板 | 我 |

> 与 round-12 那个「无主 1500 行」的告警不同，本轮**所有无 PR 分支都有分钟级活跃度**。
> 这些分支没开 PR 是因为 agent 仍在做（合 §7.2「尽快提交尽快推送，PR 待就绪」），**不是卡住**。
> PM 把它们列为「观察」，不列为「阻塞」。

### 13.6 关注点：qa-verify 同时有两个分支，需澄清 PR 意图

`qa-verify` 当前有两条领先分支：
- `qa-verify/e2e-ci`（+3，**已开 PR #38**）—— 修集成测试假故障 + 验收脚本说谎成功信息；
- `qa-verify/negative-matrix`（+3，未开 PR）—— 修 acceptance.sh 说谎成功信息 + 探针残留级联。

两者主题高度重叠（都动 acceptance.sh 的「说谎成功」）。**风险提示**：若两条都开 PR 且都含对 `acceptance.sh` 的修改，合入顺序不当会互冲突。
**建议**：qa-verify 明确哪条进 #38、哪条后续；或将 `negative-matrix` 的内容并入 `e2e-ci` 再统一开 PR，避免双线改同一文件。已发消息给 qa-verify。

### 13.7 round-12 孤儿项 D-新1：qa-e2e 孤儿抢救——**进行中，工作未丢**

round-12 担心的 `qa-e2e/ci` 938 行冒烟套件（`ad27691`）并未丢失：
- 当前 `qa-e2e/ci` tip 已改写为 `414bdf7`（三客户端端到端冒烟套件 `test/e2e/smoke.sh` + 变异生成器）、`3ff7cd8`（失败对照 `reverse-control.sh`，6 个变异全抓）；
- 旧 `ad27691`/`265bcaf` 被这两条干净提交**取代**（内容实质保留，历史变整洁）；
- 接管者确为 qa-verify（与 round-12 建议一致）。

**状态**：分支级抢救完成、工作保全；但尚未进 main（领先 +2，无 PR）。
**待办**：qa-verify 给 `qa-e2e/ci` 开 PR（或并入 #38），把这份冒烟套件落进主干。

### 13.8 待 team-lead 拍板 / 跟踪清单

| 项 | 状态 | 说明 |
|---|---|---|
| D-新2（先合 #35） | ✅ 闭环 | #35 + #40 已合，main 绿 |
| R12 data race | ✅ 闭环 | 随 #40 合入 main |
| D-新1（qa-e2e 孤儿） | 🟡 进行中 | 分支级保全完成，待开 PR 落 main |
| D-新6（#35 留 `TestQADefectDurableReconnectRebindsTree` 在 qadefect tag 后，与你此前点名回退的做法抵触） | ⏳ 仍待你复核 | 无新进展，#35 已合但口径未改；若维持现状，需你明确「此条可留在 tag 后」 |
| #26 冲突 | ⚠️ 阻塞 | 低工作量，等 win-meta owner rebase（见 §13.4） |
| qa-verify 双分支 | 🟡 关注 | 见 §13.6，防 acceptance.sh 双线冲突 |

### 13.9 一句话结论

**主干健康、进度高速、无停滞分支。唯一真正阻塞是 #26 的 3 文件低工作量冲突（等 win-meta owner 处理）；
唯一需你拍板的是 D-新6（#35 的 tag 口径）。** 其余（孤儿抢救、qa-verify 双分支）均在 agent 手上正常推进。

