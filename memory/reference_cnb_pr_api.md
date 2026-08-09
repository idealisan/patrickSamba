---
name: CNB PR API 的调用方式 + null 陷阱 + 判「合并会带来什么/合没合/SHA 在不在」三条命令的分工
description: cnb.cool 创建/更新/合并 PR 的写法（merge 用 PUT、更新用 PATCH 否则 404、参数名 merge_style、commit_title 必填、都要 Accept: application/json）；裸写 #N 有歧义（TaskList 任务 ID 与 PR 号两套编号空间在小号段撞车，查到的是无关对象且返回 200）；CNB 的 null 陷阱（PR 的 merged 与 Release 的 latest 恒为 null，null 表示「不回答」不是「否」）；判定改动是否进主干时两点 diff 会造出「大规模删除」幻觉、三点 diff 与祖先判定在 squash 下双双假阴性、且 is-ancestor 的 rc=1 同时代表「squash」与「还没合」两态，最终判据只有内容
type: reference
---

仓库托管在 CNB（cnb.cool），PR 走 REST API。环境变量 `$CNB_TOKEN`、`$CNB_REPO_SLUG`
（值 `finalappstore/stupidSamba`）容器里已有，**token 不要打印、不要落盘**。

## ⚠️ 先看这条：裸写 `#N` 是有歧义的，本项目有两套编号空间

**TaskList 的任务 ID 和 CNB 的 PR 号各自从 1 开始，在小号段重叠。**
写消息时一律带前缀：**`task #20`** / **`PR #171`**，不许裸写 `#20`。

实例（2026-08-09 19:56）：docs-honesty 写「我建了 #20 接管 fix-forward」，指的是
TaskList 里的 `task #20`（pending，「#159 合入后核验四处可替换块」）。pm 按引用纪律去查
`PR #20`，查到的是一个**真实存在但完全无关**的已关闭 PR（「server: durable handle v1/v2…」，
author=OCI）。pm 据此判定「编号错了」并拦下，往返两封消息才澄清。

**这个坑比「指向不存在的东西」更阴**：编号撞车时 API **正常返回 200**，你拿到一个语义自洽、
读起来也像那么回事的对象，没有任何一处报错——属于「成功回显 ≠ 事情真的发生」的变体，
这次是**查到了，但查的不是你要的那个**。

**而且它会随时间自己隐身**：等 PR 号涨过当前最大任务号，同一句 `#20` 就再也不会被误解，
坑消失但纪律没建立，下次在新一轮编号重叠时复发，且更难反应过来。所以前缀要**一直**写，
不是「等号段重叠时才写」。

判据：看到别人写的裸 `#N` 且 N 较小时，**两边都查一遍**再下结论，不要只查 PR 就断言对方错。

创建 PR（`POST`）：

```sh
curl -sS -X POST -H "Authorization: Bearer $CNB_TOKEN" -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -d '{"title":"...","head":"<分支>","base":"main","body":"..."}' \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/pulls"
```

返回体里的 `number` 就是 PR 号（是字符串，如 `"number":"5"`）。

合并 PR（`PUT`）：

```sh
curl -sS -X PUT -H "Authorization: Bearer $CNB_TOKEN" -H "Content-Type: application/json" \
  -H "Accept: application/json" -d '{"merge_style":"merge","commit_title":"..."}' \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/pulls/<号>/merge"
```

**更新已存在的 PR（改标题/正文/开关状态）= `PATCH`，不是 `PUT`**：

`PUT https://api.cnb.cool/<slug>/-/pulls/<号>` 会返回 `{"errcode":5,"errmsg":"Resource not found."}`（404），
很容易被误读成「PR 不存在」或「token 没权限」，其实只是方法错了。用 CLI 最省事：

```sh
cnb pulls patch-pull --repo finalappstore/stupidSamba --number 35 \
  --title "..." --body-file /tmp/body.md      # 也可 --body / --state open|closed
```

用 `--body-file` 传正文，避免长 Markdown 在 shell 里转义踩坑。

**`cnb pulls` 的快捷命令不接受 `--repo`/`--number`**（它们只对「当前仓库当前 PR」生效，
靠环境变量识别）。跨 PR 操作必须用全名命令：`list-pull-files`、`list-pull-commits`、
`patch-pull`、`get-pull`——而不是快捷版的 `list-files`、`list-commits`、`get`。
传了 `--repo` 给快捷命令会报 `error: unknown option '--repo'`。

查 CI 失败日志：`cnb pulls get-ci-logs --sn <构建号>`（构建号从 `scripts/ci-status.sh`
打印的 buildLogUrl 尾段取）。它会直接列出每个 stage 的成功/失败/skipped 与失败处日志，
比翻网页快，也比 build/logs 接口好用（后者拿不到 stage 级明细）。

