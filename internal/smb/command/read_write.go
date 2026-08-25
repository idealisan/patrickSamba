package command

import (
	"errors"
	"io"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandRead, true, true, handleRead)
	register(wire.CommandWrite, true, true, handleWrite)
	register(wire.CommandFlush, true, true, handleFlush)
}

// handleRead 处理 SMB2 READ（MS-SMB2 §3.3.5.12）。
func handleRead(ctx *Context) error {
	req, err := wire.ParseReadRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}

	// 长度上限：既不能超过协商出的 MaxReadSize，也不能让恶意的
	// Length 字段直接把服务端内存撑爆（AGENTS.md §8）。
	if req.Length > ctx.Conn.MaxReadSize {
		return status.InvalidParameter
	}
	if req.Length == 0 {
		// 读 0 字节：MS-SMB2 要求回 STATUS_END_OF_FILE 而不是空成功响应。
		return status.EndOfFile
	}

	if open.IsPipe() {
		return readPipe(ctx, open, req)
	}

	if open.IsDir {
		return status.InvalidDeviceRequest
	}
	if open.GrantedAccess&(wire.FileReadData|wire.FileExecute) == 0 {
		return status.AccessDenied
	}
	h := open.Handle
	if h == nil {
		return status.FileClosed
	}
	// offset + length 溢出防御：两者都是 64/32 位无符号，相加可能回绕。
	if req.Offset > uint64(1<<63-1)-uint64(req.Length) {
		return status.InvalidParameter
	}
	// 字节范围锁强制检查：别的句柄在目标区间上持独占锁时拒绝读。
	// 此前锁表只有 LOCK 命令自己在记账，READ/WRITE 完全不设防，
	// 依赖锁互斥的应用会真实数据竞争（bh4-A#1）。
	if open.Tree != nil && open.Tree.Share != nil {
		sh := open.Tree.Share
		if st := sh.locks.checkIO(open.Path, open, req.Offset, uint64(req.Length), false); st != status.Success {
			return st
		}
	}

	buf := make([]byte, req.Length)
	n, rerr := h.ReadAt(buf, int64(req.Offset))
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return status.FromVFSError(rerr)
	}
	if n == 0 {
		// 读到文件尾一个字节都没读到：MS-SMB2 §3.3.5.12 要求 END_OF_FILE。
		return status.EndOfFile
	}
	// MinimumCount 是客户端声明的「少于这个数就别回了」。
	if req.MinimumCount > 0 && uint32(n) < req.MinimumCount {
		return status.EndOfFile
	}

	resp := &wire.ReadResponse{Data: buf[:n]}
	out, aerr := resp.Append(ctx.Out)
	if aerr != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out
	return nil
}

// readPipe 从命名管道的待读缓冲取数据。
func readPipe(ctx *Context, open *Open, req *wire.ReadRequest) error {
	data := open.PipeRead(int(req.Length))
	if len(data) == 0 {
		// 管道里没有待读数据。客户端不该走到这里（它总是先 WRITE），
		// 回 END_OF_FILE 让它不要死等。
		return status.EndOfFile
	}

	resp := &wire.ReadResponse{
		Data:          data,
		DataRemaining: uint32(open.PipePending()),
	}
	out, err := resp.Append(ctx.Out)
	if err != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out

	if resp.DataRemaining > 0 {
		// 还有剩余：BUFFER_OVERFLOW 是「消息未读完」的标准信号，
		// 不是错误。客户端会继续 READ（MS-SMB2 §3.3.5.12）。
		ctx.Status = status.BufferOverflow
	}
	return nil
}

// handleWrite 处理 SMB2 WRITE（MS-SMB2 §3.3.5.13）。
func handleWrite(ctx *Context) error {
	req, err := wire.ParseWriteRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}

	if uint32(len(req.Data)) > ctx.Conn.MaxWriteSize {
		return status.InvalidParameter
	}

	if open.IsPipe() {
		return writePipe(ctx, open, req)
	}

	if err := ctx.RequireWritable(); err != nil {
		return err
	}
	if open.IsDir {
		return status.InvalidDeviceRequest
	}
	if open.GrantedAccess&(wire.FileWriteData|wire.FileAppendData) == 0 {
		return status.AccessDenied
	}
	h := open.Handle
	if h == nil {
		return status.FileClosed
	}
	if req.Offset > uint64(1<<63-1)-uint64(len(req.Data)) {
		return status.InvalidParameter
	}
	// 字节范围锁强制检查：别的句柄在目标区间上持任何锁（独占或共享）
	// 时拒绝写。零长度写在 checkIO 内天然放行（不占用字节）。
	if open.Tree != nil && open.Tree.Share != nil {
		sh := open.Tree.Share
		if st := sh.locks.checkIO(open.Path, open, req.Offset, uint64(len(req.Data)), true); st != status.Success {
			return st
		}
	}

	// 长度为 0 的写是合法的 no-op（客户端用它探测可写性）。
	var n int
	if len(req.Data) > 0 {
		n, err = h.WriteAt(req.Data, int64(req.Offset))
		if err != nil {
			return status.FromVFSError(err)
		}
	}

	// SMB2_WRITEFLAG_WRITE_THROUGH：本次写必须落盘后再回响应。
	if req.Flags&wire.WriteFlagWriteThrough != 0 ||
		open.CreateOptions&wire.FileWriteThrough != 0 {
		if serr := h.Sync(false); serr != nil {
			return status.FromVFSError(serr)
		}
	}

	ctx.Out = (&wire.WriteResponse{Count: uint32(n)}).Append(ctx.Out)
	return nil
}

// writePipe 把 DCERPC 请求 PDU 送进命名管道。
//
// 响应不在这里返回 —— 客户端接下来会发 READ 来取（也可能它一开始就用
// IOCTL FSCTL_PIPE_TRANSCEIVE，那条路径见 ioctl.go）。
func writePipe(ctx *Context, open *Open, req *wire.WriteRequest) error {
	if len(req.Data) == 0 {
		ctx.Out = (&wire.WriteResponse{}).Append(ctx.Out)
		return nil
	}

	// 管道响应的上限用协商出的 MaxReadSize —— 客户端此刻还没告诉我们
	// 它打算用多大的缓冲来读，用读上限是安全且足够的估计。
	err := open.PipeTransact(req.Data, int(ctx.Conn.MaxReadSize))
	if err != nil && !errors.Is(err, ErrPipeMoreData) {
		ctx.Log.Warn("命名管道处理失败", "pipe", open.Path, "err", err)
		return status.FromVFSError(err)
	}

	// WRITE 永远报「全部收下了」：DCERPC 是消息语义，部分接受没有意义。
	ctx.Out = (&wire.WriteResponse{Count: uint32(len(req.Data))}).Append(ctx.Out)
	return nil
}

// handleFlush 处理 SMB2 FLUSH（MS-SMB2 §3.3.5.14）。
//
// full=true：SMB2 FLUSH 的语义是「数据真正落到持久介质」，对应
// macOS 的 F_FULLFSYNC。Time Machine 依赖这个语义保证备份一致性
// （AGENTS.md §2 阶段二）。
func handleFlush(ctx *Context) error {
	req, err := wire.ParseFlushRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if open.IsPipe() {
		// 管道没有可刷的东西，直接成功。
		ctx.Out = (&wire.FlushResponse{}).Append(ctx.Out)
		return nil
	}
	if h := open.Handle; h != nil && !open.IsDir {
		if serr := h.Sync(true); serr != nil {
			return status.FromVFSError(serr)
		}
	}

	ctx.Out = (&wire.FlushResponse{}).Append(ctx.Out)
	return nil
}
