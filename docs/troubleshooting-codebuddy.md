# CodeBuddy 工具链故障排查

> 本文只记录 **CodeBuddy CLI 本身**的故障，不涉及 stupidSamba 产品代码。
> 调查人：`tui-diag`。首次成文日期：2026-08-09。
>
> **置信度标注规则（本文强制）**：每条结论都标了下面三档之一，不许混用。
>
> | 标记 | 含义 |
> |---|---|
> | **[实测]** | 我在本机跑过、看到结果 |
> | **[日志]** | 从 CodeBuddy 运行日志读出来的既成事实，未主动复现 |
> | **[源码]** | 只读了产品源码/文档推断，**没有实测**，可能有偏差 |

---

## 1. 故障：权限确认面板卡死，永不消失

### 1.1 现象

TUI 上出现一个 Bash 权限确认面板：

```
 Bash command
   cd /workspace && git branch -f ... && git worktree remove "$T"
 Do you want to proceed(High risk operation detected - requires confirmation every time)?
 > 1. Yes
   2. Yes, and don't ask again for session (shift + tab)
   3. No, and tell CodeBuddy what to do differently (escape)
```

按 `1` / `2` / `3` / `Esc` **全都没有反应，面板永不消失**，而后台 agent 仍在正常干活。

### 1.2 一句话根因

**后台 teammate 触发了一条被判定为 HIGH 风险的 Bash 命令 → 面板被塞进「主会话」的审批队列并渲染；
随后该审批项对应的 deferred 从待决表里消失，但队列项与驱动 UI 的 `pending$` 没有被一起回收 →
`approve()` / `reject()` 双双在「找不到 deferred」处提前 return，`dequeue()` 永远执行不到 →
面板成为孤儿，只能重启进程。**

### 1.3 证据链

日志：`~/.codebuddy/logs/<日期>/workspace__<hash>.log`（52 MB，重启即失，取证要快）。

**[日志]** 完整时间线（2026-08-09，主会话 `2d8786c5`，teammate 会话 `dc735778`）：

| 时间 | 事件 |
|---|---|
| 09:03:36.829 | `[showHead] mainSession=2d8786c5, tool=Bash, id=chatcmpl-tool-9bc58552d91d9d8c, queueSize=1` |
| 09:03:36.829 | `[HandleInterruptions] Enqueued tool: Bash, ..., sessionId: dc735778, permissionMode: bypassPermissions` |
| 09:03:37.029 | `Approval dialog shown ... waiting for user response...` |
| 09:03:38 起 | teammate **继续正常跑 Bash / 模型请求**，状态机 `TOOL_STARTED → TOOL_ENDED` 不断循环 |
| 09:11:18 – 09:59:14 | **约 60 次** `[Approve] Failed: No pending deferred found ... pendingMapSize: 0` |
| 09:13:21 / 09:13:25 | `[Reject] Failed: No pending deferred found ...`（用户也试过选 3 / Esc） |
| 09:59:37 | pid=20451 最后一条日志 |
| 09:59:44 | pid=109957 出现 —— **进程被重启** |
| 10:06:54 | `[dequeue] Queue empty for mainSession=2d8786c5` —— 恢复正常 |

要点：

1. **「后台还在正确运转」得到证实**：09:03:38 之后 teammate 的状态机一直在跑，
   它根本没有阻塞等待这次审批。
2. **卡了 56 分钟，用户按了约 60 次，approve 和 reject 都试过**，全部失败。
3. **唯一的出路是重启进程**（pid 变化），会话 `2d8786c5` 在新进程里被成功续上。
4. `pendingMapSize` 早期是 `0`、09:33 之后变成 `1` —— 说明待决表并非空表，
   只是**面板持有的那个 item 不在表里**。

**[源码]** 对应实现（`dist/codebuddy.js`，偏移量为本机 v 版本，升级后需重新定位）：

- `InterruptionServiceImpl.approve()`（约 11243700）有三条提前 return，**都不执行 `dequeue()`**：
  1. `Cannot find session for tool` —— `itemToSessionMap` 里没有该 item
  2. `Session state is null`
  3. `No pending deferred found` ← **本次命中的就是这条**
- `reject()`（约 11246929）结构完全相同，同样的死路。
- 面板由 `pending$` 这个 `BehaviorSubject` 驱动；只有 `dequeue()` 和 `publishPending(_, undefined)`
  会把它清空。三条 return 都摸不到它 → 面板不可能消失。
- `ConfirmBox` 的选择回调里还有一句 `if(!el) return;`（约 12601157），
  `pending$` 与 `session.interruptionSubject` 都为空时**静默吞掉按键**，是同类问题的第二个出口。

