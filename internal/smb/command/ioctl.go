package command

import (
	"errors"
	"math"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
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
	case wire.FSCTLSetSparse:
		return ioctlSetSparse(ctx, req)
	case wire.FSCTLSetZeroData:
		return ioctlSetZeroData(ctx, req)
	case wire.FSCTLQueryAllocatedRanges:
		return ioctlQueryAllocatedRanges(ctx, req)
	case wire.FSCTLSrvEnumerateSnapshots:
		return ioctlEnumerateSnapshots(ctx, req)
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
func ioctlValidateNegotiate(ctx *Context, req *wire.IoctlRequest) error {
	c := ctx.Conn

	// MS-SMB2 §3.3.5.15.12：3.1.1 用 preauth integrity hash 覆盖了降级攻击
	// 的威胁模型，客户端**不得**发这个 IOCTL；收到即视为对端行为异常，必须
	// 回 STATUS_FILE_CLOSED（Windows 会直接 TCP RESET）。
	if c.Dialect == dialect.SMB311 {
		return status.FileClosed
	}
	// 连接尚未完成协商：没有可复核的基准，直接拒绝。
	if c.Dialect == 0 {
		return status.FileClosed
	}

	// 2.0.2 / 2.1 / 3.0 / 3.0.2 一律正常复核并回显。
	//
	// 规范建议对 < 3.0 的方言也回 STATUS_FILE_CLOSED，但实测 smbclient 4.22
	// 会在 SMB 2.1 上发 FSCTL_VALIDATE_NEGOTIATE_INFO，收到 FILE_CLOSED 就
	// 放弃整条连接（表现为 "tree connect failed: NT_STATUS_..."）。
	// 据 AGENTS.md §9「真实客户端行为优先于规范」，这里按客户端期望处理，
	// 否则 §2 要求的 SMB 2.0.2 / 2.1 文件共享对 smbclient 不可用。
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

// ---------------------------------------------------------------------------
// 稀疏文件三兄弟：SET_SPARSE / SET_ZERO_DATA / QUERY_ALLOCATED_RANGES
//
// 这是 Time Machine 的关键路径（AGENTS.md §2 阶段二）：.sparsebundle 由几万个
// 固定大小的 band 文件组成，备份过期回收时 macOS 会把 band 里的区间打洞释放，
// 再用 QUERY_ALLOCATED_RANGES 核对哪些区间还占着盘。没有这三个 FSCTL，
// 备份卷只会越长越大、永远不会缩小。
//
// 客户端只有在 FileFsAttributeInformation 宣告了 FILE_SUPPORTS_SPARSE_FILES
// 时才会发它们（见 query_info.go 的 fsAttributes）。
// ---------------------------------------------------------------------------

// sparseTarget 做这三个 FSCTL 共同的前置校验，返回可用的 VFS 稀疏能力。
//
// write 为 true 时额外要求共享可写与写权限。
func sparseTarget(ctx *Context, req *wire.IoctlRequest, write bool) (*Open, vfs.SparseFile, error) {
	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return nil, nil, err
	}
	if open.IsPipe() {
		return nil, nil, status.InvalidDeviceRequest
	}
	// 目录不可能是稀疏文件（MS-FSA §2.1.5.9.29 / §2.1.5.9.21）。
	if open.IsDir {
		return nil, nil, status.InvalidParameter
	}
	h := open.Handle
	if h == nil {
		return nil, nil, status.FileClosed
	}
	if write {
		if err := ctx.RequireWritable(); err != nil {
			return nil, nil, err
		}
		// MS-FSA §2.1.5.9.29：FILE_WRITE_DATA 或 FILE_WRITE_ATTRIBUTES 之一即可。
		if open.GrantedAccess&(wire.FileWriteData|wire.FileWriteAttributes) == 0 {
			return nil, nil, status.AccessDenied
		}
	}

	sp, ok := h.(vfs.SparseFile)
	if !ok {
		// 后端没有打洞能力：明确回不支持，别假装成功。
		// 假装成功会让客户端以为空间已释放，容量统计从此对不上。
		return nil, nil, status.NotSupported
	}
	return open, sp, nil
}

// ioctlSetSparse 处理 FSCTL_SET_SPARSE（MS-FSCC §2.3.69 / MS-FSA §2.1.5.9.29）。
//
// 输入是**可选的** 1 字节 FILE_SET_SPARSE_BUFFER{ SetSparse BOOLEAN }，
// 规范明确规定输入为空时视为 TRUE。无输出。
//
// POSIX 语义差异：Unix 上没有「稀疏标志位」这个东西 —— 只要底层文件系统支持
// 打洞，任何文件天生就能变稀疏，不需要事先声明。vfs 层据此把 SetSparse 实现成
// 无操作（见 vfs.SparseFile.SetSparse 的注释：回 ErrNotSupported 会让 macOS
// 在建 .sparsebundle 时直接放弃）。
func ioctlSetSparse(ctx *Context, req *wire.IoctlRequest) error {
	open, sp, err := sparseTarget(ctx, req, true)
	if err != nil {
		return err
	}

	setSparse := true
	if len(req.Input) > 0 {
		setSparse = req.Input[0] != 0
	}
	if err := sp.SetSparse(setSparse); err != nil {
		ctx.Log.Warn("SET_SPARSE 失败", "path", open.Path, "sparse", setSparse, "err", err)
		return status.FromVFSError(err)
	}
	return ioctlEmptyOK(ctx, req)
}

// ioctlSetZeroData 处理 FSCTL_SET_ZERO_DATA（MS-FSCC §2.3.79 / MS-FSA §2.1.5.9.40）：
// 把 [FileOffset, BeyondFinalZero) 置零。
//
// 规范允许两种实现：真写零，或者（稀疏文件）打洞。我们走打洞 —— 读回来同样是零，
// 但盘上空间真的还给了文件系统，这正是 Time Machine 回收 band 的意义所在。
func ioctlSetZeroData(ctx *Context, req *wire.IoctlRequest) error {
	open, sp, err := sparseTarget(ctx, req, true)
	if err != nil {
		return err
	}
	// 置零是写操作，WRITE_ATTRIBUTES 不够（MS-FSA §2.1.5.9.40）。
	if open.GrantedAccess&wire.FileWriteData == 0 {
		return status.AccessDenied
	}

	z, perr := wire.ParseZeroDataInput(req.Input)
	if perr != nil {
		return status.InvalidParameter
	}
	if n := z.BeyondFinalZero - z.FileOffset; n > 0 {
		if err := sp.PunchHole(z.FileOffset, n); err != nil {
			ctx.Log.Warn("SET_ZERO_DATA 打洞失败", "path", open.Path, "err", err)
			return status.FromVFSError(err)
		}
	}
	return ioctlEmptyOK(ctx, req)
}

// ioctlQueryAllocatedRanges 处理 FSCTL_QUERY_ALLOCATED_RANGES
// （MS-FSCC §2.3.20 / MS-FSA §2.1.5.9.21）。
//
// 输入是单个 FILE_ALLOCATED_RANGE_BUFFER（16 字节小端，描述要查询的区间），
// 输出是同结构的**数组**，列出该区间内真正占着盘的部分。
func ioctlQueryAllocatedRanges(ctx *Context, req *wire.IoctlRequest) error {
	open, sp, err := sparseTarget(ctx, req, false)
	if err != nil {
		return err
	}
	// 读区间分布至少要有读或写权限之一。macOS 的 band 文件是读写打开的。
	if open.GrantedAccess&(wire.FileReadData|wire.FileWriteData) == 0 {
		return status.AccessDenied
	}

	in, perr := wire.ParseAllocatedRangesInput(req.Input)
	if perr != nil {
		return status.InvalidParameter
	}
	// 负值与相加溢出都必须挡掉（AGENTS.md §8）。
	if in.FileOffset < 0 || in.Length < 0 || in.FileOffset > math.MaxInt64-in.Length {
		return status.InvalidParameter
	}

	vr, qerr := sp.AllocatedRanges(in.FileOffset, in.Length)
	if qerr != nil {
		ctx.Log.Warn("QUERY_ALLOCATED_RANGES 失败", "path", open.Path, "err", qerr)
		return status.FromVFSError(qerr)
	}
	ranges := make([]wire.FileAllocatedRangeBuffer, len(vr))
	for i, r := range vr {
		ranges[i] = wire.FileAllocatedRangeBuffer{FileOffset: r.Offset, Length: r.Length}
	}

	// 输出装不下时截断到整数个区间并回 STATUS_BUFFER_OVERFLOW —— 这是规范
	// 规定的正常流程（客户端据此缩小查询区间重试），不是错误。
	out, overflow := truncateAllocatedRanges(ranges, req.MaxOutputResponse)
	if err := appendIoctlResponse(ctx, req, out); err != nil {
		return err
	}
	if overflow {
		ctx.Status = status.BufferOverflow
	}
	return nil
}

// allocatedRangeSize 是 FILE_ALLOCATED_RANGE_BUFFER 的字节数
// （FileOffset INT64 + Length INT64，MS-FSCC §2.3.21.1）。
const allocatedRangeSize = 16

// truncateAllocatedRanges 把区间数组编码进不超过 maxOut 字节的缓冲。
//
// 只能按**整个** FILE_ALLOCATED_RANGE_BUFFER 截断，切一半会让客户端解析出
// 一个错误的区间。返回是否发生了截断。
func truncateAllocatedRanges(ranges []wire.FileAllocatedRangeBuffer, maxOut uint32) ([]byte, bool) {
	fit := int(maxOut / allocatedRangeSize)
	overflow := false
	if fit < len(ranges) {
		ranges = ranges[:fit]
		overflow = true
	}
	if len(ranges) == 0 {
		// 全是空洞（或一条都装不下）：空输出是合法响应。
		return nil, overflow
	}
	return wire.AppendAllocatedRanges(nil, ranges), overflow
}

// ioctlEnumerateSnapshots 处理 FSCTL_SRV_ENUMERATE_SNAPSHOTS
// （MS-SMB2 §3.3.5.15.1，输出结构 §2.2.32.2）。
//
// 我们**不做卷影副本**，但必须回「0 个快照」而不是 STATUS_INVALID_DEVICE_REQUEST：
//   - smbclient 每次 `allinfo` 都会查它，报错会在输出里刷
//     "NT_STATUS_INVALID_DEVICE_REQUEST getting shadow copy data"；
//   - Windows 资源管理器的「以前的版本」属性页也查，报错会让该页卡住。
//
// 零快照时的确切字节布局（规范文本两可）已由 wire 层查实定案，
// 见 wire.SrvSnapshotArray 上方那段注释：输出最少 16 字节，
// SnapShotArraySize 按 n*50+2 上报。这里只负责流程。
func ioctlEnumerateSnapshots(ctx *Context, req *wire.IoctlRequest) error {
	// 必须是有效句柄：本 FSCTL 是对某个已打开对象所在卷发起的查询。
	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if open.IsPipe() {
		// 管道所在的 IPC$ 没有卷，也就没有快照。
		return status.InvalidDeviceRequest
	}

	// §3.3.5.15.1 原文：「If the MaxOutputResponse of the request is less than
	// 16 bytes, the server MUST fail the request with STATUS_INVALID_PARAMETER.」
	// 注意是 INVALID_PARAMETER 而不是 BUFFER_TOO_SMALL —— 16 是这个 FSCTL 的
	// 硬性下限，连「只回计数」都装不下，属于请求本身不合理。
	if req.MaxOutputResponse < wire.SrvSnapshotArrayMinSize {
		return status.InvalidParameter
	}

	// 传 nil：本服务端的 Share.SnapshotList 恒为空。
	// 装不下的截断逻辑在 NewSrvSnapshotArray 里，这里没有快照所以用不上。
	arr := wire.NewSrvSnapshotArray(nil, req.MaxOutputResponse)
	return appendIoctlResponse(ctx, req, arr.Encode())
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
