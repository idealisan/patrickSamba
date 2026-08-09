package wire

import "fmt"

// ---------------------------------------------------------------------------
// LOCK（MS-SMB2 §2.2.26 / §2.2.27）
//
// LOCK Request（§2.2.26），报文体**全部小端**：
//
//	 0 StructureSize(2) = 48   ⚠️ 固定为 48（"含单个 SMB2_LOCK_ELEMENT 的大小"），
//	                              **不随 LockCount 变化**
//	 2 LockCount(2)            必须 >= 1
//	 4 LockSequenceNumber(4bit) + LockSequenceIndex(28bit)
//	                              SMB 2.0.2 中整个 4 字节是 Reserved
//	 8 FileId(16)
//	24 Locks(24 * LockCount)   SMB2_LOCK_ELEMENT 数组
//
// LOCK Response（§2.2.27）：
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
// ---------------------------------------------------------------------------

const (
	// lockRequestStructureSize 是 §2.2.26 规定的固定值 48：
	// 24 字节固定部分 + 1 个 SMB2_LOCK_ELEMENT(24)。无论实际有几个元素都填 48。
	lockRequestStructureSize = 48
	lockRequestFixed         = 24

	lockResponseStructureSize = 4
)

// LockElementSize 是 SMB2_LOCK_ELEMENT 的固定长度（MS-SMB2 §2.2.26.1）。
const LockElementSize = 24

// LockFlags 是 SMB2_LOCK_ELEMENT 的 Flags 字段（MS-SMB2 §2.2.26.1）。
type LockFlags uint32

// MS-SMB2 §2.2.26.1 — SMB2_LOCK_ELEMENT Flags
const (
	LockFlagSharedLock      LockFlags = 0x00000001 // SMB2_LOCKFLAG_SHARED_LOCK
	LockFlagExclusiveLock   LockFlags = 0x00000002 // SMB2_LOCKFLAG_EXCLUSIVE_LOCK
	LockFlagUnlock          LockFlags = 0x00000004 // SMB2_LOCKFLAG_UNLOCK
	LockFlagFailImmediately LockFlags = 0x00000010 // SMB2_LOCKFLAG_FAIL_IMMEDIATELY
)

// IsUnlock 报告本元素是解锁请求。
func (f LockFlags) IsUnlock() bool { return f&LockFlagUnlock != 0 }

// IsExclusive 报告本元素申请的是排他锁。
func (f LockFlags) IsExclusive() bool { return f&LockFlagExclusiveLock != 0 }

// FailImmediately 报告冲突时是否立即失败（不置位则服务端应挂起等待，
// 由客户端的 CANCEL 或锁释放来了结，见 MS-SMB2 §3.3.5.14）。
func (f LockFlags) FailImmediately() bool { return f&LockFlagFailImmediately != 0 }

// LockElement 是 SMB2_LOCK_ELEMENT（MS-SMB2 §2.2.26.1），固定 24 字节：
//
//	 0 Offset(8)
//	 8 Length(8)
//	16 Flags(4)
//	20 Reserved(4)
type LockElement struct {
	Offset uint64
	Length uint64
	Flags  LockFlags
}

// LockRequest 是 SMB2 LOCK Request（MS-SMB2 §2.2.26）。
type LockRequest struct {
	FileID FileID
	// LockSequence 是偏移 4 处的**原始 32 位值**。
	//
	// SMB 2.0.2 中它整体是 Reserved（客户端填 0，服务端忽略）；
	// 2.1 及以上按 LockSequenceNumber(低 4 位) + LockSequenceIndex(高 28 位)
	// 解释，用于重放去重。保留原始值可以严格 round-trip，
	// 需要分量时用下面两个方法。
	LockSequence uint32
	Locks        []LockElement
}

// LockSequenceNumber 返回 §2.2.26 的 LockSequenceNumber —— **低 4 位**。
// 规范原文："The 4 least significant bits of this field containing integer value."
func (r *LockRequest) LockSequenceNumber() uint8 { return uint8(r.LockSequence & 0x0F) }

// LockSequenceIndex 返回 §2.2.26 的 LockSequenceIndex —— **高 28 位**，
// 合法取值 1..64（0 为保留，表示客户端不参与重放去重）。
func (r *LockRequest) LockSequenceIndex() uint32 { return r.LockSequence >> 4 }

// MakeLockSequence 把 number(4 位) 与 index(28 位) 打包成线上的 32 位字段。
func MakeLockSequence(number uint8, index uint32) uint32 {
	return index<<4 | uint32(number&0x0F)
}

// ParseLockRequest 解析 LOCK Request。b 是完整消息（含 64 字节头）。
func ParseLockRequest(b []byte) (*LockRequest, error) {
	body, err := msgBody(b, lockRequestFixed, "LOCK Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, lockRequestStructureSize); err != nil {
		return nil, fmt.Errorf("LOCK Request: %w", err)
	}
	count := int(le.Uint16(body[2:]))
	if count < 1 {
		// §2.2.26："The lock count MUST be greater than or equal to 1."
		return nil, fmt.Errorf("%w: LOCK Request LockCount=0", ErrMalformed)
	}
	r := &LockRequest{
		LockSequence: le.Uint32(body[4:]),
		FileID:       parseFileID(body[8:]),
	}
	// 元素数组紧跟固定部分，长度校验后再切片。
	arr, err := sliceAt(body, lockRequestFixed, uint64(count)*LockElementSize)
	if err != nil {
		return nil, fmt.Errorf("LOCK Request Locks: %w", err)
	}
	r.Locks = make([]LockElement, count)
	for i := range r.Locks {
		f := arr[i*LockElementSize:]
		r.Locks[i] = LockElement{
			Offset: le.Uint64(f[0:]),
			Length: le.Uint64(f[8:]),
			Flags:  LockFlags(le.Uint32(f[16:])),
		}
	}
	return r, nil
}

// Append 把 LOCK Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *LockRequest) Append(dst []byte) ([]byte, error) {
	count, err := u16(len(r.Locks), "LOCK LockCount")
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: LOCK Request 至少要有 1 个 SMB2_LOCK_ELEMENT", ErrMalformed)
	}
	dst, f := grow(dst, lockRequestFixed+int(count)*LockElementSize)
	// StructureSize 恒为 48，不随 count 变化（§2.2.26）。
	le.PutUint16(f[0:], lockRequestStructureSize)
	le.PutUint16(f[2:], count)
	le.PutUint32(f[4:], r.LockSequence)
	r.FileID.put(f[8:])
	for i, e := range r.Locks {
		p := f[lockRequestFixed+i*LockElementSize:]
		le.PutUint64(p[0:], e.Offset)
		le.PutUint64(p[8:], e.Length)
		le.PutUint32(p[16:], uint32(e.Flags))
		// p[20:24] Reserved
	}
	return dst, nil
}

// LockResponse 是 SMB2 LOCK Response（MS-SMB2 §2.2.27），无有效载荷。
type LockResponse struct{}

// Append 编码 LOCK Response 报文体。
func (r *LockResponse) Append(dst []byte) []byte {
	return appendFixedOnly(dst, lockResponseStructureSize)
}

// ParseLockResponse 解析 LOCK Response（供测试与 Go 客户端使用）。
func ParseLockResponse(b []byte) (*LockResponse, error) {
	if err := parseFixedOnly(b, lockResponseStructureSize, "LOCK Response"); err != nil {
		return nil, err
	}
	return &LockResponse{}, nil
}
