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
type oplockPending struct {
	share *Share
	entry *oplockEntry
	wait  *breakWait
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

	attr, serr := fs.Stat(path)
	if serr != nil {
		// 目标不存在：不可能有人持有它的缓存许可，也不可能有别的打开者。
		// 新建文件是 oplock 收益最大的场景之一（刚创建、马上写）。
		return grantIfDeferrable(ctx, req, wantLease), nil, status.Success
	}

	key := oplockKey{fileID: attr.FileID, stream: stream}
	share := ctx.Tree.Share

	share.oplocks.mu.Lock()
	existing := share.oplocks.lookup(key)
	share.oplocks.mu.Unlock()

	if existing != nil {
		write := access&writeAccessMask != 0
		level, state, conflict := existing.conflictsWith(ctx.Session, write)
		if !conflict {
			// 不冲突也不授予：表按对象只留一条条目（见上方第 2 条）。
			return oplockPlan{}, nil, status.Success
		}
		w, ok := share.oplocks.beginBreak(existing, level, state)
		if !ok {
			// 进行中的 break 已达上限：不排队，直接按共享冲突回绝。
			// 排队会让被推迟的打开无限期挂着，那比一次可重试的失败更糟。
			ctx.Log.Warn("oplock break 并发达上限，拒绝本次打开",
				"path", path, "share", share.Name)
			return oplockPlan{}, nil, status.SharingViolation
		}
		sendOplockBreak(ctx, existing, level, state)
		return oplockPlan{}, &oplockPending{share: share, entry: existing, wait: w}, status.Success
	}

	// 该对象上已经有别的句柄打开着 → 不授予（见上方第 1 条）。
	if share.shareModes.otherOpeners(shareModeKey{fileID: attr.FileID, stream: stream}) {
		return oplockPlan{}, nil, status.Success
	}
	return grantIfDeferrable(ctx, req, wantLease), nil, status.Success
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

// awaitOplockBreak 等一次 break 的确认，超时则按"已打破"推进。
//
// 返回是否收到了确认。false 表示超时/持有者句柄已关 —— 两种情况下调用方
// 都必须**继续**（按已打破处理），否则被推迟的打开会永久挂着。
func awaitOplockBreak(p *oplockPending) bool {
	if p == nil || p.wait == nil {
		return true
	}
	if p.wait.wait(oplockBreakTimeout) {
		return true
	}
	// 超时：把条目降到"要求降到"的状态并唤醒其它等待者。
	// 不这么做的话，等待者放行后仍会认为持有者在缓存数据 —— 那正是
	// 本特性要防的脏数据路径。
	p.share.oplocks.cancelBreak(p.entry)
	return false
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
