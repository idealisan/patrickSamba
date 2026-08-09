---
name: 配置里的路径字段按「运行平台」判定绝对性
description: isAbsPath=filepath.IsAbs 导致 example.yaml 在 Windows 上跑不起来，且 metadata_path 这个「会被忽略」的字段反而能硬拦启动
type: project
---

`internal/config/validate.go:14` 的 `isAbsPath` 直接就是 `filepath.IsAbs`，即
**当前运行平台**语义。三处调用：`shares[].path` (:266)、`shares[].metadata_path`
(:296)、`log.file` (:435)。

由此产生两个已实测/已核实的后果：

1. **`configs/example.yaml` 在 Windows 上启动不了**。Go 的 windows `IsAbs`
   （`internal/filepathlite/path_windows.go:184`）先取 `volumeNameLen`，为 0 直接
   返回 false，所以 `/srv/share/public` 在 Windows 上不算绝对路径。
   （Linux 侧实测；Windows 侧是读 Go 标准库源码得出，容器里跑不了 windows 二进制。）

2. **`metadata_path` 的校验与它「仅 Windows 生效」的定位自相矛盾**。Linux 实测三态：
   留空 → 通过；POSIX 绝对路径 → 通过 + WARN「当前平台会忽略它」；
   Windows 路径 → **硬错误、启动失败**。即一个运行时明确会被忽略的字段却能把服务
   完全拦在门外，且 `validate.go:490` 那条「会忽略它」的 WARN 在「Windows 路径 +
   Linux 运行」这个最常见组合下永远走不到（:296 的硬错误先 return）。
   后果：同一份配置没法跨平台复用。

**Why**：`configs/example.yaml` 是大多数人接触本项目的第一份文件，注释里原本写着
「填了会被忽略」而给的示例值正好是 Windows 路径 —— 照抄解开注释就启动失败，
和历史上 `addresses: [0.0.0.0, "::"]` 是同一类「开箱即坏」事故。

**How to apply**：写配置文档时，凡涉及路径字段一律说明「按运行平台判定」，
不要写「会被忽略」这种只描述运行时、不描述校验期的话。改 `internal/config` 或
Windows 后端时，这两条是待决问题（已 DM win-meta / win-vfs，改法有二：非 Windows
上降级为 WARN，或把校验挪到 `//go:build windows` 下）。
