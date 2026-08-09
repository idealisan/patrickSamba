---
name: CNB PR API 的调用方式与四个坑
description: cnb.cool 创建/合并 PR 的 curl 写法，以及 merge 必须用 PUT、参数名是 merge_style、commit_title 必填、GET/PUT 都要求 Accept: application/json 这四个会直接报错的坑
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

**四个会直接失败的坑**（不是 GitHub 那套，别照 GitHub 的记忆写）：

| 症状 | 原因 | 正确做法 |
|---|---|---|
| `404` | 合并用了 `POST`；合并必须 `PUT`，只有创建是 `POST` | 合并用 `PUT` |
| `400` | 参数名写成 `merge_method`；CNB 用 `merge_style` | 用 `merge_style` |
| `400` | 漏了 `commit_title`；它是必填不是可选 | 必填 `commit_title` |
| `406` `{"errcode":406,"errmsg":"either of 'application/json' or 'application/vnd.cnb.api+json' content type supported"}` | **GET 和 PUT 都要求 `Accept: application/json`**，缺了就报 406。报错文案说的是 content type，极易误导你去查 `Content-Type` 头——但 `Content-Type: application/json` 明明已经带了，真正缺的是 `Accept` | 请求务必带 `-H "Accept: application/json"`（上面两段 curl 已经带了，照抄即可，别漏） |

**Why**：这四条每一条都有人真的踩过并浪费时间排查，团队要求写进
`docs/dev-workflow.md` 免得下一个人再试一遍。

**How to apply**：任何时候要在本仓库开/合 PR 直接抄上面两段；发现 4xx 先对照这张表，
不要怀疑 token 或权限。
