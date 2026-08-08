package crypto

import (
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"math"
)

// ccm 实现 NIST SP 800-38C 的 CCM 模式（Counter with CBC-MAC），
// 满足 crypto/cipher.AEAD 接口。
//
// 标准库只提供 GCM，而 SMB 3.x 的 AES-128-CCM / AES-256-CCM
// （MS-SMB2 §2.2.3.1.2 EncryptionCapabilities）必须自实现。
//
// SMB3 使用 nonce 长度 11 字节、tag 长度 16 字节。
type ccm struct {
	b         cipher.Block
	nonceSize int
	tagSize   int
}

var (
	// ErrCCMParams 表示 nonce/tag 长度不符合 SP800-38C 的取值范围。
	ErrCCMParams = errors.New("crypto: invalid AES-CCM parameters")
	// ErrCCMOpen 表示认证失败（tag 不匹配）或密文格式非法。
	ErrCCMOpen = errors.New("crypto: message authentication failed")
)

// NewCCM 构造 AES-CCM AEAD。
//
// SP 800-38C Appendix A：
//   - nonce 长度 n ∈ [7, 13]，q = 15 - n 为长度域字节数；
//   - tag 长度 t ∈ {4, 6, 8, 10, 12, 14, 16}。
func NewCCM(b cipher.Block, nonceSize, tagSize int) (cipher.AEAD, error) {
	if b.BlockSize() != blockSize {
		return nil, ErrCCMParams
	}
	if nonceSize < 7 || nonceSize > 13 {
		return nil, ErrCCMParams
	}
	if tagSize < 4 || tagSize > 16 || tagSize%2 != 0 {
		return nil, ErrCCMParams
	}
	return &ccm{b: b, nonceSize: nonceSize, tagSize: tagSize}, nil
}

func (c *ccm) NonceSize() int { return c.nonceSize }
func (c *ccm) Overhead() int  { return c.tagSize }

// maxPlaintextLen 返回长度域 q 字节能表示的最大明文长度。
func (c *ccm) maxPlaintextLen() uint64 {
	q := 15 - c.nonceSize
	if q >= 8 {
		return math.MaxUint64
	}
	return uint64(1)<<(8*uint(q)) - 1
}

// Seal 加密并认证 plaintext，输出 ciphertext||tag 追加到 dst。
func (c *ccm) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	if len(nonce) != c.nonceSize {
		panic("crypto: incorrect nonce length given to AES-CCM")
	}
	if uint64(len(plaintext)) > c.maxPlaintextLen() {
		panic("crypto: message too large for AES-CCM")
	}

	var tag [blockSize]byte
	c.cbcMAC(&tag, nonce, plaintext, additionalData)

	ret, out := sliceForAppend(dst, len(plaintext)+c.tagSize)
	c.ctrCrypt(out[:len(plaintext)], plaintext, nonce, &tag)
	copy(out[len(plaintext):], tag[:c.tagSize])
	return ret
}

// Open 校验并解密 ciphertext（末尾 tagSize 字节为 tag）。
func (c *ccm) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(nonce) != c.nonceSize {
		return nil, ErrCCMParams
	}
	if len(ciphertext) < c.tagSize {
		return nil, ErrCCMOpen
	}
	ctBody := ciphertext[:len(ciphertext)-c.tagSize]
	wantTag := ciphertext[len(ciphertext)-c.tagSize:]
	if uint64(len(ctBody)) > c.maxPlaintextLen() {
		return nil, ErrCCMOpen
	}

	// 解密：CTR 从计数器 1 开始；ctrCrypt 顺带把 S0 异或进 mask，
	// 于是 mask 最终就是 S0（初值全零）。
	var mask [blockSize]byte
	plain := make([]byte, len(ctBody))
	c.ctrCrypt(plain, ctBody, nonce, &mask)

	var tag [blockSize]byte
	c.cbcMAC(&tag, nonce, plain, additionalData)
	xorInto(tag[:], mask[:])

	if subtle.ConstantTimeCompare(tag[:c.tagSize], wantTag) != 1 {
		// 认证失败时不得泄露任何明文。
		for i := range plain {
			plain[i] = 0
		}
		return nil, ErrCCMOpen
	}

	ret, out := sliceForAppend(dst, len(plain))
	copy(out, plain)
	return ret, nil
}

