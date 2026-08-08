package crypto

// SMB2 TRANSFORM_HEADER 与消息加解密（MS-SMB2 §2.2.41 / §3.1.4.3 / §3.1.4.4）。

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

// TransformHeaderSize 是 SMB2 TRANSFORM_HEADER 的固定长度。
const TransformHeaderSize = 52

// TransformProtocolID 是 TRANSFORM_HEADER 的 ProtocolId，
// 字节序列为 0xFD 'S' 'M' 'B'，按**小端**读作 uint32 即 0x424D53FD。
const TransformProtocolID uint32 = 0x424D53FD

// TRANSFORM_HEADER 各字段偏移（MS-SMB2 §2.2.41）。
const (
	offTProtocolID = 0x00 // 4
	offTSignature  = 0x04 // 16
	offTNonce      = 0x14 // 16
	offTOrigSize   = 0x24 // 4
	offTReserved   = 0x28 // 2
	offTFlags      = 0x2A // 2
	offTSessionID  = 0x2C // 8

	// aadOffset/aadLen：AAD 是头的 0x14..0x33 共 32 字节
	// （Nonce 16 + OriginalMessageSize 4 + Reserved 2 + Flags 2 + SessionId 8）。
	aadOffset = 0x14
	aadLen    = 32
)

// TransformFlagEncrypted 即 SMB2_ENCRYPTION_AES128_CCM / Flags 字段值 0x0001。
//
// 3.0/3.0.2 里该字段名为 EncryptionAlgorithm（唯一取值 0x0001 = AES-128-CCM），
// 3.1.1 里改名为 Flags，置位表示"已加密"。两者数值一致，统一按 0x0001 处理。
const TransformFlagEncrypted uint16 = 0x0001

// Cipher 是 SMB 3.1.1 ENCRYPTION_CAPABILITIES 协商出的加密算法
// （MS-SMB2 §2.2.3.1.2）。
type Cipher uint16

const (
	CipherAES128CCM Cipher = 0x0001
	CipherAES128GCM Cipher = 0x0002
	CipherAES256CCM Cipher = 0x0003
	CipherAES256GCM Cipher = 0x0004
)

// CCM 与 GCM 的 nonce 长度（MS-SMB2 §3.1.4.3）。
// Nonce 字段固定 16 字节，未用的尾部必须为 0。
const (
	ccmNonceSize = 11
	gcmNonceSize = 12
)

var (
	// ErrTransformHeader 表示 TRANSFORM_HEADER 非法（长度不足或魔数不对）。
	ErrTransformHeader = errors.New("crypto: malformed SMB2 TRANSFORM_HEADER")
	// ErrCipherUnsupported 表示加密算法未知。
	ErrCipherUnsupported = errors.New("crypto: unsupported SMB3 cipher")
	// ErrTransformKeySize 表示密钥长度与算法不匹配。
	ErrTransformKeySize = errors.New("crypto: wrong key size for SMB3 cipher")
	// ErrTransformDecrypt 表示解密/认证失败。
	ErrTransformDecrypt = errors.New("crypto: SMB3 transform decryption failed")
)

// KeySize 返回算法要求的密钥字节数。
func (c Cipher) KeySize() int {
	switch c {
	case CipherAES128CCM, CipherAES128GCM:
		return 16
	case CipherAES256CCM, CipherAES256GCM:
		return 32
	default:
		return 0
	}
}

// NonceSize 返回算法实际使用的 nonce 字节数（Nonce 字段仍占 16 字节）。
func (c Cipher) NonceSize() int {
	switch c {
	case CipherAES128CCM, CipherAES256CCM:
		return ccmNonceSize
	case CipherAES128GCM, CipherAES256GCM:
		return gcmNonceSize
	default:
		return 0
	}
}

// String 便于日志展示。
func (c Cipher) String() string {
	switch c {
	case CipherAES128CCM:
		return "AES-128-CCM"
	case CipherAES128GCM:
		return "AES-128-GCM"
	case CipherAES256CCM:
		return "AES-256-CCM"
	case CipherAES256GCM:
		return "AES-256-GCM"
	default:
		return "unknown-cipher"
	}
}

