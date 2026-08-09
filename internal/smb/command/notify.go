package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandChangeNotify, true, true, handleChangeNotify)
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
