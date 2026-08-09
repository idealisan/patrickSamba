---
name: PM 盘点法——扫全部 worktree 找「只存在于磁盘上」的成果
description: 多 agent 并行时真正的丢失风险不是「没开工」而是「干完了没推」；给出一段可复用的全 worktree 扫描脚本与三种典型残留的判读方法
type: project
---

多 agent 并行开发时，**最大的成果损失来源不是「谁没开工」，而是「干完了但只落在容器磁盘上」**。
容器一崩，未推送的提交与未提交的改动全部蒸发，而且没有任何人会收到通知。

**Why**：2026-08-09 v0.2.0 第 12 轮盘点一次性捞出两处真实损失：
① `qa-proto` 修好了卡住全队 CI 的 data race，改动**未提交**；
② 已退出的 `qa-e2e` 有 **2 个原创提交、938 行**测试设施（`test/e2e/smoke.sh` 348 行 +
smbclient/impacket/go-smb2 三客户端 + 自带反向对照的 `reverse-control.sh`）**从未推送**，
而它正是 v0.2.0 A 块的核心交付物，且有另一个 agent 正准备从头重写同样的东西。
更早还有 `tm-handle` +1677 行、`r-infra` +600 行的同类事故（最后靠代开 PR #20/#21 救回）。

**How to apply**：每轮盘点跑一次这段扫描，不要只看 `git branch -r`——
分支存在**不代表**工作推上去了：

```sh
cd /workspace && git fetch -q origin --prune
git worktree list --porcelain | grep '^worktree' | sed 's/^worktree //' | while read -r w; do
  [ -d "$w" ] || continue
  b=$(git -C "$w" branch --show-current 2>/dev/null); [ -n "$b" ] || continue
  git show-ref -q --verify "refs/remotes/origin/$b" || { echo "$w [$b] ** 远端无分支 **"; continue; }
  n=$(git -C "$w" rev-list --count "origin/$b..HEAD")
  d=$(git -C "$w" status --porcelain | wc -l)
  [ "$n" = 0 ] && [ "$d" = 0 ] && continue
  printf "%-24s %-34s 未推送=%s 未提交=%s\n" "$(basename $w)" "$b" "$n" "$d"
done
```

> **⚠️ 上面这段脚本会严重虚报，别单独用它下结论 —— 先看下面「正确的两步 R7 探测器」。**
> 它逐分支问 `origin/$b..HEAD`，也就是只问「在**同名**远端分支上吗」，
> 而同名分支根本不是唯一的持久化去处。实测一次报出 41 笔「未推」，真实丢失 **0**。

判读三要点（**别看到数字就报警，先分类**）：

1. **「未推送=N」要减去合并祖先**。`qa-e2e` 显示领先 29，逐条 `git merge-base --is-ancestor <c> origin/main`
   之后真正的原创提交只有 2 个，其余 27 个是已在 main 里的 merge 祖先。不筛会严重虚报。
   **但 `--is-ancestor` 自己在 squash 合并下会假阴性**（见 `reference_cnb_pr_api`），
   所以它只能用来**快速剔除**明显的祖先；剔不掉的那些必须再走一遍「比内容」。
2. **「未提交」先问是不是残留**。作者已退出的 worktree 里的 modified 文件多半是变异测试残留
   或半截实验，不是成果（本项目 R2 就是这么误报过一次）。判据：该 worktree 有没有未推送的原创提交。
3. **已推送 ≠ 可编译**。`go build ./...` **不编译 `_test.go`**，坏分支能大摇大摆推上去
   （`qa-proto` 就这么推了个 `vet` 挂掉的分支）。核实用
   `go vet ./<包>`（含测试文件）或 `sh test/ci/check-test-compile.sh`。

**催办确实有效**：把「你的哪个文件、处于什么状态、为什么在关键路径上」摆成证据发过去，
本轮 `qa-proto` 与 `oscap-rules` 都在数分钟内完成推送。**不要只把风险记进文档等人来看。**

## ⚠️ 问「谁改了某个文件」必须用三点 diff，两点 diff 会把「落后」算成「改动」

