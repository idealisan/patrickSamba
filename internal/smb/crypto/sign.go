package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// SMB2 Packet Header 中与签名相关的偏移与长度（MS-SMB2 §2.2.1.2）。
// 报文体一律**小端**。
const (
	HeaderSize = 64

	offCommand   = 0x0C // 2 字节
	offFlags     = 0x10 // 4 字节
	offMessageID = 0x18 // 8 字节
	offSignature = 0x30 // 16 字节

	// SignatureSize 是 SMB2 Signature 字段长度。
	SignatureSize = 16
)

// SMB2_FLAGS_SIGNED（MS-SMB2 §2.2.1.2 Flags）。
const (
	flagsServerToRedir = 0x00000001
	flagsSigned        = 0x00000008
)

// SMB2_CANCEL 命令码，AES-GMAC 的 nonce 需要区分它（MS-SMB2 §3.1.4.1）。
const commandCancel = 0x000C

// SigningAlgorithm 是 SMB 3.1.1 SIGNING_CAPABILITIES 协商出的签名算法
// （MS-SMB2 §2.2.3.1.7）。
type SigningAlgorithm uint16

const (
	SigningHMACSHA256 SigningAlgorithm = 0x0000
	SigningAESCMAC    SigningAlgorithm = 0x0001
	SigningAESGMAC    SigningAlgorithm = 0x0002
)

var (
	// ErrShortMessage 表示报文短于 SMB2 头，无法签名/校验。
	ErrShortMessage = errors.New("crypto: message shorter than SMB2 header")
	// ErrSigningAlgorithm 表示签名算法未知。
	ErrSigningAlgorithm = errors.New("crypto: unknown signing algorithm")
	// ErrBadSignature 表示签名校验失败。
	ErrBadSignature = errors.New("crypto: bad SMB2 signature")
)

// SigningAlgorithmForDialect 返回方言的默认签名算法（未协商 SIGNING_CAPABILITIES 时）。
//
//	2.0.2 / 2.1        HMAC-SHA256 取前 16 字节
//	3.0 / 3.0.2 / 3.1.1 AES-128-CMAC
func SigningAlgorithmForDialect(dialect uint16) SigningAlgorithm {
	if dialect >= DialectSMB300 {
		return SigningAESCMAC
	}
	return SigningHMACSHA256
}

// Sign 就地为一条 SMB2 消息签名（按方言选择默认算法）。
//
// 流程（MS-SMB2 §3.1.4.1）：
//  1. Signature 字段（偏移 0x30 起 16 字节）清零；
//  2. 置 SMB2_FLAGS_SIGNED（偏移 0x10 的 Flags）；
//  3. 对**整条消息**计算 MAC；
//  4. 把 MAC 前 16 字节写回 Signature。
//
// msg 必须是**单条**消息的完整字节（复合链中逐条单独签名，
// 且计算范围包含该条尾部为 8 字节对齐补的填充字节）。
func Sign(dialect uint16, key, msg []byte) error {
	return SignWith(SigningAlgorithmForDialect(dialect), key, msg)
}

// SignWith 用指定算法就地签名。
func SignWith(alg SigningAlgorithm, key, msg []byte) error {
	if len(msg) < HeaderSize {
		return ErrShortMessage
	}
	// 先清零签名字段并置 SIGNED 标志，再计算 MAC。
	sig := msg[offSignature : offSignature+SignatureSize]
	for i := range sig {
		sig[i] = 0
	}
	flags := binary.LittleEndian.Uint32(msg[offFlags:])
	binary.LittleEndian.PutUint32(msg[offFlags:], flags|flagsSigned)

	mac, err := computeMAC(alg, key, msg)
	if err != nil {
		return err
	}
	copy(sig, mac)
	return nil
}

// Verify 校验一条 SMB2 消息的签名（按方言选择默认算法）。
func Verify(dialect uint16, key, msg []byte) error {
	return VerifyWith(SigningAlgorithmForDialect(dialect), key, msg)
}

// VerifyWith 用指定算法校验签名。
//
// 校验过程需要临时清零 Signature 字段，函数返回前会原样恢复，
// 因此 msg 会被短暂修改（调用方必须独占该缓冲区）。
// 比较使用 crypto/subtle 常量时间比较。
func VerifyWith(alg SigningAlgorithm, key, msg []byte) error {
	if len(msg) < HeaderSize {
		return ErrShortMessage
	}
	sig := msg[offSignature : offSignature+SignatureSize]

	var want [SignatureSize]byte
	copy(want[:], sig)
	for i := range sig {
		sig[i] = 0
	}
	// SIGNED 标志本身参与 MAC 计算，客户端发来时它已经置位；
	// 这里不改动 Flags，按收到的原样计算。
	mac, err := computeMAC(alg, key, msg)
	copy(sig, want[:]) // 恢复现场
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(mac[:SignatureSize], want[:]) != 1 {
		return ErrBadSignature
	}
	return nil
}

// computeMAC 返回长度至少 16 字节的 MAC。
func computeMAC(alg SigningAlgorithm, key, msg []byte) ([]byte, error) {
	switch alg {
	case SigningHMACSHA256:
		h := hmac.New(sha256.New, key)
		h.Write(msg)
		return h.Sum(nil), nil

	case SigningAESCMAC:
		return CMAC(key, msg)

	case SigningAESGMAC:
		nonce := gmacNonce(msg)
		// RFC 4543 AES-GMAC = GCM 下明文为空、消息全部作为 AAD，输出即 16 字节 tag。
		// 签名密钥与 CMAC 同源（MS-SMB2 §3.1.4.2 按"用途"派生，与具体
		// MAC 算法无关），因此这里不需要新的 KDF。
		return GMAC(key, nonce[:], msg)

	default:
		return nil, ErrSigningAlgorithm
	}
}

// gmacNonce 构造 AES-GMAC 的 12 字节 nonce（MS-SMB2 §3.1.4.1）：
//
//	前 8 字节 = MessageId（小端原样）
//	第 9 字节 bit0 = 1 表示服务端发出的消息，bit1 = 1 表示 SMB2 CANCEL
//	其余为 0
func gmacNonce(msg []byte) [12]byte {
	var nonce [12]byte
	copy(nonce[:8], msg[offMessageID:offMessageID+8])

	flags := binary.LittleEndian.Uint32(msg[offFlags:])
	cmd := binary.LittleEndian.Uint16(msg[offCommand:])
	var b byte
	if flags&flagsServerToRedir != 0 {
		b |= 0x01
	}
	if cmd == commandCancel {
		b |= 0x02
	}
	nonce[8] = b
	return nonce
}

// IsSigned 报告消息头是否置了 SMB2_FLAGS_SIGNED。
func IsSigned(msg []byte) bool {
	if len(msg) < HeaderSize {
		return false
	}
	return binary.LittleEndian.Uint32(msg[offFlags:])&flagsSigned != 0
}
