package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandChangeNotify, true, true, handleChangeNotify)
	register(wire.CommandOplockBreak, true, true, handleOplockBreak)
}

// handleChangeNotify 处理 SMB2 CHANGE_NOTIFY（MS-SMB2 §3.3.5.19）。
//
// 规范语义是**长挂起**：服务端把请求收下、回 STATUS_PENDING，等目录真的
// 发生变化时再补发一条带 FILE_NOTIFY_INFORMATION 的响应；客户端会一直挂着
// 直到超时或收到通知。
//
// 本服务当前所有 handler 都是同步执行的，没有异步未决请求表，也没有文件
// 系统监视（inotify/kqueue/ReadDirectoryChangesW 都是平台相关的，还要跨
// linux/darwin/windows 各写一套）。在这种情况下有三个选择：
//
//  1. 回 STATUS_PENDING 然后永不补发 —— 客户端会挂到超时，最糟；
//  2. 立刻回一个空的成功响应 —— 客户端会理解成"目录变了但没细节"，
//     于是立刻重新发起 notify，形成忙循环；
//  3. 回 STATUS_NOT_SUPPORTED —— 客户端降级为定时轮询目录。
//
// 选 3。Samba 在 `change notify = no` 时也是这么答的，Windows 资源管理器与
// macOS Finder 都能正确降级。
//
// TODO: 待实现异步未决请求表 + 跨平台目录监视后改为规范语义。
func handleChangeNotify(ctx *Context) error {
	req, err := wire.ParseChangeNotifyRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	// 即便不支持，也要先把句柄校验做掉：句柄非法时回句柄错误比回
	// NOT_SUPPORTED 更准确，客户端据此能区分"服务端不支持"和"我用错句柄了"。
	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if !open.IsDir {
		// CHANGE_NOTIFY 只能作用于目录句柄（MS-SMB2 §3.3.5.19）。
		return status.InvalidParameter
	}

	return status.NotSupported
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
