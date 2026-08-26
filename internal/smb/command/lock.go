package command

import (
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandLock, true, true, handleLock)
}

// 阻塞锁等待参数（bh4-A#4）。
const (
	// blockingLockMaxWait 是单条阻塞锁的最长等待时长。超时回
	// STATUS_LOCK_NOT_GRANTED —— 与 Samba 的「无限等待直到 CANCEL」相比
	// 这是刻意的降级，理由见 handleLock 注释。
	blockingLockMaxWait = 10 * time.Second

	// maxBlockingLockWaiters 是锁表上同时挂起等待的阻塞锁请求上限
	// （AGENTS.md §8 资源上限）。达到上限后新来的阻塞请求立即按非阻塞
	// 处理 —— 宁可退化成立即拒绝，也不能让畸形客户端用海量挂起等待
	// 耗尽 goroutine。
	maxBlockingLockWaiters = 64
)

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

	// wake 是「锁表里有锁被释放」的广播通道：每次释放时 close 旧通道、
	// 换上新通道，所有阻塞锁等待者同时被唤醒去重试（bh4-A#4）。
	// 粒度是整表而非单条路径 —— 阻塞锁是低频事件，粗粒度换实现简单，
	// 唤醒后各自重试冲突判定，正确性不受影响。
	wake chan struct{}

	// waiters 是当前挂起等待的阻塞锁请求数（受 maxBlockingLockWaiters
	// 上限约束）。在 mu 保护下读写。
	waiters int
}

// notifyRelease 广播「有锁被释放」。调用方必须已持有 t.mu。
func (t *lockTable) notifyRelease() {
	if t.wake != nil {
		close(t.wake)
	}
	t.wake = make(chan struct{})
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
	// 有锁被释放：唤醒阻塞锁等待者去重试（bh4-A#4）。
	t.notifyRelease()
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

	released := false
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
		released = true
	}
	if released {
		// 有锁被释放：唤醒阻塞锁等待者（bh4-A#4）。
		t.notifyRelease()
	}
}

// waitLock 是阻塞锁的**同步有界**等待（bh4-A#4）：区间空闲则立即授予；
// 冲突则挂起等待锁释放后重试，直到授予 / 超时回 STATUS_LOCK_NOT_GRANTED /
// 并发等待者超过 maxWaiters 立即拒绝 / 句柄在等待期间被关闭。
//
// ⚠️ 该方法会**阻塞调用方**。当前所有 handler 都在连接的读循环里同步执行，
// 所以等待期间这条连接收不到任何新请求 —— 包括 CANCEL。这是与规范
// （interim STATUS_PENDING + 异步补发）的已知差距，见 handleLock 注释。
// timeout 建议用 blockingLockMaxWait；测试传小值。
func (t *lockTable) waitLock(path string, o *Open, e wire.LockElement, timeout time.Duration, maxWaiters int) status.Status {
	elems := []wire.LockElement{e}

	t.mu.Lock()
	if !validRange(e.Offset, e.Length) {
		t.mu.Unlock()
		return status.InvalidLockRange
	}
	if t.waiters >= maxWaiters {
		// 上限已满：按非阻塞处理，立即给出结果而不是无限堆积等待者。
		granted := !t.conflict(path, o, e.Offset, e.Length, e.Flags.IsExclusive())
		t.mu.Unlock()
		if granted {
			return t.lock(path, o, elems)
		}
		return status.LockNotGranted
	}
	t.waiters++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.waiters--
		t.mu.Unlock()
	}()

	deadline := time.Now().Add(timeout)
	for {
		if o.Closed() {
			// 等待期间句柄被别的路径关闭（durable 回收等），别再给它授锁。
			return status.LockNotGranted
		}
		if st := t.lock(path, o, elems); st != status.LockNotGranted {
			return st // 授予或回绕拒绝，直接透传
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return status.LockNotGranted
		}

		// 等下一次释放广播，至多等到期限。取 min(remain, 100ms) 兜底轮询：
		// 广播通路覆盖 unlock/releaseAll 两条路，兜底只为防实现疏漏时
		// 等待者永久沉睡。
		t.mu.Lock()
		wake := t.wake
		t.mu.Unlock()
		timer := time.NewTimer(min(remain, 100*time.Millisecond))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
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
// 实现范围：字节范围锁的授予/释放，含**单元素阻塞锁**的同步有界等待
// （bh4-A#4）—— 首元素未置 FAIL_IMMEDIATELY 的加锁请求在冲突时挂起等待，
// 持锁方释放后被授予；至多等 blockingLockMaxWait，超时回
// STATUS_LOCK_NOT_GRANTED。锁表挂在 Share 上，因此跨会话可见。
//
// 与规范的已知差距（需要异步未决请求表才能补齐，涉及 server 层装配，
// 本包无法独立完成）：Samba 对阻塞锁回 interim STATUS_PENDING（500ms，
// smb2_lock.c:157）后**异步**等待、期间可被 CANCEL 取消
// （smb2_lock.c:528-566）。我们的 handler 在连接读循环里同步执行：
//   - 回 PENDING 再异步补发需要 Connection 的异步写通路与 pending 表
//     （internal/server 的 connection.go / command 层 conn.go）；
//   - 同步等待期间本连接读不了 CANCEL，取消语义无从谈起。
//
// 因此选择「同步有界等待」：多数争用在窗口内自然消解，超时按非阻塞
// 语义拒绝。等待者数量受 maxBlockingLockWaiters 上限保护。
//
// 未实现：lock sequence 的重放抑制（MS-SMB2 §3.3.5.14 里用于多通道重传
// 去重，单通道下不会触发）。
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
		if st == status.LockNotGranted && len(req.Locks) == 1 &&
			!req.Locks[0].Flags.FailImmediately() {
			// 阻塞锁（bh4-A#4）：单元素、未置 FAIL_IMMEDIATELY 的加锁请求
			// 在冲突时挂起等待，而不是立即拒绝。Samba smb2_lock.c:341-347
			// 同判：首元素裸 SHARED/EXCLUSIVE ⇒ blocking。
			//
			// 等待是同步有界的（见本函数注释的「已知差距」）。
			st = table.waitLock(open.Path, open, req.Locks[0],
				blockingLockMaxWait, maxBlockingLockWaiters)
		}
	}
	if st != status.Success {
		return st
	}

	ctx.Out = (&wire.LockResponse{}).Append(ctx.Out)
	return nil
}