// aead 按算法与密钥构造 AEAD。
func (c Cipher) aead(key []byte) (cipher.AEAD, error) {
	if n := c.KeySize(); n == 0 {
		return nil, ErrCipherUnsupported
	} else if len(key) != n {
		return nil, ErrTransformKeySize
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	switch c {
	case CipherAES128CCM, CipherAES256CCM:
		return NewCCM(b, ccmNonceSize, SignatureSize)
	case CipherAES128GCM, CipherAES256GCM:
		return cipher.NewGCMWithNonceSize(b, gcmNonceSize)
	default:
		return nil, ErrCipherUnsupported
	}
}

// TransformHeader 是解析后的 SMB2 TRANSFORM_HEADER。
type TransformHeader struct {
	Signature           [SignatureSize]byte
	Nonce               [16]byte
	OriginalMessageSize uint32
	Flags               uint16
	SessionID           uint64
}

// IsTransform 报告 buffer 是否以 TRANSFORM_HEADER 的 ProtocolId 开头。
func IsTransform(b []byte) bool {
	return len(b) >= 4 && binary.LittleEndian.Uint32(b) == TransformProtocolID
}

// Marshal 编码 TRANSFORM_HEADER（52 字节，全部**小端**）。
func (h *TransformHeader) Marshal() []byte {
	out := make([]byte, TransformHeaderSize)
	h.marshalInto(out)
	return out
}

func (h *TransformHeader) marshalInto(out []byte) {
	binary.LittleEndian.PutUint32(out[offTProtocolID:], TransformProtocolID)
	copy(out[offTSignature:], h.Signature[:])
	copy(out[offTNonce:], h.Nonce[:])
	binary.LittleEndian.PutUint32(out[offTOrigSize:], h.OriginalMessageSize)
	binary.LittleEndian.PutUint16(out[offTReserved:], 0)
	binary.LittleEndian.PutUint16(out[offTFlags:], h.Flags)
	binary.LittleEndian.PutUint64(out[offTSessionID:], h.SessionID)
}

// ParseTransformHeader 解析 TRANSFORM_HEADER。
//
// 先校验长度与魔数再切片，畸形输入返回 ErrTransformHeader（不 panic）。
func ParseTransformHeader(b []byte) (*TransformHeader, error) {
	if len(b) < TransformHeaderSize || !IsTransform(b) {
		return nil, ErrTransformHeader
	}
	h := &TransformHeader{
		OriginalMessageSize: binary.LittleEndian.Uint32(b[offTOrigSize:]),
		Flags:               binary.LittleEndian.Uint16(b[offTFlags:]),
		SessionID:           binary.LittleEndian.Uint64(b[offTSessionID:]),
	}
	copy(h.Signature[:], b[offTSignature:offTSignature+SignatureSize])
	copy(h.Nonce[:], b[offTNonce:offTNonce+16])
	return h, nil
}

// Encrypt 把一条（或复合的多条）完整 SMB2 消息加密成 TRANSFORM 报文。
//
// nonce 只取算法所需的前 NonceSize 字节，其余补零；调用方必须使用
// **单调递增的计数器**，禁止随机数（MS-SMB2 §3.1.4.3 —— 随机 nonce 有生日碰撞风险，
// 同一密钥下 nonce 重用会直接泄露明文异或值）。
//
// 返回的字节 = TRANSFORM_HEADER(52) || 密文（长度与明文相同，tag 已写入 Signature 字段）。
func Encrypt(c Cipher, key, nonce []byte, sessionID uint64, plaintext []byte) ([]byte, error) {
	a, err := c.aead(key)
	if err != nil {
		return nil, err
	}
	n := c.NonceSize()
	if len(nonce) < n {
		return nil, ErrTransformHeader
	}

	out := make([]byte, TransformHeaderSize, TransformHeaderSize+len(plaintext)+a.Overhead())
	h := &TransformHeader{
		OriginalMessageSize: uint32(len(plaintext)),
		Flags:               TransformFlagEncrypted,
		SessionID:           sessionID,
	}
	copy(h.Nonce[:n], nonce[:n]) // 尾部保持为 0
	h.marshalInto(out)

	// AAD 必须在写入 Signature 之前取（Signature 不参与 AAD）。
	aad := make([]byte, aadLen)
	copy(aad, out[aadOffset:aadOffset+aadLen])

	sealed := a.Seal(nil, h.Nonce[:n], plaintext, aad)
	if len(sealed) < len(plaintext)+SignatureSize {
		return nil, ErrTransformDecrypt
	}
	// AEAD 的输出是 密文||tag，SMB 把 tag 放进头的 Signature 字段。
	ct := sealed[:len(plaintext)]
	tag := sealed[len(plaintext):]
	copy(out[offTSignature:], tag[:SignatureSize])
	return append(out, ct...), nil
}

// NonceCounter 生成 TRANSFORM 加密用的**单调递增** nonce。
//
// MS-SMB2 §3.1.4.3 要求同一密钥下 nonce 绝不重复。随机 nonce 在 CCM 的
// 11 字节空间里有现实可行的生日碰撞风险，一旦重用会直接泄露明文异或值，
// 因此这里用计数器而不是随机数。
//
// 每个 Session 的每个加密方向应各持有一个 NonceCounter。非并发安全，
// 调用方需自行加锁（Session 通常已有锁）。
type NonceCounter struct {
	low  uint64
	high uint64
}

// Next 返回下一个 16 字节 nonce（低 64 位在前，**小端**）。
func (n *NonceCounter) Next() [16]byte {
	n.low++
	if n.low == 0 {
		n.high++
	}
	var out [16]byte
	binary.LittleEndian.PutUint64(out[0:], n.low)
	binary.LittleEndian.PutUint64(out[8:], n.high)
	return out
}

// Decrypt 解密一条 TRANSFORM 报文，返回内部的明文 SMB2 消息。
//
// msg 必须是完整的 TRANSFORM 报文（含 52 字节头，不含 Direct TCP 长度前缀）。
// 认证失败返回 ErrTransformDecrypt。
func Decrypt(c Cipher, key, msg []byte) ([]byte, error) {
	h, err := ParseTransformHeader(msg)
	if err != nil {
		return nil, err
	}
	a, err := c.aead(key)
	if err != nil {
		return nil, err
	}
	n := c.NonceSize()

	ct := msg[TransformHeaderSize:]
	// OriginalMessageSize 必须与密文长度一致，否则是畸形报文。
	if uint64(h.OriginalMessageSize) != uint64(len(ct)) {
		return nil, ErrTransformHeader
	}

	aad := make([]byte, aadLen)
	copy(aad, msg[aadOffset:aadOffset+aadLen])

	// 还原 AEAD 期望的 密文||tag 布局。
	sealed := make([]byte, 0, len(ct)+SignatureSize)
	sealed = append(sealed, ct...)
	sealed = append(sealed, h.Signature[:]...)

	plain, err := a.Open(nil, h.Nonce[:n], sealed, aad)
	if err != nil {
		return nil, ErrTransformDecrypt
	}
	return plain, nil
}