`git diff origin/main <branch> -- <file>`（**两点**）比的是**两个快照**，
所以「main 有、分支还没有」的内容同样算差异——**一个分支只要落后于 main，
哪怕一行都没碰过该文件，也会被计入**。

**Why**：2026-08-09 v0.2.0 收尾时用两点 diff 扫共享 CI 文件，得出
「48 个分支动 `.cnb.yml`、44 个动 `test/ci/check-test-compile.sh`，是系统性合并瓶颈」，
据此差点向 team-lead 提「给这两个文件指定单一 owner」的架构建议。
换三点口径重扫，真实数字是 **0 和 0** —— 瓶颈根本不存在，
那 40 多个全是**落后于 main 的分支**被误计。
决定性反例：`rel-v010/cnb-image` 有 4 个提交、其中触碰 `.cnb.yml` 的是 **0** 个，
两点 diff 却报它「动了 `.cnb.yml`」，且差异里 9 行是 main 有而它没有。

**How to apply**：两个问题，两种判据，别混用——

| 想知道 | 用什么 | 判读 |
|---|---|---|
| 这个分支**自己改了**哪些文件 | `git diff --name-only $(git merge-base origin/main $br) $br -- <file>`，等价写法 `git diff origin/main...$br`（**三个点**） | 有输出＝真的改了 |
| 这个分支现在**能不能干净合并** | `git merge-tree --write-tree origin/main $br` | 退出码 0＝干净；1＝冲突，stdout 直接列冲突文件名 |

顺带两条同源纪律：

- **点数是语义差别不是风格差别**，`..` 与 `...` 在 `diff` 和在 `log`/`rev-list` 里
  含义还正好相反，写之前先想清楚问的是哪个问题。
- **报「N 个分支有问题」之前，先挑一个具体分支验证结论对它成立**。
  上面这次只要拿任意一个分支跑一下 `git rev-list --count <mb>..<br> -- <file>`，
  当场就会发现是 0。**聚合数字最容易掩盖口径错误**，
  这与本文件上一节「报警要点名到具体符号，不能只报差值」是同一条纪律。

## ⚠️「数量减少」≠「成果丢失」——报警前必须做去向核对

同一轮盘点里我自己栽了一次：比较两个分支时看到
`durable_defect_test.go` 的 `func Test` 从 **7 掉到 4**，当即判定「合并静默删掉了 3 个用例」，
并要求对方做并集。**错的。** 那 3 条是被**改名挪到了另一个文件**
（`TestQADefect*` in `durable_defect_test.go` → `TestQADurable*` in `durable_qa_test.go`），
一条没少，对方总数反而比另一侧多一个。

**Why**：搬迁与改名在「单文件计数」这个视角下和删除完全同形。
只看差值就报警，会把**正确的重构误判成事故**，还会指挥别人把它改回错的样子——
比不报警危害更大。而且这与项目一再强调的「判据必须可证伪、探针要有反向对照」
（R5）是同一条纪律，我等于在提出判据的同一封信里违反了它。

**How to apply**：任何「数量下降」类信号（测试函数数、文件数、行数、导出符号数），
在下结论前先做一次**全仓去向搜索**：
`grep -rn "<具体名字>" --include=*.go .`（用**名字**找，别用计数找），
确认是「消失」还是「换了地方/换了名字」。**报警要点名到具体符号，不能只报差值。**

延伸：Go 测试尤其容易出现这种搬迁——「根因未修的复现用例挂在 build tag 后面，
修好后去掉 tag 挪回常规回归文件」在本项目是**明文规定的正常流程**，
所以 tag 文件里用例变少通常是**好事**，不是事故。

## 「两个人查出的数字不一样」——先查时间线，再怀疑工具

2026-08-09 v0.2.0 收尾：PM 报「7 个开放 PR」，win-meta 同期只看到 3 个，
并据此怀疑 CNB 的 `list-pulls` 端点不可靠（他还观察到列表端点与单 PR 端点
对同一个 PR 的 `mergeable_state` 给出过相反答案，确有其事）。

**真因不是端点，是两次查询之间隔着一次四连合并。**
从 main 的合并提交时间戳看：15:59:21 一批合掉 `#119`/`#103`/`#44`/`#42`；
PM 的 7 取于 15:49–15:56，对方的 3 取于 16:01 之后。**7 − 4 = 3，两次都对。**

