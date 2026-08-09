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
	// owner 是持有该锁的句柄。同一个 Open 重复加锁不算冲突。
	owner *Open
	// offset/length 是锁定区间。length == 0 的锁不锁定任何字节。
	offset uint64
	length uint64
	// exclusive 为 true 表示独占锁（写锁），false 为共享锁（读锁）。
	exclusive bool
}

// overlaps 报告本锁与 [off, off+length) 是否有交集。
//
// 这里刻意用 128 位安全的写法：off+length 可能在 uint64 上回绕，
// 直接相加比较会把越界区间误判成不相交（AGENTS.md §8 整数溢出防御）。
func (l byteRangeLock) overlaps(off, length uint64) bool {
	if l.length == 0 || length == 0 {
		// 零长度锁不占用任何字节，与谁都不冲突（MS-FSA §2.1.4.10）。
		return false
	}
	// a 的结束位置：用减法改写 a.off+a.len > b.off，避免溢出。
	//   l.offset+l.length > off  ⟺  l.length > off-l.offset（当 off >= l.offset）
	// 分两种情况直接比较更稳妥：
	if l.offset <= off {
		return l.length > off-l.offset
	}
	return length > l.offset-off
}

// lockTable 是一个共享上的字节范围锁表，按共享内相对路径索引。
//
// 零值可用。
type lockTable struct {
	mu sync.Mutex
	m  map[string][]byteRangeLock
}

// conflict 在已有锁中查找与请求区间冲突的锁。
//
// 冲突规则（MS-FSA §2.1.4.10 / MS-SMB2 §3.3.5.14）：
// 两个区间有交集，且至少一方是独占锁，且不是同一个句柄持有 —— 才冲突。
// 同一句柄自己的锁不与自己冲突。
func (t *lockTable) conflict(path string, o *Open, off, length uint64, exclusive bool) bool {
	for _, l := range t.m[path] {
		if l.owner == o {
			continue
		}
		if !l.exclusive && !exclusive {
			// 共享锁之间可以共存。
			continue
		}
		if l.overlaps(off, length) {
			return true
		}
	}
	return false
}

// lock 原子地授予一组锁。
//
// MS-SMB2 §3.3.5.14：一条 LOCK 请求里的多个 LockElement 是**全有或全无**的，
// 任何一条冲突都不得留下部分已授予的锁。
func (t *lockTable) lock(path string, o *Open, elems []wire.LockElement) status.Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 先整体校验，再整体写入。
	for _, e := range elems {
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

// releaseAll 释放某个句柄在某个路径上的全部锁，供 CLOSE 调用。
func (t *lockTable) releaseAll(path string, o *Open) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cur := t.m[path]
	if len(cur) == 0 {
		return
	}
	kept := cur[:0]
	for _, l := range cur {
		if l.owner != o {
			kept = append(kept, l)
		}
	}
	t.setLocked(path, kept)
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
	if ctx.Tree == nil || ctx.Tree.Share == nil {
		return status.NetworkNameDeleted
	}

	// 一条请求里 UNLOCK 不能与加锁混用（MS-SMB2 §3.3.5.14：若首条是
	// UNLOCK，则全部必须是 UNLOCK，否则 STATUS_INVALID_PARAMETER）。
	unlocking := req.Locks[0].Flags.IsUnlock()
	for _, e := range req.Locks {
		if e.Flags.IsUnlock() != unlocking {
			return status.InvalidParameter
		}
		if !unlocking {
			// 加锁时 SHARED 与 EXCLUSIVE 必须二选一。
			shared := e.Flags&wire.LockFlagSharedLock != 0
			excl := e.Flags&wire.LockFlagExclusiveLock != 0
			if shared == excl {
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
