# v0.2.0 发布链路独立验证报告

- 验证者：qa（发布验证）agent
- 验证时间：2026-08-10 10:22~10:27 CST
- 工作树 / 分支：`/work/qa-v020` / `qa/v020-verify`
- 对照文档：`docs/v020-release-verification.md`
- 发布 commit：`ccc4302`（tag `v0.2.0`）
- 说明：本次为**独立验证**，未执行任何发布/推送动作。所有值均为实测。

---

## [1] Release 渠道（prerelease / is_latest / draft）

- **命令**
  ```sh
  cnb releases get-release-by-tag --repo finalappstore/stupidSamba --tag v0.2.0 \
    | grep -E '^(  )?(tag_name|prerelease|is_latest|draft):'
  ```
- **期望**：prerelease: false / is_latest: true / draft: false（tag_name: v0.2.0）
- **实际**（rc=0）：
  ```
  tag_name: v0.2.0
  draft: false
  prerelease: false
  is_latest: true
  ```
  注：任务给的 `^(tag_name|...)` 无缩进锚点会漏匹配——实际字段有 2 空格缩进；
  且必须锚定，否则 CHANGELOG 正文里的「prerelease」字样会被连带匹配、打印几十 KB body。
- **结论**：**PASS**

---

## [2] 预发布 stage 被跳过 / tag_push pipeline 成功

- **命令**
  ```sh
  curl -sS -H "Authorization: Bearer $CNB_TOKEN" -H "Accept: application/json" \
    "https://api.cnb.cool/finalappstore/stupidSamba/-/build/logs?sourceRef=v0.2.0&page_size=1" \
    | python3 -c "import sys,json;d=json.load(sys.stdin)['data'][0];print(d['event'],d['status'],d['pipelineSuccessCount'],d['pipelineFailCount'],d['buildLogUrl'])"
  ```
- **期望**：`tag_push success 1 0 <日志URL>`
- **实际**（rc=0）：
  ```
  tag_push success 1 0 https://cnb.cool/finalappstore/stupidSamba/-/build/logs/cnb-khg-1jvkl2cha
  ```
  依据文档 §2 的判据：`prerelease: false`（见 [1]）且 pipeline `success` / failCount=0
  ⇒ 正式版 stage 跑了、预发布 stage 没跑（两 stage 互斥，若都跑第二个会撞车失败）。
  stage 级逐条红绿 API 给不了，只能开 buildLogUrl 人工看，本次未做。
- **结论**：**PASS**

---

## [3] 镜像 `:latest` 与 `v0.2.0` 是否同一份 + 双架构

- **命令**
  ```sh
  V=$(docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:v0.2.0 --format '{{.Manifest.Digest}}')
  L=$(docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:latest --format '{{.Manifest.Digest}}')
  docker buildx imagetools inspect docker.cnb.cool/finalappstore/stupidsamba:v0.2.0 \
    --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}'
  ```
- **期望**：v0.2.0 digest == latest digest；平台含 linux/amd64 与 linux/arm64
- **实际**（rc=0，匿名 inspect，无需 docker login）：
  ```
  v0.2.0 = sha256:2e7c8fade9fab98b9a11e1705e8c79f3b35d7da50132797c6bc0798fb155d784
  latest = sha256:2e7c8fade9fab98b9a11e1705e8c79f3b35d7da50132797c6bc0798fb155d784
  MATCH: 同一份 manifest
  平台: linux/amd64 linux/arm64
  ```
- **结论**：**PASS**

---

## [4] 复跑本地 CI 门禁（HEAD=v0.2.0 / ccc4302）

已 `git checkout v0.2.0`，`git rev-parse HEAD` = `ccc4302987242781a10b2611aa41f980c04ba3a7`，
`git describe --tags` = `v0.2.0`（确认复跑对象正是被发布的那个 commit）。

### [4a] `test/ci/check-test-compile.sh`（CI 真跑的多平台×全tag go vet 门禁）

- **命令**：`sh test/ci/check-test-compile.sh`
- **实际**（rc=0，未用代理，本机 Go 1.25.0 缓存热，约 11s）：
  ```
  OK: build tag 清单自检通过 (TAGS=integration,smoke,metabolt,qadefect)
  >>> go vet -tags ... (GOOS=linux/amd64)
  >>> GOOS=linux GOARCH=arm64 ...
  >>> GOOS=darwin GOARCH=arm64 ...
  >>> GOOS=windows GOARCH=amd64 ...
  >>> GOOS=freebsd GOARCH=amd64 ...
  OK: 所有 .go 文件（含 _test.go、含全部 build tag、含全部目标平台）均通过类型检查
  ```
  含 `_test.go`、全部已注册 build tag、五个目标平台（四支持平台 + freebsd 编译盲区补跑）。
- **结论**：**PASS**

### [4b] `scripts/check-constraints.sh`（C1-C9 机器校验）

- **命令**：`sh scripts/check-constraints.sh`
- **实际**（rc=0）：
  ```
  OK: C1 无 CGO 依赖
  OK: C8 认证自成体系
  OK: C3 运行时无外部进程调用
  OK: C4 mDNS 无系统服务依赖
  OK: C9 操作系统仅作为文件系统与套接字提供方
  OK: 无 AGPL 依赖
  ```
- **结论**：**PASS**

---

## 总体结论

**发布链路全部达成（5/5 PASS）。** v0.2.0 在 CNB Release 为正式版（prerelease:false、
is_latest:true、draft:false）；tag_push pipeline `success 1 0`（正式版 stage 生效、
预发布 stage 未跑）；镜像 `:latest` 与 `v0.2.0` 为同一份 manifest
（`sha256:2e7c8fade9...`）且含 linux/amd64 + linux/arm64 双架构；被发布的 commit
`ccc4302` 复跑本地两道 CI 门禁（check-test-compile / check-constraints）均 rc=0，
属发布质量。未做真机 SMB 客户端往返测试与 stage 级逐条红绿人工核（超出本次范围）。
