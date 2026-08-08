package crypto

import "crypto/sha512"

// SMB 3.1.1 Preauthentication Integrity（MS-SMB2 §3.1.5.2）。
//
// 累积规则：
//
//	H_0 = 64 字节全 0
//	H_n = SHA-512(H_{n-1} || 整条 SMB2 消息字节)
//
// "整条消息"**不含** Direct TCP 的 4 字节长度前缀，但**包含** 64 字节 SMB2 头。
//
// 参与顺序（MS-SMB2 §3.3.5.4 / §3.3.5.5）：
//
//  1. NEGOTIATE Request
//  2. NEGOTIATE Response          ← 到此 Connection 级的值定型
//  3. SESSION_SETUP Request #1
//  4. SESSION_SETUP Response #1（STATUS_MORE_PROCESSING_REQUIRED 那条）
//  5. SESSION_SETUP Request #2（最后一条）
//
// **最后一条成功的 SESSION_SETUP Response 不参与。**
//
// 建立 Session 时必须把 Connection 级的值**复制**一份到 Session 级再继续累积，
// 否则多会话场景会互相污染 Connection 级的值。

// PreauthHashSize 是 preauth integrity hash 的长度（SHA-512，64 字节）。
const PreauthHashSize = sha512.Size

// PreauthHashAlgorithmSHA512 是 SMB2_PREAUTH_INTEGRITY_CAPABILITIES 里
// 目前唯一定义的算法 ID（MS-SMB2 §2.2.3.1.1）。
const PreauthHashAlgorithmSHA512 uint16 = 0x0001

// NewPreauthHash 返回初始值：64 字节全零。
func NewPreauthHash() []byte {
	return make([]byte, PreauthHashSize)
}

// UpdatePreauthHash 返回 SHA-512(prev || msg)。
//
// prev 必须是 PreauthHashSize 字节；长度不符时按全零处理，
// 这样调用方传 nil 也能得到与 NewPreauthHash 一致的结果。
// 返回新切片，不修改 prev（Connection 级的值需要保持不变）。
func UpdatePreauthHash(prev, msg []byte) []byte {
	h := sha512.New()
	if len(prev) == PreauthHashSize {
		h.Write(prev)
	} else {
		h.Write(make([]byte, PreauthHashSize))
	}
	h.Write(msg)
	return h.Sum(nil)
}

// PreauthAccumulator 是 preauth hash 的累积器，方便按顺序喂消息。
//
// 零值不可用，请用 NewPreauthAccumulator。非并发安全。
type PreauthAccumulator struct {
	value []byte
}

// NewPreauthAccumulator 创建一个从全零开始的累积器。
func NewPreauthAccumulator() *PreauthAccumulator {
	return &PreauthAccumulator{value: NewPreauthHash()}
}

// Update 累积一条完整的 SMB2 消息。
func (a *PreauthAccumulator) Update(msg []byte) {
	a.value = UpdatePreauthHash(a.value, msg)
}

// Value 返回当前哈希值的副本。
func (a *PreauthAccumulator) Value() []byte {
	out := make([]byte, len(a.value))
	copy(out, a.value)
	return out
}

// Clone 复制累积器。建立 Session 时用它从 Connection 级的值分叉，
// 避免后续的 SESSION_SETUP 污染 Connection 级状态。
func (a *PreauthAccumulator) Clone() *PreauthAccumulator {
	return &PreauthAccumulator{value: a.Value()}
}
