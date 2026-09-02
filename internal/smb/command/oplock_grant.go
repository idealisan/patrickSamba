package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// ---------------------------------------------------------------------------
// oplock / lease 的授予判定与 break 发起（MS-SMB2 §3.3.5.9）
//
// 授予规则被刻意收得很紧。原因不是"少写点代码"，而是**授予出错的后果是
// 静默的脏数据**：客户端按许可把写缓存在本地，服务端以为磁盘上就是最新的，
// 第二个客户端读到的就是旧内容 —— 没有任何一方会报错。
//
// 三条收紧（每条都对应一种具体的脏数据路径）：
//
//  1. 该对象上已经有别的句柄打开着 → 不授予写缓存类许可
//     （EXCLUSIVE / BATCH / W 位）。读缓存（II / R 位）也不授予 ——
//     表按对象只留一条条目，两条并存意味着两套互不知情的缓存。
//  2. 该对象上已经有 oplock/lease 条目 → 不授予（同上，避免双条目）。
//  3. 复合链中间的 CREATE → 不授予。见下方 planOplock 的说明。
// ---------------------------------------------------------------------------

// oplockPlanKind 区分本次 CREATE 授予的是哪一类。
type oplockPlanKind uint8

const (
	// oplockPlanNone 是不授予（回 SMB2_OPLOCK_LEVEL_NONE）。
	oplockPlanNone oplockPlanKind = iota
	// oplockPlanLevel 是 oplock 族（II / EXCLUSIVE / BATCH）。
	oplockPlanLevel
	// oplockPlanLease 是租约族（响应里带 RqLs create context）。
	oplockPlanLease
)

// oplockPlan 是本次 CREATE 的缓存许可计划。零值 = 不授予。
type oplockPlan struct {
	kind oplockPlanKind
	// level 仅在 kind == oplockPlanLevel 时有效。
	level wire.OplockLevel
	// lease 仅在 kind == oplockPlanLease 时有效：已填入服务端决定的状态。
	lease *wire.LeaseContext
}

// oplockPending 表示必须先打破既有缓存许可才能继续本次打开。
//
// 一次打开可能需要打破**多条**条目（例如一个写缓存者 + 若干个读缓存者），
// 所以这里是列表而不是单条。等待方要等它们全部确认（或超时）。
type oplockPending struct {
	share *Share
	// victims 是本次要打破的条目，与 levels / states 一一对应。
	victims []*oplockEntry
	// waits 是去重后的等待句柄（多个 victim 可能共享同一次 break）。
	waits  []*breakWait
	levels []wire.OplockLevel
	states []wire.LeaseState
	// newWrite / wantLease 供续跑后重新判定保留。
	newWrite  bool
	wantLease *wire.LeaseContext
}

// writeAccessMask 是"会改动文件内容"的访问位集合。
//
// 用于 break 决策：只有对方要写，持有者的**读**缓存才需要失效；
// 对方只读时读缓存仍然有效。
const writeAccessMask = wire.FileWriteData | wire.FileAppendData |
	wire.FileWriteEA | wire.FileWriteAttributes | wire.Delete |
	wire.WriteDAC | wire.WriteOwner

