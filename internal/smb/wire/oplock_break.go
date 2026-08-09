package wire

import "fmt"

// ---------------------------------------------------------------------------
// OPLOCK_BREAK（MS-SMB2 §2.2.23 / §2.2.24 / §2.2.25）
//
// 同一个命令码 0x0012 承载**两族**报文，靠 StructureSize 区分：
//
//	oplock 族（24 字节）  §2.2.23.1 通知 / §2.2.24.1 确认 / §2.2.25.1 响应
//	    0 StructureSize(2) = 24
//	    2 OplockLevel(1)
//	    3 Reserved(1)
//	    4 Reserved2(4)
//	    8 FileId(16)
//	  三者线上布局完全相同，方向不同而已。
//
//	lease 族
//	  §2.2.23.2 Lease Break Notification（服务端 → 客户端，44 字节）
//	    0 StructureSize(2) = 44
//	    2 NewEpoch(2)
//	    4 Flags(4)
//	    8 LeaseKey(16)
//	   24 CurrentLeaseState(4)
//	   28 NewLeaseState(4)
//	   32 BreakReason(4)      必须为 0
//	   36 AccessMaskHint(4)   必须为 0
//	   40 ShareMaskHint(4)    必须为 0
//
//	  §2.2.24.2 Lease Break Acknowledgment（客户端 → 服务端，36 字节）
//	  §2.2.25.2 Lease Break Response（服务端 → 客户端，36 字节，布局同上）
//	    0 StructureSize(2) = 36
//	    2 Reserved(2)
//	    4 Flags(4)
//	    8 LeaseKey(16)
//	   24 LeaseState(4)
//	   28 LeaseDuration(8)    必须为 0
//
// 报文体**全部小端**。
//
// 服务端发通知时，头部 MessageId 必须是 0xFFFFFFFFFFFFFFFF、TreeId/SessionId
// 按 §3.3.4.6 填，这属于状态层（internal/server）的职责，不在本包。
// ---------------------------------------------------------------------------

const (
	oplockBreakStructureSize = 24
	oplockBreakFixed         = 24

	leaseBreakNotifyStructureSize = 44
	leaseBreakNotifyFixed         = 44

	leaseBreakAckStructureSize = 36
	leaseBreakAckFixed         = 36
)

// OplockBreak 是 oplock 族的三种报文（MS-SMB2 §2.2.23.1 / §2.2.24.1 / §2.2.25.1），
// 线上布局完全一致，用同一个结构体表达。
//
//   - 通知（服务端发）：OplockLevel 是**要求客户端降到**的级别。
//   - 确认（客户端发）：OplockLevel 是客户端**愿意降到**的级别。
//   - 响应（服务端发）：OplockLevel 是**最终生效**的级别。
type OplockBreak struct {
	OplockLevel OplockLevel
	FileID      FileID
}

// ParseOplockBreak 解析 oplock 族报文（24 字节体）。b 是完整消息。
//
// 若客户端发来的是 lease 族（StructureSize=36），这里会返回 ErrStructureSize；
// 调用方应先用 PeekOplockBreakKind 判别。
func ParseOplockBreak(b []byte) (*OplockBreak, error) {
	body, err := msgBody(b, oplockBreakFixed, "OPLOCK_BREAK")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, oplockBreakStructureSize); err != nil {
		return nil, fmt.Errorf("OPLOCK_BREAK: %w", err)
	}
	return &OplockBreak{
		OplockLevel: OplockLevel(body[2]),
		FileID:      parseFileID(body[8:]),
	}, nil
}

// Append 编码 oplock 族报文体。
func (r *OplockBreak) Append(dst []byte) []byte {
	dst, f := grow(dst, oplockBreakFixed)
	le.PutUint16(f[0:], oplockBreakStructureSize)
	f[2] = byte(r.OplockLevel)
	// f[3] Reserved, f[4:8] Reserved2
	r.FileID.put(f[8:])
	return dst
}

// LeaseFlags 是 Lease Break 报文的 Flags 字段（MS-SMB2 §2.2.23.2）。
type LeaseFlags uint32

// SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED：置位表示服务端在等客户端的
// Lease Break Acknowledgment，不确认就不会继续处理冲突的请求。
const LeaseBreakAckRequired LeaseFlags = 0x00000001

// LeaseState 是租约状态位图（MS-SMB2 §2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE）。
type LeaseState uint32

const (
	LeaseNone          LeaseState = 0x00 // SMB2_LEASE_NONE
	LeaseReadCaching   LeaseState = 0x01 // SMB2_LEASE_READ_CACHING
	LeaseHandleCaching LeaseState = 0x02 // SMB2_LEASE_HANDLE_CACHING
	LeaseWriteCaching  LeaseState = 0x04 // SMB2_LEASE_WRITE_CACHING
)

