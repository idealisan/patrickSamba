# bh3 排查报告 —— DOS 属性 + 时间戳语义分歧（对照 Samba 源码）

日期：2026-08-25 18:07–18:25 CST
我方代码：/work/bh3（只读）｜参考：/work/ref/samba（source3）
格式：`[严重度][置信] 标题 | 我方证据 | Samba 证据 | 客户端可见后果 | 修复方向`

---

## 发现

### [高][高] F1: CREATE 请求携带的 FileAttributes 被整体丢弃，初始 ARCHIVE 也不入库
- **我方证据**：`internal/vfs/fs.go:198` 定义 `OpenRequest.FileAttributes`；`internal/smb/command/create.go:109` 把 `req.FileAttributes` 填进请求；但 `internal/vfs/local.go` 的 `Open/openFile/openDir`（243–434 行）与 `Mkdir`（633 行起）**没有任何一处读取 `req.FileAttributes`**（grep 全包仅定义与赋值两个点）。创建成功后也没有 `SetAttr/SetDOSAttributes/SetCreationTime` 调用。
- **Samba 证据**：`source3/smbd/smb2_create.c:182/151/1345` 把 `in_file_attributes` 传入 `SMB_VFS_CREATE_FILE`；`source3/smbd/open.c:3891–3899`：静默剥掉 `FILE_ATTRIBUTE_DIRECTORY`（注释：Windows 同款行为），并叠加 `FILE_ATTRIBUTE_ARCHIVE`（"this mode is only used if the file is created new"），经 `unix_mode`/`file_set_dosmode` 最终由 `set_ea_dos_attribute`（dosmode.c:442）持久进 `user.DOSATTRIB` xattr。
- **客户端可见后果**：`CREATE … FileAttributes=HIDDEN` 之后立即 QUERY_INFO 回不到 HIDDEN；Explorer「新建→带属性创建」退化为两步；overwrite（TRUNCATE_EXISTING）时 Samba 还会补 ARCHIVE（open.c:3803），我们保持旧值。
- **修复方向**：`ActionCreated/ActionOverwritten` 分支里把 `req.FileAttributes &^ DIRECTORY | ARCHIVE` 经 `caps.DOS().SetDOSAttributes` 落库；顺带覆盖 overwrite 补 ARCHIVE 的差异。

### [高][高] F2: 新建对象从不写 btime 记录 —— builtin/times.go:16 的注释描述的行为并不存在
- **我方证据**：`SetCreationTime` 全仓唯一产品调用点是 `internal/vfs/local_handle.go:249`（SET_INFO 的 AttrCreateTime 路径）。`openFile/openDir/Mkdir` 创建成功后均未调用。而 `internal/oscap/builtin/times.go:16` 明文写着「创建时间由 vfs 在**真正创建对象**时调 SetCreationTime 落下来」——这是假话，读路径（`creationTimeAt`, local.go:529）因此只能回落 `statCreateTime`。
- **Samba 证据**：新文件 btime 来自内核（birthtime 或 calc_create_time_stat，system.c:194–213）；且任何一次 `file_set_dosmode → set_ea_dos_attribute` 都会把当前 btime **一并钉进 xattr**（dosmode.c:462–476，version 5 固定带 `XATTR_DOSINFO_CREATE_TIME`）。
- **客户端可见后果**：Linux 上 statx 不可用/btime 缺失的文件系统（tmpfs 老实现等）+ portable 档下，新文件创建时间=ctime 且永不持久；macOS Finder/TM 看到的 btime 会随 chmod 跳动。同时该注释会误导后续维护者以为已接线（本项目最忌讳的「文档先行于事实」形态）。
- **修复方向**：`ActionCreated` 时 `SetCreationTime(now)`；改注释或在 AGENTS.md 记录缺口。

