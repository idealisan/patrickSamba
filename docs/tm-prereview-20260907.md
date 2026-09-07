# Time Machine 代码评审（2026-09-07，真机测试前）

本文件是**首次真机 Time Machine 备份之前**做的一轮静态评审记录。目的是在真机之前
尽可能把能看出来的问题挑掉，避免把"代码问题"误判成"环境问题"浪费掉真机窗口
（真机机会稀缺：无 macOS 常备环境）。

评审方式：三路并行、不同视角各看一遍，之后**每条结论都回到代码里复核过**：

| 视角 | 范围 |
|---|---|
| AAPL 协议语义 | `aapl.go`、`create_context_aapl.go`、`query_directory.go`、`create.go` 的 AAPL 分支 |
| 持久性与刷盘 | `durable.go`、`create_context_durable.go`、`set_info.go`、FLUSH 处理、容量上报 |
| 稀疏 / 元数据 / 宣告 | `ioctl.go` 的稀疏 FSCTL、`vfs/sparse_*`、`vfs/appledouble.go`、`vfs/metadata.go`、`mdns/apple.go` |

⚠️ **本文全是静态审查结论，没有一条经过真机验证。** 凡是需要核对规范的，都明确
标注「需核对规范」，不要当成已定论。

---

## P0 —— 真机前最该处理

### 1. Time Machine UUID —— **评审初判过重，已订正为"非缺陷"**

初版评审把它列为 P0："全仓库没有 TM UUID"。核对规范后**收回这条**：

- **AAPL 响应格式里根本没有 UUID 字段。** 我们自己的 `docs/protocol-notes.md:372`
  （据 Apple 文档 Table 1-3/1-4 写的）明确列出响应 Data 就是
  CommandCode / Reserved / ReplyBitmap / ServerCaps / VolumeCaps / ModelString。
  `buildAAPLResponse` 与之一致，没有漏。
- **`_adisk._tcp` 也没有 `adUU` —— 但 Samba 同样不发。** 我拉了
  `vfs_fruit.c`（samba-team/samba master）全文 5546 行检索
  `adisk|adVN|adVF|adUU`：**零命中** —— Samba 压根不做 `_adisk` 广播，
  那部分交给 Avahi/外部注册。我们的 `mdns/apple.go` 与
  `protocol-notes.md:383-385` 的约定（`dk0=adVN=…,adVF=0x82` +
  `sys=waMa=0,adVF=0x100`）与文档一致。

结论：**这不是缺陷，先不动。** 真机若出现"每次重启把备份盘认成新盘
（重新全量 / 无法增量）"，再回来补 `adUU`（每个共享一个稳定 UUID，需持久化）。

顺带一条**支持性证据**（印证第 2 条的重要性）：`vfs_fruit.c:1360-1369` 在
`fruit:time machine = yes` 时**显式打开 durable handles**，并且要求 strict sync：

```
if (config->time_machine) {
    DBG_NOTICE("Enabling durable handles for Time Machine support on [%s]");
    lp_do_parameter(..., "durable handles", "yes");
    lp_do_parameter(..., "kernel oplocks", "no");
    lp_do_parameter(..., "kernel share modes", "no");
    if (!lp_strict_sync(...)) DBG_WARNING("Time Machine without strict sync is not recommended!");
}
```

也就是说 **durable handle 是 TM 的硬性前提**，第 2 条那处 durable × oplock 的
相互作用正好打在要害上。

### 2. `InvalidateDurable()` 生产代码从未被调用

`durable.go:90` 定义，全仓只有 `create_context_durable_test.go:290` 调过；
注释明写「在 lease break 等导致 durable 资格丧失时调用」。
oplock break 路径（`oplock_grant.go`）完全不碰 durable。

让这条变得要紧的两个事实（均已复核）：

- durable 的授予前提**就是 batch oplock**：`durable.go:469-473` 的
  `durableGrantAllowed`，而 lease 分支依赖的 `SetLeaseDurableEligible`
  **从未被调用**（全仓只有注释与一处测试提到），`leaseDurableEligible` 恒为 false。
  ⇒ **每个 durable 句柄都同时持有 batch oplock。**
- **v0.7.1 把 `server.oplocks` 默认翻成 `true`**（`CHANGELOG.md` v0.7.1 段）。
  以前默认关时这条路径几乎走不到，现在 TM 默认就会拿到 durable + batch oplock。

风险链（静默脏数据）：

```
TM 断线（durable 等待重连 60s，期间 share mode / oplock / 锁都还挂着，
        open.go:262 注释明写"等待重连的句柄不走 close"）
  → 别人打开同一文件 → 需要打破 TM 的 batch oplock
  → sendOplockBreak 从已断开的连接取不到 sender，通知发不出去
  → awaitOplockBreak 干等 oplockBreakTimeout = 30s
  → 超时按"已打破"放行（第二个客户端读到的可能是 TM 本地缓存里的旧内容）
  → 但 durable 登记仍有效，TM 重连拿回一个"可能攥着脏缓存"的旧句柄
```

### 3. 目录 FLUSH 不 fsync 却回成功

`read_write.go:375-379`：`if !open.IsDir { h.Sync(true) }`。
TM 大量建目录，目录项持久化无保障。

**需核对规范**：MS-SMB2 §3.3.5.17 与 Samba `smbd_smb2_flush` / `sync_file`
对**目录句柄**的 FLUSH 是怎么处理的。

---

