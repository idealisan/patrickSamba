# 不支持稀疏的文件系统上「模拟稀疏」——社区方案调查

日期：2026-09-08
触发：`docs/tm-prereview-20260907.md:107-113`（macOS 无原生打洞 → 落 builtin 写零 →
**磁盘空间不下降，备份卷只涨不缩**）。本文只做调查与分析，**不改动任何代码**。

证据标注沿用仓库惯例：`【未查证】` 表示本次没拿到一手证据，不能当结论用。

---

## 0. 结论速览

1. **社区主流做法是「不模拟」**。Samba 在 punch hole 失败时**直接把 errno 映射成
   `NT_STATUS_NOT_SUPPORTED` 返回给客户端，一个字节的零都不写**（源码证据见 §1）。
   本项目的 builtin 写零是**比参照实现更"努力"的自选动作**，不是社区标准动作。
2. **「不写零」省的是 I/O，不是空间**。在真不支持稀疏的文件系统上，块早就在盘上了，
   不写零也拿不回来 —— 写不写零，`st_blocks` 都一样涨。
3. **想真回收空间，只有三条路**：尾部 `ftruncate`、删文件重建、改落盘表示（chunk 容器）。
   前两条覆盖面窄，第三条等于重写 VFS（§2.3）。
4. **动作建议见 §4**，性价比最高的一条是"写零前先读一遍，只补非零块"——它消掉的
   不是"不省空间"这个已知短板，而是 §3.1 那个更严重的真实回归。

---

## 1. 参照实现：Samba 到底怎么做（一手源码）

### 1.1 打洞失败 → 报错，不写零

`source3/smbd/smb2_ioctl_filesys.c` 的 `fsctl_zero_data`：

```c
	mode = VFS_FALLOCATE_FL_PUNCH_HOLE | VFS_FALLOCATE_FL_KEEP_SIZE;
	ret = SMB_VFS_FALLOCATE(fsp, mode, zdata_info.file_off, len);
	if (ret == -1)  {
		status = map_nt_error_from_unix_common(errno);
		DBG_NOTICE("zero-data fallocate(0x%x) failed: %s\n", mode, strerror(errno));
		return status;
	}
```

- 全程**没有** `pwrite` 零缓冲、没有 `ftruncate`；
- 失败即 `map_nt_error_from_unix_common(errno)`。`EOPNOTSUPP`/`ENOSYS` 典型映射到
  `NT_STATUS_NOT_SUPPORTED`【未查证：映射表原文本次未取】；
- 函数里另有注释 `/* allow regardless of whether FS supports sparse or not */` ——
  **「允许下发」不等于「替你兜底实现」**，两者是分开的两件事，本项目的 builtin 把
  它们合成了一件。

`source3/modules/vfs_default.c` 的 `vfswrap_fallocate` 同样没有写零兜底：

```c
	} else {
		/* sys_fallocate handles filtering of unsupported mode flags */
		result = sys_fallocate(fsp_get_io_fd(fsp), mode, offset, len);
	}
```

### 1.2 唯一写零的地方是「预分配」，不是「打洞」

`vfs_default.c` 的 `strict_allocate_ftruncate`：只有 `mode == 0`（`posix_fallocate`，
即**预分配**）失败且**不是** `ENOSPC` 时，才回落到 `vfs_slow_fallocate()`
（注释 `/* Write out the real space on disk. */`；该函数定义在别的文件里，本次未取到原文）。
注释原文：

> "for allocation try fallocate first. This can fail on some platforms e.g. when the
> filesystem doesn't support it and no emulation is being done by the libc (like on AIX
> with JFS1). In that case we do our own emulation. fallocate implementations can return
> ENOTSUP or EINVAL in cases like that."

也就是说：**Samba 会为「预留空间」兜底写零，但绝不会为「释放空间」兜底写零**。
本项目 `builtin/sparse.go` 恰好是反过来的：`Preallocate` 是 no-op（`:115-130`），
`PunchHole` 反而真写零（`:71-77`）。

### 1.3 QAR：非稀疏文件直接报「整段已分配」

