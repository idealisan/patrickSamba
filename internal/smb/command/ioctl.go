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
// 未实现的控制码回什么，见 unimplementedFSCTL —— 不是 STATUS_NOT_SUPPORTED。
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
		return unimplementedFSCTL(ctx, req)
	}
}

// unimplementedFSCTL 是所有我们没实现的控制码的统一出口。
//
// **不回 STATUS_NOT_SUPPORTED**，这一点是刻意的，依据是真实 Samba 的行为
// （AGENTS.md §9 真实客户端行为优先）：Samba 的
// `source3/smbd/smb2_ioctl_network_fs.c` 在 default 分支里把底层
// `SMB_VFS_FSCTL` 返回的 NT_STATUS_NOT_SUPPORTED **改写**成：
//
//	磁盘树   → STATUS_INVALID_DEVICE_REQUEST
//	IPC$ 树  → STATUS_FS_DRIVER_REQUIRED
//
// 客户端的错误处理路径是照着 Samba 的行为写的：收到 NOT_SUPPORTED 时有些
// 客户端会重试到超时，而收到 INVALID_DEVICE_REQUEST 会立刻优雅退化。
//
// 两个树类型要分开是因为 IPC$ 上「设备请求非法」讲不通 —— 那里根本没有设备，
// 缺的是能处理该控制码的文件系统驱动。
func unimplementedFSCTL(ctx *Context, req *wire.IoctlRequest) error {
	if ctx.Tree != nil && ctx.Tree.Share != nil && ctx.Tree.Share.IsIPC() {
		ctx.Log.Debug("未实现的 FSCTL（IPC$）", "ctl", req.CtlCode)
		return status.FSDriverRequired
	}
	ctx.Log.Debug("未实现的 FSCTL", "ctl", req.CtlCode)
	return status.InvalidDeviceRequest
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
		// MS-FSA §2.1.5.9.29：FILE_WRITE_DATA / FILE_WRITE_ATTRIBUTES /
		// FILE_APPEND_DATA 之一即可。bh5-F8 补上 APPEND_DATA —— Windows
		// Server 2008/2012 同样认它（Samba dosmode.c:1118-1128 引的正是
		// 这三家任一）。宽掩码只服务 SET_SPARSE；SET_ZERO_DATA 在自己的
		// handler 里仍单独要求 FILE_WRITE_DATA（置零是真写语义）。
		if open.GrantedAccess&(wire.FileWriteData|wire.FileWriteAttributes|wire.FileAppendData) == 0 {
			return nil, nil, status.AccessDenied
		}
	}

	sp, _ := h.(vfs.SparseFile)
	return open, sp, nil
}

