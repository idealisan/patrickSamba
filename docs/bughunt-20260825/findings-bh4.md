# bh4 研究报告：LOCK/READ/WRITE/FLUSH 边界语义对照（stupidSamba vs Samba）

日期：2026-08-25 18:07–18:35 CST · 只读排查，未改任何产品代码
我方代码：/work/bh4（HEAD `2af4771`）· 参照：/work/ref/samba（source3/source4）

格式：`[严重度][置信] 标题 | 我方证据 | Samba 证据 | 客户端可见后果 | 修复方向`

---

## A. 分歧发现

### [HIGH][高] 1. 字节范围锁对 READ/WRITE 完全无阻挡（advisory 空壳）

- 我方证据：`internal/smb/command/read_write.go` 全文 0 处引用 `Share.locks`；handleRead/handleWrite 只查句柄与 GrantedAccess。锁表的消费点全仓只有 close.go:85（释放）与 lock.go:236（LOCK 命令自身）。
- Samba 证据：`source3/smbd/smb2_read.c:584`、`smb2_write.c:392` 同步路径先 `SMB_VFS_STRICT_LOCK_CHECK()`，冲突即 `NT_STATUS_FILE_LOCK_CONFLICT`（异步复查 `smb2_aio.c:374,522`）。share 参数 `strict locking` 默认 Auto＝非 oplocked 文件一律检查（docs-xml/smbdotconf/locking/strictlocking.xml）；Windows 字节范围锁本就强制（MS-FSA §2.1.4.10/§2.1.5）。
- 后果：A 对 [0,N) 上独占锁后 B 的 READ/WRITE 照样穿透。依赖锁互斥的应用（Excel 共编、SQLite 类 DB 文件、advisory-lock 协议桥接）出现数据竞争/损坏。「锁存在但不设防」比没有锁更误导。
- 修复方向：在 handleRead/handleWrite 取到 open 后调 `Share.locks.conflict(path, open, off, len, isWrite)`，命中回 `status.LockConflict`（常量已有 status.go:70）。规则复用 lock.go:56 conflict：共享读锁不挡读、自己的锁不挡自己。

### [HIGH][高] 2. 非 CLOSE 路径的句柄消失不释放字节范围锁（泄漏到进程退出）

- 我方证据：`releaseAll` 全仓唯一调用点是 close.go:85（SMB2 CLOSE handler 内）。句柄消失还有三条路：TREE_DISCONNECT（session.go:277-291）、LOGOFF/连接断开（conn.go:170-191 → session.go:352-375）、durable 过期回收（durable.go:300-332 reap），全部直接调 `Open.close()`（open.go:218-250）——该方法只摘 shareMode 登记、关 fd，不碰锁表。lock_close_test.go 只测了 CLOSE handler 一条路。
- Samba 证据：close.c:503 所有关闭路径汇于 `close_file()` → `locking_close_file()`（locking/locking.c:397-424）→ `brl_close_fnum()` 无条件摘掉该 fsp 全部 range lock。
- 后果：客户端崩溃/断网/TREE_DISCONNECT 时持有的锁永久留在表里，对应路径区段对其他客户端永远 `LOCK_NOT_GRANTED` 直到重启。触发条件极普通（Excel 异常退出即中招）。持久句柄等待重连期间保留锁是正确的，但 reap 过期后同样泄漏。
- 修复方向：把 releaseAll 挪进 Open.close()（注释自称「唯一汇合处」，shareMode.remove 就在那里）；releaseAll 自带表锁，按 open.go 注释要求放 o.mu 外调用。

### [MED][高] 3. 零长度 READ 应成功回 0 字节，我们回 END_OF_FILE；且该校验排在目录/权限检查之前