// LeaseBreakNotification 是 §2.2.23.2（服务端 → 客户端，44 字节）。
type LeaseBreakNotification struct {
	// NewEpoch 只在 SMB 3.x 且客户端请求了 lease V2 时有意义。
	NewEpoch          uint16
	Flags             LeaseFlags
	LeaseKey          [16]byte
	CurrentLeaseState LeaseState
	NewLeaseState     LeaseState
}

// Append 编码 Lease Break Notification 报文体。
func (r *LeaseBreakNotification) Append(dst []byte) []byte {
	dst, f := grow(dst, leaseBreakNotifyFixed)
	le.PutUint16(f[0:], leaseBreakNotifyStructureSize)
	le.PutUint16(f[2:], r.NewEpoch)
	le.PutUint32(f[4:], uint32(r.Flags))
	copy(f[8:24], r.LeaseKey[:])
	le.PutUint32(f[24:], uint32(r.CurrentLeaseState))
	le.PutUint32(f[28:], uint32(r.NewLeaseState))
	// f[32:36] BreakReason, f[36:40] AccessMaskHint, f[40:44] ShareMaskHint
	// 三者按 §2.2.23.2 必须为 0，grow 已清零。
	return dst
}

// ParseLeaseBreakNotification 解析 Lease Break Notification（供测试与客户端使用）。
func ParseLeaseBreakNotification(b []byte) (*LeaseBreakNotification, error) {
	body, err := msgBody(b, leaseBreakNotifyFixed, "LEASE_BREAK Notification")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, leaseBreakNotifyStructureSize); err != nil {
		return nil, fmt.Errorf("LEASE_BREAK Notification: %w", err)
	}
	r := &LeaseBreakNotification{
		NewEpoch:          le.Uint16(body[2:]),
		Flags:             LeaseFlags(le.Uint32(body[4:])),
		CurrentLeaseState: LeaseState(le.Uint32(body[24:])),
		NewLeaseState:     LeaseState(le.Uint32(body[28:])),
	}
	copy(r.LeaseKey[:], body[8:24])
	return r, nil
}

// LeaseBreakAck 同时表示 §2.2.24.2 Lease Break Acknowledgment（客户端 → 服务端）
// 与 §2.2.25.2 Lease Break Response（服务端 → 客户端）—— 两者线上布局一致，
// 都是 36 字节。
type LeaseBreakAck struct {
	Flags      LeaseFlags
	LeaseKey   [16]byte
	LeaseState LeaseState
}

// Append 编码 Lease Break Acknowledgment / Response 报文体。
func (r *LeaseBreakAck) Append(dst []byte) []byte {
	dst, f := grow(dst, leaseBreakAckFixed)
	le.PutUint16(f[0:], leaseBreakAckStructureSize)
	// f[2:4] Reserved
	le.PutUint32(f[4:], uint32(r.Flags))
	copy(f[8:24], r.LeaseKey[:])
	le.PutUint32(f[24:], uint32(r.LeaseState))
	// f[28:36] LeaseDuration 必须为 0，grow 已清零。
	return dst
}

// ParseLeaseBreakAck 解析 Lease Break Acknowledgment / Response。
func ParseLeaseBreakAck(b []byte) (*LeaseBreakAck, error) {
	body, err := msgBody(b, leaseBreakAckFixed, "LEASE_BREAK Ack")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, leaseBreakAckStructureSize); err != nil {
		return nil, fmt.Errorf("LEASE_BREAK Ack: %w", err)
	}
	r := &LeaseBreakAck{
		Flags:      LeaseFlags(le.Uint32(body[4:])),
		LeaseState: LeaseState(le.Uint32(body[24:])),
	}
	copy(r.LeaseKey[:], body[8:24])
	return r, nil
}

// OplockBreakKind 区分命令码 0x0012 上的两族报文。
type OplockBreakKind int

const (
	// OplockBreakKindUnknown 表示 StructureSize 不是 24 也不是 36。
	OplockBreakKindUnknown OplockBreakKind = iota
	// OplockBreakKindOplock 是 24 字节的 oplock 族。
	OplockBreakKindOplock
	// OplockBreakKindLease 是 36 字节的 lease 族（客户端只会发 Ack）。
	OplockBreakKindLease
)

// PeekOplockBreakKind 只读 StructureSize 判别收到的是哪一族，
// 不做完整解析。b 是完整消息。
//
// 只有本服务在 NEGOTIATE 里声明了 SMB2_GLOBAL_CAP_LEASING 时客户端才可能发
// lease 族；没声明却收到 lease 族属于协议违规，调用方应回
// STATUS_INVALID_PARAMETER。
func PeekOplockBreakKind(b []byte) OplockBreakKind {
	if len(b) < HeaderSize+2 {
		return OplockBreakKindUnknown
	}
	switch le.Uint16(b[HeaderSize:]) {
	case oplockBreakStructureSize:
		return OplockBreakKindOplock
	case leaseBreakAckStructureSize:
		return OplockBreakKindLease
	default:
		return OplockBreakKindUnknown
	}
}
