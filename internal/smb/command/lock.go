package command

import (
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandLock, true, true, handleLock)
}

// ---------------------------------------------------------------------------
// 字节范围锁（MS-SMB2 §3.3.5.14 + MS-FSA §2.1.4.10 / §2.1.5.7）
//
// 这里实现的是 SMB 层面的**咨询锁**：只在本进程内部记账，不下沉到宿主
// 文件系统的 flock/fcntl。理由：
//   - SMB 的锁语义（按 Open 归属、强制型、冲突即失败）与 POSIX 咨询锁
//     （按进程归属、fcntl 会在任意 fd 关闭时全丢）根本对不上，硬映射
//     只会制造更难查的 bug；
//   - AGENTS.md C7 要求跨平台，Windows 的 LockFileEx 语义又是另一套。
//
// 锁表挂在 **Share** 上而不是 Session/Tree 上 —— 冲突判定必须跨会话、
// 跨连接生效，否则两个客户端各锁各的，锁就等于没有。
// ---------------------------------------------------------------------------

// byteRangeLock 是一条已授予的字节范围锁。
type byteRangeLock struct {
	// owner 是持有该锁的句柄。同一个 Open 内部不互相冲突。
	owner *Open
	// offset / length 来自 SMB2_LOCK_ELEMENT，单位字节。
	// length == 0 的锁不与任何范围冲突（MS-FSA：零长度锁总是成功）。
	offset uint64
	length uint64
	// exclusive 为 false 表示共享锁（读锁）。
	exclusive bool
}

// overlaps 报告两个范围是否有交集。
//
// offset+length 可能溢出 uint64（客户端可以送 offset=2^64-1, length=2^64-1），
// 所以用"起点比较"而不是"终点比较"，全程不做加法。
func (l byteRangeLock) overlaps(offset, length uint64) bool {
	if l.length == 0 || length == 0 {
		// 零长度锁不占据任何字节，永不冲突。
		return false
	}
	// [l.offset, l.offset+l.length) 与 [offset, offset+length) 相交
	//   ⇔ l.offset < offset+length 且 offset < l.offset+l.length
	// 用减法改写以避免溢出：
	if offset >= l.offset {
		return offset-l.offset < l.length
	}
	return l.offset-offset < length
}

// lockTable 是一个共享上的字节范围锁表，按路径分桶。并发安全。
type lockTable struct {
	mu sync.Mutex
	// byPath 的 key 是相对共享根的路径（大小写敏感，与 VFS 一致）。
	byPath map[string][]byteRangeLock
}

// conflict 检查在 path 上为 owner 申请 [offset, length) 是否与他人冲突。
// 调用方必须已持有 t.mu。
func (t *lockTable) conflict(path string, owner *Open, offset, length uint64, exclusive bool) bool {
	for _, l := range t.byPath[path] {
		if l.owner == owner {
			// 同一句柄的锁互不冲突（简化：不做同句柄重叠排他锁的自冲突判定，
			// 真实客户端不会这么用，Samba 也放行）。
			continue
		}
		if !l.exclusive && !exclusive {
			// 共享锁之间可以共存。
			continue
		}
		if l.overlaps(offset, length) {
			return true
		}
	}
	return false
}

// add 无条件登记一条锁。调用方必须已持有 t.mu 并先做过冲突判定。
func (t *lockTable) add(path string, l byteRangeLock) {
	if t.byPath == nil {
		t.byPath = make(map[string][]byteRangeLock)
	}
	t.byPath[path] = append(t.byPath[path], l)
}

// remove 摘除 owner 在 path 上**完全匹配** [offset, length) 的一条锁。
//
// MS-SMB2 §3.3.5.14.2：解锁必须与加锁的范围精确一致，
// 部分解锁 / 跨锁解锁一律 STATUS_RANGE_NOT_LOCKED。
// 调用方必须已持有 t.mu。
func (t *lockTable) remove(path string, owner *Open, offset, length uint64) bool {
	locks := t.byPath[path]
	for i, l := range locks {
		if l.owner != owner || l.offset != offset || l.length != length {
			continue
		}
		locks = append(locks[:i], locks[i+1:]...)
		if len(locks) == 0 {
			delete(t.byPath, path)
		} else {
			t.byPath[path] = locks
		}
		return true
	}
	return false
}

// releaseAll 释放某个句柄在 path 上持有的全部锁。
//
// CLOSE、LOGOFF、TREE_DISCONNECT、连接断开都要走到这里，
// 否则一个崩掉的客户端会把文件永久锁死（MS-SMB2 §3.3.5.10）。
//
// 只扫 path 一个桶就够：锁登记时用的 key 恒为 open.Path（见 handleLock），
// 一个 Open 的路径在其生命周期内不变，所以它不可能在别的桶里留下锁。
func (t *lockTable) releaseAll(path string, owner *Open) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	locks := t.byPath[path]
	if len(locks) == 0 {
		return
	}
	kept := locks[:0]
	for _, l := range locks {
		if l.owner != owner {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		delete(t.byPath, path)
	} else {
		t.byPath[path] = kept
	}
}