- 我方证据：read_write.go:34-37 无条件 `req.Length == 0 → status.EndOfFile`；位置在 IsPipe(:39)/IsDir(:43)/权限(:46) 检查之前，零长度读目录或无权限句柄也回 END_OF_FILE。
- Samba 证据：smb2_read.c:404-407 仅 `nread==0 && in_length!=0` 才 END_OF_FILE；length==0 时 read_file（fileio.c:50-58，`if (n > 0)` 才 pread）返回 0 落入成功分支回 0 字节。torture source4/torture/smb2/read.c:92-97（Windows 归纳）：length=0,min_count=0 → OK 且 data.length==0；length=0,min_count=1 → END_OF_FILE。
- 后果：规范客户端的零长探测读拿到意外错误码；错误顺序还泄露「先看长度后鉴权」的实现细节。
- 修复方向：删掉 :34-37 无条件分支让零长走正常路径，仅保留 `MinimumCount > 0 && n < MinimumCount → EndOfFile`；把 IsDir/权限检查挪到一切状态判定之前。

### [MED][高] 4. 阻塞锁（未置 FAIL_IMMEDIATELY）应挂起等待，我们一律立即拒绝

- 我方证据：lock.go:84-91 注释自认未实现挂起、TODO 待异步未决请求表；server 层无任何 pending/async 基础设施（internal/server grep StatusPending 只有 status 常量）。
- Samba 证据：smb2_lock.c:341-347 首元素裸 SHARED/EXCLUSIVE ⇒ blocking=true；:157 pending_queue(req, subreq, 500) 500ms 后发 interim STATUS_PENDING；:528-566 重试退避（0.1s→1s）直至授予或 CANCEL。
- 后果：用阻塞锁的应用等的是响应不是轮询——立即 LOCK_NOT_GRANTED 让它们把暂时争用当永久失败上报（协作场景弹「文件被占用」）。可用性降级，非数据安全问题。
- 修复方向：按既定 TODO 实现异步未决请求表（interim PENDING + 锁释放补发），先支持单元素阻塞锁即可。

### [LOW-MED][高] 5. 零长度锁的「分界点」冲突语义缺失

- 我方证据：lock.go:29-41 overlaps() 中 `l.length == 0 || length == 0 → false`，任何零长锁与谁都不冲突。
- Samba 证据：torture smb2/lock.c:1319-1351 zero_byte_tests（Windows 归纳）：持 {10,0} 独占锁时 {9,2}/{9,3} 必须 LOCK_NOT_GRANTED（跨过偏移 10 这个点），{10,2}/{11,1} 则 OK。Samba brlock.c:158-210 byte_range_overlap 用 `ofs > last(=ofs+len-1)` 判交，空区间 last=ofs−1 恰好给出「跨点才冲突」，仅 {0,0} 因下溢特判；locking.c:305 明注「0 byte ranges ARE allowed and should be stored」。
- 后果：「EOF 处上零长独占锁当写入意图标记」（Excel 等）失效，两个写方都能拿到看似互斥的标记。
- 修复方向：overlaps 改为 last=ofs+len−1 比较，仅特判 {0,0}；请求侧 offset+len 回绕另行校验（见 #7）。

### [LOW][高] 6. 同一句柄重复上独占锁应被拒，我们放行

- 我方证据：lock.go:57-59 conflict() 里 `if l.owner == o { continue }` —— 跳过自己全部既有锁，独占叠独占也授予。
- Samba 证据：torture smb2/lock.c:2311-2321「two exclusive locks do not stack」同句柄二次独占必须 LOCK_NOT_GRANTED；shared-over-exclusive 叠加允许（:2205-2236）。brlock.c:225-245 brl_conflict 即此矩阵：READ×READ 不冲突、同 context READ 可叠 WRITE、其余一律冲突。
- 后果：smbtorture lock 套件红；应用重复加锁的防御性探测失真。
- 修复方向：conflict() 改为复刻 brl_conflict 矩阵而非跳过 owner。unlock 匹配现状正确（Samba 同样只匹配 start+size 不看类型位，brlock.c:985-1032）。

### [LOW][高] 7. 回绕的锁区间应报 STATUS_INVALID_LOCK_RANGE，我们接受并当作「与一切冲突」

- 我方证据：handleLock（lock.go:197-249）对 Offset+Length 无回绕校验即入库；overlaps() 的减法改写使该锁表现得与全地址空间冲突。status 包无此常量（status.go:70-73 只有 LockConflict/LockNotGranted/RangeNotLocked）。
- Samba 证据：brlock.c:392-400 brl_lock_windows_default 先 `byte_range_valid()`（即 MS-FSA §2.1.4.10 的 `(ofs+len−1)<ofs && len!=0` 判据），失败 ⇒ NT_STATUS_INVALID_LOCK_RANGE（0xC00001A1）。
- 后果：畸形客户端能用一条回绕锁干扰全文件所有后续加锁行为；错误码也与 Windows 不同。
- 修复方向：handleLock 入口校验区间有效性，新增 `InvalidLockRange Status = 0xC00001A1`。

