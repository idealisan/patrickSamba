package command

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// FSCTL_SRV_ENUMERATE_SNAPSHOTS 的 handler 测试。
//
// 我们不做卷影副本，这个 FSCTL 唯一的价值就是**把"没有快照"这件事说清楚**：
// 回错误会让 smbclient 每次 allinfo 都刷一行假故障、让 Windows 的
// 「以前的版本」属性页卡住。所以测试的重点不是功能，而是
// 「零快照的应答字节到底长什么样」—— 这正是规范文本含糊、
// 只能靠对齐真实实现来定的部分（详见 wire.SrvSnapshotArray 的注释）。

// TestIoctlEnumerateSnapshotsEmpty：没有快照时回一个格式正确的空数组。
func TestIoctlEnumerateSnapshotsEmpty(t *testing.T) {
	ctx, req := newSparseCtx(t, 0)
	req.CtlCode = wire.FSCTLSrvEnumerateSnapshots
	req.MaxOutputResponse = 65536 // smbclient allinfo 用的就是 64K

	if err := ioctlEnumerateSnapshots(ctx, req); err != nil {
		t.Fatalf("零快照应回成功，实际 err=%v", err)
	}

	out := ioctlRespOutput(t, ctx.Out)

	// 最少 16 字节：Samba 客户端 cli_smb2_shadow_copy_data_fnum_recv 硬性要求
	// length >= 16，只回 Windows 那种 14 字节形式会被判 INVALID_NETWORK_RESPONSE。
	if len(out) < wire.SrvSnapshotArrayMinSize {
		t.Fatalf("输出 %d 字节，少于 smbclient 要求的 %d 字节下限",
			len(out), wire.SrvSnapshotArrayMinSize)
	}

	arr, err := wire.ParseSrvSnapshotArray(out)
	if err != nil {
		t.Fatalf("自己编的应答自己解不动: %v", err)
	}
	if arr.NumberOfSnapShots != 0 {
		t.Errorf("NumberOfSnapShots = %d, 期望 0", arr.NumberOfSnapShots)
	}
	if len(arr.SnapShots) != 0 {
		t.Errorf("SnapShots = %v, 期望空", arr.SnapShots)
	}
	// 零快照时 SnapShotArraySize 仍是 2（末尾那个 UTF-16 NUL），不是 0。
	// 依据是 MS-SMB2 附录 A 注 <68> 与 Samba 的 labels_data_count = n*50 + 2，
	// 两边独立一致。填 0 在 Windows 上会让「以前的版本」页表现异常。
	if got, want := arr.SnapShotArraySize, wire.SrvSnapshotArraySize(0); got != want {
		t.Errorf("SnapShotArraySize = %d, 期望 %d", got, want)
	}
}

// TestIoctlEnumerateSnapshotsMaxOutputTooSmall：MaxOutputResponse < 16
// 必须回 STATUS_INVALID_PARAMETER。
//
// ⚠️ 这里**不是** BUFFER_TOO_SMALL，容易写错。MS-SMB2 §3.3.5.15.1 原文：
// "If the MaxOutputResponse of the request is less than 16 bytes, the server
// MUST fail the request with STATUS_INVALID_PARAMETER."
// 语义上 16 是这个 FSCTL 的硬下限 —— 连"只回个计数"都装不下，
// 属于请求本身不合理，而不是缓冲区偏小可以重试。
func TestIoctlEnumerateSnapshotsMaxOutputTooSmall(t *testing.T) {
	for _, maxOut := range []uint32{0, 1, 12, 15} {
		ctx, req := newSparseCtx(t, 0)
		req.CtlCode = wire.FSCTLSrvEnumerateSnapshots
		req.MaxOutputResponse = maxOut

		if err := ioctlEnumerateSnapshots(ctx, req); err != status.InvalidParameter {
			t.Errorf("MaxOutputResponse=%d: err = %v, 期望 %v",
				maxOut, err, status.InvalidParameter)
		}
	}

	// 边界另一侧：恰好 16 必须成功。
	ctx, req := newSparseCtx(t, 0)
	req.CtlCode = wire.FSCTLSrvEnumerateSnapshots
	req.MaxOutputResponse = wire.SrvSnapshotArrayMinSize
	if err := ioctlEnumerateSnapshots(ctx, req); err != nil {
		t.Errorf("MaxOutputResponse=16 应成功，实际 err=%v", err)
	}
}

