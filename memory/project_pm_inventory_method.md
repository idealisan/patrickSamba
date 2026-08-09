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

判读三要点（**别看到数字就报警，先分类**）：

1. **「未推送=N」要减去合并祖先**。`qa-e2e` 显示领先 29，逐条 `git merge-base --is-ancestor <c> origin/main`
   之后真正的原创提交只有 2 个，其余 27 个是已在 main 里的 merge 祖先。不筛会严重虚报。
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