### [LOW][高] 8. FLUSH 缺访问校验：只读文件句柄/无 ADD 权限目录都回成功，IPC 管道语义不同

- 我方证据：read_write.go:194-216 handleFlush 只判 IsPipe（成功）、`Handle != nil && !IsDir`（Sync(true)），从不查 GrantedAccess；目录一律静默成功。
- Samba 证据：smb2_flush.c:171-173 IPC ⇒ NOT_IMPLEMENTED；:174-199 要求 FILE_WRITE_DATA|FILE_APPEND_DATA，否则 ACCESS_DENIED；目录需 FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY 才可 flush；fd 为 -1 ⇒ INVALID_HANDLE；另注：默认 `strict sync = no` 时 Samba 直接 no-op 回成功。
- 后果：只读句柄上 FLUSH 我们回成功而 Samba/Windows 回 ACCESS_DENIED；行为面差异会被一致性测试（smbtorture/pytest 对拍）捕捉。
- 修复方向：handleFlush 补 GrantedAccess 校验（普通文件 WRITE_DATA|APPEND_DATA，目录 ADD_FILE|ADD_SUBDIRECTORY），管道维持成功即可（比 NOT_IMPLEMENTED 更宽松无害）。F_FULLFSYNC 范围本身没问题（见 B 组）。

### [LOW][中] 9. 多元素 LOCK 含任一阻塞元素时应报 INVALID_PARAMETER，我们接受

- 我方证据：lock.go:221-234 只校验 UNLOCK 一致性与 shared/exclusive 二选一，不看 FAIL_IMMEDIATELY 与元素个数的关系。
- Samba 证据：smb2_lock.c:364-378：isunlock=false 且 lock_count>1 时任一元素缺 FAIL_IMMEDIATELY ⇒ INVALID_PARAMETER（MS-SMB2 §3.3.5.14.2 SHOULD）。另 :341-360 首元素 flags 用精确 switch（SHARED|EXCLUSIVE 同时置位、裸 0x8 等组合都 INVALID_PARAMETER），我们只挡了部分非法组合。
- 后果：畸形请求被宽容接受；因我们本就不支持阻塞（#4），实际风险低。
- 修复方向：补两条校验：多元素必须全 FAIL_IMMEDIATELY；flags 组合精确匹配枚举。

### [LOW][中] 10. LOCK 用于目录句柄应报 INVALID_DEVICE_REQUEST，我们照常授锁

- 我方证据：handleLock（lock.go:207-214）只拦 IsPipe，不拦 IsDir，目录句柄的字节范围锁会进表生效。
- Samba 证据：locking/locking.c do_lock：`!fsp->can_lock` 且 `is_directory` ⇒ NT_STATUS_INVALID_DEVICE_REQUEST。
- 后果：目录上的锁无意义却占用表项，并与 #2 泄漏叠加放大。
- 修复方向：handleLock 增加 `open.IsDir → status.InvalidDeviceRequest`。

### [INFO][中] 11. WRITE 的 DataOffset 未做「必须等于头+49」强校验

- 我方证据：wire/write.go ParseWriteRequest 用 sliceAt 只做边界校验，DataOffset 指到别处也能解析。
- Samba 证据：smb2_write.c:74-76：`in_data_offset != SMB2_HDR_BODY + body_size` ⇒ INVALID_PARAMETER。
- 后果：仅健壮性/一致性差异。修复方向：Parse 层加 DataOffset 精确校验。

### [INFO][中] 12. CreditCharge 不校验是否覆盖载荷

- 我方证据：server/connection.go:442 + credit.go:85-95 Charge() 只取头部声明值（≥1），不按 payload 复核。
- Samba 证据：smbd_smb2_request_verify_creditcharge（read/write 各自调用）：ceil(len/64K) > 头部声明 ⇒ INVALID_PARAMETER。
- 后果：多信用客户端可少付 credit 跑大 IO（策略问题，非边界安全洞）。修复方向：read/write 入口按载荷复核 charge。

