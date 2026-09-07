package command

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandRead, true, true, handleRead)
	register(wire.CommandWrite, true, true, handleWrite)
	register(wire.CommandFlush, true, true, handleFlush)
}

// creditChargeQuantum 是单个 credit 覆盖的载荷字节数
// （MS-SMB2 §3.3.5.2.6，SMB2_PER_CREDIT_PAYLOAD_SIZE = 65536）。
const creditChargeQuantum uint64 = 64 * 1024

// creditChargeCovers 校验头部声明的 CreditCharge 是否覆盖载荷（bh4-A#12）。
//
// 规范要求（MS-SMB2 §3.3.5.2.6）：CreditCharge >= ceil(payload/65536)，
// 少付即 STATUS_INVALID_PARAMETER —— 否则多信用客户端可以少付 credit
// 跑大 IO。Samba 在 read/write 各自入口做同一校验
// （smbd_smb2_request_verify_creditcharge）。
//
// 保守策略：payload <= 一个 quantum 时恒放行。Windows 对小 IO 的
// charge=0 并不拒绝，各实现子 quantum 策略不一；这里只堵「大载荷少付」
// 这个真实策略洞。TODO: 待验证子 quantum 是否也该强制。
//
// 调用方必须先确认连接支持多信用（Conn.SupportsMultiCredit）：
// 2.0.2 上 CreditCharge 字段保留为 0（MS-SMB2 §3.3.5.4），校验会误伤。
func creditChargeCovers(declared uint16, payload uint64) bool {
	if payload <= creditChargeQuantum {
		return true
	}
	need := (payload + creditChargeQuantum - 1) / creditChargeQuantum
	return uint64(declared) >= need
}

