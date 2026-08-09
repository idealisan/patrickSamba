# stupidSamba v0.2.0 进度看板

> 维护者：`pm`（专职项目管理 agent），分支 `pm/status`。
> **本文件的判断只采信客观证据**：分支上的 commit、已合并的 PR、可复现的实测记录。
> agent 的自我汇报不作为进度依据。
>
> 最近盘点：**2026-08-09（第 6 轮 · win-vfs / win-meta / qa 三方回函后）**
> 上轮：13:22（第 4 轮）、13:14（第 3 轮）、13:06（第 2 轮）、12:15（第 1 轮 · 基线）
> 本期核对基线：`origin/main` = `ef73b549`（Merge PR #23）

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

> **owner 待定**：win-meta 回函说等拍板期间会继续写 `open_windows.go`（解决 winopen 缺口），
> 但截至本期**无对应 PR**。建议把它作为独立关键路径条目，owner 落到 win-vfs / win-meta 之一，
> 由 team-lead 定。

### 6.3 win-meta 的 `bolt.go`「消失」是误报——已澄清

第 5 轮 §4.1 我记「`internal/meta/bolt.go` 那处 modified 改动既不在 commit 也不在工作树」。
win-meta 澄清：那 1 行是把 `subtreeSkipByte` 由 `"0"` 改成 `"/"` 的**变异测试残留**，
提交它会引入 bug；他在还原前跑完对照（`TestGetDir` 在 `bolt.go:178` 死循环 90s 超时），
确认该常量被测试真实覆盖，已 `git checkout` 还原，**工作树干净**。
两个 commit（`20ade80`、`66da303`）在 12:20 就已 push，`git log origin/win-meta/metadata-store..HEAD` 为空。
**所以不是丢失、不是违规，是我的快照误读。R2 在 win-meta 身上并未兑现。** 记一笔方法论：
未提交改动可能是变异测试残留，下次先问「是不是测试残留」再记风险。

### 6.4 当前开着的 PR（4 个，已用 pull-ref 与 main 比对确认未合入）

#18 / #20 / #21 / #22 / #23 **均已合入** `main`（`ef73b549`）。现余 4 个：

| PR | 分支 | 主题 | 档 | 备注 |
|---|---|---|---|---|
| #24 | `pm/status` | 本看板（第 5 轮） | 🟡 | 我自己的，排最后合 |
| #25 | `team-lead` 系 | AGENTS.md §7.5 高危命令禁令 | 🟡 | 团队纪律，建议早合 |
| #26 | `win-meta/metadata-store` | `internal/meta` bbolt/noop + 单测 | 🟡 **draft** | ⚠️ **见 §4.1 阻断项，勿合** |
| #27 | `win-vfs` 系 | 抽 `IsWindowsSlash` 为分隔符唯一真源 | 🟡 | 与 #7/#19 名字词法相关 |

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
并对 v0.1.0 旧库文件做兼容（换 bucket 名或 `Open` 时一次性迁移）。PR #26 描述里已写明保持 draft。

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
          ──► ⚠️ 与 vfs/metadata_windows.go 同 bucket "posix" 撞车(R11) 勿合
          ──► 待 team-lead 定处置（vfs 依赖 meta / 删 metadata_windows.go bbolt / 迁移旧库）
win-vfs: winopen.go 的 winOpenParamsFor/isNameSurrogateTag 待 open_windows.go 消费方（owner 待定，§6.2）
```

**当前瓶颈（按紧迫度，第 6 轮）**：

1. **⚠️ R11 双 bbolt 撞车**：`internal/meta`(PR #26) 与 `vfs/metadata_windows.go` 并存会静默损坏数据，
   PR #26 必须保持 draft 直到 team-lead 定处置（vfs 依赖 meta / 删 metadata_windows.go bbolt / 迁移旧库）。
   这是当前唯一会「合了反而更糟」的 PR。
2. **评审吞吐**：现仅 4 个开 PR（#24/#25/#26/#27）。建议先合零冲突的 #25（团队纪律）、#27（名字词法相关），
   #24（本看板）我排最后。#26 在 R11 解决前勿合。
3. **winopen 缺口**：`winOpenParamsFor`/`isNameSurrogateTag` 仍零生产调用点，需 `open_windows.go`，owner 待定。
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
| **R9** | **「被架空的逻辑」**：PR #7 新增 764 行 | 🟠 **半关闭（第 6 轮）**：`validateWindowsName` 已接线（`path.go:203`，PR #19）；`winOpenParamsFor`/`isNameSurrogateTag` 仍零生产调用点，待 `open_windows.go` | §6.2；**今后凡新增函数零调用点，合并前必问一句** |
| **R10** | **CI「假红」**：旧 `.cnb.yml` 致 push 构建必红，与代码无关 | 🟢 已不阻塞：各分支已 rebase 拿到新 `.cnb.yml` | §2.4。**别拿 push 的红拦 PR** |
| **R11** | **🔴 新增·双 bbolt 实现撞车**：`internal/meta`（PR #26）与 `internal/vfs/metadata_windows.go` 同写 bucket `posix`、记录布局不同 → 静默数据损坏（见 §4.1） | 🔴 **新增，已查实** | PR #26 保持 draft 勿合；team-lead 定处置（vfs 依赖 meta / 删 metadata_windows.go bbolt / 迁移旧库） |

---

## 7. 待决事项（需 `team-lead` 拍板）

| # | 事项 | 谁在等 |
|---|---|---|
| D1 | 是否采纳 §2.1 的分批合并顺序？（照此只需 1 人 rebase，乱序则 tm-vfs 要 rebase 3 次） | 全队 | **已历史解决**：#7/#10/#11/#12/#13/#14/#15/#16/#17/#18/#20/#21/#22/#23 全合入 |
| D2 | bbolt 依赖是否批准（纯 Go / License / 零 CGO 三项核验由谁出结论） | `win-meta`、`win-vfs` | **第 6 轮已核验通过（带 `-tags metabolt`；不带 tag 假绿）**，无需再批；但引出 R11 |
| D3 | `tm-handle` / `r-infra` 是否已关闭？其分支由谁代为开 PR？ | `pm`、Time Machine 块整体 | ✅ 已闭环：#20 / #21 已合入 |
| D4 | `qa` 是否还活着？`save.sh` 历史改写 bug 要不要插队进第 1 批？ | 全队（它会改写别人 PR 的历史） | ✅ 已闭环：save.sh 修复随 PR #22 合入 main |
| D5 | PR #7 的接线是刻意拆两步还是漏做？合并时如何措辞才不误判为「已完成」？ | `win-vfs`、CHANGELOG | **第 6 轮已答**：`validateWindowsName` 漏了、已 PR #19 接线；`winOpenParamsFor`/`isNameSurrogateTag` 刻意拆，待 `open_windows.go`（owner 待定，§6.2） |
| **D6** | ⚠️ **R11 双 bbolt 撞车如何处置？** `internal/meta`(PR #26) 与 `vfs/metadata_windows.go` 谁留谁删、旧库怎么迁移、归属谁？ | `win-meta`、`win-vfs`、`team-lead` | **最高优先**：在 D6 定下来前，PR #26 保持 draft 勿合 |

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