`fsctl_qar` 里对非稀疏文件走的是：

```c
	if (!fsp->fsp_flags.is_sparse) {
		/* file is non-sparse, claim file_off->max_off is allocated */
```

与本项目「多报已分配是安全的」（`oscap/ports.go`、`builtin/sparse.go:167-169`）同一口径。

**但有一处分歧值得记录**：稀疏文件走 `fsctl_qar_seek_fill`，整个扫描体在
`#ifdef HAVE_LSEEK_HOLE_DATA` 里，平台没有 `SEEK_DATA/SEEK_HOLE` 时该函数直接返回
`NT_STATUS_NOT_SUPPORTED` —— **Samba 在查不到洞时是报错**，本项目是降级报整段已分配。
本项目的选择更保守也更符合 `ports.go` 的论证，属于有意分歧，保持不变即可。

### 1.4 顺带回答一个未决项

`docs/tm-prereview-20260907.md:167-168` 记着「`SET_ZERO_DATA` 的 `BeyondFinalZero`
是否扩展文件」存疑。Samba 引 MS-FSCC `<58>` Section 2.3.67 明确写的是
**"sets the range of bytes to zero without extending the file size"**，靠
`KEEP_SIZE` 保证，全程不调 `ftruncate`。本项目 `builtin/sparse.go:66-69` 裁到 EOF
的做法与参照实现**一致**，这一条可以结案（不必改成扩展）。

---

## 2. 社区已有方案分类

### 2.1 直接拒绝（Samba / Windows 同款）

打不了洞就回 `NOT_SUPPORTED`，让客户端自己承担后果（对 Time Machine 而言＝
bundle 只涨不缩，但语义诚实、账目对得上）。

- 优点：零实现成本、零数据风险、与参照实现一致。
- 缺点：功能缺失。是否可接受取决于「macOS TM 收到 `SET_ZERO_DATA` 的
  `NOT_SUPPORTED` 会不会报错/放弃备份」——**这一点本次没查到权威说法【未查证】**。
  但 Samba 在大量非稀疏 NAS 上承载 Time Machine 是既成事实，侧面说明客户端能容忍。

### 2.2 旁路洞图 + 拦截读写（"迷你稀疏层"）

不写零，只在旁路 KV 记 `[off,len)` 是洞，然后：

- 读：命中洞区间直接返回零；
- 写：命中洞区间就从洞图里删掉（否则会重演本项目注释里那个"少报会让客户端以为
  数据丢了"的风险，`builtin/sparse.go:12-21`）；
- 截断扩展：新区间天然是洞（POSIX 扩展出来的本来就是零）；
- 删除/改名：跟走 `Migration`。

**可行性前提：IO 必须有唯一收口。** 本项目满足——数据面只有
`local_handle.go:87 ReadAt` 与 `:106 WriteAt` 两个入口，全仓库没有
`sendfile` / `copy_file_range` 之类的旁路（已 grep 确认）。

代价（这三条是它没被社区广泛采用的真正原因）：

1. **不省空间**，只是省了写零的 I/O 和那次 `fsync`；
2. **语义分裂**：盘上那段还是**旧数据**，只是服务端读的时候返回零。宿主侧
   `rsync`/备份/另一个进程直接读这个文件，看到的是打洞前的内容；
3. **洞图丢了＝旧数据复活**：KV 损坏或丢失时，本该是零的区间会把上一次的明文
   重新暴露给 SMB 客户端。这是**信息泄露**，不是简单的不一致。相比之下"真写零"
   的坏处只是慢和占盘。

### 2.3 改落盘表示：chunk / 容器格式（唯一能真省空间）

思路：一个文件不再是一个宿主文件，而是"索引 + 若干定长 chunk"，洞＝chunk 缺失。

已知先例：**qcow2**（L1/L2 表 + refcount，未分配簇读回零）、**VHDX**（BAT 里
"payload block not present"）、**Apple 的 `.sparsebundle`**（band 目录，本身就是
这个设计的客户端版本）、**S3QL / JuiceFS** 一类对象存储文件系统（slice 缺失即洞）。