// planOplock 判定本次 CREATE 应授予什么，以及是否需要先打破既有的缓存许可。
//
// 返回值：
//   - pending != nil：必须先把本次 CREATE 挂起（Context.Defer），
//     等 pending.wait 结束（收到确认或超时）之后再继续真正的打开。
//     这条路径下 plan 无意义。
//   - st != Success：判定过程本身失败（例如租约状态位非法），整个 CREATE 失败。
//
// **为什么必须在 fs.Open 之前**：需要打破时本次 CREATE 会被挂起并在
// 后台 goroutine 里从这一步继续。挂起前若已经改动过文件系统，
// 继续时会改第二次（FILE_CREATE 之类的非幂等 disposition 会直接报错）。
//
// **为什么复合链中间的 CREATE 一律不授予**（见 Context.Defer 的「末条限制」）：
// 异步响应是单发的，链中间挂起意味着要把一条复合响应链拆成两帧。
// 不授予就没有缓存许可，也就永远不需要 break，从根上避开这个坑。
func planOplock(ctx *Context, req *wire.CreateRequest, fs vfs.FileSystem,
	path, stream string, access wire.Access) (oplockPlan, *oplockPending, status.Status) {

	if ctx.Tree == nil || ctx.Tree.Share == nil || ctx.Conn == nil {
		return oplockPlan{}, nil, status.Success
	}
	if !ctx.Conn.Settings.Oplocks {
		return oplockPlan{}, nil, status.Success
	}
	// 客户端请求的租约（没有就是 nil，不是错误 —— 大多数请求都没有）。
	wantLease, err := wire.FindLeaseContext(req.Contexts)
	if err != nil {
		// 载荷长度既不是 32 也不是 52：按 wire 层的约定是畸形请求。
		return oplockPlan{}, nil, status.InvalidParameter
	}
	if wantLease != nil && wantLease.LeaseState & ^(wire.LeaseReadCaching|
		wire.LeaseHandleCaching|wire.LeaseWriteCaching) != 0 {
		// 未知的状态位不猜含义（AGENTS.md §9）。
		return oplockPlan{}, nil, status.InvalidParameter
	}

	newWrite := access&writeAccessMask != 0
	share := ctx.Tree.Share

	attr, serr := fs.Stat(path)
	if serr != nil {
		// 目标不存在：不可能有人持有它的缓存许可，也不可能有别的打开者。
		// 新建文件是 oplock 收益最大的场景之一（刚创建、马上写）。
		return grantOplock(req, wantLease), nil, status.Success
	}

	key := oplockKey{fileID: attr.FileID, stream: stream}
	entries := share.oplocks.lookupAll(key)
	otherOpeners := share.shareModes.otherOpeners(shareModeKey(key))

	// ---- 授予判定 ----
	//
	// 三级，与 Samba / Windows 的口径一致：
	//   1. 独占打开（没有别的打开者、也没有既有条目）→ 按请求授予最高级别
	//   2. 有别人打开着，且我只读，且没人缓存写、也没人握着写权限
	//      → 授予**读缓存**。多个这样的读者可以并存（v0.7.2 起放开）。
	//   3. 其余 → 不授予
	//
	// 注意第 2 级的两个"且"：otherWrite 只看其他句柄的**权限**不看它是否
	// 真的在写 —— SMB 的 break 只在打开时触发，写入时无从感知，
	// 所以判据必须放在授予这一刻。
	var grant oplockPlan
	switch {
	case len(entries) == 0 && !otherOpeners:
		grant = grantOplock(req, wantLease)
	case newWrite:
		grant = oplockPlan{}
	default:
		if !anyCachesWrite(entries) && !share.shareModes.hasWriteOpener(shareModeKey(key)) {
			grant = readOnlyPlan(req, wantLease)
		}
	}
	if grant.kind != oplockPlanNone {
		// 有可授予的东西也要看是否还有必须打破的既有条目。
		// 独占分支下 entries 为空，不会走到这里。
		if len(entries) == 0 {
			return grant, nil, status.Success
		}
	}

	// ---- 需要打破哪些既有条目 ----
	//
	//	newWrite  → 打破所有缓存数据的（连读缓存也要去掉，否则本方写完后
	//	            对方的读缓存是旧内容）
	//	!newWrite → 只打破缓存写的；缓存读的可以留着（大家都是只读）
	var victims []*oplockEntry
	for _, e := range entries {
		if newWrite {
			if e.cachingData() {
				victims = append(victims, e)
			}
			continue
		}
		if e.cachesWrite() {
			victims = append(victims, e)
		}
	}
	if len(victims) == 0 {
		return grant, nil, status.Success
	}

	// 同一会话自己的另一个句柄不算"别人" —— 客户端不会打破自己的缓存，
	// 它知道自己有几只手在动这个文件。
	kept := victims[:0]
	for _, e := range victims {
		if e.owner != nil && ctx.Session != nil && e.owner.Session == ctx.Session {
			continue
		}
		kept = append(kept, e)
	}
	victims = kept
	if len(victims) == 0 {
		return grant, nil, status.Success
	}

	// 对每条条目发起 break，并把**所有**等待汇成一个 pending。
	levels := make([]wire.OplockLevel, len(victims))
	states := make([]wire.LeaseState, len(victims))
	waits := make([]*breakWait, 0, len(victims))
	for i, e := range victims {
		levels[i], states[i] = e.breakTarget(newWrite)
		w, ok := share.oplocks.beginBreak(e, levels[i], states[i])
		if !ok {
			// 进行中的 break 已达上限：不排队，直接按共享冲突回绝。
			// 排队会让被推迟的打开无限期挂着，那比一次可重试的失败更糟。
			ctx.Log.Warn("oplock break 并发达上限，拒绝本次打开",
				"path", path, "share", share.Name)
			return oplockPlan{}, nil, status.SharingViolation
		}
		// beginBreak 已存在时返回同一个 wait —— 只有新建的那次才需要发通知，
		// 否则会给同一个持有者重复发第二遍 break。
		if !w.sent {
			w.sent = true
			sendOplockBreak(ctx, e, levels[i], states[i])
		}
		if !containsWait(waits, w) {
			waits = append(waits, w)
		}
	}

	return oplockPlan{}, &oplockPending{
		share:     share,
		victims:   victims,
		waits:     waits,
		levels:    levels,
		states:    states,
		newWrite:  newWrite,
		wantLease: wantLease,
	}, status.Success
}