// TestIoctlEnumerateSnapshotsRejectsBadHandle：无效句柄必须先被挡下。
func TestIoctlEnumerateSnapshotsRejectsBadHandle(t *testing.T) {
	ctx, req := newSparseCtx(t, 0)
	req.CtlCode = wire.FSCTLSrvEnumerateSnapshots
	req.MaxOutputResponse = 65536
	// 复合链里没有前序句柄，且 FileID 不是复合标记 → 无效。
	ctx.Chain = &Chain{}
	req.FileID = wire.FileID{Persistent: 1, Volatile: 1}

	if err := ioctlEnumerateSnapshots(ctx, req); err == nil {
		t.Error("无效句柄上的 ENUMERATE_SNAPSHOTS 应失败")
	}
}

// TestIoctlEnumerateSnapshotsDispatched：控制码必须真的接到 handler 上。
//
// 这条看着多余，其实是本条改动的核心价值 —— 之前 handleIoctl 的 switch 里
// 没有这个 case，落到 default 回 STATUS_INVALID_DEVICE_REQUEST，
// 表现就是 smbclient 每次 allinfo 都打印
// "NT_STATUS_INVALID_DEVICE_REQUEST getting shadow copy data"。
// 只测 handler 不测分发的话，case 被误删了测试也发现不了。
func TestIoctlEnumerateSnapshotsDispatched(t *testing.T) {
	ctx, req := newSparseCtx(t, 0)
	req.CtlCode = wire.FSCTLSrvEnumerateSnapshots
	req.MaxOutputResponse = 65536

	ctx.Msg = encodeIoctlRequestMsg(t, req)
	if err := handleIoctl(ctx); err != nil {
		t.Fatalf("经 handleIoctl 分发应成功，实际 err=%v（case 是不是漏了？）", err)
	}
	if _, err := wire.ParseSrvSnapshotArray(ioctlRespOutput(t, ctx.Out)); err != nil {
		t.Errorf("分发出来的应答解析失败: %v", err)
	}
}

// TestUnimplementedFSCTLStatusByTreeType：未实现的控制码在磁盘树与 IPC$ 树上
// 回不同的状态码，且都**不是** STATUS_NOT_SUPPORTED。
//
// 依据 Samba `smb2_ioctl_network_fs.c` 的 default 分支改写规则（见
// unimplementedFSCTL 的注释）。客户端的退化路径是照 Samba 写的：
// 收到 NOT_SUPPORTED 有些客户端会重试到超时。
func TestUnimplementedFSCTLStatusByTreeType(t *testing.T) {
	const bogus = wire.CtlCode(0x00090000) // 一个我们肯定不实现的 FSCTL

	// 磁盘树 → STATUS_INVALID_DEVICE_REQUEST
	ctx, req := newSparseCtx(t, 0)
	req.CtlCode = bogus
	req.Input = nil
	if err := unimplementedFSCTL(ctx, req); err != status.InvalidDeviceRequest {
		t.Errorf("磁盘树 err = %v, 期望 %v", err, status.InvalidDeviceRequest)
	}

	// IPC$ 树 → STATUS_FS_DRIVER_REQUIRED
	ctx.Tree.Share.Type = wire.ShareTypePipe
	if err := unimplementedFSCTL(ctx, req); err != status.FSDriverRequired {
		t.Errorf("IPC$ 树 err = %v, 期望 %v", err, status.FSDriverRequired)
	}

	// 没有树上下文（IOCTL 不要求 TreeConnect）时不能 panic，按磁盘树处理。
	ctx.Tree = nil
	if err := unimplementedFSCTL(ctx, req); err != status.InvalidDeviceRequest {
		t.Errorf("无树上下文 err = %v, 期望 %v", err, status.InvalidDeviceRequest)
	}
}

// --- 辅助 -------------------------------------------------------------------

// ioctlRespFixedSize 是 SMB2 IOCTL Response 的固定部分长度（MS-SMB2 §2.2.32）。
const ioctlRespFixedSize = 48

// ioctlRespOutput 从 IOCTL Response 报文体里切出 Output 段。
//
// 我们组装的响应没有 Input，Output 紧跟在固定部分之后一直到末尾。
func ioctlRespOutput(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < ioctlRespFixedSize {
		t.Fatalf("IOCTL 响应体 %d 字节，短于固定部分 %d", len(body), ioctlRespFixedSize)
	}
	return body[ioctlRespFixedSize:]
}

// encodeIoctlRequestMsg 把 IoctlRequest 编成一条完整的 SMB2 消息（含 64 字节头），
// 供走 handleIoctl 的分发路径测试使用。
//
// 必须带头：wire 的 Parse* 约定入参是完整消息，报文体里的
// InputOffset/OutputOffset 也是相对消息起点算的（见 appendIoctlBuffers）。
func encodeIoctlRequestMsg(t *testing.T, req *wire.IoctlRequest) []byte {
	t.Helper()
	msg := wire.Header{Command: wire.CommandIoctl}.Append(nil)
	msg, err := req.Append(msg)
	if err != nil {
		t.Fatalf("编码 IOCTL Request: %v", err)
	}
	return msg
}