// cbcMAC 按 SP 800-38C §6.1 计算格式化后的 CBC-MAC，结果写入 tag（完整分组）。
//
// 注意：这里返回的是 **未经 S0 掩码** 的 T。调用方（ctrCrypt）会在生成
// 密钥流时用 S0 异或它。
func (c *ccm) cbcMAC(tag *[blockSize]byte, nonce, plaintext, aad []byte) {
	q := 15 - c.nonceSize

	// B0 = Flags || Nonce || Q
	// Flags = 64*Adata + 8*((t-2)/2) + (q-1)
	var b0 [blockSize]byte
	flags := byte((c.tagSize-2)/2)<<3 | byte(q-1)
	if len(aad) > 0 {
		flags |= 1 << 6
	}
	b0[0] = flags
	copy(b0[1:], nonce)
	putUintBE(b0[blockSize-q:], uint64(len(plaintext)))

	c.b.Encrypt(tag[:], b0[:])

	if len(aad) > 0 {
		// a 的长度编码（SP 800-38C A.2.2）。
		var hdr [10]byte
		var n int
		a := uint64(len(aad))
		switch {
		case a < (1<<16 - 1<<8):
			binary.BigEndian.PutUint16(hdr[:2], uint16(a))
			n = 2
		case a <= math.MaxUint32:
			hdr[0], hdr[1] = 0xff, 0xfe
			binary.BigEndian.PutUint32(hdr[2:6], uint32(a))
			n = 6
		default:
			hdr[0], hdr[1] = 0xff, 0xff
			binary.BigEndian.PutUint64(hdr[2:10], a)
			n = 10
		}
		c.macBlocks(tag, hdr[:n], aad)
	}
	c.macBlocks(tag, plaintext)
}

// macBlocks 把若干段数据串接后按 16 字节分组做 CBC-MAC，
// 末尾不足一整块的部分用 0x00 补齐（SP 800-38C 的格式化要求）。
func (c *ccm) macBlocks(tag *[blockSize]byte, parts ...[]byte) {
	var buf [blockSize]byte
	n := 0
	for _, p := range parts {
		for len(p) > 0 {
			k := copy(buf[n:], p)
			p = p[k:]
			n += k
			if n == blockSize {
				xorInto(tag[:], buf[:])
				c.b.Encrypt(tag[:], tag[:])
				n = 0
			}
		}
	}
	if n > 0 {
		for i := n; i < blockSize; i++ {
			buf[i] = 0
		}
		xorInto(tag[:], buf[:])
		c.b.Encrypt(tag[:], tag[:])
	}
}

// counterBlock 构造 A_i = Flags(q-1) || Nonce || [i]_q。
func (c *ccm) counterBlock(out *[blockSize]byte, nonce []byte, i uint64) {
	q := 15 - c.nonceSize
	for j := range out {
		out[j] = 0
	}
	out[0] = byte(q - 1)
	copy(out[1:], nonce)
	putUintBE(out[blockSize-q:], i)
}

// ctrCrypt 用 CTR 模式加/解密 src 到 dst（长度相同），
// 同时把 S0 异或进 tag（tag 为 CBC-MAC 的输出，全零表示不需要掩码）。
func (c *ccm) ctrCrypt(dst, src []byte, nonce []byte, tag *[blockSize]byte) {
	var a, s [blockSize]byte

	c.counterBlock(&a, nonce, 0)
	c.b.Encrypt(s[:], a[:])
	xorInto(tag[:], s[:]) // T ^= S0

	for i := uint64(1); len(src) > 0; i++ {
		c.counterBlock(&a, nonce, i)
		c.b.Encrypt(s[:], a[:])
		n := len(src)
		if n > blockSize {
			n = blockSize
		}
		for j := 0; j < n; j++ {
			dst[j] = src[j] ^ s[j]
		}
		dst, src = dst[n:], src[n:]
	}
}

// putUintBE 把 v 以大端写入 dst（dst 长度即为字节数，高位截断由调用方保证不发生）。
func putUintBE(dst []byte, v uint64) {
	for i := len(dst) - 1; i >= 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
}

// sliceForAppend 与标准库 crypto/cipher 内部同名函数语义一致：
// 为 dst 扩容 n 字节并返回整体切片与新增区域。
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}
