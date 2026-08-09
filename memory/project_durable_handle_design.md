---
name: durable handle 的三条已定型设计约束
description: R12 修复后确立的三条约束（Persistent 全局唯一 / 加锁次序 / 不起常驻回收 goroutine），改 durable.go 前必读，附各自的反证
type: project
---

R12（durable handle 6 缺陷）已于 2026-08-09 由 PR #40 合入 main。修复过程中确立了
三条**已被裁定、有反证支撑**的设计约束。后续改 `internal/smb/command/durable.go`
或 `session.go` 的句柄分配时，先读这三条，不要凭直觉重开。

## 1. `Open.Persistent` 必须全进程唯一，不能是会话内计数器

**Why:** v1 durable 重连（DHnC）时客户端带回来的**就是** Persistent FileId，
服务端只能拿它当登记表键。所以唯一性**必须长在 `Open.Persistent` 本身上**——
「另起一个内部唯一键」在数学上够不着，因为键必须能从客户端回传的值反推。
会话内计数器会让每条连接的第一个句柄都是 1，两个会话撞键 → 重连时把**另一个
文件的句柄**交回去（跨会话数据泄漏，身份校验拦不住：同一用户开两条连接很正常）。
顺带这也是合规修正：MS-SMB2 §3.3.1.10 的 GlobalOpenTable 本就以 Persistent 为索引，
作用域是整个服务端。

**已否决的方案**：「把 SessionId 编进 key」——v1 重连按设计就是跨会话的，
绑死 SessionId 会让合法重连找不到自己的句柄。

**How to apply:** 分配器 `newPersistentID()`（`durable.go`）的不变量是
**返回值 ∈ [1, 0xFFFFFFFFFFFFFFFF)**。两端都不能碰：0 表示未分配；全 1 是
`wire.CompoundFileID` 的一半（复合请求「复用上一条 CREATE 的句柄」占位值，
§3.2.4.1.4，macOS 大量使用），吐出它会让 `IsCompound()` 把正常句柄误判成占位符。
当前靠「单调递增、首值为 1」天然成立；**一旦改成复用回收 ID / 时间戳 / 随机数，
必须显式排除这两个值**。`TestQAPersistentIDNeverCollidesWithCompoundSentinel` 钉着它。

`register()` 里「键被占则拒绝授予」的防线**不能撤**：v1 撞不了了，但 v2 的键是
客户端自己给的 CreateGuid，恶意客户端可以故意重复，那是唯一拦得住的东西。

## 2. 加锁次序：`o.mu → r.mu`、`s.mu → r.mu`，持 `r.mu` 时绝不再取另外两个

**Why:** `Open.close()` 内部会调 `durableTable.remove()`（取 `r.mu`）；
`Session.RemoveTree/Close` 先摘句柄再调 `disconnect()`（取 `r.mu`）。
所以在 `r.mu` 临界区里调 `o.close()` 会二次申请 `r.mu` —— sync.Mutex 不可重入，
直接自死锁。反过来，在 `s.mu` 内读 `o.Durable.Invalidated`（该字段受 `r.mu` 保护）
既是 data race 又是顺序反转。

**How to apply:** 两条具体写法，都别改：
- `reap()` / `reconnect()` 的过期分支：**锁内摘表、锁外 close**。
- `Session.RemoveTree` / `Session.Close`：**锁内只摘、锁外判定**，
  `s.mu` 内只把句柄放进切片，`disconnect()` 与 `Invalidated` 判定全在锁外。
  且 **`disconnect()` 返回 false 时必须 `close()`** —— 不然句柄既不在等待表、
  又没关闭，纯 fd 泄漏。

反过来也要注意：只写 `DurableState` 自己字段（`key` / `Invalidated`）的操作
**必须放在 `r.mu` 内**，它们不碰别的锁不会请回死锁，放锁外就是 data race
（`-race` 能稳定复现，曾表现为 `durable.go` remove vs reconnect 的 WARNING）。

## 3. 刻意不起常驻回收 goroutine（team-lead 裁定）

**Why:** 「起了个 goroutine 但没人管它生命周期」在本项目是另一类坑。

**How to apply:** 过期回收采用**机会式**：`register()`（有新句柄进来）与
`Session.Close()`（有旧连接离开）各扫一遍，覆盖了表可能变大的两个方向，
无界增长因此被堵死。**残留**：最后一批句柄断连后若再无任何 SMB 流量，
那批记录会留到下次有人连进来——数量上界＝最后一条连接的 durable 句柄数，
不随时间增长。这条残留由 `TestQADefectExpiryHappensWithoutReconnect`
（`qadefect` tag）**故意留红**记录，**是裁定结果不是遗漏**，别当缺陷统计、
更别为了让它变绿去加常驻 goroutine。日后真要消它，最低成本是给每条 entry 挂
`time.AfterFunc` 并在 reconnect/remove 时 `Stop()`（有明确宿主与销毁点的一次性
定时器，不是常驻 goroutine）。

## 4. 授予前提判的是「请求的」oplock，而我们恒授予 NONE —— 未决

**状态：2026-08-09 由 qa-proto 查出并上报 team-lead，尚未定夺，别当已修。**

`durableGrantAllowed`（`durable.go:465`）判的是 `req.RequestedOplockLevel == Batch`，
即**客户端请求的**级别；而 `create.go:167/246` 恒回 `wire.OplockLevelNone`。
于是「请求 BATCH 的客户端拿到 `OplockLevel=NONE` **外加**一个 durable 句柄」这条路是通的。

**Why 这是问题：** Samba 判的是**已授予的**。`source3/smbd/smb2_create.c:1903`
用 `state->durable_requested && (fsp_lease_type(state->result) & SMB2_LEASE_HANDLE)`，
而 `source3/locking/leases_util.c:51` 的 `fsp_lease_type()` 读 `fsp->oplock_type`
（非 LEASE_OPLOCK 时走 `map_oplock_to_lease_type()`，`NO_OPLOCK → 0`）——
即 Samba 在我们这个场景下**不授予** durable。
后果：客户端断连后句柄被扣住整个 durable timeout，却没有任何 oplock/lease 可以 break 它，
第二个客户端被 sharing-mode 挡住，且这个扣留对外不可见。

**How to apply:** 真要改，判据应从「请求的」换成「已授予的」oplock/lease。
注意别顺手把 `leaseDurableEligible` 那个 seam 一起接线 ——
它当前**无任何调用方**是**有意的**（lease 只做了 break 通道、不授予、不宣告 CAP_LEASING），
由 `TestQADurableGrantRequiresBatchOplockOnly` 正向钉着：一旦有人调了
`SetLeaseDurableEligible`，该用例第二段就会变红提示重新评估。那是提示，不是回归。