## ⚠️ `build/logs` 的 pipeline 计数字段：`totalCount` = 定义数，不是执行数

`GET /-/build/logs?sourceRef=<ref>` 返回体在 `data` 键下（**不是顶层 list**，直接
`json.load()[i]` 会 `AttributeError: 'str' object has no attribute 'get'`）。每条记录有三个计数：

| 字段 | 含义 | 实测（2026-08-09） |
|---|---|---|
| `pipelineTotalCount` | `.cnb.yml` 里**定义**了几条 pipeline | push 与 pull_request **都是 1** |
| `pipelineSuccessCount` | 真正**执行并通过**的 | push=**0**，pull_request=**1** |
| `pipelineFailCount` | 执行并失败的 | 都是 0 |

**判「有没有真的跑门禁」只能看执行数 `success+fail`，不能看 `totalCount`。**
被 `ifModify` 跳过的 pipeline **仍计入 total**（它被定义了），但 success/fail 都是 0。
所以 `total=1 且 success=0` 的含义是「定义了门禁但本次没执行」，**不是「有一条门禁跑了」**。

这正是「报绿但什么都没跑」的数据层原相：`totalCount` 看着有 1 条，让人以为「有覆盖」，
实际执行数是 0。任何用 build/logs 判「这次改动被门禁验过没有」的脚本/夹具，
**判据必须是 `success+fail>=1`，用 `totalCount` 会永远为真（total 恒 1），验不出这个坑**。

同源：同一个 SHA 的 `push` 与 `pull_request` 两条记录执行数可以相反（push 因增量是文档被跳过、
PR 比 main 全量跑）。判「能不能合」取 **pull_request** 那条，见 ci-trigger 的
`reference_cnb_push_pr_diff_baseline.md`。

**会直接失败的坑**（不是 GitHub 那套，别照 GitHub 的记忆写）：

| 症状 | 原因 | 正确做法 |
|---|---|---|
| `404` | 合并用了 `POST`；合并必须 `PUT`，只有创建是 `POST` | 合并用 `PUT` |
| `404` `{"errcode":5}` | **更新** PR 用了 `PUT /-/pulls/<号>` | 更新用 `PATCH`（或 `cnb pulls patch-pull`） |
| `400` | 参数名写成 `merge_method`；CNB 用 `merge_style` | 用 `merge_style` |
| `400` | 漏了 `commit_title`；它是必填不是可选 | 必填 `commit_title` |
| `406` `{"errcode":406,"errmsg":"either of 'application/json' or 'application/vnd.cnb.api+json' content type supported"}` | **GET 和 PUT 都要求 `Accept: application/json`**，缺了就报 406。报错文案说的是 content type，极易误导你去查 `Content-Type` 头——但 `Content-Type: application/json` 明明已经带了，真正缺的是 `Accept` | 请求务必带 `-H "Accept: application/json"`（上面两段 curl 已经带了，照抄即可，别漏） |

> **本文档下方有专门的「null 陷阱」小节**（`merged` 与 Release 的 `latest`），
> 遇到任何 CNB 字段返回 `null` 先去看那一节：**`null` 是「我不回答」，不是「否」**。

**⚠️ 判断 PR 是否已合并：不要信 `.merged` / `.merged_at` / `.merge_commit_sha`。**
CNB 的 `GET /-/pulls/<号>` 对**已经合并**的 PR 依然返回
`state=closed, merged=null, merged_at=null, merge_commit_sha=null` ——
三个字段全空，看起来就像「被关掉但没合」。实测：PR #120 已合进 main
（main HEAD 就是 `Merge pull request #120`），API 照样报 null。
照这个字段判会得出**完全相反**的结论，属于本项目「成功回显 ≠ 事情真的发生」的镜像版
（这次是「事情发生了但回显说没有」）。**改用 git 判**，但用哪条命令要看你到底想知道什么
（见下方「三条命令的分工」表）。最常被误用的是这条：

```sh
git fetch -q origin && git merge-base --is-ancestor <你的提交> origin/main \
  && echo "该 SHA 在主干历史里" || echo "该 SHA 不在主干历史里"
```

它回答的是「**某个 SHA** 在不在主干历史」，**不等于**「这份改动有没有进主干」——
squash 之下 SHA 已经变了，见下。

顺带：拿文件内容判「改动是否进了 main」时，grep 的字符串要从**文件正文**里取，
别顺手抄 PR 标题——标题和正文常常差几个字，grep 落空会让你误判成没合。

**⚠️ 但祖先关系也不是万能的：CNB 的合并方式逐 PR 不同，squash 合并下祖先判定会假阴性。**
实测（2026-08-09，`git show --no-patch --format='%P'`）：