### [高][高] F3: rename/remove 不迁移不清除 bucketTimes/bucketDOS —— 路径复用会继承陌生文件的创建时间与 DOS 属性
- **我方证据**：`internal/vfs/local.go:654 Rename` 收尾只调 `l.renameMetadata(src,dst)`（880 行），而它只迁 `l.meta`（POSIX 元数据库，**Linux/macOS 恒为 nil**，metadata.go:11）；`Remove`(616)/`Close` deleteOnClose(561–569) 只 `forgetMetadata`（同样只清 l.meta）。oscap builtin 的 `bucketTimes`("btime") / `bucketDOS`("dosattr") 按 `pathKey(ref.Path)`（store.go:41–42, 283–293）存取，store.go **没有提供任何按前缀迁移/删除的 API**，六项能力共用此缺陷面（xattr/stream/holes 同理，本报告只认领 times/dos 两桶）。
- **Samba 证据**：btime 与 DOS 属性存在 inode 的 `user.DOSATTRIB` xattr 里（dosmode.c:442–543），`rename(2)` 天然跟随文件走，unlink 随 inode 消失，不存在「按路径记账」的陈旧记录问题。
- **客户端可见后果**：
  1. 改名后客户端设置的 CreationTime/HIDDEN/READONLY「丢」（回退 ctime/重新合成）；
  2. **更糟**：A 文件改名去 B 后，其旧路径 P 上的 btime/dosattr 记录仍在；此后任何人**新建** P 路径文件，`creationTimeAt(P)`/`mergeStoredDOS(P)` 会把 A 的创建时间和 HIDDEN/READONLY 位安到这个毫不相干的新文件上（跨对象元数据泄漏）；
  3. `setRename`（set_info.go:330）只更新 `open.Path`，底层 `localHandle.host/name` 仍是旧值——改名后同一句柄 QUERY_INFO 继续拿**旧路径**当 key 读记录、按**旧名**判 HIDDEN。
- **修复方向**：builtin store 增加 `RenamePrefix/DeletePrefix`；vfs 的 Rename/Remove/Close(deleteOnClose) 对 caps 各桶同步迁移/清理；setRename 成功后刷新 handle 的 host/rel/name。

### [高][高] F4: READONLY 位没有任何强制面 —— 设了只读照样可写、可 DELETE_ON_CLOSE
- **我方证据**：
  - 写路径 `localHandle.checkWritable`（local_handle.go:125–141）只查共享只读/isDir/f==nil/句柄 writable 标志，不看 DOS READONLY；
  - `setDisposition`（set_info.go:249–271）置 delete-on-close 只查 `GrantedAccess&Delete` 和目录非空；
  - CREATE 带 `FILE_DELETE_ON_CLOSE`（create.go:170–179）也只查 `RequireWritable()`；
  - 唯一消费 READONLY 的地方是 MxAc create context（create_context_mxac.go:126，向客户端**报告**剥掉写位）；
  - 且 POSIX 上 `caps.DOS()` 由 builtin 兑现（native 侧只有 dos_windows.go），`SetDOSAttributes` 只是往 bbolt 记一个 uint32——`setDOSAttributesPlatform` 的 chmod 只在 ErrNotSupported 回落时执行（local_handle.go:286–327），builtin 路径**根本不会收走宿主写权限位**。
- **Samba 证据**：
  - 打开即拒：`open.c:451–456` 带 `FILE_WRITE_DATA|FILE_APPEND_DATA` 打开 READONLY 文件 → `ACCESS_DENIED`；`open.c:4160–4166` 同款检查；
  - 访问掩码裁剪：`open.c:3497–3510`（引 MS-FSA 2.1.5.1.2.1）对 READONLY 常规文件剥 `FILE_WRITE_DATA/APPEND_DATA/ADD_SUBDIRECTORY/DELETE_CHILD`；
  - 删除拒绝：`file_access.c:192–224` `can_set_delete_on_close` 对 READONLY 回 `NT_STATUS_CANNOT_DELETE`（`delete readonly` 默认 no，docs-xml/smbdotconf/misc/deletereadonly.xml）；CREATE 与 SET_INFO disposition 两条路都会走到它（open.c:4191/4505/5458，smb2_trans2.c:4167）。