**Why**：把「数字对不上」直接归因为「工具不可靠」是最省力的解释，但代价很大——
它掩盖真因，并让团队从此不信任一个其实没毛病的工具，之后每次盘点都要多绕一圈。
这与上一节「数量减少 ≠ 成果丢失」是同一类错误：**拿差值下结论，不做去向核对。**
在快速合并期，PR 列表这类状态的半衰期可以短到几分钟。

**How to apply**：任何跨人/跨时刻的计数分歧，先取**带时间戳的客观时序**再谈对错——
`git log --format='%h %cd %s' --date=format:'%H:%M:%S' origin/main`
把合并事件排出来，通常几秒钟就能对上账。**报状态数字时必须同时写下取数时刻。**

**升级（同日再撞两次后）**：这个现象在三轮之内出现了 **3 次**，三个方向都占全了——
我落后于 win-meta、win-meta 落后于我（他说「开放 PR 只剩 #120」时 #120 已合入 12 分钟）、
qa-verify 落后于我（他给 `main = b6d16b4`，23 分钟后已是 `ebd9b59`）。
**所以这不是谁疏忽，是并行团队里状态快照的固有属性**：活跃期 `main` 几分钟变一次，
**任何跨 agent 传递的 sha / 计数出厂即过期。**

因此纪律有两条，别只记第一条：

1. **报**数字时带上取数时刻 —— 这只让分歧**可诊断**，不能预防。
2. **收到**别人的 sha / 计数时，**默认它已过期**；拿它做决策前先自己 `git fetch` 复核一次。
   qa-verify 那次做对的正是第 2 条：他没直接用我给的 sha，重新取了 live 数据才回话。

## ⚠️ 判据会**随任务规模退化**：精确核对时用对的命令，批量扫描时会被「顺手简化」

同一份文档 `docs/status-v0.2.0.md` 里，§15.3 逐个核对 7 个 PR 时写的是
**正确的三点** `git diff --name-only origin/main...<分支>`；
两节之后 §16.5 批量扫 55 个分支时，却换成了**两点** `git diff origin/main <分支> -- <file>`，
于是得出「48 个分支动 `.cnb.yml`」这个纯属虚构的数字（真实为 0），
还差点据此向 team-lead 提出一条架构建议。

**Why**：错误的根源**不是不懂语义**——正确写法就在同一份文档里，是我自己两节之前写的。
真正的机制是：**逐点核对时慢工出细活，一转成批量统计就本能地挑更省事的写法**。
而批量统计恰恰是**最不能出错**的场合：它产出的是要拿去做决策的聚合数字，
而且**没有任何单点可供人肉核对** —— 7 个 PR 的表格错了一眼能看出来，55 个分支的计数错了看不出来。

**How to apply**：
- **批量扫描必须复用逐点核对时验证过的那条命令**，不允许「批量时简化一下」。
  （与 `save.sh` 那条「门禁不许顺手简化」同源。）
- 批量结论出来后，**随机抽 1 个样本用精确方法复验**。这次只要抽任意一个分支跑
  `git rev-list --count <merge-base>..<branch> -- .cnb.yml`，当场就会看到 0。
- 更一般地：**聚合数字是掩盖口径错误的最佳场所**，报聚合数字前先证明它对某个具体样本成立。

## `mergeable_state` 有保质期，基线是 main 的 sha；判可合并性用 `git merge-tree`

同一轮的第二个教训（win-meta 提出，PM 复核成立）：
`#26` 在 main=`413ebf3` 时是 `mergeable`，几分钟后**分支一个字没动**、
仅因别的 PR 合入使 main 前进，同一个分支 sha 就变回 `conflict`。

可证伪复现（拿两个历史 sha 直接算，不依赖任何服务端状态）：

```sh
git merge-tree --write-tree <含冲突源的 main sha> <分支 sha>
# → exit 1，并在 stdout 列出冲突文件名
```

**退出码语义已实测**：`0` = 真能干净合并，`1` = 有冲突。
干净/冲突/自身合自身三种场景都验过。