**[日志]** 三条已知的「正常清理路径」（`cancelAllForSession`、compaction abort、generic abort）
**都没有在本次事故中执行过**：它们都会同时删掉 `itemToSessionMap`，
而 `approve()` 明明越过了 `findSessionByItem` 才走到「No pending deferred」，
说明 `itemToSessionMap` 里那一项还在。

**[日志推断]** 因此最可能的成因是：**同一个逻辑工具调用产生了两个不同的 item 对象**——
`pending$` 抓着旧的那个 A，待决表里换成了新的 B。
待决表以 **item 对象**为 key，A 查不到 → 三条 return 的第三条 → 面板永生。
这解释了 `pendingMapSize` 从 0 变成 1 却依然失败。**此条是推断，未复现。**

### 1.4 触发条件：什么命令会被判定为 HIGH

**[实测]** 判定逻辑在 `CommandUtils.checkCommandSafety`（约 13389763）：

```
s      = normalizeCommandPaths(stripHeredocBodies(cmd)).toLowerCase()
parts  = splitCommandRespectingQuotes(stripRedirections(s))   # 按 ; && || | 拆，引号内不拆
风险   = max(每段单独判定)                                     # 取最高档
```

五张正则表：`SAFE` → `CRITICAL` → `HIGH` → `MEDIUM` → 已知命令白名单 → 未知命令(LOW)。

仓库里提供了复现工具 **`scripts/diag/risk-replica.js`**，它在运行时**从 CodeBuddy
安装包里直接抽取这五张正则表**（不是人工誊抄），因此不会随版本漂移：

```sh
node scripts/diag/risk-replica.js 'git worktree remove /work/foo'
#  => HIGH   [eB/HIGH] /git\s+worktree\s+(remove|prune)/
```

**[实测]** 与本项目日常操作相关的判定结果：

| 命令 | 档位 | 命中规则 |
|---|---|---|
| `git worktree remove <dir>` | **HIGH** | `git\s+worktree\s+(remove\|prune)` |
| `git worktree prune` | **HIGH** | 同上 |
| `rm -rf /tmp/clean-$$` | **HIGH** | `\brm\s+(-[^\s]*r\|-[^\s]*R\|--recursive)` |
| `git rm -r <dir>` | **HIGH** | 同上（被通用 `rm -r` 规则误伤） |
| `git checkout -- <path>` | **HIGH** | `git\s+checkout\s+--\s+\S` |
| `git restore <file>` | **HIGH** | `git\s+restore\b` |
| `git stash drop` | **HIGH** | `git\s+stash\s+drop` |
| `sudo <任何东西>` / `chmod 777` | **HIGH** | `^\s*sudo\b` / `\bchmod\s+777` |
| `git reset --hard` | CRITICAL | `git\s+reset\s+--hard` |
| `git clean -fd` | CRITICAL | `git\s+clean\s+-[fdxX]` |
| `pkill -x stupidsamba` | MEDIUM | `\bpkill\b` |
| `kill -9 <pid>` | MEDIUM | `\bkill\s+-9` |
| `git branch -d/-D`、`git tag -d` | MEDIUM | `git\s+branch\s+-d\b` 等 |
| `git branch -f <b> <ref>` | SAFE | 白名单命令 `git` |
| `git worktree add --detach` | SAFE | 同上 |
| `git push --force-with-lease`（非主干） | SAFE | 同上 |
| `git restore --staged <file>` | **SAFE** | 被 `SAFE` 表**显式豁免** |

**[实测] 两个反直觉点**：

1. `git restore --staged` 是 SAFE，**去掉 `--staged` 反而是 HIGH**。
2. `git rm -r` 会被通用 `rm -r` 规则命中成 HIGH，和 git 无关。

**[实测] 被排除的嫌疑**（team-lead 原先怀疑、经复现器证伪的）：
多条命令用 `;`/`&&` 串联本身**不加分**；`git branch -f`、`git worktree add --detach`、
`mktemp -d`、`git -c user.name=... commit`、`2>/dev/null` **全部 SAFE**；
`-m` 里带换行也不触发（有 `stripHeredocBodies` + 引号感知分割兜底）。
截图那条命令的真凶是**末尾被显示截断的 `git worktree remove "$T"`**。

### 1.5 为什么 `bypassPermissions` 挡不住

**[源码]** `handleInterruptions`（约 11249900）里的自动放行分支是有先后顺序的：

