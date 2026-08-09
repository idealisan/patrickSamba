package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	// CANCEL 是**唯一**永不产生响应的 SMB2 命令（MS-SMB2 §3.3.5.16）。
	// registerNoResponse 会让分发器跳过前置检查（会话定位、验签、
	// needSession/needTree）直接执行本 handler —— 因为这些检查失败时
	// 同样一个字节都不能回，检查也就失去了意义。
	registerNoResponse(wire.CommandCancel, handleCancel)
}

// handleCancel 处理 SMB2 CANCEL（MS-SMB2 §3.3.5.16）。
//
// 规范要点（每一条都很容易踩坑）：
//
//  1. **服务端绝不能为 CANCEL 请求本身发送任何响应**，连 ERROR Response
//     都不行。原因：CANCEL 复用被取消请求的 MessageId，任何以该 MessageId
//     回出的消息都会被客户端错配成被取消请求的应答，导致客户端状态机错乱。
//     返回值恒为 nil，调用方（Dispatch）也会忽略它 —— 这里刻意不返回
//     status.NotSupported 之类的错误。
//
//  2. 定位被取消的请求：Flags 含 SMB2_FLAGS_ASYNC_COMMAND 时用 AsyncId
//     匹配，否则用 (SessionId, MessageId) 匹配。
//
//  3. 命中时，**被取消的那条请求**回 STATUS_CANCELLED；未命中则静默丢弃
//     （客户端在响应与 CANCEL 交错时本来就会出现取消不到的情况，这不是错误）。
//
// 当前实现：本服务端所有 handler 都是**同步**执行的 —— 一条请求在读循环里
// 处理完才会读下一帧，因此当 CANCEL 抵达时，被它引用的那条请求要么早已完成
// 响应，要么就是同一复合帧里的兄弟消息（同样已同步执行完）。也就是说
// **不存在可被取消的未决请求**，第 2、3 步必然落在"未命中 → 静默丢弃"分支。
//
// TODO: 待实现异步未决请求表。等 CHANGE_NOTIFY / 阻塞式 LOCK / 长 READ 改成
// 异步（回 STATUS_PENDING + SMB2_FLAGS_ASYNC_COMMAND）之后，需要在 Conn 上
// 维护 map[cancelKey]*pendingRequest，在这里查表并触发其取消。
func handleCancel(ctx *Context) error {
	// 不解析报文体：CANCEL Request 的 body 只有 StructureSize(4) 与
	// Reserved，没有任何可用信息；而且**解析失败也无法回错误**，
	// 校验它除了浪费 CPU 没有别的作用（AGENTS.md §5 P6）。
	if ctx.Header.IsAsync() {
		ctx.Log.Debug("收到 SMB2 CANCEL（async），无匹配的未决请求，静默丢弃",
			"async_id", ctx.Header.AsyncID, "remote", ctx.Conn.RemoteAddr)
	} else {
		ctx.Log.Debug("收到 SMB2 CANCEL，无匹配的未决请求，静默丢弃",
			"message_id", ctx.Header.MessageID, "session", ctx.Header.SessionID,
			"remote", ctx.Conn.RemoteAddr)
	}
	return nil
}