**How to apply**：
1. **「X 个 PR 全部 mergeable」这种结论必须同时标注当时的 main sha**，
   并注明「前提是未合入任何 PR」——否则读者会以为是分支自己坏了。
   **合掉第一个 PR 之后，其余 PR 的该字段全部作废，必须重算。**
2. 判可合并性以**本地 `git merge-tree`** 为终审，API 的 `mergeable_state` 只当参考信号
   （它会因缓存/端点不同而自相矛盾）。这条同时满足「判据必须可证伪」那条纪律：
   本地、离线、可复现、还顺带给出冲突文件清单。
3. 合并顺序规划时，用 `git diff --name-only origin/main...<分支>` 两两求交集，
   **只有交集非空的 PR 对才需要排序**，其余任意顺序。本项目实测该方法
   提前 7 分钟、精确到文件名地预言了一次冲突（`#103` 抢先合导致 `#26` 的两处 `.md` 冲突）。

## ✅ 正确的两步 R7 探测器：第一步问「所有远端 ref」，不是问同名分支

本文件开头那段逐 worktree 脚本方向对（磁盘与远端两路对账），**但计数方式是错的**。

**Why**：2026-08-09 v0.2.0 第 19 轮，PM 用逐分支 `git log origin/$b..$b` 扫全仓，
报出 **41 笔未推**（`main` 1 / `qa-e2e/ci` 29 / `vfs/deflake-path-perf` 8 /
`win-meta/agents-append-only` 3），看着像大出血。逐条核完**一笔都没丢**：

| 分支 | 报的 | 真相 |
|---|---|---|
| `main` | 1 | 那笔已推，只是推在 `origin/oscap-gate/memory-silent-failures` 上 |
| `vfs/deflake-path-perf` | 8 | 7 笔是 main 的历史；唯一新的那笔**随作者换的新分支**推出去了 |
| `win-meta/agents-append-only` | 3 | 两笔是 PR merge 提交（内容在 main），一笔在别的远端分支上 |
| `qa-e2e/ci` | 29 | 27 笔是 main 的历史；余 2 笔是 rebase 前的重复 SHA，内容已在 main |

根因：`origin/$b..$b` 问的是「这些提交在**同名**远端分支上吗」。
而**人会换分支**（把提交带去新分支）、**内容会被 squash 进 main**（SHA 对不上但东西在）。
**同名分支不是唯一的持久化去处。**

**How to apply**：两步，缺一不可。

```sh
# 第 1 步：本地有、而任何远端 ref 都没有的提交（一次问全部 remotes）
git log --oneline --branches --not --remotes
# 第 2 步：对每个候选比内容，确认是否已由别的路径进了 main
git diff --stat origin/main <sha> -- <该提交碰过的文件>      # 空 = 已进，不算丢
```

实测第 1 步把 41 降到 2，第 2 步把 2 降到 0（那 2 笔产物的 9 个文件与 main **blob hash 逐个相同**）。

**母题**：判「有没有丢」和判「有没有合」是同一件事的两面 ——
**都不能靠 ref 关系（ancestor / 分支名 / 三点 diff）终审，终审判据永远是内容。**

## ⚠️ 三点 diff 也不能判「是否已合并」（squash 之后仍非空）

上面 §「问『谁改了某个文件』必须用三点 diff」解决的是「两点会把落后算成删除」。
但三点口径**只回答一个问题**：分支相对共同祖先加了什么。**它不回答「main 里有没有」。**

**Why**：2026-08-09，某 PR 已经 squash 合进 main 之后再跑
`git diff --stat origin/main...origin/lead/crash-forensics`，输出**仍然是 `2 files, +96`**，
看着像完全没合。原因是 squash 让 main **独立地**引入了同一份内容，而 merge-base 没动。

**How to apply**：三个问题三种命令，别串台 ——

| 想知道 | 用 | 不能用 |
|---|---|---|
| 合并会带来/删掉什么 | `git diff A...B` + `git merge-tree --write-tree A B` | `git diff A B`（两点，把落后算成删除） |
| 内容**是否已进** main | 逐文件比 blob hash；或 `git diff main 分支 -- <文件>` 为空 | `git diff A...B`（squash 后仍非空）、`merge-base --is-ancestor`（squash 后假阴性） |
| 分支落后多少 | `git log 分支..main` | — |