| 合并提交 | PR | 父提交数 | `--is-ancestor` 判定 |
|---|---|---|---|
| `e4f0f80` | #129 | **2**（真 merge） | 分支是 main 祖先 ✅ 判对 |
| `ff77acb` | #135 | **1**（squash） | 分支**不是** main 祖先 ❌ **判错** |
| `d351683` | #138 | 1 | 同上 |
| `c269b76` | #141 | 1 | 同上 |

单父的合并提交里，分支上那串 commit 的 SHA 一个都不是 main 的祖先，
`git merge-base --is-ancestor` 会回「未合并」——**而工作其实已经完整进主干了**。
这个假阴性比 `merged=null` 更危险：它会让你以为白干了，进而去重推、重做、
甚至在已经合并的分支上继续 rebase 制造重复提交。

**判定改动是否进了主干，最终判据是内容不是 SHA**：

```sh
git fetch -q origin
git diff --stat origin/main <你的分支> -- <你负责的那几个文件>   # 空 = 内容已在 main
```

`git cherry` 在这里同样不可靠：squash 把 N 个提交压成 1 个，逐个 patch-id 对不上。

先看合并提交有几个父，再决定用哪种判据：
`git show --no-patch --format='%P' <合并提交>` 输出一个 SHA = squash，两个 = 真 merge。

### 三条命令的分工（先想清楚要回答哪个问题，再挑命令）

这三条命令**回答的是三个不同的问题**，互相不能替代。本项目三条都用错过，
每次都造成了实质误判，所以按「想知道什么」排表，不再按「坑」排：

| 想知道什么 | 用什么 | 为什么别的不行 |
|---|---|---|
| **合并会带来什么改动** | `git diff A...B`（三点）+ `git merge-tree --write-tree A B` 试合 | 两点 `git diff A B` 把「分支落后于 main」也算成删除。实测 PR #139：两点显示 146 文件 **-21298**，三点是 9 文件 **+955/-0**、merge-tree rc=0 无冲突。团队差点据此判定该 PR「大规模删除他人工作」 |
| **内容是否已在 main** | 比内容：`git rev-parse main:<路径>` 与 `分支:<路径>` 的 blob hash 对比，或 `git diff --stat origin/main <分支> -- <路径>` 为空 | 三点 diff 与祖先判定在 squash 下**双双假阴性**，见下 |
| **某个 SHA 在不在主干历史** | `git merge-base --is-ancestor <sha> origin/main` | 只对**同一个 SHA** 有效。squash 之后主干上的 SHA 已经不是分支上那个了，问它等于问一个不存在的东西 |

**特别提醒：三点 diff 非空 ≠ 没合并。** 这条是本项目 2026-08-09 才补上的认知。
squash 让 main **独立引入**同一份内容，`merge-base` 不动，于是 `A...B` 仍然把那份内容
算成「B 独有」，三点 diff 非空——看着像完全没合。
它和祖先判定假阴性**是同一个根因的两种表现**：两者都建立在 ref 拓扑上，
而 squash 恰好切断了拓扑与内容的对应关系。
**判「合没合」的最终判据只有内容。** 实测 PR #139：三点 diff 9 文件 +955/-0（看着有货），
但 9 个文件的 blob hash 与 main **逐个相同**，内容早由 `a8db071` 进的主干，合并是彻底的 no-op。

**⚠️ 再补一个假阳性：`--is-ancestor` 的 `rc=1` 有两种截然不同的含义。**
表里第 3 行说它「只对同一个 SHA 有效」，那是**已经合了**的前提下的用法。
真实世界还有第三种状态：**根本还没合**。三者的 rc 是这样的：

| 真实状态 | `is-ancestor <T1> origin/main` |
|---|---|
| 真合并（提交保留） | **0** |
| squash 合入 | **1** |
| **还没合入** | **1** ← 与 squash 撞在同一个值上 |

2026-08-09 21:41 实测：`is-ancestor 524bb43 origin/main` → rc=1，但 `origin/main`
还停在 `762e335`，#171 一行都没进去。若照「1=squash」执行，就会在没有目标可 rebase 的时候
去做 `--onto`。**两态判据套在三态现实上，必然有一态被吞掉。**

**修法：先过一道内容门，确认「事情发生了没」，再判「以什么形态发生」。**

```sh
git fetch origin
# 门 1（内容判据，squash / 真合并通用，且不看标题）：那句被 PR 删掉的话还在不在 main
git show origin/main:CHANGELOG.md | grep -c '<该 PR 删掉的那句原文>'
#   1 → 还没合。停在这里，什么都别做。
#   0 → 已合入，继续门 2。
# 门 2（形态）
git merge-base --is-ancestor <PR 冻结 tip> origin/main; echo $?   # 0=真合并  1=squash
```