```js
// 1) FullAccess：无条件放行，没有任何危险命令豁口
if (tool !== AskUserQuestion && mode === PermissionMode.FullAccess) { approve(); continue }

// 2) 子会话继承 BypassPermissions：有豁口！
if (session.meta?.parentSessionId && tool !== AskUserQuestion) {
  if (mode === BypassPermissions
      && !this.isDangerousBashCommand(item)        // ← 就是这里被拦下
      && !await this.isMandatoryApprovalDeferTool(session, item)) { approve(); continue }
  ...
}
```

而 `isDangerousBashCommand`（约 11274897）的定义就是：

```js
isDangerousBashCommand(item) {
  if (item 不是 Bash) return false
  return checkCommandSafety(cmd).riskLevel === CRITICAL || === HIGH
}
```

**结论（可证伪）：`bypassPermissions` 对 HIGH/CRITICAL 的 Bash 命令完全无效，
只有 `fullAccess` 能放行。** MEDIUM / LOW / 未知命令不受影响，`bypassPermissions` 能挡住。

**[日志]** 与事故完全吻合：出事那条日志明明白白写着
`permissionMode: bypassPermissions`，面板照弹。

**[源码]** 顺带证伪另外几个常见误解：

- `-y` / `--dangerously-skip-permissions` 映射到的是 **`BypassPermissions`**（约 11222789
  `resolvePermissionMode`），**不是** `fullAccess`，所以**同样挡不住**。
- `dontAsk` / `auto` 属于 `isRestrictedDirectApproveMode`（约 11774213），
  它们不但不放行，还会**关掉后台任务会话的直接放行通道**，更容易出问题。
- `plan` 模式与本问题无关。

---

## 2. 防复发

### 2.1 配置层（根治，但**未实测**）

**[源码]** teammate 的权限模式解析优先级（约 6756132，从高到低）：

1. `Agent` 工具调用里的 `mode` 参数
2. 自定义 agent 定义里的 `permissionMode`
3. `resolveSubagentPermissionModeFromConfig()`：
   a. CLI `--subagent-permission-mode`
   b. 环境变量 `CODEBUDDY_SUBAGENT_PERMISSION_MODE`
   c. `settings.json` → `permissions.subagentPermissionMode`
4. 兜底：继承主会话的模式

**关键差异**：第 1、2 条走 `parsePermissionMode()`，它的映射表是
`{default, acceptEdits, bypassPermissions, plan, dontAsk, auto, ignore}` —— **没有 `fullAccess`**；
`Agent` 工具的 zod enum 同样是 `["acceptEdits","bypassPermissions","default","plan","dontAsk","auto"]`。
**也就是说：`fullAccess` 根本没法通过 `Agent` 工具的 `mode` 参数设置。**

而第 3 条的三个来源都只校验 `Object.values(PermissionMode).includes(x)`，
`PermissionMode` 枚举（约 11438723）是
`default / acceptEdits / plan / auto / dontAsk / bypassPermissions / fullAccess / ignore / work / delegate`
—— **`fullAccess` 在里面，能通过校验。**

**建议写法（三选一，未实测，需按 §2.4 验证后再全队推广）**：

```jsonc
// ~/.codebuddy/settings.json
{
  "trustedDirectories": ["/workspace/**"],
  "permissions": {
    "subagentPermissionMode": "fullAccess"
  }
}
```

```sh
# 或者环境变量（对整个 CLI 进程生效）
export CODEBUDDY_SUBAGENT_PERMISSION_MODE=fullAccess
```

```sh
# 或者启动参数
codebuddy --subagent-permission-mode fullAccess
```

> **[源码] 重要陷阱**：优先级第 1 条会**短路**掉第 3 条。
> 只要 `Agent` 调用里传了 `mode`（哪怕传的是 `bypassPermissions`），
> 上面三种配置就**全部失效**。
> 所以采用本方案时，**team-lead 派发 teammate 时不要再传 `mode` 参数**。

> **安全边界**：`fullAccess` 的官方描述是
> 「Skips ALL permission checks including dangerous commands for all agents」。
> 它会连 `rm -rf`、`git reset --hard` 一起放行。
> 本项目的 teammate 都在自己的 git worktree 里、且代码已推远端，风险可控；
> 但这是**降低安全等级**的决定，**必须由项目所有者拍板，不要由 agent 自行修改**。

**[源码]** 另有 `--allowedTools` 支持 `Bash(git:*)` 这种前缀白名单形式
（见 CLI 帮助，约 7430223）。但危险命令判定发生在
`handleInterruptions` 的自动放行分支里，**没有查询 allowedTools 的分支**，
所以**它大概率绕不开 HIGH 判定**。此条置信度低，未验证。

### 2.2 命令形态层（立即可用，**已实测**）