- 优点：**真的能把空间还回去**，且不依赖宿主任何可选能力；
- 代价：共享目录**不再是"一堆普通文件"**。C9 与 §1.2 没有明文禁止改落盘表示
  （`._name` 旁车、`user.DosStream.*` 已是先例），但这会波及硬链接、ADS/命名流、
  AppleDouble 旁车、宿主侧直接读写、目录枚举性能，等于把 `internal/vfs` 重做一遍。
  §1.2 已明确禁掉 FUSE/loop 那条捷径（`AGENTS.md:105`），所以只能自己实现。
- 结论：**记录，不做**。除非将来明确要支持"必须在 FAT/exFAT 上跑 Time Machine
  且必须能回收空间"这个场景。

### 2.4 只写非零块（read-before-write）

写零之前先读一遍：整块本来就是零 → **只记账不写**；否则才补写。

思路与 coreutils `cp --sparse=always`（零块不落盘）、
`fallocate --dig-holes`（找零区打洞）同源，只是方向反过来。

- 能消掉 §3.1 那个真实回归（把已有的洞填实）；
- 不省空间，且每次 punch 多一次读。但相比当前"无脑写全零 + 每次 fsync"，
  在 Time Machine 这种大量重复回收同一批 band 的场景下大概率是净赚。

### 2.5 尾部 `ftruncate`（真回收，零风险）

若洞区间末端 ≥ EOF，直接 `ftruncate(off)`：**空间真的还回去了**，且不需要任何
宿主可选能力，不引入任何新的语义风险。

覆盖面窄（Time Machine band 里的洞多在中间），但属于白捡，且可以和上面任何
方案叠加。

### 2.6 运维侧缓解：把共享放在压缩卷上

ZFS `compression=lz4` / btrfs `compress=zstd` 上，全零块压缩后几乎不占盘。
这不改代码，把"写零"的代价从"占空间"降级为"花一点 CPU 和 IO"。

注意一个交互：这类文件系统**能不能 punch hole 决定走 native 还是 builtin**，
而能力判定是"整项打包"的（§3.2），所以实际落到哪条路径要实测。
【未查证：OpenZFS 各版本对 `FALLOC_FL_PUNCH_HOLE` 的支持情况本次没查到一手来源，
不要在文档里写死结论。】

---

## 3. 对本项目现状的诊断

按严重度排序：

### 3.1 【真回归】builtin 会把"本来就是洞"的区间填实

`builtin/sparse.go:71-77` 无条件写零。于是**支持稀疏写、但 punch hole 不可用**的
文件系统上（探测判据就是"能不能打洞"，`probe_linux.go:51-53` 点名 tmpfs；
macOS 上 `probe_darwin.go:21-31` 恒 false，APFS 同理），客户端对一个本来是洞的
区间发 `SET_ZERO_DATA`，结果不是"没释放"，而是**凭空多占一大块**。
一个逻辑 1 GiB 的稀疏 band 在 tmpfs 上会因此瞬间吃掉 1 GiB 内存。

这比"回收没效果"严重得多：**它把一次回收操作变成了放大操作**。

### 3.2 【设计副作用】能力粒度把"查询"和"打洞"绑死了

`capability.go:5-9` 定下"稀疏四项要么全 native 要么全 builtin"。于是"能查洞但
打不了洞"的文件系统连 native 的 `SEEK_HOLE` 查询也一起丢掉，只能用 builtin 的
**回读校验**（`builtin/sparse.go:186-216`，代价 O(打过的洞的字节数)，每次查询重读）。

这多少违背了"降级粒度＝能力粒度"的初衷：颗粒度还是太粗了。
是否把 Sparse 拆成"查询"与"打洞"两个半项，值得单开一个议题（不在本文范围）。

### 3.3 【已知短板】不省空间

`docs/tm-prereview-20260907.md:112` 已记录。**注意：§2.2/§2.4 都治不了这一条**，
只有 §2.3/§2.5/§2.6 能。别把"改成不写零"当成"解决只涨不缩"。

