---
name: 给一个「已建成但没人调用」的抽象层接线时，用什么当验收判据
description: oscap 接进 vfs 数据路径的实战结论——go list -deps 计数是最硬的机器判据；双向宿主探针缺一侧就是平凡通过；「只接最小一项」与「删掉重复实现」在同一批调用点上不可兼得
type: project
---

`internal/oscap` 于 2026-08-09（PR #159）从「port + 双适配器全套写完、31 个单测全绿、
产品代码零调用」变成真正在数据路径上跑的那一份。接线过程沉淀出三条判据/教训，
**下次给剩余四项能力（Sparse / StableFileID / CreationTime / DOSAttributes）接线时照抄**。

## 1. 最硬的机器判据是链接依赖计数，不是测试绿

```sh
go list -deps ./cmd/stupidsamba | grep -c oscap
# 接线前 = 1（只有 port 包，被 config 拉去做 ParseMode）
# 接线后 = 3（port + native + builtin）
```

**Why**：一个抽象层「有没有被真正用上」，测试全绿证明不了 —— 那些测试测的是抽象层自己。
链接依赖是编译器算出来的事实：适配器没被 import，它就**一个字节都不在发布二进制里**。
这条数字变化无法靠写测试伪造，也不需要读代码去信任谁的自述。

**How to apply**：接线类 PR 的正文首屏就贴接线前/后这两个数。评审人扫一眼就能判断
「是真接上了还是又加了一层没人调用的包装」。同源判据也适用于别的 port
（将来 `internal/oscap` 之外若再出现「造好但没接」的子系统，先问这个数）。

## 2. 双向宿主探针，缺一侧就是平凡通过

只验 `portable` 那侧「宿主 `getxattr` 读不到」是**假阳性温床**：
一个什么都不做的空实现可以平凡满足它 —— 它连写都没写。必须配上：

| 模式 | 宿主原始系统调用 | 我们的接口读回 |
|---|---|---|
| native | **必须读得到** | 必须读得到 |
| portable | **必须为空** | 必须读得到（走旁路存储） |

**Why**：这与 `encryption_required` 只测允许路径是同一个母题（见
[验证策略开关要同时测两条路径](feedback_verify_policy_switch_both_paths.md)）。
两侧都要正例，才能排除「接口根本没落盘」这种双重失败。

**How to apply**：宿主侧探针要**绕开本项目全部代码**，直接用 `golang.org/x/sys/unix`
的 `Listxattr/Getxattr/Setxattr` 问内核（本仓库落在 `internal/vfs/hostxattr_unix_test.go`）。
用自己的 API 去验自己的 API，bug 会同时蒙蔽两边。

## 3. 「只接最小一项」与「删掉重复实现」可能不可兼得

team-lead 先定「v0.2.0 只接 CapXattr，其余留 v0.3.0」，同时又要求删掉重复的
`internal/vfs/xattr_unix.go`（R11 去重）。这两条在本例中**互相矛盾**：
`stream_xattr.go` 那 4 处命名流调用原本也走 `newXattrAccessor`，
删了旧文件它们就没有落点，只能一并接到 `CapNamedStream` 上。

**Why**：范围边界应该按「**哪些调用点共用同一份旧实现**」切，不是按「几项能力」切。
按能力数切会切出一个必须保留旧实现的中间态，而两份实现操作同一批宿主状态正是 R11
（`internal/meta` × `metadata_windows.go` 静默吐垃圾）的形态。

**How to apply**：接线前先 `grep` 出旧实现的**全部**调用点并按语义分组，
再拿这份分组去和 team-lead 谈范围。清单本身就是范围谈判的依据 ——
本例中 PM 数出的 9 处（5 处通用 xattr + 4 处命名流）直接决定了范围必须是两项而不是一项。

顺带两条容易漏的：

- **快路径要单独钉一条断言**。`optional.go` 的 `appleInfoAt` 是绕过普通读写路径的
  优化分支，漏接它会导致 portable 档下 Finder 元数据依然直穿宿主 setxattr，
  而普通 xattr 那条路是通的 —— **测试全绿**。这是「接了但没接全」的典型形态。
- **配置穿透要单独测**。`filesystem_mode` 从 YAML 到运行期中间隔着装配层，
  只测 vfs 层证明不了配置生效。做法是给 `LocalFS` 开一个导出的
  `CapabilityMatrix()`，让「这个共享实际走哪一侧」从包外可见，
  再用 `go test -overlay` 把装配层改成忽略配置做变异对照（工作树零修改、零 CI 开销）。
  **变异体必须自己能编译**：把 `FilesystemMode: mode` 换成常量会留下未使用变量，
  改成 `mode, err := ...; mode = oscap.ModeAuto` 才编得过。