不改配置也能规避：**别让任何一段命中 HIGH/CRITICAL 表**。
判定是**逐段取最高**，所以把危险段单独拆出来没用 —— 要换成不匹配正则的写法。

| 原写法（HIGH） | 替代写法 | 复现器判定 |
|---|---|---|
| `git worktree remove /work/x` | 直接 `rm -r` 也不行；**改为不清理**，或让 team-lead 在 `/workspace` 统一收尾 | — |
| `git worktree prune` | 同上，交给 team-lead | — |
| `rm -rf /tmp/x` | `find /tmp/x -mindepth 1 -delete` 也是 HIGH（`find .*-delete`）；用 `mktemp -d` 后**不清理** | — |
| `git checkout -- <path>` | `git checkout HEAD -- <path>`（中间夹一个 ref 就不匹配） | **SAFE** [实测] |
| `git restore <file>` | `git checkout HEAD -- <file>` | **SAFE** [实测] |
| `git rm -r <dir>` | `git rm -- <dir>`（去掉 `-r`） | **SAFE** [实测] |

**[实测]** 悬空 worktree 记录是无害的，不清理不影响任何人；
真要清理，由 team-lead 在自己的会话里做一次即可（一次面板总比 10 次好）。

改任何命令前先自查：

```sh
node scripts/diag/risk-replica.js '<你要跑的命令>'
```

### 2.3 卡住之后怎么自救

**[日志]** 本次事故实际走通的、也是**唯一**走通的路径：

1. **重启 CodeBuddy 进程**（面板队列是纯内存态，重启即清空）。
2. 从 `history/INDEX.md` 里挑**行数最多**的那个 uuid（不是时间最新的）。
3. `codebuddy --resume=<uuid>`。

**[日志]** 本次会话 `2d8786c5` 在新进程（pid 109957）里被成功续上，
09:03 之前的上下文完整保留，**没有重跑任何工作**。

**[日志]** 已证伪的自救手段：按 `1`、按 `2`、按 `3`、按 `Esc` —— 60 次全部无效。
面板持有输入焦点，Esc 被映射成 `reject`（同样死路），**打不出真正的会话中断信号**。

**⚠️** `codebuddy -c` / `--continue` 是陷阱：它接「最近一次」会话，
崩溃后新开的空会话就是最近的，会恢复出一个空壳。**永远用 `--resume=<uuid>` 指名道姓。**

### 2.4 怎么验证「加了配置真的不会再卡」（证伪方案）

光看配置生效与否不够，要**同时验证放行路径与拦截路径**：

1. **反向对照（必须先做）**：不加配置，派一个 teammate 跑
   `git worktree list && git worktree prune`。
   预期：**弹面板**。日志出现
   `[HandleInterruptions] Enqueued tool: Bash, ..., permissionMode: <mode>`。
   —— 如果这一步不弹，说明测试用例本身无效，后面的「通过」全是假阳性。
2. **正向验证**：加上 `permissions.subagentPermissionMode: "fullAccess"`，重启，
   **并确认派发时没传 `mode` 参数**，重跑同一条命令。
   预期：**不弹面板**，且日志出现
   `[HandleInterruptions] Auto-approving tool Bash for sub-session <id> (FullAccess mode)`。
3. **判据**：以日志里那行 `Auto-approving ... (FullAccess mode)` 为准，
   **不要**以「我没看见面板」为准 —— 面板也可能只是还没渲染出来。

一键检查当前判定：

```sh
node scripts/diag/risk-replica.js --file <每行一条命令的文件>
```

---

## 3. 附：调查工具

| 文件 | 用途 |
|---|---|
| `scripts/diag/risk-replica.js` | 从 CodeBuddy 安装包运行时抽取五张风险正则表并复现判定。**改命令写法前先用它自查。** |
| `scripts/diag/jsgrep.py` | 在 22 MB 单行压缩 JS 里按正则取**字节偏移窗口**（`Grep` 工具会在 2000 字符处截断，读不了压缩包） |
| `scripts/diag/jsslice.py` | 按字节偏移 + 长度切片，配合 `jsgrep.py` 定点阅读 |

**[实测]** 排查 22 MB 压缩 JS 的正确姿势：先用 `jsgrep.py` 拿偏移量，再用 `jsslice.py`
定点读 2~3 KB。**不要整文件 Read**，会撑爆上下文（容器仅 4 GiB 无 swap）。

---

## 4. 仍未解决 / 待跟进

1. **[日志推断]** 「两个 item 对象」的推断尚未复现。要坐实需要在
   `enqueue` / `publishPending` 处加日志打印 item 的身份，本地改产品包不现实。
