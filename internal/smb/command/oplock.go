package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandOplockBreak, true, true, handleOplockBreak)
}

// ---------------------------------------------------------------------------
// oplock / lease 的**发送通道**。
//
// 这里只建立"协议层能把一条 break 通知交给传输层写出去"的能力，
// **不包含任何授予逻辑** —— 本服务当前仍然一律授予
// SMB2_OPLOCK_LEVEL_NONE，也不宣告 SMB2_GLOBAL_CAP_LEASING。
//
// 为什么先做通道后做授予：授予一个永远不会 break 的 oplock/lease 比
// 不授予**更糟** —— 客户端会据此把写缓存在本地，第二个客户端读到的
// 就是脏数据。所以先把 break 通路打通并实测，再谈授予。
// ---------------------------------------------------------------------------

// BreakTarget 显式指明一条 break 通知发给谁。
//
// break 是**异步**发出的：从"决定要 break"到"报文真正落到 socket"之间，
// 触发它的那条请求早就返回了，对应的 tree 甚至 session 都可能已经消失。
// 因此身份必须在这里以**值拷贝**的形式带上，发送时绝不回头去读 Conn 的
// "当前"状态 —— 那个状态在异步语境下是没有意义的。
type BreakTarget struct {
	// SessionID 直接填进 break 通知头的 SessionId 字段。
	//
	//   - oplock 族：填持有该 Open 的会话 ID。
	//   - lease 族：**恒为 0**。租约由 LeaseKey 定位，跨会话共享，
	//     与某一个具体会话无关。
	//
	// 依据：Samba source3/smbd/smb2_server.c
	// smbd_smb2_send_oplock_break() 传 op->compat->vuid，
	// smbd_smb2_send_lease_break() 显式传 0 并注释 /* no session_id */。
	SessionID uint64

	// TreeID 仅用于日志与审计定位，**不会写进报文头**。
	//
	// break 通知的 TreeId 恒为 0（Samba 上面那段
	// SIVAL(hdr, SMB2_HDR_TID, 0)）。留这个字段是为了让"这个 break 是
	// 哪个共享上的哪个句柄引起的"在日志里可追溯，不必事后去反查。
	TreeID uint32
}

// BreakSender 由传输层（internal/server）实现并注入进 Conn，
// 供协议层主动向客户端推送 oplock / lease break 通知。
//
// 分层约束（AGENTS.md §5）：依赖只能自上而下，`command` **不得** import
// `server`。所以这里只定义接口，实现放在 internal/server/oplock_send.go。
//
// 实现方必须保证：
//   - 与读循环的响应写出**互斥**（Transport 内部复用同一个写头缓冲，
//     并发写会同时造成 data race 与报文交错）；
//   - 可从任意 goroutine 调用；
//   - 连接已关闭时返回错误而不是 panic。
type BreakSender interface {
	// SendOplockBreak 推送 oplock 族 break 通知（MS-SMB2 §2.2.23.1）。
	SendOplockBreak(t BreakTarget, b wire.OplockBreak) error
	// SendLeaseBreak 推送 lease 族 break 通知（MS-SMB2 §2.2.23.2）。
	SendLeaseBreak(t BreakTarget, b wire.LeaseBreakNotification) error
}

// SetBreakSender 注入 break 发送通道。由 internal/server 在连接建立时调用。
//
// 传 nil 等于关闭主动推送能力（单元测试里构造裸 Conn 时就是这种情况）。
func (c *Conn) SetBreakSender(s BreakSender) {
	c.mu.Lock()
	c.breakSender = s
	c.mu.Unlock()
}

// BreakSender 返回已注入的发送通道；未注入时返回 nil。
//
// 加锁读而不是裸读：注入发生在读循环启动前，但**调用**发生在别的
// goroutine 上，裸读会被 -race 判定为竞争（即便实际时序安全）。
// break 是低频动作，这把锁的代价可以忽略。
func (c *Conn) BreakSender() BreakSender {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.breakSender
}

// handleOplockBreak 处理 SMB2 OPLOCK_BREAK（MS-SMB2 §3.3.5.22）。
//
// 本服务在 CREATE 里一律授予 SMB2_OPLOCK_LEVEL_NONE（见 create.go），
// 也没有在 NEGOTIATE 里声明 SMB2_GLOBAL_CAP_LEASING，因此客户端**不应该**
// 发来任何 oplock/lease break acknowledgment —— 它手上没有可以被打破的
// oplock。收到就说明对端状态与我们不一致。
//
// 规范对"找不到匹配 oplock 的确认"要求回 STATUS_INVALID_OPLOCK_PROTOCOL
// （§3.3.5.22.1）。
func handleOplockBreak(ctx *Context) error {
	switch wire.PeekOplockBreakKind(ctx.Msg) {
	case wire.OplockBreakKindOplock:
		req, err := wire.ParseOplockBreak(ctx.Msg)
		if err != nil {
			// 报文本身就解不开，属于格式错误而非 oplock 协议错误。
			return status.InvalidParameter
		}
		ctx.Log.Warn("收到 oplock break 确认，但本服务从不授予 oplock",
			"level", req.OplockLevel, "session", ctx.Header.SessionID)
		return status.InvalidOplockProtocol

	case wire.OplockBreakKindLease:
		// 没声明 SMB2_GLOBAL_CAP_LEASING 却收到 lease 族属于协议违规。
		ctx.Log.Warn("收到 lease break 确认，但本服务未声明 LEASING 能力",
			"session", ctx.Header.SessionID)
		return status.InvalidOplockProtocol

	default:
		// StructureSize 既不是 24 也不是 36，连是哪一族都判不出来。
		return status.InvalidParameter
	}
}
