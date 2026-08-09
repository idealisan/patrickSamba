package wire

import "fmt"

// ---------------------------------------------------------------------------
// 租约 create context（MS-SMB2 §2.2.13.2.8 / §2.2.13.2.10 请求侧，
//                     §2.2.14.2.10 / §2.2.14.2.11 响应侧）
//
// 四种结构**共用同一个 context 名字 "RqLs"**（0x52714C73），
// 靠 DataLength 区分版本 —— 规范原文：
//
//	"This context value is the same as the SMB2_CREATE_REQUEST_LEASE value;
//	 the server differentiates these requests based on the value of the
//	 DataLength field."
//
// 所以判别只能按长度做（32 = v1，52 = v2），这一点与 OPLOCK_BREAK 按
// StructureSize 区分 oplock/lease 两族是同一个套路。
//
// v1（§2.2.13.2.8 / §2.2.14.2.10），32 字节，**全部小端**：
//
//	 0 LeaseKey(16)
//	16 LeaseState(4)
//	20 LeaseFlags(4)
//	24 LeaseDuration(8)    必须为 0
//
// v2（§2.2.13.2.10 / §2.2.14.2.11），52 字节：在 v1 之后追加
//
//	32 ParentLeaseKey(16)
//	48 Epoch(2)
//	50 Reserved(2)         必须为 0
//
// 请求与响应布局完全相同，因此用同一组结构体表达，只在 Flags 的含义上有区别
// （见下方 LeaseFlags 常量注释）。
// ---------------------------------------------------------------------------

// 租约 create context 的两种合法载荷长度（MS-SMB2 §2.2.13.2.8 / §2.2.13.2.10）。
const (
	// LeaseContextV1Size 是 SMB2_CREATE_REQUEST_LEASE / _RESPONSE_LEASE 的长度。
	LeaseContextV1Size = 32
	// LeaseContextV2Size 是 SMB2_CREATE_REQUEST_LEASE_V2 / _RESPONSE_LEASE_V2 的长度。
	LeaseContextV2Size = 52
)

// 租约 create context 的 Flags 取值。
//
// ⚠️ 注意 LeaseFlags 这个类型被**三个互不相同的位域命名空间**共用，
// 同名字段在不同报文里含义完全不同，不要混用：
//
//	Lease Break Notification（§2.2.23.2）  LeaseBreakAckRequired      0x01
//	租约 create context 请求（§2.2.13.2.10）LeaseFlagParentLeaseKeySet 0x04
//	租约 create context 响应（§2.2.14.2.10）LeaseFlagBreakInProgress   0x02
//	                                       LeaseFlagParentLeaseKeySet 0x04（仅 v2）
//
// 规范没有把它们定义成同一张表，只是恰好都叫 Flags。
const (
	// LeaseFlagBreakInProgress 只出现在**响应**里（§2.2.14.2.10）：
	// 服务端已经在对该租约发起 break，客户端应当等待 Lease Break Notification。
	LeaseFlagBreakInProgress LeaseFlags = 0x00000002
	// LeaseFlagParentLeaseKeySet 表示 ParentLeaseKey 字段有效（§2.2.13.2.10）。
	// 只在 v2 里有意义；v1 没有 ParentLeaseKey 字段。
	LeaseFlagParentLeaseKeySet LeaseFlags = 0x00000004
)

// LeaseContext 是租约 create context 的载荷（请求与响应共用布局）。
//
// V2 为 true 时编码成 52 字节并带上 ParentLeaseKey / Epoch，否则编码成
// 32 字节的 v1。解析时由载荷长度决定 V2 的取值。
type LeaseContext struct {
	LeaseKey   [16]byte
	LeaseState LeaseState
	Flags      LeaseFlags

	// V2 标记本结构对应 SMB2_CREATE_REQUEST_LEASE_V2 / _RESPONSE_LEASE_V2
	// （52 字节）。为 false 时下面两个字段无意义。
	V2 bool
	// ParentLeaseKey 仅在 V2 且 Flags 含 LeaseFlagParentLeaseKeySet 时有效。
	ParentLeaseKey [16]byte
	// Epoch 仅在 V2 时有效，用于跟踪租约状态变更的版本号。
	Epoch uint16
}

// HasParentLeaseKey 报告 ParentLeaseKey 字段是否有效
// （必须同时是 v2 且置了 SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET）。
func (c *LeaseContext) HasParentLeaseKey() bool {
	return c.V2 && c.Flags&LeaseFlagParentLeaseKeySet != 0
}

// ParseLeaseContext 解析 "RqLs" create context 的载荷。
//
// **只按长度判别版本**：32 → v1，52 → v2，其余一律返回 ErrMalformed
// （不猜、不截断、不补零 —— AGENTS.md §9）。
//
// LeaseDuration(v1 偏移 24) 与 Reserved(v2 偏移 50) 规范要求必须为 0，
// 这里按「客户端置 0、服务端忽略」处理：不校验、不保存，编码时恒写 0。
func ParseLeaseContext(data []byte) (*LeaseContext, error) {
	switch len(data) {
	case LeaseContextV1Size, LeaseContextV2Size:
	default:
		return nil, fmt.Errorf("%w: 租约 create context 载荷 %d 字节，只接受 %d(v1) 或 %d(v2)",
			ErrMalformed, len(data), LeaseContextV1Size, LeaseContextV2Size)
	}
	c := &LeaseContext{
		LeaseState: LeaseState(le.Uint32(data[16:])),
		Flags:      LeaseFlags(le.Uint32(data[20:])),
		V2:         len(data) == LeaseContextV2Size,
	}
	copy(c.LeaseKey[:], data[0:16])
	if c.V2 {
		copy(c.ParentLeaseKey[:], data[32:48])
		c.Epoch = le.Uint16(data[48:])
	}
	return c, nil
}

// Encode 编码租约 create context 的载荷。V2 决定输出 32 还是 52 字节。
func (c *LeaseContext) Encode() []byte {
	n := LeaseContextV1Size
	if c.V2 {
		n = LeaseContextV2Size
	}
	b := make([]byte, n)
	copy(b[0:16], c.LeaseKey[:])
	le.PutUint32(b[16:], uint32(c.LeaseState))
	le.PutUint32(b[20:], uint32(c.Flags))
	// b[24:32] LeaseDuration 必须为 0（§2.2.13.2.8），make 已清零。
	if c.V2 {
		copy(b[32:48], c.ParentLeaseKey[:])
		le.PutUint16(b[48:], c.Epoch)
		// b[50:52] Reserved 必须为 0。
	}
	return b
}

// FindLeaseContext 在 create context 链里找到 "RqLs" 并解析。
//
// 返回 (nil, nil) 表示客户端没有请求租约 —— 这是最常见的情况，
// 调用方不应把它当错误。
func FindLeaseContext(ctxs []CreateContext) (*LeaseContext, error) {
	data, ok := FindCreateContext(ctxs, CreateContextRqLs)
	if !ok {
		return nil, nil
	}
	return ParseLeaseContext(data)
}