2. **架构级隐患（建议上报 CodeBuddy）**：
   后台 teammate 有能力在主 TUI 上留下一个**任何按键都关不掉**的模态面板，
   而它自己继续跑。`approve()` / `reject()` 的三条提前 return
   应当在放弃前**无条件 `dequeue()` 并清空 `pending$`**，否则 UI 与状态必然失同步。
   源码里已有 `sandbox popup may stay visible on UI side until manual dismiss`
   这样的自认注释（约 11875286），说明这类孤儿面板是**已知**问题。
3. **[未验证]** `--allowedTools` 前缀白名单能否绕开 HIGH 判定（见 §2.1 末）。

---

## 5. 第二种机制：权限桥（独立进程 teammate → leader 面板）

> 本节由 team-lead 并行定位，tui-diag 于 2026-08-09 13:1x 在 `dist/codebuddy.js`
> 偏移 8557700~8560200 逐字复核，全部与描述一致。
> 置信度统一标 **[源码]**（只读源码推断，未实测触发）。

### 5.1 架构

独立进程的 teammate（非进程内 subagent）需要审批时，其 `PermissionBridgeClient`
（`n8` 之外另一类，偏移 ~8549697）向 leader 的
`POST /api/v1/permission-bridge/request` 发起 **long-poll**（客户端 HTTP 超时 6 分钟）。
leader 侧 `PermissionBridgeService`（`n8`，偏移 ~8557800）的 `handleRequest`：

1. `findLeaderSession(teamName)` 找目标会话；
2. `createApprovalItem` 造一个带 `_bridgeRequestId` 的合成审批项；
3. `leaderSession.interruptionSubject.next(item)` 推到 TUI 渲染面板；
4. `watchForDecision` **猴补丁** `interruptionService.approve/reject` 拦截决策。

### 5.2 三处缺陷（[源码]）

- **缺陷1（超时只回话不撤面板）**：`setTimeout(n5=3e5=5min)` 回调仅做
  `logger.warn` + `ed.resolve({decision:"deny"})` + `pendingRequests.delete(id)`。
  **不调** `interruptionService.reject(item)`、**不** unsubscribe、**不**还原猴补丁。
  → 注入 TUI 的审批项无任何代码路径移除，面板永久残留。
  这正好解释用户说的「后台还在正确运转」（teammate 拿到 deny 继续跑），
  而主 TUI 的面板没人回收。

- **缺陷2（超时后点 Yes 变僵尸）**：`patchedApprove` 命中 `_bridgeRequestId===eA` 时
  先 `resolveRequest(eA,...)` 再 `return`，**不调**原始 `ed(el,ec)`。
  而 `resolveRequest` 在 `pendingRequests` 已被缺陷1超时删空时，只打
  `no pending entry for requestId=..., already resolved or timed out` 后返回。
  → 用户点 Yes → 什么都没发生 → 面板还在。

- **缺陷3（`findLeaderSession` 空表达式）**：
  `return el?.meta?.teamName===eA||el?.meta?.isTeamLead, el`
  是逗号表达式，team 归属判断结果被丢弃，**永远返回 `getCurrent()`**。
  → 面板可能被注入「当前会话」而非真正的 leader 会话，
  解释了「面板有时和当前上下文对不上」。

量化：客户端 6 分钟 / 服务端 5 分钟，顺序上服务端先回，无竞态；
但客户端 `catch` 分支同样返回 `deny`，所以桥一旦出错 teammate 一律被拒。

### 5.3 与本会话卡死面板的关系（[日志]）

本会话日志 `workspace__eab0d61a….log` 中：
`PermissionBridge` 命中数 = **0**，`no pending entry` 命中数 = **0**。

→ **本会话实际卡死的面板不是权限桥路径触发的。**
team-lead 查到的桥缺陷是**真实潜伏 bug**（独立进程 teammate 一旦走桥 + 5 分钟超时必卡），
但与「我们此刻卡住的那块面板」不是同一条通道。本会话更可能是 §1 的
`InterruptionServiceImpl` orphaned-deferred 路径，或 §4.2 提到的
`SandboxWriteRuleGuard`「popup may stay visible until manual dismiss」路径。
两条具体哪条命中，仍待二分（见 §4.1）。

### 5.4 对「`Agent` 工具的 `mode` 参数」是否生效（[源码] / 未查清）

- leader 侧 `handleRequest` **无条件**造面板、不读任何 mode；
- 能否避开面板，只取决于 teammate 进程「自己是否还去问」，
  即其自身 `permissionMode` 是否为 `bypassPermissions` / `fullAccess`。
