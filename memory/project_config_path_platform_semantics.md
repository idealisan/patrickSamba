---
name: 配置里的路径字段按「运行平台」判定绝对性
description: isAbsPath=filepath.IsAbs 导致 example.yaml 在 Windows 上跑不起来；metadata_path 那条同源 bug 已由 PR #18 修掉，附可注入平台的做法
type: project
---

`internal/config/validate.go` 的 `isAbsPath` 直接就是 `filepath.IsAbs`，即
**当前运行平台**语义。原有三处调用：`shares[].path`、`shares[].metadata_path`、
`log.file`。由此产生两个后果，**目前一个已修、一个仍开着**。

## 1. `metadata_path` 那条 —— 已修（PR #18，分支 `win-meta/metadata-path`）

原症状：一个运行时明确「会被忽略」的字段却能把服务完全拦在门外。Windows 路径
（`C:\ProgramData\...`）在 Linux 上被 POSIX 版 `IsAbs` 判成相对路径 → 硬错误 →
`validate.go` 里那条「当前平台会忽略它」的 WARN 永远走不到（硬错误先 return）。

改法（三件事，缺一不可）：

- 非 Windows **完全跳过** `metadata_path` 校验，WARN 成为唯一出口；
- `Validate`/`Warnings` 签名不变，内部转调 `validateOn(c, hostOS)` /
  `warningsOn(c, hostOS)`，**平台是形参不是 `runtime.GOOS`**；
- 新增 `isAbsPathOn(p, windows)` 按**指定平台**语义判断。
  这一条是前一条能成立的前提：只把 `runtime.GOOS` 换成形参还不够，
  判定函数本身若仍是 `filepath.IsAbs`，在 Linux 上模拟 Windows 分支拿到的
  依然是 POSIX 语义，Windows 那半张表等于没测。

`isAbsPath` 本身**保留不动**：`share.path` / `log.file` 要在本机被真正
`os.Stat`/`OpenFile`，用本平台语义才是对的。两者的适用边界写进了注释。

## 2. `configs/example.yaml` 在 Windows 上启动不了 —— 仍未修

Go 的 windows `IsAbs` 先取 `volumeNameLen`，为 0 直接返回 false，所以
`/srv/share/public` 在 Windows 上不算绝对路径。（Linux 侧实测；Windows 侧是读
Go 标准库源码得出，容器里跑不了 windows 二进制。）这条属于 `share.path`，
**不能**照 metadata_path 的办法「跳过校验」—— 那个路径必须在本机真实存在。

### 2.1 调研结论（example.yaml 跨平台，尚未动手改产品代码）

- **(a) 一份配置无法跨平台**：绝对路径在两个平台上**没有交集**——
  `/srv/...` 在 Windows 不算绝对，`C:\...` 在 Linux 不算绝对，UNC（`\\host\share`）
  同理。所以「一份 example.yaml 在两种 OS 上都跑得起来」在数学上不可能，
  **必须提供两份示例**（如 `example.yaml` + `example-windows.yaml`），而非通用一份。
- **(b) 报错时要提示改用对应示例文件**：校验 `share.path` 在异平台被判为非绝对时，
  错误信息应指明「Windows 上请用 `example-windows.yaml`」之类，降低开箱即坏。
- **(c) 只有 `metadata_path` 一例是「仅某平台使用却在所有平台硬校验」的 bug**：
  逐字段过一遍确认，`shares[].path` / `log.file` 本就要在本机存在、`metadata_path`
  是唯一「运行时声明忽略却全平台硬校验」的字段；其余配置字段无同形态问题。
  判据是「仅某平台使用却在所有平台硬校验」即算 bug，已用 §1 三件套修掉唯一一例。

**Why**：`configs/example.yaml` 是大多数人接触本项目的第一份文件，
和历史上 `addresses: [0.0.0.0, "::"]` 是同一类「开箱即坏」事故。

**How to apply**：写配置文档时，凡涉及路径字段一律说明「按运行平台判定」，
不要写「会被忽略」这种只描述运行时、不描述校验期的话 —— 校验期和运行期对同一个
字段给出矛盾的待遇，就是这个 bug 的本质。再遇到「只在某平台生效」的配置字段，
直接照 §1 的三件套办：跳过 + 平台形参 + 平台化判定函数。
