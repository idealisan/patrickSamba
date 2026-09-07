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
// 挂不起来时**不授予也不强闯** —— 按共享冲突回绝，见 create.go 的
// deferCreateForOplockBreak。
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
	// closed 表示 done 已经关过。
	//
	// 关通道必须**只做一次**，而能关它的有三条彼此并发的路径：
	// 持有者确认（ack）、wait 等到超时、持有者句柄关闭（release 的
	// orphan 收尾）。它们谁先谁后都不确定，且后到的仍会走完整的收尾流程
	// （超时那条紧接着就要 cancelBreak）。没有这个标记就是
	// "close of closed channel" —— panic 发生在挂起请求的 goroutine 上，
	// 不在 serve 的 recover 范围内，一次 break 超时会带走整个进程。
	closed bool
	// epoch 是租约的版本号，确认报文必须带回同一个值（MS-SMB2 §2.2.25.2）。
	epoch uint16
	// sent 表示 break 通知是否已经发出过。
	//
	// beginBreak 对"已经在等的条目"返回**同一个** wait，多个等待方会拿到它。
	// 没有这个标记的话，第二个等待方会给同一个持有者再发一遍 break ——
	// 客户端对重复 break 的处置各家不一，最坏会直接断连。
	//
	// 由 oplockTable.mu 保护，只经 markSent 读写：等待方各自跑在自己的
	// goroutine 上，裸读写会让两个并发打开同时看到 false、各发一遍。
	sent bool
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
	w.finish()
}

// finish 结束本次等待并唤醒等待者。
//
// 三条彼此并发的路径都会走到这里：持有者确认（ack）、wait 等到超时、
// 持有者句柄先关了（release 的 orphan 收尾）。**必须幂等** —— 典型的一幕是
// wait 超时后先关一次，紧接着 awaitOplockBreak 又对同一条目调 cancelBreak，
// 那会再关一次；close 一个已关闭的 channel 是 panic，且它发生在挂起请求的
// goroutine 上（不在 serve 的 recover 范围内），一次 break 超时就带走整个进程。
func (w *breakWait) finish() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	close(w.done)
}

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
		// "关通道"与"有没有收到确认"是两件事：确认可能恰好在这一瞬到达，
		// 那时 done 已经关了（finish 幂等，此处是 no-op），acked 才是判据。
		w.finish()
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.acked
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

// oplockTable 是一个共享上的 oplock/lease 表。零值可用。
//
// 挂在 Share 上（与 locks / shareModes 同理）：缓存许可的意义就是跨会话。
type oplockTable struct {
	mu sync.Mutex
	// m 按文件身份索引，**值是列表**。
	//
	// 为什么是列表而不是单条目：标准做法允许**多个并发只读者各持一份读缓存**
	// （Level II oplock / R lease）。只留一条的话，第二个只读客户端什么也
	// 拿不到 —— 正确性没问题，但白白丢掉一份收益，而且与 Samba/Windows 的
	// 行为不一致。
	//
	// 不变式：列表里**至多一个**条目在缓存写（cachesWrite）。缓存写必须与
	// 所有其他打开者互斥，两个并存的写缓存者会各自写脏对方的数据。
	m map[oplockKey][]*oplockEntry
	// leases 按 LeaseKey 索引，用于把客户端回的 lease break 确认定位到条目。
	leases map[[16]byte]*oplockEntry
	// breaking 是当前进行中的 break 数（受 maxOplockBreaksPerShare 约束）。
	breaking int
}

// lookupAll 按文件身份取出全部条目（拷贝一份，调用方可安全持有）。
func (t *oplockTable) lookupAll(k oplockKey) []*oplockEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m[k]) == 0 {
		return nil
	}
	out := make([]*oplockEntry, len(t.m[k]))
	copy(out, t.m[k])
	return out
}

// grant 登记一次授予，追加到该对象的条目列表。
func (t *oplockTable) grant(k oplockKey, e *oplockEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[oplockKey][]*oplockEntry)
	}
	t.m[k] = append(t.m[k], e)
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
	var orphans []*breakWait
	for k, list := range t.m {
		kept := make([]*oplockEntry, 0, len(list))
		for _, e := range list {
			if e.owner != o {
				kept = append(kept, e)
				continue
			}
			if e.lease {
				delete(t.leases, e.leaseKey)
			}
			if e.breaking != nil {
				// 句柄都关了，不可能再有确认 —— 唤醒等待者，
				// 别让它干等到超时。finish 在锁外调（它会关 channel，
				// 唤醒的 goroutine 可能立刻回头取 t.mu）。
				t.breaking--
				orphans = append(orphans, e.breaking)
				e.breaking = nil
			}
		}
		if len(kept) == 0 {
			delete(t.m, k)
		} else {
			t.m[k] = kept
		}
	}
	t.mu.Unlock()

	for _, w := range orphans {
		w.finish()
	}
}