// writeShouldSync 报告一次 WRITE 完成后是否需要落盘（bh4-A#13）。
//
// 触发条件（任一）：
//   - 请求带 SMB2_WRITEFLAG_WRITE_THROUGH（MS-SMB2 §2.2.21，任意方言）；
//   - 请求带 SMB2_WRITEFLAG_WRITE_UNBUFFERED 且方言 >= 3.0.2 —— Samba
//     smb2_write.c:289-293 同判：3.0.2+ 上 UNBUFFERED 置 write_through=true；
//     2.1/3.0 上该标志未定义，忽略；
//   - 打开时带了 FILE_WRITE_THROUGH 创建选项（MS-SMB2 §2.2.1.4.1）。
func writeShouldSync(reqFlags uint32, open *Open, connDialect dialect.Dialect) bool {
	if reqFlags&wire.WriteFlagWriteThrough != 0 {
		return true
	}
	if reqFlags&wire.WriteFlagWriteUnbuffer != 0 && connDialect >= dialect.SMB302 {
		return true
	}
	return open != nil && open.CreateOptions&wire.FileWriteThrough != 0
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
	// CreditCharge 必须覆盖载荷（bh4-A#12，MS-SMB2 §3.3.5.2.6）：
	// 多信用连接上请求 >64K 而少付 credit 的，拒绝。
	if ctx.Conn.SupportsMultiCredit() &&
		!creditChargeCovers(ctx.Header.CreditCharge, uint64(req.Length)) {
		return status.InvalidParameter
	}

	if open.IsPipe() {
		return readPipe(ctx, open, req)
	}

	// 目录与访问权检查必须先于一切「按长度/内容判定」的状态 —— 零长度读
	// 也不例外。此前 `Length==0 → END_OF_FILE` 排在它们前面，无权句柄或
	// 目录句柄读 0 字节会拿到 END_OF_FILE，等于向客户端泄露「先看长度后
	// 鉴权」的实现细节（bh4-A#3）。
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

	// ---- 单次分配读路径（v0.5 去双拷贝）----
	// 旧路径每次 READ 先 make([]byte, Length) 把文件读进临时缓冲，
	// 再由 Append 整体二次拷进响应缓冲 —— heap profile 里各占 34%/32%
	// （perf-v050 报告 §4）。现在把数据窗口直接开在响应缓冲里，
	// ReadAt 一步写进最终位置，线上字节序列与旧路径逐字节一致
	// （wire golden 与下方 reference 对照测试钉死）。
	if req.Length == 0 {
		// 零长度读没有可预留的数据窗口，走常规编码（16+1 占位定长）。
		// 语义同旧路径（Samba smb2_read.c:404-407 / torture read.c:92-97）：
		// length=0,min_count>0 → END_OF_FILE；否则成功回 0 字节。
		if req.MinimumCount > 0 {
			return status.EndOfFile
		}
		out, aerr := (&wire.ReadResponse{}).Append(ctx.Out)
		if aerr != nil {
			return status.InsuffServerResources
		}
		ctx.Out = out
		return nil
	}

	out, rsv, aerr := wire.ReserveReadResponse(ctx.Out, int(req.Length))
	if aerr != nil {
		return status.InsuffServerResources
	}
	n, rerr := h.ReadAt(rsv.Data, int64(req.Offset))
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// 预留的响应体由 Dispatch 的 fail→ResetBody 统一回滚到响应头，
		// 不留残迹 —— 与旧路径「读完才写缓冲」的错误面一致。
		return status.FromVFSError(rerr)
	}
	// 一个字节都没读到且确实请求了至少 1 字节 → END_OF_FILE
	// （offset ≥ EOF）；MinimumCount 不足同样 END_OF_FILE。
	if n == 0 {
		return status.EndOfFile
	}
	if req.MinimumCount > 0 && uint32(n) < req.MinimumCount {
		return status.EndOfFile
	}

	out, aerr = rsv.Commit(out, n)
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
	// CreditCharge 必须覆盖载荷（bh4-A#12，MS-SMB2 §3.3.5.2.6）。
	if ctx.Conn.SupportsMultiCredit() &&
		!creditChargeCovers(ctx.Header.CreditCharge, uint64(len(req.Data))) {
		return status.InvalidParameter
	}
	// DataOffset 精确校验（bh4-A#11）：Length>0 时必须等于 SMB2 头 +
	// 固定体长（64+48=112）。Samba smb2_write.c:74-76 同判；实测抓包
	// wire/testdata/capture/create-read-write/017-c2s-WRITE.bin 亦为 112。
	// 指到别处时数据会被从错误位置切片 —— 解析层只做边界校验拦不住它。
	//
	// 零长度写豁免：此时字段无意义，部分客户端发 0。先验长度再切片
	// （AGENTS.md §5）。
	//
	// TODO: 待 wire 包（他人所有）在 ParseWriteRequest 里暴露 DataOffset
	// 字段后把本检查挪进解析层。
	if len(req.Data) > 0 && len(ctx.Msg) >= 68 {
		// DataOffset 在 WRITE Request 体内偏移 2（MS-SMB2 §2.2.21），
		// 即消息绝对偏移 64+2=66，2 字节**小端**。
		if dataOffset := binary.LittleEndian.Uint16(ctx.Msg[66:68]); dataOffset != wire.HeaderSize+48 {
			return status.InvalidParameter
		}
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
	// FILE_ATTRIBUTE_READONLY 的目标拒绝写入（bh3-F4 写面）。
	// 判据是打开时的属性快照（create.go 填充），授权依据仍是配置与
	// 属性位，不读宿主 ACL（AGENTS.md §1.1）。已知边界：句柄存续期间
	// 目标被 SET_INFO(FileBasicInformation) 改掉 READONLY 位时，本快照
	// 不跟随 —— 与 Windows「打开时定生死」的主判定点一致，误差窗口极小。
	if open.FileAttributes&wire.FileAttributeReadonly != 0 {
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

	// 落盘判定（bh4-A#13）：WRITE_THROUGH 任意方言生效；
	// WRITE_UNBUFFERED 在 ≥3.0.2 上等价（Samba smb2_write.c:289-293）；
	// 打开选项 FILE_WRITE_THROUGH 独立生效。
	if writeShouldSync(req.Flags, open, ctx.Conn.Dialect) {
		if serr := h.Sync(false); serr != nil {
			return status.FromVFSError(serr)
		}
	}

	// 变更记账（CHANGE_NOTIFY）。
	//
	// 零长度写是客户端的**可写性探测**，不做任何改动，不记账 —— 否则一次
	// 探测就会触发一轮毫无意义的目录刷新，而 Finder/Explorer 的探测相当频繁。
	//
	// 流与主流分开记：改备用数据流不改变文件长度与最后写时间，
	// 对应的过滤位是 STREAM_SIZE / STREAM_WRITE（MS-SMB2 §2.2.35）。
	if n > 0 {
		if open.Stream != "" {
			open.notifyHub().notifyModified(open.Path,
				wire.NotifyChangeStreamSize|wire.NotifyChangeStreamWrite)
		} else {
			open.notifyHub().notifyModified(open.Path,
				wire.NotifyChangeSize|wire.NotifyChangeLastWrite)
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
//
// 访问校验（bh4-A#8，对照 Samba smb2_flush.c:171-199）：
//   - 管道：没有可刷的数据，直接成功。规范允许 NOT_IMPLEMENTED，
//     我们选更宽松的成功（对客户端无害，findings-bh4 A#8 认可）；
//   - 普通文件：需要 FILE_WRITE_DATA|FILE_APPEND_DATA，否则 ACCESS_DENIED
//     （只读句柄刷缓存本就无意义）；
//   - 目录：需要 FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY（与写权限同值，
//     wire 包按同值复用常量），否则 ACCESS_DENIED；目录本体不做 fsync，
//     与既有行为一致；
//   - 无底层 fd（attr-only 打开等）：STATUS_FILE_CLOSED，对应 Samba 的
//     fd==-1 ⇒ INVALID_HANDLE —— 不能静默成功假装刷过了。
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

	// 访问校验在一切状态判定之前：无权句柄连「能不能刷」都轮不到问，
	// 与 READ/WRITE 的鉴权顺序一致（bh4-A#3 的教训）。
	// 文件要 FILE_WRITE_DATA|FILE_APPEND_DATA，目录要
	// FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY —— MS-FSCT 里两组位同值
	// （0x2/0x4），wire 包按同值复用常量。
	if open.GrantedAccess&(wire.FileWriteData|wire.FileAppendData) == 0 {
		return status.AccessDenied
	}

	h := open.Handle
	if h == nil {
		return status.FileClosed
	}
	// 目录也要真的刷。
	//
	// 此前这里写的是 `if !open.IsDir`，目录句柄直接跳过 Sync 回成功。
	// 对照 Samba（source3/smbd/smb2_flush.c:155-196）并不是这样：
	// 它在访问校验里为目录单开一道（注释原话是"if opened with *either*
	// FILE_ADD_FILE or FILE_ADD_SUBDIRECTORY they can be flushed"），
	// 之后**不区分目录与否**，一律 SMB_VFS_FSYNC_SEND。
	//
	// 对我们的影响：AAPL 在 time_machine 共享上宣告了
	// kAAPL_SUPPORTS_FULL_SYNC，macOS 据此相信 FLUSH 之后数据已落盘。
	// 而 Time Machine 会大量建目录 —— 目录项不刷，断电后就是"备份显示成功、
	// 恢复时少文件"，且没有任何一方报错。
	if serr := h.Sync(true); serr != nil {
		// 某些平台/文件系统刷不了目录句柄（Windows 的目录句柄尤其可疑，
		// 本容器无法验证）。这种"做不到"不该让整次 FLUSH 失败 ——
		// 客户端（尤其是 Time Machine）会把 FLUSH 失败当成备份失败中止，
		// 而我们对这个错误其实无能为力。记为 WARN，其余照常成功。
		if errors.Is(serr, vfs.ErrNotSupported) {
			ctx.Log.Warn("FLUSH：本平台不支持刷目录句柄，目录项持久化无保证",
				"path", open.Path, "err", serr)
		} else {
			return status.FromVFSError(serr)
		}
	}

	ctx.Out = (&wire.FlushResponse{}).Append(ctx.Out)
	return nil
}
