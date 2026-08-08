package command

import (
	"errors"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	// IOCTL 不要求树连接：FSCTL_VALIDATE_NEGOTIATE_INFO 与
	// FSCTL_DFS_GET_REFERRALS 都可能在没有有效 TreeId 时到达。
	register(wire.CommandIoctl, true, false, handleIoctl)
}

// handleIoctl 处理 SMB2 IOCTL（MS-SMB2 §3.3.5.15）。
//
// 认不出的控制码一律回 STATUS_INVALID_DEVICE_REQUEST —— 这是 Windows 的行为，
// 客户端会据此优雅退化。回 NOT_SUPPORTED 有些客户端会重试到超时。
func handleIoctl(ctx *Context) error {
	req, err := wire.ParseIoctlRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}
	if !req.IsFSCTL() {
		// 我们不提供任何设备 IOCTL，只提供文件系统控制码。
		return status.InvalidDeviceRequest
	}

	switch req.CtlCode {
	case wire.FSCTLValidateNegotiateInfo:
		return ioctlValidateNegotiate(ctx, req)
	case wire.FSCTLPipeTransceive:
		return ioctlPipeTransceive(ctx, req)
	case wire.FSCTLQueryNetworkInterface:
		// 不宣告多通道（我们不支持 SMB3 multichannel），
		// 回空表示「没有额外接口」比回错误更让客户端满意。
		return ioctlEmptyOK(ctx, req)
	default:
		ctx.Log.Debug("未实现的 FSCTL", "ctl", req.CtlCode)
		return status.InvalidDeviceRequest
	}
}

// ioctlValidateNegotiate 处理 FSCTL_VALIDATE_NEGOTIATE_INFO（MS-SMB2 §3.3.5.15.12）。
//
// ⚠️ 这是防降级攻击的复核：客户端把它**当初发出的** NEGOTIATE 请求内容
// 再发一遍，要求服务端回显自己**当初响应过的**方言/能力/GUID/安全模式。
// 任何字段对不上，Windows 会立刻 TCP RESET，且不给任何提示。
//
// 只在 3.0 / 3.0.2 上有意义：3.1.1 用 preauth integrity hash 覆盖了同样的
// 威胁模型，客户端不会再发这个（MS-SMB2 §3.3.5.15.12 明确要求
// 3.1.1 时回 STATUS_FILE_CLOSED）。
func ioctlValidateNegotiate(ctx *Context, req *wire.IoctlRequest) error {
	c := ctx.Conn

	if c.Dialect == dialect.SMB311 {
		// 规范要求的行为：3.1.1 上收到它说明对端行为异常。
		return status.FileClosed
	}
	if c.Dialect < dialect.SMB300 {
		return status.FileClosed
	}

	in, err := wire.ParseValidateNegotiateInfoRequest(req.Input)
	if err != nil {
		// 输入畸形也按「校验失败」处理：断连比放行降级攻击安全。
		ctx.Log.Warn("VALIDATE_NEGOTIATE_INFO 输入畸形", "err", err)
		return status.AccessDenied
	}

	// 复核客户端重放的内容与它当初 NEGOTIATE 时说的一致。
	// 不一致说明中间有人改过报文（MS-SMB2 §3.3.5.15.12）。
	if in.ClientGUID != c.ClientGUID ||
		in.SecurityMode != c.ClientSecurityMode ||
		in.Capabilities != c.ClientCapabilities ||
		!sameDialects(in.Dialects, c.ClientDialects) {
		ctx.Log.Warn("VALIDATE_NEGOTIATE_INFO 校验失败，疑似降级攻击",
			"remote", c.RemoteAddr)
		return status.AccessDenied
	}

	resp := &wire.ValidateNegotiateInfoResponse{
		Capabilities: c.ServerCapabilities,
		ServerGUID:   c.Settings.ServerGUID,
		SecurityMode: c.ServerSecurityMode,
		Dialect:      wire.Dialect(c.Dialect),
	}
	return appendIoctlResponse(ctx, req, resp.Encode())
}

// ioctlPipeTransceive 处理 FSCTL_PIPE_TRANSCEIVE（MS-SMB2 §3.3.5.15.10）：
// 在一次往返里完成命名管道的写+读。这是现代客户端做 DCERPC 调用的默认方式。
func ioctlPipeTransceive(ctx *Context, req *wire.IoctlRequest) error {
	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if !open.IsPipe() {
		return status.InvalidDeviceRequest
	}
	if len(req.Input) == 0 {
		return status.InvalidParameter
	}

	maxOut := int(req.MaxOutputResponse)
	if maxOut <= 0 {
		return status.InvalidParameter
	}
	// 上限同时受协商出的 MaxTransactSize 约束（AGENTS.md §8 资源上限）。
	if maxOut > int(ctx.Conn.MaxTransactSize) {
		maxOut = int(ctx.Conn.MaxTransactSize)
	}

	terr := open.PipeTransact(req.Input, maxOut)
	if terr != nil && !errors.Is(terr, ErrPipeMoreData) {
		ctx.Log.Warn("命名管道 transceive 失败", "pipe", open.Path, "err", terr)
		return status.FromVFSError(terr)
	}

	data := open.PipeRead(maxOut)
	if err := appendIoctlResponse(ctx, req, data); err != nil {
		return err
	}
	if open.PipePending() > 0 {
		// 响应没吐完：BUFFER_OVERFLOW 通知客户端继续 READ。
		ctx.Status = status.BufferOverflow
	}
	return nil
}

// ioctlEmptyOK 回一个成功但输出为空的 IOCTL 响应。
func ioctlEmptyOK(ctx *Context, req *wire.IoctlRequest) error {
	return appendIoctlResponse(ctx, req, nil)
}

// appendIoctlResponse 组装并追加 IOCTL Response。
func appendIoctlResponse(ctx *Context, req *wire.IoctlRequest, output []byte) error {
	if uint32(len(output)) > req.MaxOutputResponse {
		return status.BufferTooSmall
	}
	resp := &wire.IoctlResponse{
		CtlCode: req.CtlCode,
		FileID:  req.FileID,
		Flags:   req.Flags,
		Output:  output,
	}
	out, err := resp.Append(ctx.Out)
	if err != nil {
		ctx.Log.Error("编码 IOCTL Response 失败", "ctl", req.CtlCode, "err", err)
		return status.InsuffServerResources
	}
	ctx.Out = out
	return nil
}

// sameDialects 比较两个方言列表是否完全一致（顺序敏感）。
//
// 顺序敏感是有意的：VALIDATE_NEGOTIATE_INFO 的意义就是逐字节复核客户端
// 当初发过的内容，放松成集合比较会给攻击者留下重排的空间。
func sameDialects(a []wire.Dialect, b []dialect.Dialect) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if uint16(a[i]) != uint16(b[i]) {
			return false
		}
	}
	return true
}