- **客户端可见后果**：「设为只读」后文件照常被写入、被删除——Explorer/备份软件依赖的只读保护完全失效；且 MxAc 已经告诉客户端「你没有写权限」，实际却放行，语义自相矛盾。
- **修复方向**：CREATE 打开已存在文件时若 `(合成|存储) READOFFLY && 申请写访问` → ACCESS_DENIED；`setDisposition`/DeleteOnClose 两处查 READONLY 回 `STATUS_CANNOT_DELETE`。

### [中][高] F5: 合成方向相反 —— 我们永远叠加 POSIX 推导位，Samba 默认「存储值优先」
- **我方证据**：`internal/vfs/attr.go:79–111` `dosAttributes` **无条件**从 perm（属主无 w → READONLY）、点开头（→ HIDDEN）、alloc<size（→ SPARSE）合成 base；`local.go:560–566` `mergeStoredDOS` 再把存储值 **OR** 进来。OR 语义意味着存储记录**无法清除**任何推导位：宿主 chmod 0444 的文件客户端永远清不掉 READONLY（SET_INFO 清了，下次读又被合成回来）。
- **Samba 证据**：`fdos_mode`（dosmode.c:710–748）：result 先从 VFS xattr 取（`fget_ea_dos_attribute`），**只有** VFS 回 NOT_IMPLEMENTED（即 `store dos attributes=no`）才走 `dos_mode_from_sbuf` 的 map_readonly/map_archive/map_system/map_hidden 推导（dosmode.c:190–247）；而 loadparm.c:3218 默认 `"store dos attributes"="yes"` —— 即默认 Samba 下权限位推导**根本不参与读路径**。唯一恒加的名字派生位是 hide_dot_files 的 HIDDEN（dos_mode_post → dos_mode_from_name，dosmode.c:594–608），且只在尚未 hidden 时追加。
- **客户端可见后果**：与默认 Samba 相比我方多报 READONLY/SPARSE；某些宿主权限组合下客户端的「清除只读」操作静默失效；auto/portable 两档在这套合成逻辑上行为一致（差异只在记录来源），但都偏离 Samba。
- **修复方向**：有存储记录时以记录为权威（保留 DIRECTORY 强制 + dot-HIDDEN 追加以对齐 hide_dot_files），无记录时才做权限推导；或至少让 merge 支持「显式清除」标记而非 OR。

### [中][中] F6: 显式设置 write time 后缺 sticky/pending 语义，后续写会冲掉所设值
- **我方证据**：`set_info.go:156–159` 直接 `h.SetAttr` → `setTimes`（local.go:893）`os.Chtimes` 落盘；此后任何 WriteAt 让内核自然更新 mtime，无任何「冻结」机制。
- **Samba 证据**：`smb2_trans2.c:3888–3902` `setting_write_time` 时调 `set_sticky_write_time_fsp`（dosmode.c:1274–1285，置 `write_time_forced`）；`fileio.c:135/174/198` 在后续写/close 时跳过自然更新（bug #2045："kept sticky even after a write"）。
- **客户端可见后果**：「先 SET_INFO 设时间、后写数据」的客户端（部分复制/同步工具）再查询发现 mtime 不是自己设的值。Windows 服务端同为 sticky 行为。
- **修复方向**：Open 上记 `writeTimeForced` + 所设值；WriteAt/Close 时跳过或回写所设值（POSIX 内核必然更新 mtime，只能在 close/flush 前补偿回写）。

### [低][高] F7: btime 回退值口径不同
- **我方证据**：Linux 回退链 = statx STATX_BTIME → 失败则保留 `fillSysAttr` 填的 **ctime**（attr_linux.go:52–54 `CreateTime = ChangeTime`；statCreateTime 同文件底部）。
- **Samba 证据**：system.c:131–150 `calc_create_time_stat` = **MIN(ctime, mtime, atime)**（atime 异常为零时退 MIN(ctime,mtime)）。
- **后果**：atime 早于 ctime 的文件（rsync/cp -p 保时拷贝）两边报的创建时间不同。影响小，仅一致性观感。
- **修复方向**：统一成 min(ctim,mtim[,atim]) 或明确记录取舍。