// anyCachesWrite 报告这批条目里是否有任何一个在缓存写。
func anyCachesWrite(entries []*oplockEntry) bool {
	for _, e := range entries {
		if e.cachesWrite() {
			return true
		}
	}
	return false
}

// containsWait 报告 waits 里是否已经有 w（避免重复等待同一个 break）。
func containsWait(waits []*breakWait, w *breakWait) bool {
	for _, cur := range waits {
		if cur == w {
			return true
		}
	}
	return false
}

// readOnlyPlan 生成"只授予读缓存"的计划。
//
// 用于"别人也开着我们只读"的场景：不能缓存写（会与别人冲突），
// 但只要没人能改数据，读缓存就是安全的 —— 而且**多个这样的读者可以并存**。
//
// 租约族保留 R 与 H（去掉 W）；oplock 族给 Level II。
func readOnlyPlan(req *wire.CreateRequest, wantLease *wire.LeaseContext) oplockPlan {
	if wantLease != nil {
		granted := *wantLease
		granted.LeaseState = granted.LeaseState &
			(wire.LeaseReadCaching | wire.LeaseHandleCaching)
		granted.Flags = 0
		if granted.LeaseState == 0 {
			// 客户端只要了 W：== 什么都不该给它，回 NONE。
			return oplockPlan{}
		}
		return oplockPlan{kind: oplockPlanLease, lease: &granted}
	}
	// oplock 族：请求了 II / EXCLUSIVE / BATCH 都降级为 II。
	switch req.RequestedOplockLevel {
	case wire.OplockLevelII, wire.OplockLevelExclusive, wire.OplockLevelBatch:
		return oplockPlan{kind: oplockPlanLevel, level: wire.OplockLevelII}
	}
	return oplockPlan{}
}

// grantIfDeferrable 决定授予，但对**复合链中间**的 CREATE 一律不授予。
//
// 它需要挂起通路才能在将来冲突时等 break 确认，而异步响应是单发的，
// 链中间挂起等于把一条复合响应链拆成两帧（见 Context.Defer 的「末条限制」）。
// 不授予就没有缓存许可，也就永远不需要由它触发 break —— 从根上避开。
//
// ⚠️ 这条判断**只能**放在"是否授予"这一步，绝不能提前到 planOplock 开头：
// 那样会让复合链中间的 CREATE 连带跳过 break 检查，
// 直接放行一次会读到脏数据的访问。
func grantIfDeferrable(ctx *Context, req *wire.CreateRequest, wantLease *wire.LeaseContext) oplockPlan {
	if ctx.Header.NextCommand != 0 {
		return oplockPlan{}
	}
	return grantOplock(req, wantLease)
}

// grantOplock 按客户端的请求决定授予什么。
//
// 前置条件：调用方已经确认该对象上没有别的打开者、也没有既有缓存许可。
//
// 租约优先：客户端给了 RqLs 就走租约族（它是 oplock 的超集，
// 且 SMB2_OPLOCK_LEVEL_LEASE(0xFF) 与 oplock 级别互斥）。
func grantOplock(req *wire.CreateRequest, wantLease *wire.LeaseContext) oplockPlan {
	if wantLease != nil {
		granted := *wantLease
		// 服务端可以只授予请求状态的一部分；这里全量授予请求的所有位 ——
		// 前置条件已经保证没有别人在动这个文件，全量授予是安全的。
		granted.Flags = 0
		return oplockPlan{kind: oplockPlanLease, lease: &granted}
	}

	switch req.RequestedOplockLevel {
	case wire.OplockLevelBatch:
		return oplockPlan{kind: oplockPlanLevel, level: wire.OplockLevelBatch}
	case wire.OplockLevelExclusive:
		return oplockPlan{kind: oplockPlanLevel, level: wire.OplockLevelExclusive}
	case wire.OplockLevelII:
		return oplockPlan{kind: oplockPlanLevel, level: wire.OplockLevelII}
	}
	return oplockPlan{}
}

