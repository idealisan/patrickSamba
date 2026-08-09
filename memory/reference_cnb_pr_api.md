---
name: CNB PR API 的调用方式与七个坑
description: cnb.cool 创建/更新/合并 PR 的写法，以及 merge 用 PUT、更新用 PATCH（PUT 会 404）、参数名是 merge_style、commit_title 必填、GET/PUT 都要求 Accept: application/json、已合并的 PR 仍回 merged=null，且 squash 合并下祖先关系判定也会假阴性（要比内容）
type: reference
---

仓库托管在 CNB（cnb.cool），PR 走 REST API。环境变量 `$CNB_TOKEN`、`$CNB_REPO_SLUG`
（值 `finalappstore/stupidSamba`）容器里已有，**token 不要打印、不要落盘**。

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

**会直接失败的坑**（不是 GitHub 那套，别照 GitHub 的记忆写）：

| 症状 | 原因 | 正确做法 |
|---|---|---|
| `404` | 合并用了 `POST`；合并必须 `PUT`，只有创建是 `POST` | 合并用 `PUT` |
| `404` `{"errcode":5}` | **更新** PR 用了 `PUT /-/pulls/<号>` | 更新用 `PATCH`（或 `cnb pulls patch-pull`） |
| `400` | 参数名写成 `merge_method`；CNB 用 `merge_style` | 用 `merge_style` |
| `400` | 漏了 `commit_title`；它是必填不是可选 | 必填 `commit_title` |
| `406` `{"errcode":406,"errmsg":"either of 'application/json' or 'application/vnd.cnb.api+json' content type supported"}` | **GET 和 PUT 都要求 `Accept: application/json`**，缺了就报 406。报错文案说的是 content type，极易误导你去查 `Content-Type` 头——但 `Content-Type: application/json` 明明已经带了，真正缺的是 `Accept` | 请求务必带 `-H "Accept: application/json"`（上面两段 curl 已经带了，照抄即可，别漏） |

**⚠️ 判断 PR 是否已合并：不要信 `.merged` / `.merged_at` / `.merge_commit_sha`。**
CNB 的 `GET /-/pulls/<号>` 对**已经合并**的 PR 依然返回
`state=closed, merged=null, merged_at=null, merge_commit_sha=null` ——
三个字段全空，看起来就像「被关掉但没合」。实测：PR #120 已合进 main
（main HEAD 就是 `Merge pull request #120`），API 照样报 null。
照这个字段判会得出**完全相反**的结论，属于本项目「成功回显 ≠ 事情真的发生」的镜像版
（这次是「事情发生了但回显说没有」）。**一律用 git 祖先关系判**：

```sh
git fetch -q origin && git merge-base --is-ancestor <你的提交> origin/main \
  && echo "已合并" || echo "未合并"
```

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

**Why**：这些坑每一条都有人真的踩过并浪费时间排查，团队要求写进
`docs/dev-workflow.md` 免得下一个人再试一遍。

**How to apply**：任何时候要在本仓库开/合 PR 直接抄上面两段；发现 4xx 先对照这张表，
不要怀疑 token 或权限。
