---
name: CNB PR API 的调用方式与五个坑
description: cnb.cool 创建/更新/合并 PR 的写法，以及 merge 用 PUT、更新用 PATCH（PUT 会 404）、参数名是 merge_style、commit_title 必填、GET/PUT 都要求 Accept: application/json 这些会直接报错的坑
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

**Why**：这四条每一条都有人真的踩过并浪费时间排查，团队要求写进
`docs/dev-workflow.md` 免得下一个人再试一遍。

**How to apply**：任何时候要在本仓库开/合 PR 直接抄上面两段；发现 4xx 先对照这张表，
不要怀疑 token 或权限。
