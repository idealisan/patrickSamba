---
name: push 前必须单独跑 gofmt -l .
description: check-test-compile.sh 不跑 gofmt，本地只跑它是漏网，push 会被 gate_gofmt 打红
type: feedback
---

`test/ci/check-test-compile.sh` 只做 `go vet`（含全部 build tag × 四平台），**不跑 gofmt**。
而 `.cnb.yml` 里 gofmt 是独立一关（`gate_gofmt`，`gofmt -l .` 非空即 `exit 1`）。

**Why**: 一次 enc-noop 探针 PR，本地 `check-test-compile.sh` 过了就推，结果 push 事件红——
根因不是编译错误，是手写的新测试文件 `encryption_disabled_test.go` 没过 gofmt。
`cnb pulls get-ci-logs --sn <构建号>` 取到 stage 级日志才知道是 `gofmt 检查` 阶段 261ms 挂。
`check-test-compile.sh` 这关名字像「全量校验」，实际不含格式化，给人「过了就安全」的错觉。

**How to apply**: 任何要 push / 开 PR 之前，除了 `sh test/ci/check-test-compile.sh`，
务必再跑一遍 `gofmt -l .`（应为空），两者都过才算本地自检验收。
开 PR 后看 **PR 事件**构建（不是 push 事件，`reference_cnb_ci_status.md`）；
本仓库 push 与 pull_request 跑的是不同版本 `.cnb.yml`，同一 commit 可 push 红/PR 绿。