// removeEntry 从某个对象的条目列表里摘掉一条。调用方必须已持有 t.mu。
func (t *oplockTable) removeEntry(k oplockKey, e *oplockEntry) {
	list := t.m[k]
	for i, cur := range list {
		if cur != e {
			continue
		}
		list = append(list[:i], list[i+1:]...)
		break
	}
	if len(list) == 0 {
		delete(t.m, k)
		return
	}
	t.m[k] = list
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
		// 降到"什么都不缓存"= 不再持有，整条摘掉（H 位单独留着仍是有效授予，
		// 只有全 0 才代表放弃）。
		t.removeEntry(oplockKeyOf(e), e)
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
	for k, list := range t.m {
		for _, e := range list {
			if e.owner == o && !e.lease {
				hit, key = e, k
				break
			}
		}
		if hit != nil {
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
		t.removeEntry(key, hit)
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

// cachesWrite 报告本条目是否许可客户端**缓存写**。
//
// 缓存写意味着客户端可以把写攒在本地不落盘 —— 只要还有一个写缓存者存在，
// 其他任何访问者看到的都可能是过期数据。因此它是**排他**的：
// 表的不变式是「同一对象至多一个写缓存者」。
func (e *oplockEntry) cachesWrite() bool {
	if e.lease {
		return e.leaseState&wire.LeaseWriteCaching != 0
	}
	return e.level == wire.OplockLevelExclusive || e.level == wire.OplockLevelBatch
}

// cachesRead 报告本条目是否许可客户端**缓存读**。
//
// 缓存读（Level II / R 位）**不是排他的**：多个并发只读者可以各持一份，
// 因为它们谁也不会改数据。这正是标准做法与"每文件只留一条条目"的差别所在。
func (e *oplockEntry) cachesRead() bool {
	if e.lease {
		return e.leaseState&wire.LeaseReadCaching != 0
	}
	// EXCLUSIVE / BATCH 同时缓存读和写。
	return e.level == wire.OplockLevelII ||
		e.level == wire.OplockLevelExclusive ||
		e.level == wire.OplockLevelBatch
}

// cachingData 报告本条目是否缓存了**数据**（读或写任一）。
//
// 只缓存句柄（H 位）的条目返回 false —— 它不影响数据一致性，
// 因此永远不需要被打破。
func (e *oplockEntry) cachingData() bool {
	return e.cachesRead() || e.cachesWrite()
}

// breakTarget 给出一次 break 应当要求本条目降到哪一级。
//
// 判定（Samba source3/smbd/oplock.c 与 smbd_smb2_create 的 break 决策）：
//
//	newOpenerWrites = true  → 降到什么都不缓存。
//	  对方要写，本方的**读**缓存也会变成旧内容，所以 R 位必须一起去掉。
//	newOpenerWrites = false → 降到只读缓存。
//	  对方只读，数据不会被改，本方的读缓存仍然有效，只需交出写缓存。
//
// H（句柄缓存）两种情形都保留：它只影响"要不要重新打开"，与数据无关。
func (e *oplockEntry) breakTarget(newOpenerWrites bool) (wire.OplockLevel, wire.LeaseState) {
	if e.lease {
		if newOpenerWrites {
			return 0, e.leaseState & wire.LeaseHandleCaching
		}
		// 保留 R 与 H，去掉 W。
		return 0, e.leaseState &^ wire.LeaseWriteCaching
	}
	if newOpenerWrites {
		return wire.OplockLevelNone, 0
	}
	// 交出写缓存后至少还能留着 Level II（读缓存）。
	return wire.OplockLevelII, 0
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

// markSent 报告 break 通知**是否该由本调用方发出**（真=是）。
//
// 判定与置位在 t.mu 下一次完成：多个等待方可能同时拿到 beginBreak 返回的
// 同一个 wait，各自跑在自己的 goroutine 上，谁先谁后不确定。裸读写 sent
// 既是一场数据竞争，也会在交错不利时给同一个持有者发出两遍 break。
func (t *oplockTable) markSent(w *breakWait) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w.sent {
		return false
	}
	w.sent = true
	return true
}

// breakNow 立刻把一条条目按"已打破"推进，**不等确认**。
//
// 只用于**不可能回确认**的持有者：句柄处于 durable 等待重连态，客户端已经断开，
// sendOplockBreak 拿不到 Sender，等下去必然熬满整个 oplockBreakTimeout（30 秒）
// 才由超时路径做同样的事。这里把它提前做掉，省下的 30 秒是**第二个客户端**
// 的等待时间。
//
// 语义与 cancelBreak 完全一致（按"已打破"降级，最坏是丢缓存收益而不是丢数据），
// 差别只是不需要先 beginBreak —— 没有等待方，也就没有 wait 要收尾。
func (t *oplockTable) breakNow(e *oplockEntry, newOpenerWrites bool) {
	t.mu.Lock()
	if e.breaking != nil {
		t.mu.Unlock()
		// 上一轮已经有人在等它了：走现成的超时收尾，顺带唤醒那个等待方。
		t.cancelBreak(e)
		return
	}
	level, state := e.breakTarget(newOpenerWrites)
	if e.lease {
		e.leaseState &= state
		if e.leaseState == wire.LeaseNone {
			t.removeEntry(oplockKeyOf(e), e)
			delete(t.leases, e.leaseKey)
		}
	} else {
		e.level = level
		if e.level == wire.OplockLevelNone {
			t.removeEntry(oplockKeyOf(e), e)
		}
	}
	t.mu.Unlock()
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
			t.removeEntry(oplockKeyOf(e), e)
			delete(t.leases, e.leaseKey)
		}
	} else {
		e.level = w.wantLevel
		if e.level == wire.OplockLevelNone {
			t.removeEntry(oplockKeyOf(e), e)
		}
	}
	t.mu.Unlock()
	w.finish()
}