- `resolveSubagentPermissionModeFromConfig`（偏移 ~6740828）只服务**进程内 subagent**，
  读取顺序：CLI `--subagent-permission-mode` / env `CODEBUDDY_SUBAGENT_PERMISSION_MODE` /
  settings.json `permissions.subagentPermissionMode`（均经 `Object.values(PermissionMode)`
  校验，**接受 `fullAccess`**）。
- 独立进程 teammate 走 HTTP 桥，是否经由同一套解析、**`Agent` 工具的 `mode` 参数能否传入
  其 permissionMode**，本文在预算内未追完 → **标注：未查清（待跟）**。
  当前可执行的规避仍按 §2.1 的 `subagentPermissionMode: "fullAccess"` + 「dispatch 时不传 `mode`」。

---

## 6. 根治手段：`scripts/env/patch-codebuddy.sh`

> 本节由 `env-patch` 于 2026-08-09 补写，需求来自项目所有者：
> 「把这个做成脚本，放进仓库，并且写好说明，最好是直接在 .cnb.yml 里面看能不能直接写进去自动运行。」
> 除特别标注外，本节结论全部为 **[实测]**（验证记录见 §6.6）。

### 6.1 它解决什么，为什么必须脚本化

§1.5 已证实：**`bypassPermissions` 对 HIGH/CRITICAL 的 Bash 命令完全无效**，
面板照弹；而 §1.3 证实那个面板一旦孤儿化就**任何按键都关不掉**，
实测卡死主 TUI 56 分钟，唯一出路是重启进程。
§2.2 的「换命令写法」只是绕，绕不干净 —— 判定是逐段取最高档，日常操作里
`rm -r`、`git restore`、`git worktree remove` 随时会撞上。

手工把 `isDangerousBashCommand` 改成直接返回 false 是有效的，**但补丁活不过重启**：
本项目的开发容器会不定期崩溃重建，`/usr/local/lib/node_modules` 属于「git 仓库以外的一切」，
按 AGENTS.md §10 的说法就是**不可信**。所以要把它做成仓库里的脚本 + 开机自动跑。

### 6.2 用法

```sh
sh scripts/env/patch-codebuddy.sh              # 打补丁（幂等，随便跑多少次）
sh scripts/env/patch-codebuddy.sh --check      # 只报告状态，一个字节都不改
sh scripts/env/patch-codebuddy.sh --wait 600   # 等 CodeBuddy 装好再打，最多等 10 分钟
```

| 退出码 | 含义 |
|---|---|
| 0 | 打补丁模式：补丁已就位（本次打上，或先前就已打上）。`--check`：全部就绪，无事可做 |
| 1 | **出错**：正则失配 / 命中多处 / `node --check` 失败（已回滚）/ settings.json 非法 |
| 3 | 仅 `--check`：尚未打补丁，需要跑一次 |
| 4 | 仅 `--check`：找不到 CodeBuddy 安装 |

调试用参数：`--dist <目录>`（指定安装目录，**排他**，过不了判据就算没找到，
不会偷偷回退去改真实安装）、`--settings <文件>`、`--skip-settings`。

**注意 0 与 4 的不对称**：打补丁模式下「找不到 CodeBuddy」是**退出 0**。
本地开发机、CI 构建机上没装它是正常的，为此把构建判红纯属自找麻烦；
而 `--check` 是给人和诊断脚本看的，那里需要能区分「没装」和「装了但没打」。

### 6.3 它到底改了哪几个字节

```js
// 改前
isDangerousBashCommand(eA){if(("name"in eA.rawItem?eA.rawItem.name:eA.rawItem.type)!==...
// 改后（只在 { 之后插入 9 个字符，原函数体一字不动）
isDangerousBashCommand(eA){return!1;if(("name"in eA.rawItem?...
```

两个入口各改一处：`dist/codebuddy.js`（交互式 TUI）与 `dist/codebuddy-headless.js`
（`-p` 非交互模式）。只打前者的话，将来有人用 headless 跑批会再撞上同一堵墙，
而那时没人会想起来还有第二个入口。

**为什么是插入而不是按偏移替换**：最初手工改是拿字节偏移（约 11274897）
定位、整体替换函数体的。偏移量随每次发版漂移，压缩后的参数名（当前是 `eA`）
也随打包器心情变化。写死任何一个，下次升级后要么改错位置（灾难），
要么**静默什么都没改而照样回显成功**（更糟）。脚本改成正则匹配
`isDangerousBashCommand(<标识符>){` 并在 `{` 后插入，两个易变量都不依赖。

