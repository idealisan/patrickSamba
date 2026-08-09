# ORIGIN — `go.etcd.io/bbolt`（internal/meta 的底层 KV 存储）

> 说明：bbolt 是 **go module 依赖**（`go.mod` 引入），**不是 vendor 复制进
> `internal/` 的代码**。AGENTS.md §4 的 `ORIGIN.md` 本用于「vendor 复制」的场景，
> 这里沿用同一格式登记这个模块依赖及其合规核验结论（team-lead 要求）。

## 来源

- 模块：`go.etcd.io/bbolt`
- 版本：`v1.5.0`
- 上游：`https://github.com/etcd-io/bbolt`
- License 全文：`$(go env GOMODCACHE)/go.etcd.io/bbolt@v1.5.0/LICENSE` —— **MIT**
  （"The MIT License (MIT), Copyright (c) 2013 Ben Johnson"）

## 我们怎么用它

- `internal/meta` 用 bbolt 做 Windows POSIX 属主/权限位的旁路存储
  （uid/gid/mode/FileID，AGENTS.md §5 P7）。
- build tag：`//go:build windows || metabolt`。
  `metabolt` 这个 tag **仅供 Linux CI 编译/测试用，不是生产配置**——
  本包只在 Windows 上真正启用，Linux/macOS 走 noop、零开销。
- `go.mod` 里 bbolt 标着 `// indirect`：因为 build tag 的关系，
  `go mod tidy` 在 linux 视角下看不到直接引用。按 `GOOS=windows` 跑 tidy 会保留它，
  **不会被误删，但也不要为了「去掉 indirect」而 tidy 掉它**。

## 改了什么

**无。** 原样使用上游 v1.5.0，没有 fork、没有 patch、没有复制源码进仓库。

## §1.1 / AGENTS.md 合规核验结论

| 要求 | 结论 | 证据 |
|---|---|---|
| 纯 Go / 无 CGO（C1） | ✅ 通过 | `CGO_ENABLED=0 go build -tags metabolt ./internal/meta` OK；`-deps` 下 bbolt 全为纯 Go 包，无 `CgoFiles` |
| License 兼容（§4，禁 AGPL） | ✅ MIT，白名单内 | `bbolt@v1.5.0` MIT；间接依赖 `golang.org/x/sys@v0.47.0` BSD-3-Clause，均无 AGPL |
| `CGO_ENABLED=0` 四平台可编译（C7） | ✅ 通过 | 以下均 OK：<br>`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags metabolt ./internal/meta`<br>`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ...`<br>`CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 ...`<br>`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 ...` |

> 注：选 bbolt 而非 `mattn/go-sqlite3`，是因为后者需要 CGO，违反 C1；bbolt 是纯 Go
> 的嵌入式 KV，符合「单个静态二进制、零外部动态库」的硬性约束。