### [低][高] F8: SET_INFO 的 FileAttributes 未滤客观位即入库
- **我方证据**：`set_info.go:164–167` 把 `info.FileAttributes` 原样塞进 Attr；`local_handle.go:260–262` 原样 `SetDOSAttributes` 落 bbolt——DIRECTORY/SPARSE/REPARSE 等「文件系统客观事实」也被存进记录（读路径 OR 回来暂时无害）。
- **Samba 证据**：`smb_set_file_dosmode`（smb2_trans2.c:3906–3914）目录强制加 DIRECTORY、非目录剥掉；`file_set_dosmode`（dosmode.c:953）先 `& SAMBA_ATTRIBUTES_MASK` 过滤合法集合；parse 侧也单独处理 SPARSE/REPARSE「valid on get but not on set」（dosmode.c:377–380）。
- **后果**：当下无害；一旦按 F5 改成 stored-wins 语义，记录里的假 DIRECTORY/SPARSE 会直接冒出来。
- **修复方向**：入库前 `attrs &= settableDOSAttributes`（attr.go:39 已有现成常量）。

---

## 查过没问题的（单列）

| # | 项目 | 我方证据 | Samba 证据 |
|---|---|---|---|
| OK1 | **时间戳特殊值 0/-1 三分语义** | time.go:33–48：`FiletimeUnspecified=0`、`FiletimeNoChange=-1` 都不动，其余真设（set_info.go:148–163） | time.h:57–67 `NTTIME_OMIT=0`、`NTTIME_FREEZE=-1`、`NTTIME_THAW=-2`；time.c:1096–1110 三者一律映射 omit；torture/smb2/timestamps.c:412–449 断言 FREEZE 设置后时间戳不变。MS-FSA 的 freeze/thaw（停自动更新）**双方都没实现**（Samba 注释明说 "Samba doesn't implement this yet"），行为等价 |
| OK2 | **FileAttributes=0 表示不改** | set_info.go:164 `!= 0` 才进 mask | smb2_trans2.c:3925–3928 `if (dosmode == 0) return NT_STATUS_OK` |
| OK3 | **ChangeTime 字段被忽略** | SetAttr（local_handle.go:212–278）无 AttrChangeTime 分支，静默忽略 | smb2_trans2.c:4646 pull 了 ft.ctime 但 file_ntimes 只落 atime/mtime（POSIX 无接口） |
| OK4 | **目录 DIRECTORY 位强制** | attr.go:86–91 `mode.IsDir() → \|DIRECTORY`，与文件推导同函数、口径一致 | dos_mode_post（dosmode.c:662–666）S_ISDIR 强制加（reparse 除外） |
| OK5 | **普通文件兜底 ARCHIVE** | attr.go:107–109 out==0 → ARCHIVE，新建文件恰好等效 Samba 的 create 强加 ARCHIVE | open.c:3893–3899（但 overwrite 场景不等效，归入 F1 备注） |
| OK6 | **mtime 写入/回读精度** | setTimes 用 os.Chtimes（ns），QUERY 经 TimeToFiletime（100ns 向下取整，time.go:74–93），逐秒往返一致 | round_timespec 按 conn->ts_res 取整（smb2_trans2.c:3846–3849），同类粒度处理 |
| OK7 | **wire 编解码** | wire/query_info.go:171–189 FileBasicInfoSize=40、兼容 36 短形式 | smb2_trans2.c:4631 total_data<36 拒绝 |

## 严重度统计

| 严重度 | 数量 | 条目 |
|---|---|---|
| 高 | 4 | F1 CREATE 属性丢弃 / F2 btime 从不落库 / F3 rename-remove 记录泄漏 / F4 READONLY 零强制 |
| 中 | 2 | F5 合成方向相反 / F6 缺 sticky write time |
| 低 | 2 | F7 btime 回退口径 / F8 客观位入库 |

> 说明：F2 与 F3 的「注释声称的行为不存在」属于同型风险——AGENTS.md §1.2 记载过的「策略开关只接了允许路径」在本组能力上再现：mxac 报告接了 READONLY（F4），真正的强制面全空；times.go 注释声称创建时落库（F2），实际唯一调用点在 SET_INFO。