## 「分支不存在」也 ≠「没干活」（本文件主旨的镜像版）

本文件主旨是「分支存在 ≠ 推上去了」。**镜像同样成立**：
2026-08-09 PM 只扫 `refs/remotes/origin`，没看到 `env-patch` 的分支，判成「未开工」——
实际上 `/work/env-patch` 里有分支、有 1 笔未推提交、共享任务板上他两项任务已标 completed，
**活干完了，成果只在磁盘上**。报警后本人数分钟内推送，风险解除。

所以盘点必须**远端 ref 与磁盘 worktree 双向对账**，任一单边都会得出错误结论。
另一条线索是**共享任务板**：任务标了 completed 而远端无对应产出，就是现行 R7。

## 判「产物是否只存在于磁盘上」：唯一可信的是 `git ls-remote origin`

盘点时最容易信错的两个东西：

| 看什么 | 它到底证明了什么 | 陷阱 |
|---|---|---|
| `git worktree list` | 目录存在、分支被检出 | **完全不证明任何东西到过远端**。有提交没推，它照样显示得好好的 |
| 本地 `refs/remotes/origin/*` | 你**上次 fetch 时**远端的样子 | 不 fetch 就是陈旧数据；别人刚推的分支你看不见，会误判成「没开工」 |
| `git ls-remote origin 'refs/heads/<前缀>/*'` | **此刻远端真实有什么** | 无 —— 这才是判据 |

实例：2026-08-09 18:15 我按本地 remote-ref 判定 `env-patch` 「零分支、未开工」并报了警；
18:19 一条 `git ls-remote origin 'refs/heads/env/*'` 直接返回
`9212877 refs/heads/env/patch-codebuddy` —— 人家早就在干，只是我看的是快照。
**盘点前必 `git fetch --prune`，拿不准就 `ls-remote` 现问一次。**

## 「保命提交」自身也会漏：commit 挡不住容器整个消失

2026-08-09 18:34 有人给 10 个离队 agent 的工作树做了一轮保命提交（体量不小，
单笔 +331 / +216 / +207），**10 笔全部只在本地，一笔没推**。到 18:42 复查仍是 10。

这是「成功回显 ≠ 事情真的发生」的又一形态：**commit 成功了，于是所有人都以为工作安全了**，
但 commit 只挡住「工作树被清」，挡不住「容器整个没了」。
而且这些分支的主人已经离队 —— **没有任何人会自己来推**，它必须由做清扫的那个人收口。

盘点时把这条做成固定检查项：

```sh
git log --oneline --branches --not --remotes | wc -l   # 应为 0；非 0 就逐条追到人
```

## 「零产出」必须再细分成三类，否则会记错账

工作树文件 mtime / 提交数只能区分「写没写」，**区分不了「为什么没写」**。
只报「某某 20 分钟零产出」会把「在等你拍板」的人写成「掉链子」，账记在了错的人头上。

| 类别 | 判据 | 账记给谁 |
|---|---|---|
| **等决策** | team-lead 与该 agent 之间**最后一条消息是 agent 发出的问题** | team-lead |
| **在读代码 / 调研** | 有明确的调研指令，或本人自报在读；探针文件可能在 `/tmp` 且已删 | 无人，正常状态 |
| **真卡住** | 最后一条是 team-lead 的指令，此后无消息、无提交、无脏文件 | 该 agent |

实例：`oscap-wire` 18:00~18:20 零提交，我一度记成「20 分钟零产出」；
实际他 18:17 已发出调研结论并明确写了「我没动手，等你回复」——属**等决策**，
后来按 team-lead 口径订正为「按令处于调研阶段，不记停滞」。

**最省事的做法**：盘点时顺带看一眼双方最近一次消息的方向，一眼就能分类。

## 盘点必跑 `git ls-remote`：本地引用**推断不出**远端状态（同一盲区已栽两次）

2026-08-09 一天之内，同一个错误换了两副面孔：