## P1 —— 大概率影响首次备份

### 4. AAPL ReplyBitmap 原样回显请求位（已复核）

`aapl.go:178-179` 把 `r.RequestBitmap` 直接传进 `buildAAPLResponse`，`:269` 原样写回。
若客户端请求未知位（如 `0x8`），回复里置了该位却不提供对应字节 → 客户端解析错位。
修法是掩码到 `aaplServerCaps|aaplVolumeCaps|aaplModelInfo`。
现有 `aapl_test.go` 只测已知位，覆盖不到。

### 5. `_adisk._tcp` 的魔数未经真机验证

`mdns/apple.go:43`（`adiskPort=0`）、`:53`（`adVF=0x82`）、`:60`（`sys=waMa=0,adVF=0x100`）
全部来自 avahi 社区配方，代码自己标着「TODO: 待真实抓包验证」。
**这是首次备份「发现不了磁盘」的头号嫌疑** —— 容器内无法证伪。

### 6. macOS 上跑服务端没有原生打洞

`internal/oscap/native/native_darwin.go:35` `Sparse: nil` → 整项落
`oscap/builtin/sparse.go:71`（写零 + bbolt 记账）。语义诚实（读回是零、
`AllocatedRanges` 不再报），但**磁盘空间不下降** ⇒ 备份卷只涨不缩。
文档化且经 probe 一致判定，不是 bug，但是真机验收的已知短板。

### 7. 操作层面的坑（不改代码）

`kAAPL_SUPPORTS_FULL_SYNC` 只在 `time_machine: true` 的共享上宣告（`aapl.go:227`）。
**真机测试前先确认配置里那个共享标了 `time_machine: true`**，否则 macOS 不认它是
合法目的地。

---

## P2 —— 会误导后续排查

### 8. 一批死代码

`vfs/sparse_unix.go` / `sparse_other.go` / `sparse_windows.go` 的
`platformAllocatedRanges` / `platformSetSparse`，以及 `sys_*.go` 的
`platformPunchHole`，**都没有任何调用点**（已复核）。真实路径是
`optional.go:401 → oscap/native/sparse_linux.go:77`（或 `builtin/sparse.go:147`）。

`sparse_other.go:5-7` 声称的「非目标平台兜底」是假的 ——
**改这三份文件不会改变任何行为**。真机调试期间若误改这里，会白白烧掉排查时间。

### 9. 测试抓不到真实的刷盘失败

`vfs/sync_test.go` 自承「数据真到盘片上要断电才知道」；
`TestSyncPersistsData` 靠 `os.ReadFile` 读回验证，page cache 一致 ⇒
**把 `Sync` 改成 no-op，全量测试照样绿**。

---

## 已核对正确（免得后续重复怀疑）

- AAPL 常量与 Samba `SMB2_CRTCTX_AAPL_*` 一致（`aapl.go:58-98`）；
  **能力诚实不虚报**：`0x2` / `0x8` 故意不宣告（`:216`）、`SUPPORT_RESOLVE_ID` 永
  不置位（`:240`）。
- readdir 布局偏移正确：EaSize@64 / ShortNameLength=24@68 / Reserved@69=0 /
  rfork@70 / finder@78 / Reserved2@94 / FileId@96（`query_directory_test.go:115-186`
  逐个断言）。AAPL 扩展是塞进既有的 24 字节 ShortName 字段，**条目长度不变**。
- FLUSH 真的强制刷盘：`read_write.go:376` `h.Sync(true)` → `local_handle.go:190`
  `platformFullSync` → darwin `F_FULLFSYNC`（`sys_darwin.go:35-43`，失败退 fsync）/
  linux `fsync` / windows `FlushFileBuffers`。
- FSCTL 控制码正确（`wire/ioctl.go:74-76`）；区间裁剪做了两层、游标单调防死循环、
  只用 `SEEK_DATA/SEEK_HOLE` 符号常量（darwin 数值相反，这点做对了）。
- AppleDouble / AFP_Info **无静默丢失**：xattr 全走 oscap，无原生能力由 builtin
  （bbolt）兜底；`._` 在 readdir 中隐藏。
- 配额已修：不再是"空共享报 0"（`local.go:888-931`，且强制 `Avail<=Free<=Total`
  防倒挂）。

---

## 待核对规范（不要当成已定论）

1. TM UUID 的放置位置（AAPL 回复 / `_adisk` 的 `adUU` / 两处）。
2. 目录句柄 FLUSH 是否应当 fsync。
3. `QUERY_ALLOCATED_RANGES` 的 `Length == 0` 该不该回 `INVALID_PARAMETER`。
4. `SET_ZERO_DATA` 的 `BeyondFinalZero` 是否要求把文件扩展到该位置。
5. ModelString 的 `Length` 是字节数还是 UTF-16 码元数（`aapl.go:282` 写的是字节数，
   但 `aapl_test.go:135` 只是复述代码自己的选择，不构成外部证据）。
6. `date added` 字段用 BigEndian（`aapl.go:403`，理由是「Samba 用 RSIVAL」）未独立确认。

---

## 关联

- `docs/timemachine-status.md` —— ⚠️ 该文档正文是 **v0.1.0 时点**的定级，
  其中「oplock/lease 仍未实现」已被 v0.6.0 推翻，读的时候注意时间锚点。
- `README.md` Time Machine 章节 —— 截至 v0.7.2（含）**从未跑过一次真实备份，
  更没有做过恢复**。
