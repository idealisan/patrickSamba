---
name: metadata 存储双实现冲突的定夺
description: vfs 与 internal/meta 两个 bbolt 旁路存储撞同一文件/桶，已定夺由 internal/meta 单一化，并暴露 -tags metabolt 的 CI 假绿洞
type: project
---

`internal/vfs/metadata_windows.go`（v0.1.0 已接线，264 行 bbolt）与 `internal/meta`（win-meta，PR #26，1611 行，bbolt+noop 双实现，`//go:build windows || metabolt`）会写**同一个库文件、同一个 bucket `posix`**，编码互不兼容，静默吐垃圾。

**定夺（win-vfs 给出方向，team-lead 已收）**：以 `internal/meta` 为唯一真源，vfs 改为依赖它，删除 vfs 自带的 bbolt 实现（`vfs.MetadataStore` 退化成 `meta.Store` 的薄适配）。

**Why**:
- 两者默认落点完全一致：`<UserConfigDir>/stupidsamba/metadata-<fnv32a(Clean(root))>.db`（vfs 用 ToLower 哈希，meta 用 Fold=ToLower，文件名相同），且都叫 bucket `posix`。
- vfs 记录 12 字节、无版本字节、无 FileID、key 不带前导 `/` 且**大小写敏感**；meta 记录 21 字节、version@0、FileID@13、key 带前导 `/` 且 Fold 小写。vfs 的 `decodeMetadata` 只查 `len<12`，把 meta 的 21 字节当合法→垃圾；meta 的 `decodeRecord` 把 vfs 首字节当 version（UID 低字节恰为 1 时判为合法 v1）→垃圾。v0.1.0 升级必中。
- meta 多了 `GetDir`（O(1) 目录枚举，Time Machine `.sparsebundle` band 目录几万文件是性能命门）和 `Reap`（孤儿回收，vfs 那份没有）；且 meta 的 `Fold` 正好修掉 vfs key 大小写敏感、在 NTFS 上潜伏查不到的 bug。把 GetDir/Reap 抄进 vfs 违反 AGENTS §4。

**How to apply（实现时）**:
- 新记录进**新 bucket**（建议 `posix2`）以隔离旧格式；`meta.Open` 启动时把旧 `posix` bucket 经**旧 12 字节无版本解码器**迁移进新 bucket（旧解码器搬进 meta 当 `migrateV010` 兼容 shim，FileID 填 0）；旧 `posix` bucket **保留不删**（回滚到 v0.1.0 仍可读到旧数据，双向不炸）。退路是「新 bucket + 直接丢旧数据」，也安全。
- vfs 侧：删 `metadata_windows.go` 的 bbolt；新建 `//go:build windows || metabolt` 适配文件，`openMetadataStore` 改调 `meta.Open`，`metaAdapter` 做 `Metadata`↔`Record` 互转并暴露 `GetDir`/`Reap`（query_directory 用类型断言按需取，取不到退回逐条 Get）；`metadata_other.go` 改 `!windows && !metabolt`。
- **CI 假绿洞（务必修）**：这两个包都是 Windows-only 编译，Linux CI 从不真正编/测（`go test ./internal/meta` 不加 `-tags metabolt` 是 noop 假绿；vfs 那份在 Linux CI 同样零编译）。CI 矩阵必须加 `go build -tags metabolt ./...` 与 `go test -tags metabolt ./internal/meta/...`，并把 vfs 适配层也标 `windows||metabolt`，让 `-tags metabolt` 在 Linux 上真编真测整条 vfs→meta 委派链。
- PR #26 维持未合，等两边对接完再合。分工：win-meta 拥有 `internal/meta`，win-vfs 拥有 vfs 适配层 + 删除 + CI。

附：另有小活已落地——`vfs.IsWindowsSlash(c)`（winpath.go）是 Windows 分隔符单一真源，config 的 `isWindowsSlash` 应改为调用它（config 已 import vfs），分支 `win-vfs/windows-slash-shared` 已推，待 win-meta 改 `config/validate.go:59` 一行。