// ioctlSetSparse 处理 FSCTL_SET_SPARSE（MS-FSCC §2.3.69 / MS-FSA §2.1.5.9.29）。
//
// 输入是**可选的** 1 字节 FILE_SET_SPARSE_BUFFER{ SetSparse BOOLEAN }，
// 规范明确规定输入为空时视为 TRUE。无输出。
//
// POSIX 语义差异：Unix 上没有「稀疏标志位」这个东西 —— 只要底层文件系统支持
// 打洞，任何文件天生就能变稀疏，不需要事先声明。于是：
//
//	SetSparse(TRUE)  → 无操作成功，客户端要的效果本来就成立；
//	SetSparse(FALSE) → STATUS_NOT_SUPPORTED，我们**真的做不到**。
//
// FALSE 为什么不谎称成功：本实现的 FILE_ATTRIBUTE_SPARSE_FILE 不是存下来的
// 标志位，而是由 `Alloc < Size` 现算的（vfs/attr.go）。假装取消成功，客户端
// 回头查属性会照样看到 SPARSE 位，得到一个自相矛盾的视图。真要取消就得把所有
// 洞填零 —— 一个 8 MiB 的 Time Machine band 会从占几 KiB 涨到占满 8 MiB，
// 绝不能作为某个 FSCTL 的副作用悄悄发生。
//
// ⚠️ 这里与 Samba 有意分歧：Samba 把稀疏位**存进 user.DOSATTRIB 扩展属性**
// （source3/smbd/dosmode.c:file_set_sparse），所以它的 SET_SPARSE(FALSE) 只是
// 清一个 bit；之后 fsctl_qar 见 `!fsp->fsp_flags.is_sparse` 就直接谎报「整个
// 区间已分配」。那套模型要求属性有持久化后端，我们的属性是现算的，学不来。
// 实测客户端只发 TRUE（macOS 建 .sparsebundle、Windows 建 VHD 都是），
// 没有已知客户端依赖 FALSE。若将来真遇到，正确的退让是「文件本来就没有洞
// （Alloc >= Size）时把 FALSE 当无操作成功」，而不是回去无条件谎称成功。
func ioctlSetSparse(ctx *Context, req *wire.IoctlRequest) error {
	open, sp, err := sparseTarget(ctx, req, true)
	if err != nil {
		return err
	}

	// bh5-F9：流句柄上 SET_SPARSE 无操作成功。
	//
	// Samba 的 vfswrap_fsctl 开头就 fsp = metadata_fsp(fsp)，对流句柄的
	// 稀疏位设置更是直接假装成功（dosmode.c:1147-1155，引 MS-FSA §2.1.1.5：
	// 流永远不是稀疏文件，设不设都改变不了它的形态）。回 NOT_SUPPORTED 会让
	// 对 `file:stream` 句柄做常规稀疏初始化的客户端把整个共享当成「不支持
	// 稀疏」而放弃打洞 —— 比假装成功伤害大得多。macOS 对 band 文件本体
	// 操作，正常流量走不到这里；这是对齐参照实现的兜底。
	if sp == nil {
		if open.Stream == "" {
			// 主数据流且后端没有打洞能力：明确回不支持，别假装成功。
			// 假装成功会让客户端以为空间已释放，容量统计从此对不上。
			return status.NotSupported
		}
		return ioctlEmptyOK(ctx, req)
	}

	// ⚠️ 输入为空必须视为 TRUE（MS-FSCC §2.3.69）。macOS 与 Windows 都会发
	// 不带 Input 的 SET_SPARSE，按 FALSE 处理的话 .sparsebundle 的 band
	// 一个都稀疏不了。
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
	// bh5-F9：流句柄与无稀疏能力的后端都如实回 NOT_SUPPORTED（同 QAR，
	// 见 ioctlQueryAllocatedRanges 里对 metadata_fsp 映射的记录）。
	if sp == nil {
		return status.NotSupported
	}

	z, perr := wire.ParseZeroDataInput(req.Input)
	if perr != nil {
		return status.InvalidParameter
	}
	if n := z.BeyondFinalZero - z.FileOffset; n > 0 {
		// bh5-F4：strict locking 检查。打洞会改写区间内容，属于写操作，
		// 必须与 READ/WRITE 入口共用同一张锁表、同一个冲突矩阵
		// （lockTable.checkIO，WRITE_LOCK 语义）。Samba 对照：
		// smb2_ioctl_filesys.c:459-468 对 zero_data 先做
		// SMB_VFS_STRICT_LOCK_CHECK，冲突回 NT_STATUS_FILE_LOCK_CONFLICT；
		// 缺了这道闸，持锁客户端锁定的字节会被别人的打洞悄悄清零。
		//
		// 键必须与 handleLock 的登记键一致（open.Path），豁免单位同样是
		// 句柄 —— 自己的锁不挡自己的打洞。
		if open.Tree == nil || open.Tree.Share == nil {
			return status.NetworkNameDeleted
		}
		if st := open.Tree.Share.locks.checkIO(open.Path, open,
			uint64(z.FileOffset), uint64(n), true); st != status.Success {
			return st
		}
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
	// bh5-F9：流句柄与无稀疏能力的后端都如实回 NOT_SUPPORTED（sp 为 nil）。
	// Samba 在 vfswrap_fsctl 开头 fsp = metadata_fsp(fsp) 把 fsctl 映射到
	// 基础文件；我们不在命令层为流重开基础文件 —— 那要绕过共享模式检查并
	// 凭空多出一个 fd，代价远超收益（macOS 对 band 本体操作，正常流量
	// 走不到这条），记录在案即可（findings-bh5 F9 修复方向原文允许）。
	if sp == nil {
		return status.NotSupported
	}
	// bh5-F7：读区间分布只要求 FILE_READ_DATA。此前放宽到「读或写任一」，
	// 让只写句柄也能探到分配分布；Samba 只认 READ_DATA
	// （smb2_ioctl_filesys.c:632 check_any_access_fsp(fsp, FILE_READ_DATA)），
	// 收紧对齐。
	if open.GrantedAccess&wire.FileReadData == 0 {
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
	if len(vr) == 0 {
		// 查询长度为 0、文件为空、窗口整个落在 EOF 之外、或区间内全是空洞：
		// 空输出 + STATUS_SUCCESS 是合法应答，**不是**错误。
		// Samba fsctl_qar 在这几种情况下同样直接 `return NT_STATUS_OK` 且
		// 不写 out_output（source3/smbd/smb2_ioctl_filesys.c）。
		// 注意这条早退在缓冲区大小检查**之前**，与 Samba 的顺序一致。
		return ioctlEmptyOK(ctx, req)
	}
	// 连一条区间都装不下：Samba 回 NT_STATUS_BUFFER_TOO_SMALL 而不是
	// BUFFER_OVERFLOW（"must have enough space for at least one range"）。
	// 这里跟随 Samba —— 它才是 smbclient/macOS 期望的对端行为。
	if req.MaxOutputResponse < allocatedRangeSize {
		return status.BufferTooSmall
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