### 3.4 【账目】配额与统计口径

`usage.go:52` 按 `st_blocks×512` 统计，看到的是涨了的那份；而 builtin 的
`AllocatedRanges` 因回读是零会把这段报成"未分配"——**客户端以为释放了，盘上并没有**。
`quota_bytes` 只上报不拦截，写入前无校验，一路写到 ENOSPC 才停。

---

## 4. 建议（按性价比排序）

| # | 动作 | 解决什么 | 代价 | 处置 |
|---|---|---|---|---|
| 1 | `PunchHole` 写零前先读，整块已零则只记账不写（§2.4） | §3.1 填实回归 | 每次 punch 多一次读；改动集中在 `builtin/sparse.go:81` | **已落地**（2026-09-08）→ `zeroNonZero` / `nonZeroBlocks` |
| 2 | 尾部洞走 `ftruncate` 真回收（§2.5） | §3.3 的一部分 | 几行；无新风险 | **已落地**（2026-09-08）→ `reclaimTail` |
| 3 | 评估把 builtin `PunchHole` 改成 `ErrNotSupported`（§2.1，Samba 口径） | §3.3 + §3.4 账目一致性 | 需先回答 §5 的客户端行为问题；可能让 TM 直接放弃回收 |
| 4 | 拆分"查询/打洞"能力粒度（§3.2） | native QAR 白白丢失 | 动 `oscap` 矩阵，需要新能力项 + builtin 实现（§1.2 不许赊账） |
| 5 | chunk 容器（§2.3） | 真·省空间 | 重写 VFS，**不建议** |

第 1、2 条已合并为一次改动落地（见上表），第 3 条是**决策**不是实现，卡在 §5 的未查证项上。

### 已落地的两条（2026-09-08）

- `zeroNonZero`（`internal/oscap/builtin/sparse.go`）：按 4 KiB、文件绝对偏移对齐
  逐块判零，**只写含非零字节的块**。整段本来就是零时连写句柄都不开、fsync 也省掉。
  判零边界与 `AllocatedRanges` 的回读校验（appendZeroBlocks）互为补集，由
  `TestPortableNonZeroBlocksIsComplementOfAppendZeroBlocks` 钉住。
- `reclaimTail`（同文件）：打洞区间顶到 EOF 时，`ftruncate(off)` 释放块再
  `ftruncate(原长度)` 还原；还原时取 `max(原长度, 当前长度)`，避免剪掉并发写入的
  尾巴；`ftruncate` 不能扩展的宿主（Samba 也踩过：Linux 上 fat）退化为在末尾写
  一个零字节顶回长度。失败则回落到写零路径 —— 写零本身会把长度顶回去，不会留下
  一个变短的文件。

两条用例都做过**变异自检**：把行为退回旧实现后，
`TestPortablePunchHoleLeavesExistingHoleAlone` 报「块数 8 → 16」（填实），
`TestPortablePunchHoleReclaimsTail` 报「块数 64 → 64」（没回收）——
说明它们不是空测试。全量 `go test ./...` 与 `scripts/check-constraints.sh` 均通过。

---

## 5. 待验证清单（动手前先补的坑）

1. **macOS Time Machine 收到 `FSCTL_SET_ZERO_DATA` = `STATUS_NOT_SUPPORTED` 会怎样**
   —— 忽略？记日志？整个备份失败？决定第 3 条能不能做。【未查证，最高优先级】
2. `map_nt_error_from_unix_common(EOPNOTSUPP)` 的实际映射值（§1.1）。【未查证】
3. `vfs_slow_fallocate` 的定义（确认它就是写零循环）。【未查证，低价值】
4. OpenZFS / btrfs 对 `FALLOC_FL_PUNCH_HOLE` 的支持现状，以及在其上本项目的探测
   实际落到哪一侧（§2.6）。【未查证】
5. 真实场景下"重复打洞同一区间"占比有多高 —— 决定第 1 条是净赚还是净亏。
   可用现有 TM 用例在 tmpfs 上实测 `st_blocks` 变化，成本很低。
