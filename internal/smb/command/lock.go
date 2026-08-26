package command

import (
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandLock, true, true, handleLock)
}

// byteRangeLock 是一条已授予的字节范围锁。
type byteRangeLock struct {
	// owner 是持有该锁的句柄。
	owner *Open
	// offset/length 是锁定区间。
	// length == 0 的锁是「分界点」标记：不占用字节，但挡住一切跨过
	// offset 这个点的区间（见 overlaps 的推导）。
	offset uint64
	length uint64
	// exclusive 为 true 表示独占锁（写锁），false 为共享锁（读锁）。
	exclusive bool
}

// overlaps 报告本锁与请求区间 [off, off+length) 是否有交集。
//
// 判交模型与 Samba brlock.c:158-210 byte_range_overlap 一致：把区间写成
// **闭区间 [ofs, ofs+len-1]**，两个闭区间相交当且仅当
//
//	ofs1 <= last2 && ofs2 <= last1
//
// length == 0 的空区间的 last 恰好是 ofs-1（uint64 下溢自然给出），
// 于是零长锁自动获得「分界点」语义：只挡住同时包含 ofs-1 与 ofs 的
// 区间 —— 也就是「跨过 ofs 这一点」的区间（bh4-A#5，
// torture smb2/lock.c:1319-1351 zero_byte_tests：持 {10,0} 时 {9,2} 拒、
// {10,2} 允许）。唯一必须特判的是 {0,0}：ofs-1 会下溢成 MaxUint64，
// 不特判它就变成「与一切冲突」，而 Samba locking.c:305 明注
// 「0 byte ranges ARE allowed and should be stored」。
//
// 前置条件：调用方已保证 off+length 不回绕（LOCK 入口有回绕校验见
// handleLock；READ/WRITE 入口有溢出防御），因此这里的加法不会溢出。
func (l byteRangeLock) overlaps(off, length uint64) bool {
	if l.length == 0 && l.offset == 0 {
		return false // {0,0} 下溢特判：空集，与谁都不相交
	}
	if length == 0 && off == 0 {
		return false // 同上，针对请求侧
	}
	return l.offset <= off+length-1 && off <= l.offset+l.length-1
}

// lockTable 是一个共享上的字节范围锁表，按共享内相对路径索引。
//
// 零值可用。
type lockTable struct {
	mu sync.Mutex
	m  map[string][]byteRangeLock
}

// conflict 在已有锁中查找与请求区间冲突的锁 —— **LOCK 命令语义**。
//
// 冲突矩阵复刻 Samba brlock.c:225-245 brl_conflict（bh4-A#6）：
//
//	READ × READ          永不冲突（任意两个句柄之间）
//	同句柄且涉及 READ    不冲突（shared-over-exclusive 可叠，
//	                     torture smb2/lock.c:2205-2236）
//	其余                 一律冲突 —— 包括**同一句柄**的 W×W
//	                     （torture :2311-2321「two exclusive locks do
//	                     not stack」要求 LOCK_NOT_GRANTED）
//
// 注意：这只约束 LOCK 命令自身的授予判定。READ/WRITE 入口的强制检查走
// blocksIO，那里的同句柄豁免是**完全豁免**（Samba STRICT_LOCK_CHECK 按
// fsp 豁免自己），两者不能混用。
func (t *lockTable) conflict(path string, o *Open, off, length uint64, exclusive bool) bool {
	for _, l := range t.m[path] {
		if !l.overlaps(off, length) {
			continue
		}
		if !l.exclusive && !exclusive {
			// 共享锁之间可以共存。
			continue
		}
		if l.owner == o && !(l.exclusive && exclusive) {
			// 同一句柄：只要不是独占叠独占，共享/独占混合叠加允许。
			continue
		}
		return true
	}
	return false
}

// blocksIO 是 READ/WRITE 入口的字节范围锁强制检查（strict locking）用的
// 冲突判定。与 conflict()（LOCK 命令语义）的区别只有一点：
// **同一句柄自己的锁完全豁免** —— Samba 的 SMB_VFS_STRICT_LOCK_CHECK 按
// fsp 豁免自己（smb2_read.c:584 / smb2_write.c:392），否则句柄连自己刚用
// LOCK 锁住的区间都写不了。
//
// write 为 true 表示写操作：与任何重叠的外句柄锁（独占或共享）冲突；
// false 表示读操作：只与重叠的外句柄**独占**锁冲突。规则矩阵
// （MS-FSA §2.1.4.10 / MS-SMB2 §3.3.5.14）：
//
//	        对方独占   对方共享
//	本方读    冲突       放行
//	本方写    冲突       冲突
//
// length == 0 的 IO（零长读/零长写探测）不占用任何字节，直接放行 ——
// 这与零长锁的分界点语义无关：IO 语义下空操作就是碰不到任何字节。
func (t *lockTable) blocksIO(path string, o *Open, off, length uint64, write bool) bool {
	if length == 0 {
		return false
	}
	for _, l := range t.m[path] {
		if l.owner == o {
			continue // 同句柄完全豁免（STRICT_LOCK_CHECK 语义）
		}
		if !write && !l.exclusive {
			// 读只被外句柄的独占锁挡住。
			continue
		}
		if l.overlaps(off, length) {
			return true
		}
	}
	return false
}

