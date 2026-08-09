# 第三方依赖登记

按 AGENTS.md §4 的要求，所有第三方依赖在此登记。

## 依赖政策回顾

- 只接受**纯 Go**、License 兼容（MIT / BSD / Apache-2.0）、维护活跃的库。
- **禁止 AGPL**（本项目是网络服务，AGPL 会传染到整个服务）。
  特别地，`macos-fuse-t/go-smb2` 是 AGPL-3.0，**只可作为设计参考阅读，
  不得 import、不得复制其代码**。
- 协议栈本身（SMB1/2/3、NTLM、SPNEGO、DCERPC、mDNS/DNS-SD、NetBIOS）
  一律自实现，不依赖第三方（C5）。
- 所有依赖必须满足 `CGO_ENABLED=0` 且不引入动态库（C1 / C2）。

## 运行时依赖（进入产品二进制）

| 模块 | 版本 | License | 用途 | 备注 |
|---|---|---|---|---|
| `github.com/goccy/go-yaml` | v1.19.2 | MIT | YAML 配置解析（`internal/config`） | 选它而非 `gopkg.in/yaml.v3` 是因为它能给出**带行号的错误**，满足 §6「人话错误信息」要求 |
| `golang.org/x/net` | v0.44.0 | BSD-3-Clause | `ipv4`/`ipv6` 组播控制（`internal/mdns`） | 标准库 `net` 无法逐网卡加入组播组、无法设置组播 TTL |
| `golang.org/x/sys` | v0.47.0 | BSD-3-Clause | 纯 Go syscall：`F_FULLFSYNC`、`statx`、`fallocate`、xattr、`SO_REUSEPORT` | Go 官方准标准库，纯 Go 无 CGO |
| `go.etcd.io/bbolt` | v1.5.0 | MIT | 旁路元数据存储，两个消费方：① Windows POSIX 属主/权限位（`internal/meta`，`//go:build windows \|\| metabolt`）；② **builtin OS 能力适配器的六项能力**（`internal/oscap/builtin`，**无 build tag，全平台**） | 选它而非 SQLite 是因为 `mattn/go-sqlite3` 需要 CGO，违反 C1。②内部用 mmap + flock —— 属于平台调用约定而非外部动态库/服务，与 C9 相容，完整论证见 `internal/oscap/builtin/builtin.go` 包注释。**自 ② 起 go.mod 里不再是 `// indirect`**：`internal/oscap/builtin` 无条件 import 它，linux 视角也看得见直接引用，原先「`go mod tidy` 会在 linux 视角误删、不要 tidy」的告诫随之作废。①的 `metabolt` tag 仍仅供 Linux CI 编译/测试，非生产配置。 |

## 仅测试 / 开发用（不进产品二进制）

| 模块 | License | 用途 |
|---|---|---|
| `github.com/hirochachacha/go-smb2` | BSD-2-Clause | 验收测试的 Go SMB **客户端**（`scripts/clients/gosmb2/`，**独立 go module**，刻意不污染主模块 go.mod） |

外部测试工具（系统安装，非 Go 依赖，仅本地/CI 验收用）：
`smbclient`、`mount.cifs`、`impacket`、`samba`（仅用于采集参考抓包）。
这些**不是运行时依赖**，产品二进制不 fork/exec 它们（C3）。

## vendor 的代码

目前没有 vendor 任何第三方代码。

若将来需要 vendor，按 AGENTS.md §4 的规则：
1. 保留原始版权头与 License 全文，放在该目录的 `LICENSE` 文件；
2. 目录内写 `ORIGIN.md`：来源 URL、commit hash、License、**改了什么**；
3. 回到本文件登记一行。

## 如何核对

```sh
# 列出真正进入产品二进制的第三方包（而不是 go.mod 里列的）
go list -deps ./cmd/... ./internal/... | grep -vE '^(internal/|[a-z]+$|[a-z]+/)' 

# 校验硬性约束（C1/C3/C4/C8 + 无 AGPL）
./scripts/check-constraints.sh
```