// sendOplockBreak 向持有者发出 break 通知。
//
// 走 Context.Conn 上注入的 BreakSender（internal/server 实现），
// 与读循环的响应写出互斥。没有注入时（单元测试）什么都不做 ——
// 调用方随后会等超时并按"已打破"推进，行为仍然正确，只是慢。
func sendOplockBreak(ctx *Context, e *oplockEntry, level wire.OplockLevel, state wire.LeaseState) {
	if e.owner == nil {
		return
	}
	// ⚠️ 必须用**持有者**那一条连接的 BreakSender，不是 ctx.Conn。
	//
	// ctx.Conn 是**发起新打开的那个客户端**的连接 —— 把 break 通知写进它，
	// 通知会发错人：该让出缓存的是持有者，而发起者此刻正等着被推迟的响应。
	// 这个错误不会报错，只会表现为"永远等不到确认、每次都熬到超时"，
	// 现象是第二个客户端的打开莫名地慢 30 秒。
	var sender BreakSender
	if e.owner.Session != nil && e.owner.Session.Conn != nil {
		sender = e.owner.Session.Conn.BreakSender()
	}
	if sender == nil {
		return
	}
	target := BreakTarget{}
	if e.owner.Session != nil {
		target.SessionID = e.owner.Session.ID
	}
	if e.owner.Tree != nil {
		target.TreeID = e.owner.Tree.ID
	}

	if e.lease {
		newEpoch := e.epoch
		if e.leaseV2 {
			// Epoch 只在 v2 有意义（§2.2.23.2），每次 break 递增。
			newEpoch = e.epoch + 1
		}
		_ = sender.SendLeaseBreak(target, wire.LeaseBreakNotification{
			NewEpoch:          newEpoch,
			Flags:             wire.LeaseBreakAckRequired,
			LeaseKey:          e.leaseKey,
			CurrentLeaseState: e.leaseState,
			NewLeaseState:     state,
		})
		ctx.Log.Debug("发出 lease break 通知",
			"lease_key", e.leaseKey, "current", e.leaseState, "new", state)
		return
	}

	_ = sender.SendOplockBreak(target, wire.OplockBreak{
		OplockLevel: level,
		FileID:      wire.FileID{Persistent: e.owner.Persistent, Volatile: e.owner.Volatile},
	})
	ctx.Log.Debug("发出 oplock break 通知",
		"level", level, "fid", e.owner.Volatile)
}

// awaitOplockBreak 等本次涉及的全部 break 确认，超时则按"已打破"推进。
//
// 返回是否**全部**都收到了确认。false 表示至少有一个超时/持有者已关 ——
// 两种情况下调用方都必须继续（按已打破处理），否则被推迟的打开会永久挂着。
//
// 超时路径必须逐条调 cancelBreak：等待者随后就要访问这个文件了，
// 若仍按原状态认为持有者在缓存数据，就会放行一次会造成脏读的访问。
// cancelBreak 幂等（已经收到确认的条目 breaking 已是 nil，直接跳过）。
func awaitOplockBreak(p *oplockPending) bool {
	allAcked := true
	for _, w := range p.waits {
		if !w.wait(oplockBreakTimeout) {
			allAcked = false
		}
	}
	if !allAcked {
		for _, e := range p.victims {
			p.share.oplocks.cancelBreak(e)
		}
	}
	return allAcked
}

// applyGrant 在句柄建立之后登记本次授予。
//
// plan.kind == oplockPlanNone 时是 no-op。
func applyGrant(share *Share, o *Open, plan oplockPlan) {
	if plan.kind == oplockPlanNone {
		return
	}
	key := oplockKey{fileID: o.oplockFileID, stream: o.Stream}
	switch plan.kind {
	case oplockPlanLevel:
		share.oplocks.grant(key, &oplockEntry{owner: o, level: plan.level})
	case oplockPlanLease:
		share.oplocks.grant(key, &oplockEntry{
			owner:      o,
			lease:      true,
			leaseKey:   plan.lease.LeaseKey,
			leaseState: plan.lease.LeaseState,
			leaseV2:    plan.lease.V2,
			epoch:      plan.lease.Epoch,
		})
	}
}