// count 返回某个句柄当前持有的锁数量，用于测试。
func (t *lockTable) count(owner *Open) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := 0
	for _, locks := range t.byPath {
		for _, l := range locks {
			if l.owner == owner {
				n++
			}
		}
	}
	return n
}

// handleLock 处理 SMB2 LOCK（MS-SMB2 §3.3.5.14）。
//
// 支持的语义：
//   - 加锁（共享/排他）：与**其它句柄**的锁做重叠冲突判定，无冲突即授予；
//   - 解锁：必须与加锁范围精确一致；
//   - 数组内要么全是加锁、要么全是解锁，混用回 STATUS_INVALID_PARAMETER；
//   - 数组内任意一条失败，本请求中**已经授予的部分必须回滚**（原子性）。
//
// 未实现：阻塞等待。SMB2_LOCKFLAG_FAIL_IMMEDIATELY 未置位时规范要求服务端
// 挂起该请求（回 STATUS_PENDING，等锁释放或收到 CANCEL 再了结）。本服务端
// 目前所有 handler 都是同步执行的，没有异步未决请求表，因此一律按"立即失败"
// 处理并回 STATUS_LOCK_NOT_GRANTED。
// TODO: 等异步未决请求表（见 cancel.go）做好后改为真正的阻塞等待。
func handleLock(ctx *Context) error {
	req, err := wire.ParseLockRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	// 目录上不能加字节范围锁（MS-SMB2 §3.3.5.14）。
	if open.IsDir {
		return status.InvalidParameter
	}
	if open.IsPipe() {
		// 管道没有字节范围的概念。
		return status.InvalidDeviceRequest
	}

	// §3.3.5.14："If the flags of the first element ... SMB2_LOCKFLAG_UNLOCK,
	// and any other element does not have it set, the server MUST fail the
	// request with STATUS_INVALID_PARAMETER."（反之亦然）
	unlockMode := req.Locks[0].Flags.IsUnlock()
	for _, e := range req.Locks {
		if e.Flags.IsUnlock() != unlockMode {
			return status.InvalidParameter
		}
		if err := validateLockFlags(e.Flags); err != nil {
			return err
		}
	}

	// 锁表是 Share 的零值可用字段，直接取地址，不额外抽访问器。
	table := &ctx.Tree.Share.locks
	table.mu.Lock()
	defer table.mu.Unlock()

	path := open.Path

	if unlockMode {
		for i, e := range req.Locks {
			if !table.remove(path, open, e.Offset, e.Length) {
				// 回滚：把本请求中已经解掉的锁装回去。
				for _, done := range req.Locks[:i] {
					table.add(path, byteRangeLock{
						owner:     open,
						offset:    done.Offset,
						length:    done.Length,
						exclusive: done.Flags.IsExclusive(),
					})
				}
				return status.RangeNotLocked
			}
		}
		ctx.Out = (&wire.LockResponse{}).Append(ctx.Out)
		return nil
	}

	for i, e := range req.Locks {
		exclusive := e.Flags.IsExclusive()
		if table.conflict(path, open, e.Offset, e.Length, exclusive) {
			// 回滚本请求中已经授予的锁。
			for _, done := range req.Locks[:i] {
				table.remove(path, open, done.Offset, done.Length)
			}
			if !e.Flags.FailImmediately() {
				ctx.Log.Debug("阻塞式 LOCK 暂按立即失败处理（未实现异步等待）",
					"path", path, "offset", e.Offset, "length", e.Length)
			}
			return status.LockNotGranted
		}
		table.add(path, byteRangeLock{
			owner:     open,
			offset:    e.Offset,
			length:    e.Length,
			exclusive: exclusive,
		})
	}

	ctx.Out = (&wire.LockResponse{}).Append(ctx.Out)
	return nil
}

// validateLockFlags 校验单个 SMB2_LOCK_ELEMENT 的 Flags 组合
// （MS-SMB2 §2.2.26.1 / §3.3.5.14）。
func validateLockFlags(f wire.LockFlags) error {
	if f.IsUnlock() {
		// UNLOCK 不能与 SHARED / EXCLUSIVE / FAIL_IMMEDIATELY 同时出现。
		if f&(wire.LockFlagSharedLock|wire.LockFlagExclusiveLock|
			wire.LockFlagFailImmediately) != 0 {
			return status.InvalidParameter
		}
		return nil
	}
	shared := f&wire.LockFlagSharedLock != 0
	excl := f&wire.LockFlagExclusiveLock != 0
	if shared == excl {
		// 既没指定也不能两个都指定。
		return status.InvalidParameter
	}
	return nil
}