顺带：脚本把 `{return!1` 认作「已打过」，**分号可有可无** ——
手工那版是整体替换写成 `{return!1}`，插入式写出来是 `{return!1;…}`，
两种都必须认，否则会往手工补丁上再插一次。

同时它还会**合并写入**（不是覆盖）`~/.codebuddy/settings.json` 三项：
`permissions.defaultMode` / `permissions.subagentPermissionMode` /
`trustedDirectories` 含 `/workspace/**`。已经是更宽松的 `fullAccess` 时不降级；
文件不是合法 JSON 时**报错退出、绝不覆盖**（那是用户的配置，里面可能有 MCP、hooks）。

### 6.4 CodeBuddy 升级后正则失配了怎么办（重新定位手册）

症状：`--check` 报 `没找到 isDangerousBashCommand(...)`（脚本退出 1），
或者报 `命中 N 处（预期 1 处）`。脚本此时**什么都不改**，这是故意的 ——
0 次命中意味着实现变了，静默跳过会让所有人以为补丁还在生效。

按下面五步走，不要跳步：

**Step 1 —— 先确认这个洞还在不在。** 官方可能已经修了面板 bug，那就不需要补丁了。

```sh
node scripts/diag/risk-replica.js 'git worktree prune'      # 期望仍打印 HIGH
```

如果连它都跑不出结果，说明打包结构变化很大，后面几步的定位也会更费劲。

**Step 2 —— 从「放行分支」下手，别从方法名下手。** 方法名会变，
但那段代码里的日志串 `Auto-approving` 是给人看的，改动概率低得多：

```sh
python3 scripts/diag/jsgrep.py \
  /usr/local/lib/node_modules/@tencent-ai/codebuddy-code/dist/codebuddy.js \
  'Auto-approving' 800 400
```

在窗口里找形如
`…===<X>.PermissionMode.BypassPermissions&&!this.<方法名>(<参数>)&&!await this.<另一个>(…)`
的条件式。**中间那个 `this.<方法名>` 就是新的危险命令判定入口**，不管它叫什么。

**Step 3 —— 定位它的定义，确认语义与命中数。**

```sh
python3 scripts/diag/jsgrep.py <上面那个 codebuddy.js> \
  '<方法名>\([A-Za-z_$][A-Za-z0-9_$]*\)\{' 100 500
```

要确认两件事：(a) 函数体里确实在拿 `riskLevel` 比 `CRITICAL` / `HIGH`；
(b) 全文件只命中 1 处。命中多处说明打包结构变了（比如同名方法被内联进两个类），
这时**不要盲改**，先把两处都读明白。

**Step 4 —— 只改脚本里的方法名。** 改动点集中在
`scripts/env/patch-codebuddy.sh` → `js_patch()` 里的两行：

```js
const reDef     = () => /isDangerousBashCommand\(([A-Za-z_$][A-Za-z0-9_$]*)\)\{/g;
const rePatched = () => /isDangerousBashCommand\([A-Za-z_$][A-Za-z0-9_$]*\)\{return!1/;
```

把方法名换掉即可，**不要顺手把 `([A-Za-z_$][A-Za-z0-9_$]*)` 改成写死的 `eA`** ——
那就把刚才修好的坑又挖回来了。

**Step 5 —— 按 §6.6 的清单重跑验证，尤其是反向对照。**
只跑一次正向就提交是本项目明令禁止的（AGENTS.md「验收判据必须可证伪」）。

**Step 2 找不到 `Auto-approving` 怎么办**：说明自动放行分支被重写了。
这时不要硬改二进制包，退回配置层方案（§2.1 的
`permissions.subagentPermissionMode: "fullAccess"`），
并把新的结构写进本节 —— 下一个人不该再从零查一遍。

**另一种「补丁失效」的形态**：`--check` 说已打补丁，但面板照弹。
那说明弹它的是**另一条通道**（§5 的权限桥，独立进程 teammate 走 HTTP long-poll），
本补丁管不着，去看 §5.2 的三处缺陷。

### 6.5 安全边界：这确实是在降低安全等级

**这个补丁等于让 CodeBuddy 对 `rm -rf`、`git reset --hard` 这类命令不再二次确认。**
和 §2.1 里 `fullAccess` 的告示牌是同一块：

- 适用前提是本项目当前的工作方式 —— 每个 agent 在自己的 git worktree 里、
  代码已推远端、容器本身就是一次性的。
- **它不是「让 agent 可以随便删东西」的许可证。** AGENTS.md §7.5 那张禁用命令表
  **依然全部有效**：不许 `git worktree remove`、不许 `rm -r`、不许 `git reset --hard`。
  补丁拆掉的是「问一句」，不是「不许做」。以前是面板挡着，现在只剩纪律挡着，
  所以纪律反而更要紧。
