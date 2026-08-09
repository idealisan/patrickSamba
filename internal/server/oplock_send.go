package server

import (
	"math"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// breakNotifyMessageID 是 break 通知头里的 MessageId。
//
// MS-SMB2 §3.3.4.6 / §3.3.4.7：服务端主动发起的 oplock / lease break
// 通知不对应任何客户端请求，MessageId 必须是 0xFFFFFFFFFFFFFFFF。
// Samba source3/smbd/smb2_server.c smbd_smb2_break_send() 同样写
// SBVAL(hdr, SMB2_HDR_MESSAGE_ID, UINT64_MAX)。
const breakNotifyMessageID uint64 = math.MaxUint64

// breakSender 把 command.BreakSender 接口实现在一条 Connection 上。
//
// 用独立的小结构体而不是让 *Connection 直接实现接口，是为了不把
// SendOplockBreak / SendLeaseBreak 这两个方法暴露到 Connection 的公共面上 ——
// 它们只该被协议层通过接口调用。
type breakSender struct{ c *Connection }

// SendOplockBreak 推送 oplock 族 break 通知（MS-SMB2 §2.2.23.1）。
func (s breakSender) SendOplockBreak(t command.BreakTarget, b wire.OplockBreak) error {
	// oplock 族的 SessionId 填持有该 Open 的会话
	// （Samba smbd_smb2_send_oplock_break 传 op->compat->vuid）。
	frame := b.Append(breakHeader(t.SessionID).Append(nil))
	s.c.log.Debug("推送 oplock break 通知",
		"level", b.OplockLevel, "session", t.SessionID, "tree", t.TreeID)
	return s.c.sendUnsolicited(frame)
}

// SendLeaseBreak 推送 lease 族 break 通知（MS-SMB2 §2.2.23.2）。
func (s breakSender) SendLeaseBreak(t command.BreakTarget, b wire.LeaseBreakNotification) error {
	// lease 族的 SessionId **恒为 0**：租约按 LeaseKey 定位、跨会话共享，
	// 不属于某一个具体会话（Samba smbd_smb2_send_lease_break 显式传 0）。
	// t.SessionID 只进日志，不进报文。
	frame := b.Append(breakHeader(0).Append(nil))
	s.c.log.Debug("推送 lease break 通知",
		"current", b.CurrentLeaseState, "new", b.NewLeaseState,
		"flags", b.Flags, "session", t.SessionID, "tree", t.TreeID)
	return s.c.sendUnsolicited(frame)
}

// breakHeader 构造 break 通知的 SMB2 头。
//
// 字段取值逐条对齐 Samba source3/smbd/smb2_server.c 的
// smbd_smb2_break_send()（MS-SMB2 §3.3.4.6 / §3.3.4.7）：
//
//	CreditCharge = 0    通知不消耗 credit
//	Status       = 0
//	Command      = SMB2 OPLOCK_BREAK (0x0012)
//	Credits      = 0    **不授予 credit**。credit 是对请求的回赠，
//	                    通知不是响应，给了会让客户端水位虚高。
//	Flags        = SMB2_FLAGS_SERVER_TO_REDIR，仅此一位。
//	               特别注意**不置** SMB2_FLAGS_SIGNED。
//	NextCommand  = 0    通知永远单发，不参与复合链
//	MessageId    = 0xFFFFFFFFFFFFFFFF
//	TreeId       = 0    恒 0，与该句柄实际所在的树无关
//	SessionId    = 见调用方
//	Signature    = 全 0
//
// 关于**不签名也不加密**：这是刻意的，不是偷懒。
// Samba 在 smbd_smb2_break_send() 里 memset 签名区为 0，并且把
// TRANSFORM_HEADER 的 iovec 置为 {NULL, 0}（即不走加密封装），
// 即使会话协商了签名/加密也一样。break 通知在会话密钥之外，
// 客户端不会对它验签。真实客户端行为优先于对规范的字面理解
// （AGENTS.md §9）。
//
// 这条约束在 PR-2 上线真实授予后必须复测：如果某个客户端（尤其
// macOS）拒收未签名的 break，就要回来改这里，并在此记录差异。
func breakHeader(sessionID uint64) wire.Header {
	return wire.Header{
		Command:   wire.CommandOplockBreak,
		Flags:     wire.FlagServerToRedir,
		MessageID: breakNotifyMessageID,
		SessionID: sessionID,
	}
}

// sendUnsolicited 把一条**服务端主动发起**的 SMB2 消息写出去。
//
// 与读循环的响应写出共用 c.writeMu。这不是可选的：
// Transport 内部复用同一份 4 字节写头缓冲（Transport.whdr），并且
// 头与载荷是两个 iovec 分别提交的，两个 goroutine 同时 WriteFrame
// 会既触发 data race 又把两条帧的字节交错在一起 —— 交错出来的流对
// 客户端就是不可恢复的协议错误。
//
// 连接已关闭时不额外维护标志位，直接让底层 net.Conn 报
// net.ErrClosed —— 多一个标志位就多一处要和 closeOnce 保持同步的状态，
// 而这里并不需要区分"关了"和"写失败"。调用方一律按"送不到"处理。
//
// 可从任意 goroutine 调用。
func (c *Connection) sendUnsolicited(frame []byte) error {
	if len(frame) == 0 {
		return ErrEmptyFrame
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.tr.WriteFrame(frame)
}
