package command

import (
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// ---------------------------------------------------------------------------
// oplock / lease 状态机（MS-SMB2 §3.3.5.9 / §2.2.23 / §2.2.24 / §2.2.25）
//
// 授予一个 oplock/lease 等于许可客户端**在本地缓存数据**：
//
//	BATCH / EXCLUSIVE   缓存写（也可能缓存读）
//	LEASE_WRITE_CACHING 同上
//	LEVEL II            缓存读
//	LEASE_READ_CACHING  同上
//	LEASE_HANDLE_CACHING 只缓存"我持有这个句柄"这件事，不缓存数据
//
// 因此**授予之后必须在别人要动这个文件时先把它打破，并且等客户端确认** ——
// 否则第二个客户端读到的可能是第一个客户端还攥在本地缓存里的旧数据。
// 这是本文件存在的全部理由：授予很容易，安全地把授予收回来才是难点。
//
// 「等确认」这条在这里不阻塞读循环：需要打破时 CREATE 走 Context.Defer
// 挂起（见 async.go），读循环照常收帧，客户端的 break 确认才能进得来。
// 挂不起来（复合链中间）时**不授予也不强闯** —— 见 handleOplockConflict。
// ---------------------------------------------------------------------------

const (
	// oplockBreakTimeout 是等 break 确认的上限。
	//
	// Samba 用 35 秒（OPLOCK_BREAK_TIMEOUT）后强行推进，Windows 同数量级。
	// 超时后按"已打破"处理并放行被推迟的打开 —— 与 Samba/Windows 一致：
	// 宁可在极端情况下让一个不响应的客户端承担后果，也不能让第二个客户端
	// 永久等下去。
	oplockBreakTimeout = 30 * time.Second

	// maxOplockBreaksPerShare 是一个共享上同时进行的 break 上限
	// （AGENTS.md §8 资源上限）。超出后新的冲突打开按 SHARING_VIOLATION 处理。
	maxOplockBreaksPerShare = 64
)

// oplockKey 标识一个被 oplock/lease 保护的对象。
//
// 与 shareModeKey 同一口径：宿主文件身份 + 流名。用**身份**而不是路径，
// 因为句柄改名后路径会变，而"谁持有这个文件的缓存许可"不会随之改变。
type oplockKey struct {
	fileID uint64
	stream string
}

// breakWait 是一次 break 的等待状态。
//
// 谁在等：被推迟的那次 CREATE（的后台 goroutine）。
// 等什么：持有者回 break 确认，或者超时。
type breakWait struct {
	done chan struct{}

	mu sync.Mutex
	// wantLevel 是 oplock 族"要求降到"的级别。
	wantLevel wire.OplockLevel
	// wantState 是 lease 族"要求降到"的状态。
	wantState wire.LeaseState
	// acked 表示持有者已经确认（而不是等超时）。
	acked bool
	// epoch 是租约的版本号，确认报文必须带回同一个值（MS-SMB2 §2.2.25.2）。
	epoch uint16
}

// ack 记录一次确认并唤醒等待者。重复确认无害。
func (w *breakWait) ack(level wire.OplockLevel, state wire.LeaseState) {
	w.mu.Lock()
	if w.acked {
		w.mu.Unlock()
		return
	}
	w.acked = true
	w.wantLevel = level
	w.wantState = state
	w.mu.Unlock()
	close(w.done)
}

// finish 由超时路径调用：按"已打破"推进，不置 acked。
func (w *breakWait) finish() { close(w.done) }

// wait 等到确认或超时。返回是否收到了确认。
func (w *breakWait) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.acked
	case <-timer.C:
		w.mu.Lock()
		already := w.acked
		w.mu.Unlock()
		if !already {
			w.finish()
		}
		return already
	}
}

// oplockEntry 是一个已授予的 oplock 或 lease。
type oplockEntry struct {
	// owner 是持有者句柄。
	owner *Open

	// lease 为 true 表示租约族（按 LeaseKey 定位），否则是 oplock 族。
	lease bool
	// leaseKey 仅在 lease 族有效，用于匹配客户端回的 lease break 确认。
	leaseKey [16]byte
	// leaseState 是 lease 族当前状态（R/W/H 位）。
	leaseState wire.LeaseState
	// leaseV2 表示客户端用的是 v2 租约（带 ParentLeaseKey / Epoch）。
	leaseV2 bool
	// epoch 是租约版本号，每次状态变更递增。
	epoch uint16

	// level 仅在 oplock 族有效。
	level wire.OplockLevel

	// breaking 非 nil 表示已发出 break、正在等确认。
	breaking *breakWait
}