### [INFO][高] 13. WRITE_UNBUFFERED 标志被忽略（WRITE_THROUGH 已处理）

- 我方证据：wire/write.go 定义了 WriteFlagWriteUnbuffer(0x2)，handler（read_write.go:154-160）只看 WRITE_THROUGH 与 FileWriteThrough。
- Samba 证据：smb2_write.c:289-293：3.0.2+ 上 WRITE_UNBUFFERED 同样置 write_through=true。
- 后果：3.0.2+ 客户端要求免缓存落盘时我们不落盘即回包。修复方向：方言 ≥3.0.2 时并入 Sync(false) 分支。

---

## B. 查证无误项（双方一致）

1. **读越界（offset ≥ EOF，Length>0）回 END_OF_FILE** —— 任务清单第 1 条的前提「应返回 0 字节成功」**不成立**：Samba smb2_read.c:404-407 与 torture read.c:88-90 都明确 END_OF_FILE（Windows 同）。我方 read_write.go:63-66 行为一致 ✓
2. 读 Length > MaxReadSize ⇒ INVALID_PARAMETER（smb2_read.c:83-88 vs read_write.go:31-33）✓
3. MinimumCount 语义：n < min_count ⇒ END_OF_FILE，min=0 不触发（smb2_read.c:414-425 vs read_write.go:68-70；torture read.c:107-128 全例吻合）✓
4. 读目录 ⇒ INVALID_DEVICE_REQUEST（Length>0 时；smb2_read.c:497-500 vs read_write.go:43-45）✓
5. 读权限 FileReadData|FileExecute ⇒ ACCESS_DENIED（CHECK_READ_SMB2 vs read_write.go:46-48）✓
6. 写 offset+len 上界防护（我方 2^63 截断 vs Samba sys_valid_io_range/vfs_valid_pwrite_range），均拒绝回绕 ✓（错误码细节略异，影响极小）
7. 写 count==0 合法 no-op 成功（smb2_write.c:269-273 仅 len≠0 且 nwritten==0 才 DISK_FULL vs read_write.go:145-152）✓
8. 写超 EOF 由 pwrite 天然零填充扩展，无多余拒绝（os.File.WriteAt）✓；写后 size 经 fstat 即时可见（local_handle.go Stat 优先 f.Stat()）✓
9. 写 clamp 与协商一致：MaxRead/MaxWrite=64KiB(≤2.1)/1MiB(3.x)（dialect.go:139-152），帧上限 1MiB+512 容纳 1MiB 写（transport.go:37）✓
10. UNLOCK 必须精确匹配 start+size 否则 RANGE_NOT_LOCKED；匹配不看锁类型位（brl_unlock_windows_default brlock.c:1001-1007 vs lock.go:126）✓
11. 多元素加锁/解锁的全有或全无原子性：Samba 失败时逐条 undo（blocking.c:60-86）⇔ 我方先验证后提交（lock.go:80-104,116-143）✓
12. CLOSE 释放该句柄全部锁（含改名后旧路径桶——releaseAll 全表扫描，lock.go:147-178）⇔ Samba locking_close_file→brl_close_fnum ✓（但仅 CLOSE 路径，见 A#2）
13. FLUSH 是单 fd 语义（darwin F_FULLFSYNC/linux fsync/win FlushFileBuffers，local_handle.go:164-186），与 Samba per-fsp fsync 同范围，**不是整卷** ✓；比 Samba 默认（strict sync=no 时 no-op）更强但兼容
14. wire 锁标志常量正确：UNLOCK=0x4、FAIL_IMMEDIATELY=0x10（Samba libcli/smb/smb2_constants.h:221-224 + impacket smb3structs.py:282-285 双源印证）✓
15. LockCount≥1 校验（smb2_lock.c:95-98 vs lock.go:202-205）；首元素定 UNLOCK/加锁模式不可混用 ✓

## C. 严重度统计

HIGH 2 · MED 2 · LOW-MED 1 · LOW 5（#6,#7,#8,#9,#10）· INFO 3 · 查证无误 15