| 用的命令 | 报了什么 | 真相 | 盲区 |
|---|---|---|---|
| `git log origin/<b>..<b>` 逐分支 | **41 笔未推送** | 真实丢失 0 | 逐个问同名远程跟踪分支，问不到就当没推 |
| `git log --branches --not --remotes` | **10 笔未推送** | 10 笔 18:34 就在远端 | `--remotes` **只展开 `refs/remotes/*`**；服务端的 `refs/rescue/*` 本地没有跟踪引用 |

**共同根因：拿本地引用去推断远端状态。** 本地引用只是「上次 fetch 时的快照」，
而且**只覆盖 `refs/heads` 这一个命名空间**。

盘点固定跑这三条，缺一不可：

```sh
git fetch -q origin --prune
git log --oneline --branches --not --remotes     # 初筛，只覆盖 refs/heads
git ls-remote origin 'refs/heads/*'              # 分支的权威答案
git ls-remote origin 'refs/rescue/*'             # 保命用的 rescue ref，前两条都看不见
```

**`--branches --not --remotes` 从「判据」降级为「初筛」**：它报 0 才有意义，
报非 0 时必须再用 `ls-remote` 复核，否则就会像我这样**公开报一次不存在的丢失**。

顺带：误报的代价不比漏报小 —— 它让 team-lead 去处置一个已经解决的问题，
而真正的敞口（`oscap-wire` 的在途文件）反而被稀释在同一条消息里。

## ⚠️ 「净化分支」是一次静默删除：重建后必须对旧 head 做全量去向核对

把一条脏分支（混了 merge 提交、别人的内容、跑偏的记忆文件）重建成干净分支的标准手法是
**从 `origin/main` 新开一条，只 cherry-pick 自己那几笔**。这一步的语义是
**「白名单保留」**，也就是**没被点名的东西一律消失**，且不留任何痕迹 ——
`git status` 干净、编译通过、测试全绿、PR diff 变漂亮，**没有一个信号会提示你删过东西**。

**Why**：2026-08-09 `oscap-wire` 净化 PR #159（`37f0c0f` → `6309e0c`）时，
预期删掉的是两处 merge 噪声，实际连带删掉了 `memory/project_port_wiring_acceptance.md`
（4528 字节，自己刚写的接线验收判据）和它在 `MEMORY.md` 里的索引行。
**1 小时 40 分钟无人发现**，连本人的记忆工作副本里也一并没了 ——
它当时唯一的存身之处是净化前顺手推的 `refs/rescue/oscap-wire-pr159-precleanup`。
若当初省掉那个 rescue ref，这份记忆就随净化永久蒸发，而且**没有任何人会收到通知**。

**How to apply**：净化/重建分支固定三步，第 3 步不许省——

```sh
git push origin "旧head:refs/rescue/<角色>-precleanup"      # 1. 旧 head 先上远端
# 2. 从 origin/main 新开分支，cherry-pick 自己的提交
git diff --name-only <旧head> <新head>                       # 3. 逐个文件判「该不该消失」
```

第 3 步的输出会同时包含「本来就该没有的（别人的内容/merge 噪声）」和
「误伤（自己的产物）」，**两者在 diff 里长得一模一样，只能逐条人工判归属**。
判归属的快捷判据：`git log --oneline <旧head> -- <该文件>` 看最后一笔是谁写的。

**母题**：这与本文件「数量减少 ≠ 成果丢失」是同一枚硬币的反面 ——
那条讲**不要**把搬迁误判成删除，这条讲**不要**把真删除当成清理成功。
两条共用同一个动作：**别看聚合信号（diff 变干净了/计数掉了），去做逐项去向核对。**

## 「推了」和「排进队列了」是三层，每层都会漏一批

盘点交付物不能只看 PR 列表。三层各有漏网形态，都真实发生过：

| 层 | 漏网形态 | 判据 | 实例 |
|---|---|---|---|
| 1 | 只在磁盘，没推 | 各 worktree `git log --oneline @{u}..HEAD` 有输出 | 见上文 qa-e2e / tm-handle |
| 2 | 推了，**没开 PR** | 远端有分支，PR 列表里查不到 | `docs-honesty/memory-buffer`（oscap-wire 抓到）、`ci/vscode-patch-hook`（f47da5c，只改 .cnb.yml +22 行） |
| 3 | 有 PR，但**不在发布序列里** | PR open 却没人排它 | 上面两条被发现后才补进队列 |

