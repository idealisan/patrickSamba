# v0.2.0 发布通道验证清单（打 tag 前后照着跑）

> 用途：`v0.2.0` 这个 **无连字符** 的 tag 会同时走三条改过的路径 ——
> Release 正式版 stage / 跳过预发布 stage / 镜像 `:latest`。
> 本文把每一项的**判据、命令、以及已经用探针 tag 跑出来的对照值**写死，
> 避免发版当天现编命令、现想「什么样算通过」。
>
> 命令全部在 2026-08-09 19:20~19:22 CST 实跑过（对象是探针 tag `v0.0.99` /
> `v0.0.99-probe`），不是照文档抄的。

---

## 0. 前置：先合 PR #163，再打 tag

`:latest` 目前在制品库里**根本不存在**：

```
$ docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:latest
ERROR: docker.cnb.cool/finalappstore/stupidsamba:latest: not found
```

所以顺序反了不是「`:latest` 停在旧版本」，而是「**压根没有 `:latest`**」——
NAS 用户 `docker pull docker.cnb.cool/finalappstore/stupidsamba`（不带 tag）
会直接 not found。#163 必须在打 tag 之前合。

---

## 1. Release 渠道（prerelease / is_latest）

**⚠️ 必须用锚定的 grep**，否则 CHANGELOG 正文里的「prerelease」字样会被匹配出来，
连带整个 body 打印几十 KB：

```sh
cnb releases get-release-by-tag --repo finalappstore/stupidSamba --tag v0.2.0 \
  | grep -E '^  (tag_name|prerelease|is_latest):'
```

**期望**：

```
  tag_name: v0.2.0
  prerelease: false
  is_latest: true
```

**⚠️ 字段名是 `is_latest` 不是 `latest`**。API 里两个字段都有，`latest` 恒为 null，
拿它当判据会得到「永远不通过」的假阴性。

**已跑出来的双向对照**（这两格已经绿了，v0.2.0 只是复现「无连字符」那一格）：

| tag | 连字符 | prerelease | is_latest |
|---|---|---|---|
| `v0.0.99-probe` | 有 | **true** ✅ | false |
| `v0.0.99` | 无 | **false** ✅ | **true** ✅ |

---

## 2. 「预发布 stage 确实被跳过」怎么证

构建日志 API（`/-/build/logs?sourceRef=<tag>`）**只给 pipeline 级信息，没有 stage 级**，
所以 stage 是否 skip 没法直接从 API 读。但有一条**不依赖日志的可证伪判据**：

两个 Release stage 是**互斥**的（`if:` 里 `case "$CNB_BRANCH" in *-*)` 一正一反）。
若它们**都**执行了，第二个会撞上「release 已存在」而失败，或把渠道覆盖成另一档。
因此：

- `prerelease: false` **且** pipeline `success`（下面命令的 `pipelineFailCount: 0`）
  ⇒ 正式版 stage 跑了、预发布 stage 没跑。
- 反之若看到 `prerelease: true`，说明两个 stage 的 `if:` 判定反了，当场就能发现。

```sh
curl -sS -H "Authorization: Bearer $CNB_TOKEN" -H "Accept: application/json" \
  "https://api.cnb.cool/finalappstore/stupidSamba/-/build/logs?sourceRef=v0.2.0&page_size=1" \
  | python3 -c "import sys,json;d=json.load(sys.stdin)['data'][0];print(d['event'],d['status'],d['pipelineSuccessCount'],d['pipelineFailCount'],d['buildLogUrl'])"
```

**期望**：`tag_push success 1 0 <日志URL>`
（`v0.0.99` 实测：`tag_push` / `success` / `1` / `0`，耗时 151.7s）

要看 stage 逐条红绿只能开那个 `buildLogUrl` 页面人工看，API 给不了。

---

## 3. 镜像 `:latest` 与 `v0.2.0` 是不是同一份

制品库**可匿名 inspect**（实测不需要先 `docker login`）：

```sh
V=$(docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:v0.2.0 --format '{{.Manifest.Digest}}')
L=$(docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:latest --format '{{.Manifest.Digest}}')
echo "v0.2.0 = $V"; echo "latest = $L"
[ "$V" = "$L" ] && echo "✅ 同一份 manifest" || echo "❌ 不是同一份"
```

双架构：

```sh
docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:v0.2.0 \
  --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}'
```

**期望**：`linux/amd64 linux/arm64`
（`v0.0.99` 实测就是这两个，说明多架构那条链子本来就是通的）

**镜像侧的 digest 基线**（发版后应当变，别拿这两个当期望值）：

| tag | digest |
|---|---|
| `v0.0.99` | `sha256:96fbb97b791b1ac5bfeb789a328b62206e229fec976cbf7c085c9a3c2634e4e0` |
| `v0.0.99-probe` | `sha256:37b2d2e7b422b68219d92b7b4facd032006b529f5bd559b6e2bdbcea6adea06d` |
| `latest` | **不存在**（#163 合入前的基线） |

---

## 4. 镜像侧反向格子的诚实说明

「带连字符的 tag **不**推 `:latest`」这一格 **没有单独实测过**。

理由：#163 是在两个探针 tag 跑完**之后**才写的，那时的代码本来就不推 `:latest`
（上表 `latest` = not found 就是证据），所以探针跑出来的「没有 latest」
**不能**用来证明 `case` 判定生效 —— 那是「功能还不存在」，不是「判定生效」。

但它与 Release 侧那两个 stage 用的是**逐字符相同**的表达式：

```sh
case "$CNB_BRANCH" in *-*) ... ;; *) ... ;; esac
```

而 Release 侧的双向对照是**真跑过的**（第 1 节那张表）。
所以镜像侧属于「共用同一条已验证表达式」，不是「已验证」。
下一次出现带连字符的 tag（例如某个 `-rc1`）时，顺手核一下 `:latest` 的 digest
**没有**跟着变，这一格就补齐了。**在那之前不要在任何文档里写成「已验证」。**
