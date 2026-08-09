---
name: CNB Release API 与 tag_push 发布链路的实测事实
description: 查 Release 用 /-/releases/tags/<tag>，判渠道看 prerelease 与 is_latest（latest 字段恒为 null，照它判必错）；git:release 是内置任务、options 不支持变量替换，要按 tag 名分渠道得用两个互斥 stage + if:；发布流水线实测 150~176 秒 / 0.16~0.19 核时
type: reference
---

2026-08-09 用两个一次性 tag（`v0.0.99-probe` / `v0.0.99`）真跑出来的，不是读文档推的。
另见 `reference_cnb_ci_status.md`（查构建状态）与 `reference_cnb_pr_api.md`（PR 读写）。

## 查 Release：字段名有坑

```sh
curl -sS -H "Authorization: Bearer $CNB_TOKEN" -H "Accept: application/json" \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/releases/tags/<tag>" \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print({k:d.get(k) for k in ('tag_name','prerelease','is_latest','draft')})"
```

- **判「是不是最新版」看 `is_latest`（bool），不是 `latest`。** `latest` 字段实测**恒为 `null`**，
  照它判会得出「所有 Release 都不是最新版」的错误结论。这是 `reference_cnb_pr_api.md` 里
  「已合并的 PR 仍回 `merged=null`」的同型坑：**回显说没有，其实发生了。**
- `prerelease`（bool）、`draft`（bool）、`assets[]`（附件列表，`.name` 可用来核对四平台包是否齐）都正常可读。

## `git:release` 的 `options` **不支持**变量替换 —— 要分渠道就用两个互斥 stage

CNB 文档只对**插件任务的 `settings`** 明文声明支持 `$VAR` / `${VAR}` 替换；
`git:release` 是**内置任务**（`type:` + `options:`），`options` **没有**这条声明。
往 `options` 里塞 `$VAR` 是赌未写进文档的行为，赌输的代价是发出一个渠道错误的 Release。

可用的正式语法是 **stage 级 `if:`**（一段 shell，退出码为 0 才执行该 stage），实测有效：

```yaml
- name: 创建 Release（正式版）
  if: |
    case "$CNB_BRANCH" in *-*) exit 1 ;; *) exit 0 ;; esac
  type: git:release
  options: { descriptionFromFile: CHANGELOG.md, preRelease: false, latest: true }
- name: 创建 Release（预发布）
  if: |
    case "$CNB_BRANCH" in *-*) exit 0 ;; *) exit 1 ;; esac
  type: git:release
  options: { descriptionFromFile: CHANGELOG.md, preRelease: true, latest: false }
```

实测（两条 tag，结论互为镜像）：

| tag | 「正式版」stage | 「预发布」stage | `prerelease` | `is_latest` |
|---|---|---|---|---|
| `v0.0.99-probe` | skipped | success | True | False |
| `v0.0.99` | success | skipped | False | True |

**顺带白送的可观测性**：`if:` 哪天失效，两个 stage 会**同时** success（而不是一个 skipped），
一眼可辨，不需要额外加探测 stage。

⚠️ **`if` / `ifModify` / `ifNewBranch` 三者是 OR**（有一个满足就执行）。
想表达「既要 A 又要 B」不能靠并排写这三个字段，那样只会互相放宽。

## 发布链路的真实代价：比想象便宜，可以真跑

整条 `tag_push`（13 关门禁 + 四平台构建 + 多架构镜像 push + `git:release` + 附件上传）
实测 **150~176 秒 / 0.16~0.19 核时**，只比普通 push 门禁（约 130 秒 / 0.14）贵一点点。

**所以「发布路径太贵、不好验证」是个错觉。** 在 160 核时/月的额度下，
用一次性 tag 做端到端验证完全付得起，不要因为怕贵就只验一半路径。
（省额度的正确姿势见 `feedback_ci_quota_frugality.md`：能本地 overlay 的别开分支，
但**本地做不到的验证不许省**。）

## 两条 tag 相关的操作事实

- **`tag_push` 下 `$CNB_BRANCH` 就是 tag 名**（不是分支名），`build-release.sh` 与
  `docker-build.sh --tag` 都靠它取版本号。
- **远端已存在的 tag 再推会被拒绝**，不会产生第二条流水线 —— 这是一个**天然互斥锁**，
  怀疑有重复实例在并行干同一件事时，可以靠它防重复触发（推之前先
  `git ls-remote --tags origin "<pattern>"` 看一眼）。
- `git:release` 默认 `overlying=false`（同名 Release 先删后建），所以重跑流水线是自洽的：
  附件会被清掉再重传。