// cachingData 报告本条目是否许可客户端**缓存数据**（而不是只缓存句柄）。
//
// 这是"能不能不等确认就放行"的分界线：只缓存句柄（H）的条目被强行打破
// 也不会造成数据不一致，缓存读/写的条目必须等确认。
func (e *oplockEntry) cachingData() bool {
	if e.lease {
		return e.leaseState&(wire.LeaseReadCaching|wire.LeaseWriteCaching) != 0
	}
	return e.level != wire.OplockLevelNone && e.level != wire.OplockLevelII
}

// oplockTable 是一个共享上的 oplock/lease 表。零值可用。
//
// 挂在 Share 上（与 locks / shareModes 同理）：缓存许可的意义就是跨会话。
type oplockTable struct {
	mu sync.Mutex
	// m 按文件身份索引。
	m map[oplockKey]*oplockEntry
	// leases 按 LeaseKey 索引，用于把客户端回的 lease break 确认定位到条目。
	leases map[[16]byte]*oplockEntry
	// breaking 是当前进行中的 break 数（受 maxOplockBreaksPerShare 约束）。
	breaking int
}

// lookup 按文件身份查找。调用方必须已持有 t.mu。
func (t *oplockTable) lookup(k oplockKey) *oplockEntry {
	return t.m[k]
}

// grant 登记一次授予。
//
// 同一对象上只允许一个条目：oplock/lease 的语义就是"排他地允许缓存"，
// 两个条目并存意味着两套互相不知道对方的缓存。
func (t *oplockTable) grant(k oplockKey, e *oplockEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[oplockKey]*oplockEntry)
	}
	t.m[k] = e
	if e.lease {
		if t.leases == nil {
			t.leases = make(map[[16]byte]*oplockEntry)
		}
		t.leases[e.leaseKey] = e
	}
}

// release 释放某个句柄持有的全部条目（句柄关闭时调用）。幂等。
func (t *oplockTable) release(o *Open) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if e.owner != o {
			continue
		}
		delete(t.m, k)
		if e.lease {
			delete(t.leases, e.leaseKey)
		}
		if e.breaking != nil {
			// 句柄都关了，不可能再有确认 —— 唤醒等待者，别让它干等到超时。
			t.breaking--
			e.breaking.finish()
		}
	}
}

// ackLease 处理客户端回的 lease break 确认（MS-SMB2 §2.2.25.2）。
func (t *oplockTable) ackLease(key [16]byte, state wire.LeaseState) bool {
	t.mu.Lock()
	e := t.leases[key]
	if e == nil || e.breaking == nil {
		t.mu.Unlock()
		return false
	}
	w := e.breaking
	// 确认到达即落地新状态：客户端说它降到了什么就是什么，
	// 它比服务端要求的更低也合法（例如直接放弃全部缓存）。
	e.leaseState = state
	if state == wire.LeaseNone {
		delete(t.m, oplockKeyOf(e))
		delete(t.leases, key)
	}
	t.breaking--
	e.breaking = nil
	t.mu.Unlock()

	w.ack(0, state)
	return true
}

// ackOplock 处理客户端回的 oplock break 确认（MS-SMB2 §2.2.24.1）。
//
// oplock 族没有 LeaseKey 可用，只能按句柄定位（确认报文里的 FileId 就是
// 被打破的那个句柄）。
func (t *oplockTable) ackOplock(o *Open, level wire.OplockLevel) bool {
	t.mu.Lock()
	var (
		hit *oplockEntry
		key oplockKey
	)
	for k, e := range t.m {
		if e.owner == o && !e.lease {
			hit, key = e, k
			break
		}
	}
	if hit == nil || hit.breaking == nil {
		t.mu.Unlock()
		return false
	}
	w := hit.breaking
	hit.level = level
	if level == wire.OplockLevelNone {
		delete(t.m, key)
	}
	t.breaking--
	hit.breaking = nil
	t.mu.Unlock()

	w.ack(level, 0)
	return true
}