// checkIO 在 READ/WRITE 入口做字节范围锁强制检查（strict locking）。
//
// 冲突判定走 blocksIO（同句柄完全豁免，读只被外句柄独占锁挡住）。
// Samba 对照：smb2_read.c:584 / smb2_write.c:392 的同步路径先做
// SMB_VFS_STRICT_LOCK_CHECK，冲突即 NT_STATUS_FILE_LOCK_CONFLICT。
//
// path 必须与加锁时的键一致（handleLock 用 open.Path，这里同样用 open.Path）。
func (t *lockTable) checkIO(path string, o *Open, off, length uint64, write bool) status.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.blocksIO(path, o, off, length, write) {
		return status.LockConflict
	}
	return status.Success
}

// validRange 报告锁区间 [ofs, ofs+length) 是否有效。
//
// length == 0 恒有效（零长「分界点」标记，见 overlaps）。
// length > 0 时要求区间不回绕：MS-FSA §2.1.4.10 的判据是
// (ofs+len-1) < ofs && len != 0 —— 即 ofs > MaxUint64-(len-1)。
// 回绕区间必须回 STATUS_INVALID_LOCK_RANGE（bh4-A#7，
// Samba brlock.c:392-400 brl_lock_windows_default 先 byte_range_valid()
// 再谈授予，失败即 NT_STATUS_INVALID_LOCK_RANGE）。
func validRange(ofs, length uint64) bool {
	if length == 0 {
		return true
	}
	return length-1 <= ^uint64(0)-ofs
}

// lock 原子地授予一组锁。
//
// MS-SMB2 §3.3.5.14：一条 LOCK 请求里的多个 LockElement 是**全有或全无**的，
// 任何一条冲突都不得留下部分已授予的锁。回绕的锁区间整条回
// STATUS_INVALID_LOCK_RANGE（bh4-A#7），同样不留任何部分状态。
func (t *lockTable) lock(path string, o *Open, elems []wire.LockElement) status.Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 先整体校验，再整体写入。
	for _, e := range elems {
		if !validRange(e.Offset, e.Length) {
			return status.InvalidLockRange
		}
		exclusive := e.Flags.IsExclusive()
		if t.conflict(path, o, e.Offset, e.Length, exclusive) {
			// 未置 FAIL_IMMEDIATELY 时规范要求把请求挂起、等锁释放再回应
			// （异步 STATUS_PENDING）。当前所有 handler 都是同步执行的，
			// 挂起会占住整条连接，反而更糟，所以一律立即拒绝。
			//
			// TODO: 待实现异步未决请求表后，改为对未置 FAIL_IMMEDIATELY
			// 的请求回 STATUS_PENDING 并在锁释放时补发响应。
			return status.LockNotGranted
		}
	}

	if t.m == nil {
		t.m = make(map[string][]byteRangeLock)
	}
	for _, e := range elems {
		t.m[path] = append(t.m[path], byteRangeLock{
			owner:     o,
			offset:    e.Offset,
			length:    e.Length,
			exclusive: e.Flags.IsExclusive(),
		})
	}
	return status.Success
}

// unlock 释放一组锁。区间必须与加锁时**完全一致**，否则回
// STATUS_RANGE_NOT_LOCKED（MS-SMB2 §3.3.5.14）。
//
// 同样是全有或全无：先确认每一条都能找到，再统一删除。
func (t *lockTable) unlock(path string, o *Open, elems []wire.LockElement) status.Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	cur := t.m[path]
	// idx 记录每条请求对应的下标，-1 表示没找到。
	idx := make([]int, len(elems))
	taken := make(map[int]bool, len(elems))
	for i, e := range elems {
		idx[i] = -1
		for j, l := range cur {
			if taken[j] || l.owner != o {
				continue
			}
			if l.offset == e.Offset && l.length == e.Length {
				idx[i] = j
				taken[j] = true
				break
			}
		}
		if idx[i] < 0 {
			return status.RangeNotLocked
		}
	}

	kept := cur[:0]
	for j, l := range cur {
		if !taken[j] {
			kept = append(kept, l)
		}
	}
	t.setLocked(path, kept)
	return status.Success
}

