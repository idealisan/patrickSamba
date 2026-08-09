---
name: 跨 agent 的 sha/计数出厂即过期 + 批量扫描要抽验
description: 别信队友给的 sha/计数，决策前先自己 fetch；批量扫描别换简单命令，结论出来后随机抽 1 个用精确方法复验
type: feedback
---

活跃开发期 `main` 几分钟一变，**任何跨 agent 传递的 sha / commit 计数出厂即过期**。
轮次内同一现象三轮出现了三个方向：§16.2 pm 落后于 win-meta、§17.1 win-meta 落后于 pm、
§17.9 qa-verify 落后于 pm（16:12 查 `main=b6d16b4`，16:35 复核已是 `ebd9b59`）。

**两条铁律（pm 在 §17.9 定的，对本仓库 CI 复核工作直接相关）：**

1. **收到别人的 sha/计数用于决策前，先自己 fetch 一次。** 不要直接采信。
   本轮 qa-verify 做对的关键一步就是：没信 pm 给的那批 sha，而是重新 `git ls-remote` /
   `build/logs` 取了 live 数据才回话 —— 结果 pm 的 sha 当时已经又被顶掉了。

2. **批量扫描/批量查状态时，别为了省事换更简单的命令；批量结论出来后，
   随机抽 1 个样本用精确方法复验一遍。**
   判据会随任务规模退化：逐个核对 7 个 PR 时用对了三点写法 `git diff --name-only A...B`，
   转成 55 个分支批量扫描时图快换成两点，把「落后于 main」误计成「改动」
   （声称 44 个分支动过 check-test-compile.sh，真实是 0，整节撤回）。
   批量统计产出的是拿去做决策的聚合数字，且**没有单点可供人肉核对**，最怕判据错。

**Why:** 两点 diff `A..B` 与三点 `A...B` 语义不同；用错会把「已落后于 main」算成「有改动」。
只要随机抽任意一个分支跑 `git rev-list --count <merge-base>..<branch> -- <文件>` 或
`git diff --name-only origin/main...<分支>`，当场就能看到 0。

**How to apply（qa-verify 的 CI 复核场景）：**
- 接到队友的 sha/计数并要据此行动 → 先 `git fetch` + `git ls-remote` / CNB `build/logs` 自取。
- 做全仓/全 PR 批量扫描 → 别替换命令；扫完从结果里随机挑 1 个，用最精确的单点方法复验，
  不一致就怀疑整批口径。
- 报告里始终带时间戳（AGENTS.md §7.6），让分歧可诊断。

## 新形态（2026-08-09 19:00，pm 自己栽的）：**分支的远端 tip 也会陈旧**，祖先判定对此静默容忍

判「某份工作有没有进 main」时，pm 用了 `git merge-base --is-ancestor <分支远端 tip> origin/main`，
命中 **假阳性**：那个 tip `96afd87` 是该分支**开工时从 main 拉进来的合并提交「Merge #136」**，
本来就已是 main 的祖先——判的其实是「这个分支曾经基于 main」，不是「工作进了 main」。

**根因**：分支的 `origin/<b>` 只反映「上次 fetch 时的远端状态」，它会**落后于本地**、也会落后
于 main 的演进；而 `--is-ancestor` 对「陈旧的 fork-base tip」**不会报错，只会静默通过**。
这和「跨 agent 的 sha 出厂即过期」是同一类——**引用层面的数据也会过期，且过期时工具不报警**。

**判据改写**（判「工作有没有进主干」死用内容，别用分支 tip 的祖先关系）：

```sh
# 这份工作必然会删/改的文件，在 main 上还是旧样吗？
git show origin/main:internal/vfs/xattr_unix.go   # 还在 → 没合；不在 → 合了
# 或比 blob：分支上该文件的 blob 与 origin/main 上同路径是否相同
```

**How to apply:** 任何「某分支的工作进了 main 吗」的提问，统一走 `git show origin/main:<文件>`
或内容/blob 比较；**禁止**用 `<分支 tip> --is-ancestor origin/main` 当合并判据（那只能证明分支
曾经基于 main）。这条与 `reference_cnb_pr_api.md` 的「判合并三条命令」表互锁：祖先判定只对一个
固定 SHA 有效，对「分支 tip」无效。

## 再补一条（2026-08-09 19:03，pm 亲历）：**`fetch` 之后的 `origin/<分支>` 也可能不是服务端事实**

18:58:54 跑完 `git fetch -q origin main pm/v020-board`，读本地 `origin/pm/v020-board`
得到 `f247ec3`，据此判定「合并提交还没推上去」；19:01 `git ls-remote origin` 显示服务端
**已经是 `c5d35be`**，CNB 的 PR API 也已把 head sha 更到 `c5d35be`。
`git reflog show refs/remotes/origin/pm/v020-board` 显示该跟踪引用在 **18:59:31** 才
`update by push` 写上——中间有一段窗口，**本地引用与服务端不一致**。

成因未查到底（并发推送在途，或自己被自动后台化的任务在推，见 AGENTS.md §10.3 第 11 条），
**所以只记可观测事实、不安因果**：

> 判「推没推 / 远端现在是什么」的权威来源是 **`git ls-remote origin '<ref 或通配>'`**，
> 不是本地 `origin/<分支>`，也不是刚跑完 `fetch` 的错觉。

多 worktree 共享一份 `.git`、多个实例可能同时推同一分支的环境下，这个窗口是常态而非异常。
同源前科：`refs/rescue/*` 那次误报也是「本地引用看不见服务端事实」
（见 `project_rescue_ref_inflight.md` 配套事实 2）。**两次栽在同一件事的两副面孔上。**