// oplockKeyOf 取条目所属对象的键（用于 ackLease 里反查 m）。
//
// 反向扫描而不是把键存在条目上，是为了让键只有一个真源 —— 存两份迟早会
// 不同步，那种 bug 的表现是"确认到达却删错了对象"。
func oplockKeyOf(e *oplockEntry) oplockKey {
	if e.owner == nil {
		return oplockKey{}
	}
	return oplockKey{fileID: e.owner.oplockFileID, stream: e.owner.Stream}
}

// conflict 判定既有条目与一次新的打开是否冲突，冲突时给出"应降到"的级别/状态。
//
// 判定口径（Samba source3/smbd/oplock.c: attempt_oplock_break /
// smbd_smb2_create 的 break 决策）：
//
//	既有 EXCLUSIVE / BATCH / W 位：别人**读写都**会脏 → 写打开降到 NONE，
//	  只读打开降到 II（读缓存仍然有效 —— 没人改数据）
//	既有 II / R 位：别人只读没问题；别人要写 → 降到 NONE，否则会读到旧内容
//	既有 H 位：只影响句柄缓存，数据共享不受影响，不需要打破
//
// 同一**会话**（持有者自己再开一次）永不冲突 —— 客户端不会打破自己的缓存，
// 它知道自己有几只手在动这个文件。跨会话才是需要协调的情形。
//
// 口径选会话而不是句柄：一次会话内的多个句柄共享同一份客户端缓存，
// 打破其中一个等于打破全部，没有意义。
func (e *oplockEntry) conflictsWith(sess *Session, write bool) (wire.OplockLevel, wire.LeaseState, bool) {
	if e.owner != nil && sess != nil && e.owner.Session == sess {
		return 0, 0, false
	}
	if e.lease {
		has := e.leaseState
		switch {
		case has&wire.LeaseWriteCaching != 0:
			// 写缓存：别人读写都必须先让它落盘。
			keep := wire.LeaseHandleCaching
			if !write {
				// 对方只读：数据不会被改，读缓存可以留着。
				keep |= wire.LeaseReadCaching
			}
			return 0, has & keep, true
		case has&wire.LeaseReadCaching != 0 && write:
			// 读缓存 + 对方要写：不打破会读到旧内容。
			return 0, has &^ wire.LeaseReadCaching, true
		}
		return 0, 0, false
	}

	switch e.level {
	case wire.OplockLevelExclusive, wire.OplockLevelBatch:
		if write {
			return wire.OplockLevelNone, 0, true
		}
		return wire.OplockLevelII, 0, true
	case wire.OplockLevelII:
		if write {
			return wire.OplockLevelNone, 0, true
		}
	}
	return 0, 0, false
}

// beginBreak 登记一次进行中的 break 并构造其等待句柄。
//
// ok 为 false 表示进行中的 break 已达上限（见 maxOplockBreaksPerShare）。
func (t *oplockTable) beginBreak(e *oplockEntry, wantLevel wire.OplockLevel, wantState wire.LeaseState) (*breakWait, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.breaking >= maxOplockBreaksPerShare {
		return nil, false
	}
	if e.breaking != nil {
		// 已经在等了：复用同一次等待，不要并发地发第二遍 break。
		return e.breaking, true
	}
	t.breaking++
	e.breaking = &breakWait{
		done:      make(chan struct{}),
		wantLevel: wantLevel,
		wantState: wantState,
		epoch:     e.epoch,
	}
	return e.breaking, true
}

// cancelBreak 把一次进行中的 break 收尾（超时/取消路径）。
//
// **必须按"已打破"推进**：等待者（被推迟的打开）随后就要访问这个文件了，
// 若仍按原状态认为持有者在缓存数据，就会放行一次会造成脏读的访问。
// 降级到"要求降到"的状态后，最坏情况是持有者不响应而丢了缓存收益，
// 不会丢数据。
func (t *oplockTable) cancelBreak(e *oplockEntry) {
	t.mu.Lock()
	if e.breaking == nil {
		t.mu.Unlock()
		return
	}
	t.breaking--
	w := e.breaking
	e.breaking = nil
	if e.lease {
		e.leaseState &= w.wantState
		if e.leaseState == wire.LeaseNone {
			delete(t.m, oplockKeyOf(e))
			delete(t.leases, e.leaseKey)
		}
	} else {
		e.level = w.wantLevel
		if e.level == wire.OplockLevelNone {
			delete(t.m, oplockKeyOf(e))
		}
	}
	t.mu.Unlock()
	w.finish()
}