// releaseAll 释放某个句柄持有的**全部**锁，供 CLOSE 调用。
//
// ⚠️ path 只是调用方（close.go）顺手给的提示，本函数**故意不依赖它**，
// 而是扫全表按 owner 摘除。这不是保守，是修一个真实的锁泄漏：
//
//	SET_INFO 的 FileRenameInformation 成功后会改写 open.Path
//	（见 set_info.go 的 `open.Path = dst` —— 句柄在改名后仍然有效）。
//	于是「加锁 → 用同一句柄改名 → 关闭」这条路径上，锁登记在旧路径的桶里，
//	而 CLOSE 传进来的是新路径。只扫一个桶的话那把锁**永远不会被释放**，
//	直到进程退出为止都挡着旧路径上的那段字节。
//
// 全表扫描的代价可以忽略：releaseAll 每个句柄一生只调用一次，
// 而锁表里的条目数以「当前被锁住的文件数」为上界，通常是个位数。
// 用正确性换这点常数是划算的。
func (t *lockTable) releaseAll(_ string, o *Open) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for path, cur := range t.m {
		kept := cur[:0]
		for _, l := range cur {
			if l.owner != o {
				kept = append(kept, l)
			}
		}
		if len(kept) == len(cur) {
			// 这个桶里没有该句柄的锁，原样留着。
			continue
		}
		t.setLocked(path, kept)
	}
}

// setLocked 写回某路径的锁列表，空列表时删除条目避免表无限增长。
// 调用方必须已持有 t.mu。
func (t *lockTable) setLocked(path string, locks []byteRangeLock) {
	if len(locks) == 0 {
		delete(t.m, path)
		return
	}
	t.m[path] = locks
}

// handleLock 处理 SMB2 LOCK（MS-SMB2 §3.3.5.14）。
//
// 实现范围：字节范围锁的**非阻塞**语义 —— 无冲突即授予，有冲突立即回
// STATUS_LOCK_NOT_GRANTED。锁表挂在 Share 上，因此跨会话可见。
//
// 未实现：阻塞等待（需要异步未决请求表）、lock sequence 的重放抑制
// （MS-SMB2 §3.3.5.14 里用于多通道重传去重，单通道下不会触发）。
func handleLock(ctx *Context) error {
	req, err := wire.ParseLockRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}
	if len(req.Locks) == 0 {
		// LockCount 必须 >= 1（MS-SMB2 §2.2.26）。
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if open.IsPipe() {
		// 管道不支持字节范围锁。
		return status.InvalidDeviceRequest
	}
	if open.IsDir {
		// 目录句柄上的字节范围锁无意义（bh4-A#10）。Samba
		// locking/locking.c do_lock：!fsp->can_lock && is_directory ⇒
		// NT_STATUS_INVALID_DEVICE_REQUEST。加锁/解锁一并拒绝 ——
		// 目录上本就不可能有锁，解锁同样没有可匹配的对象。
		return status.InvalidDeviceRequest
	}
	if ctx.Tree == nil || ctx.Tree.Share == nil {
		return status.NetworkNameDeleted
	}

	// 一条请求里 UNLOCK 不能与加锁混用（MS-SMB2 §3.3.5.14：若首条是
	// UNLOCK，则全部必须是 UNLOCK，否则 STATUS_INVALID_PARAMETER）。
	unlocking := req.Locks[0].Flags.IsUnlock()
	for _, e := range req.Locks {
		if unlocking {
			if e.Flags != wire.LockFlagUnlock {
				// 解锁元素的 flags 必须精确等于 UNLOCK：混入 SHARED /
				// EXCLUSIVE / 未知位都是畸形请求（Samba smb2_lock.c:341-360
				// 精确 switch，bh4-A#9）。
				return status.InvalidParameter
			}
			continue
		}
		switch e.Flags {
		case wire.LockFlagSharedLock,
			wire.LockFlagExclusiveLock,
			wire.LockFlagSharedLock | wire.LockFlagFailImmediately,
			wire.LockFlagExclusiveLock | wire.LockFlagFailImmediately:
			// 合法四态（MS-SMB2 §2.2.26.1）。
		default:
			// SHARED 与 EXCLUSIVE 同时置位、两者都不置、或带未知位，
			// 一律 STATUS_INVALID_PARAMETER（bh4-A#9）。
			return status.InvalidParameter
		}
	}

	if !unlocking && len(req.Locks) > 1 {
		// 多元素**加锁**请求里只要有一个元素未置 FAIL_IMMEDIATELY，
		// 就必须整条回 STATUS_INVALID_PARAMETER（MS-SMB2 §3.3.5.14.2 的
		// SHOULD；Samba smb2_lock.c:364-378 同判。bh4-A#9）。理由：
		// 阻塞语义只对单元素定义，多元素混合阻塞会让「部分授予后等待」
		// 的原子性无从谈起。
		for _, e := range req.Locks {
			if !e.Flags.FailImmediately() {
				return status.InvalidParameter
			}
		}
	}

	table := &ctx.Tree.Share.locks
	var st status.Status
	if unlocking {
		st = table.unlock(open.Path, open, req.Locks)
	} else {
		st = table.lock(open.Path, open, req.Locks)
	}
	if st != status.Success {
		return st
	}

	ctx.Out = (&wire.LockResponse{}).Append(ctx.Out)
	return nil
}
