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
// 异步未决请求表（async.go）建起来之后，CHANGE_NOTIFY 与阻塞 LOCK
// 都会通过 Context.Defer 把请求挂起并登记进表，于是第 2、3 步真正生效：
// 命中即回 STATUS_CANCELLED，未命中静默丢弃。
//
// 仍然**不同步**执行的长命令（例如一个大 WRITE）不会被 CANCEL 打断 ——
// 它们在读循环里一口气跑完，CANCEL 抵达时结果已经发出去了。这不违反
// 规范：CANCEL 是"尽力而为"，客户端必须能处理"取消不到"的情况。
func handleCancel(ctx *Context) error {
	// 不解析报文体：CANCEL Request 的 body 只有 StructureSize(4) 与
	// Reserved，没有任何可用信息；而且**解析失败也无法回错误**，
	// 校验它除了浪费 CPU 没有别的作用（AGENTS.md §5 P6）。
	if ctx.Conn.cancelPending(cancelKey(ctx.Header)) {
		ctx.Log.Debug("已取消一条未决请求",
			"async", ctx.Header.IsAsync(),
			"async_id", ctx.Header.AsyncID,
			"message_id", ctx.Header.MessageID,
			"session", ctx.Header.SessionID)
		return nil
	}
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