扫第 2 层（推了但没 PR）：

```sh
git fetch -q origin
for b in $(git for-each-ref --format='%(refname:short)' refs/remotes/origin | grep -v HEAD); do
  n=$(git rev-list --count origin/main..$b 2>/dev/null)
  [ "${n:-0}" -gt 0 ] && echo "$b  +$n"
done
```
已合的分支 count 为 0，不会误报；输出逐条对 PR 列表，对不上的就是孤儿分支。

### 孤儿分支的耦合风险比漏合本身更贵

合一条被遗忘的分支时，**它可能推翻文档里某句当前为真的话**。
实例：`ci/vscode-patch-hook` 把 `patch-codebuddy.sh` 挂进 `.cnb.yml`，
而 AGENTS.md §10.3 第 12 条正写着「挂接还没进 main，所以开工第一件事仍需手工跑一次」，
并附可复算判据 `grep -c patch-codebuddy .cnb.yml → 0`（实查确为 0，该句当前为真）。
**合入当天那句话就变成假的。**
所以孤儿分支进队列时必须连带问一句：**「它会让哪句已写下的话失效？」**
失效的那句要**同 PR** 改掉，否则就是块外腐烂。

### squash 时代判「是否已合」只有两条能用

CNB 自 2026-08-09 17:17 起默认 squash，于是：
`is-ancestor <分支tip> origin/main` **恒 rc=1**（假红），
`git log | grep 'Merge pull request'` **恒有命中**（假绿）。两条都废了。

```sh
# 权威：CNB API 的 is_merged（注意 state=closed 不等于合了，可能是关闭）
curl -s -H "Authorization: Bearer $CNB_TOKEN" \
  "https://api.cnb.cool/<owner>/<repo>/-/pulls/<N>" | grep -o '"is_merged":[a-z]*'
# 最硬：拿该分支独有的文件做逐字节比对
git diff --quiet origin/main origin/<分支> -- <该分支独有的文件> && echo 已合
```
实例：`#152` 的 `is-ancestor` = rc=1（看着没合），而
`git diff --quiet ... -- scripts/env/patch-codebuddy.sh` = SAME、API `is_merged=True` —— 早就合了。

### 自我更正：分支扫描证明不了「没有 PR」

我用上面那段扫描发现 `ci/vscode-patch-hook` 领先 main，就向全队宣布它是「没有 PR 的孤儿分支」。
**错。它是 PR #166（open）。**
分支扫描只能证明「分支存在且领先 main」，**证明不了「没有 PR」** —— 后者必须查 PR 端点。
用**不完整的方法**下**完整的结论**，是本文件反复讲的同一个错误，这次是我自己犯的。

正确判据：扫出来的分支逐条查 PR，查不到才是孤儿。
```sh
curl -s -H "Authorization: Bearer $CNB_TOKEN" \
  "https://api.cnb.cool/<owner>/<repo>/-/pulls?state=open&page_size=100" | grep -o '"ref":"refs/heads/[^"]*"'
```
（该列表端点与单 PR 端点对同一 PR 的 `mergeable_state` 给过相反答案，**关键判定用单 PR 端点**；
列表端点用来做「有没有 PR」这种存在性判断是够的。）

### 两点 diff 幻觉的第三种形态：误报**删除**

前两种是误报「改动」和误报「合并瓶颈」，第三种最唬人：
`git diff <A> <B>` 显示 B **删除了 6 个文件、-255 行**，实际上一个都没删 ——
那些文件是 A 后来新增的，B 只是**落后**。

判据（不要用两点 diff 判删除）：
```sh
git merge-tree --write-tree <A> <B> >/dev/null 2>&1; echo $?   # 只取 rc
git ls-tree --name-only <结果树> <路径>                        # 逐个查文件还在不在
```
实测：rc=1（只有 `MEMORY.md` 一处内容冲突），5 个「被删」的文件在合并结果树里**全部健在**。
**看到「删除/数量减少」先做三方模拟，别照着两点 diff 报警。**