- 真要临时恢复原状：`--check` 能看出当前状态，
  备份就在 `<原文件>.orig-<日期>`，`cp` 回去即可（然后重启 CodeBuddy 进程）。

### 6.6 验证记录（2026-08-09，全部在**副本**上做）

原则：**「成功回显」不等于事情真的发生了**。所以每条都有可证伪的判据，
而且**先做反向对照**（故意喂坏输入，确认脚本真的会红）。

| # | 场景 | 判据 | 结果 |
|---|---|---|---|
| 1 | 未打补丁的原版副本 → 打补丁 | `node --check` 通过；`grep` 得到 `{return!1;`；文件恰好 +9 字节 | 通过 |
| 2 | 幂等：连跑三次 | 第 2/3 次报「已打过」，三个文件 **md5 完全不变** | 通过 |
| 3 | **反向对照**：方法被改名 | 退出码 **1**，报「没找到」，**目标文件字节未变** | 通过 |
| 4 | **反向对照**：命中 2 处 | 退出码 **1**，报「命中 2 处（预期 1 处）」，文件未变 | 通过 |
| 5 | **变异测试**：把插入串改成会破坏语法的 `return!1;}` | `node --check` 报 `SyntaxError`，**自动回滚**，退出码 1，回滚后 md5 == 动手前 | 通过 |
| 6 | `--check` 在「未打 / 已打」两态 | 分别退出 **3** / **0** | 通过 |
| 7 | settings.json 是坏 JSON | 退出码 1，**原文件一个字节没动** | 通过 |
| 8 | settings.json 不存在 | 自动创建，只含该有的三项 | 通过 |
| 9 | settings.json 已有 `model`/`mcpServers`/别的 trustedDirectories | 全部保留，`/workspace/**` 是**追加**进数组 | 通过 |
| 10 | `defaultMode` 已是更宽松的 `fullAccess` | **不降级**，且完全不写文件（md5 不变） | 通过 |
| 11 | 没装 CodeBuddy / 没有 node | `--check` 退出 **4**，打补丁模式退出 **0**（不打断 CI） | 通过 |
| 12 | `--dist` 打错字指向不存在的目录 | 判定为「没找到」，**不回退去改真实安装** | 通过 |
| 13 | `--wait 30`，目标文件 8 秒后才出现 | 等到后照常打补丁，实测耗时 10s | 通过 |
| 14 | `--wait 8`，目标始终不出现 | 等满后优雅跳过，退出 0 | 通过 |

第 5 条的输出（这是**回滚真的发生了**的证据，不是「我觉得它会回滚」）：

```
  OK  已备份到 codebuddy.js.orig-20260809
错误: 打完补丁后 node --check 不通过，已回滚 codebuddy.js
      class X{isDangerousBashCommand(eA){return!1;}return eA.x===1}}
      SyntaxError: Unexpected identifier 'eA'
rc=1
--- 目标文件是否被还原成动手前的字节？
4a7751c59d897290b142fa5c20824d20  （与动手前一致）
```

> 顺带一个实现细节，因为它差点被写错：**报错信息必须在回滚之前抓**。
> 先回滚再跑一次 `node --check` 拿到的是原文件的结果（当然是通过的），
> 打印出来只会误导人 —— 又一个「回显和事实脱节」的小陷阱。

### 6.7 自动运行：挂在 `.cnb.yml` 的 `vscode` 事件里

`$: vscode:` 那个 job 的 stage 会在**云原生开发环境启动时**执行（不是 CI）。
补丁脚本挂在那里，容器每次重建都会自动重打。

**一个实测出来的时序坑，别踩**：

```
容器启动           2026-08-09 07:55:38
codebuddy-code 包   2026-08-09 07:57:30    ← 晚约 2 分钟
```

CodeBuddy 是容器起来**之后**才装上的。stage 若跑在它出现之前，
脚本会「优雅跳过、退出 0」，构建日志一片绿而补丁一次都没打上 ——
第八例「成功回显 ≠ 事情真的发生」。所以 stage 里是**打一次 + 后台 `--wait` 守候**
两手都要，且全程 `|| true` / `exit 0`，绝不能挡住开发环境启动。

后台那次的日志在 `/tmp/patch-codebuddy.log`。**判断补丁到底打上没有，
不要看 stage 的绿灯，跑一句：**

```sh
sh scripts/env/patch-codebuddy.sh --check ; echo "rc=$?"   # 0 才是真就位
```