门 1 必须用**内容**不能用标题：CNB 的 squash 同样生成「Merge pull request #N」，
标题判据恒定假绿（见 [成功回显](project_silent_success_failures.md) 第 11 条）。
选那句话时挑**该 PR 明确删掉/新增的一句原文**，它天然就是「这个 PR 到底进没进」的指纹。

同源提醒：判「工作有没有丢」也一样不能靠 ref 关系，见
[PM 盘点法](project_pm_inventory_method.md)——`git log origin/<b>..<b>` 逐分支问会得出
41 笔「未推送」的假警，正确做法是 `git log --branches --not --remotes` 一次性问全部远端 ref，
再对剩下的候选比内容。**判「有没有丢」和判「有没有合」是同一个母题。**

## null 陷阱：API 用 `null` 表达「我不回答这个问题」，调用方却读成「否」

2026-08-09 ci-trigger 在 Release 链路实测中发现，与「已合并 PR 仍回 `merged=null`」**同型**：

| 字段 | 实测值 | 真实含义 | 读成「否」会得出 |
|---|---|---|---|
| PR `merged` | `null`（即便已合并） | 「我不回答合并状态，去看内容」 | 「没合并」→ 以为白干了 |
| Release `latest` | `null`（**恒为 null**） | 「我不回答是否最新，去看 `is_latest`」 | 「所有 Release 都不是最新版」 |
| Release `is_latest` | `true`/`false` | 这才是真判据 | — |

**共同母题**：CNB 的 REST 层遇到「它不想/不能在该端点回答」的字段，统一返回 **`null` 而不是省略或 false**。
调用方用 `"if d.get('merged')"` / `"if d.get('latest')"` 判定 → `null` 是 falsy →
**把「不知道」读成了「否」**。这与本项目反复出现的「成功回显 ≠ 事情真的发生了 / 没发生」是同一类：
回显说没有，其实发生了（PR 已合、Release 是最新），只是字段不负责告诉你。

**判据改写**：
- 判 PR 合没合 → 比内容（`git diff --stat origin/main <分支> -- <文件>` 为空 = 已合），**绝不**信 `merged` 字段。
- 判 Release 是否最新 → 看 **`is_latest`（bool）**，**绝不**看 `latest`。
- 任何字段是 `null` 时，先假设「该端点不回答」，去找它指定的替代字段，不要当 false 用。

**怎么当场认出一个「不可用字段」**（可证伪的做法，一次请求就够）：
拿一个**已知为真**的样本去问它 —— PR #120 确已合进 main、`v0.0.99` 确已发布 ——
如果它在这个样本上**仍然**回 `null`，那它就是不可用字段，此后不许出现在任何判定里。
不要只在「不确定」的样本上试，那种情况下 `null` 看着像个合理答案，你会把它当结论用。

另见 `reference_cnb_release_api.md`（Release 字段、`git:release` 的 `options` 不支持变量替换、
按 tag 名分渠道用两个互斥 stage + `if:`）。

**Why**：PR `merged=null` 与 Release `latest=null` 已各造成一次反向误判；两条并成一条规则后，
凡是遇到 CNB 返回的 `null` 字段，统一先查「它有没有指定替代字段」。

**How to apply**：写任何消费 CNB API 的代码/巡检脚本时，对 `merged`/`latest` 这类「状态」字段，
一律改用内容判据或 `is_latest`；脚本里 `d.get('x')` 之前先确认 x 不会是「null=不知道」的语义。

## 分支 tip 也会陈旧（判「工作合没合」的额外陷阱）

**禁止**用 `<分支远端 tip> --is-ancestor origin/main` 当「这份工作进了 main」的判据。
分支的 `origin/<b>` 只反映上次 fetch 的远端状态，可能落后于本地、也落后于 main 演进；
若那个 tip 正好是分支开工时从 main 拉进来的旧合并提交（例如「Merge #136」），
`--is-ancestor` 会**静默通过**——因为它本来就是 main 的祖先，与「工作是否合并」无关。
判「工作进了 main」死用内容：`git show origin/main:<该工作必删/改的文件>` 是否仍是旧样。
详见 `feedback_stale_sha_refetch_and_batch_spotcheck.md` 的「新形态」小节。

**Why**：2026-08-09 19:00 pm 自己用分支旧 tip 做祖先判定，把 Blocker②（R17，oscap 接线）
**误报成已合入 main**，而真实状态是 PR #159 还在跑 CI。代价是给最大的阻塞项发了假绿灯。

**How to apply**：任何「某分支工作进了 main 吗」的提问 → 比内容，不比分支 tip 的祖先关系。

**Why**：这些坑每一条都有人真的踩过并浪费时间排查，团队要求写进
`docs/dev-workflow.md` 免得下一个人再试一遍。

**How to apply**：任何时候要在本仓库开/合 PR 直接抄上面两段；发现 4xx 先对照这张表，
不要怀疑 token 或权限。
