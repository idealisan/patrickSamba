// Package crypto 实现 SMB2/3 所需的密码学原语：
// AES-CMAC、AES-CCM、SP800-108 KDF、报文签名、preauth 完整性哈希与 SMB3 加解密。
//
// 本包不依赖任何其他内部包（尤其**不 import wire**，避免循环依赖），
// 所有接口都以 []byte 交换数据。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
)

// AES 分组长度恒为 16 字节。
const blockSize = aes.BlockSize

// rb 是 RFC 4493 §2.3 中 128 位分组的常量 R_b = 0x00000000000000000000000000000087。
const rb = 0x87

// CMACSize 是 AES-CMAC 输出长度（字节）。RFC 4493 §2.4 T 为一个完整分组。
const CMACSize = blockSize

// cmacSubkeys 按 RFC 4493 §2.3 Subkey Generation Algorithm 生成 K1、K2。
//
//	L  = AES-128(K, const_Zero)
//	K1 = (MSB(L) == 0) ? L << 1 : (L << 1) XOR R_b
//	K2 = (MSB(K1) == 0) ? K1 << 1 : (K1 << 1) XOR R_b
func cmacSubkeys(b cipher.Block) (k1, k2 [blockSize]byte) {
	var l [blockSize]byte
	b.Encrypt(l[:], l[:]) // L = E(K, 0^128)

	k1 = shiftLeft1(l)
	k2 = shiftLeft1(k1)
	return k1, k2
}

// shiftLeft1 对 128 位大端整数左移一位，若移出的最高位为 1 则异或 R_b。
func shiftLeft1(in [blockSize]byte) [blockSize]byte {
	var out [blockSize]byte
	msb := in[0] >> 7
	for i := 0; i < blockSize-1; i++ {
		out[i] = in[i]<<1 | in[i+1]>>7
	}
	out[blockSize-1] = in[blockSize-1] << 1
	// 常量时间地条件异或 R_b，避免依赖密钥相关分支。
	out[blockSize-1] ^= rb & -msb
	return out
}

// CMAC 计算 AES-CMAC（RFC 4493）。
//
// key 长度决定使用 AES-128/192/256；分组长度恒为 16，故 R_b 恒为 0x87。
// 返回 16 字节 MAC。key 非法时返回错误。
func CMAC(key, msg []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return CMACWithBlock(b, msg), nil
}

// CMACWithBlock 用已构造好的分组密码计算 AES-CMAC，便于复用 cipher.Block
// 避免在热路径（每条报文签名）上重复做密钥扩展。
//
// b 的分组长度必须是 16 字节（AES 恒满足）。
func CMACWithBlock(b cipher.Block, msg []byte) []byte {
	k1, k2 := cmacSubkeys(b)

	// RFC 4493 §2.4：n = ceil(len/16)；len 为 0 时 n = 1 且视为不完整分组。
	var last [blockSize]byte
	n := len(msg) / blockSize
	complete := len(msg) > 0 && len(msg)%blockSize == 0

	if complete {
		n--
		copy(last[:], msg[n*blockSize:])
		xorInto(last[:], k1[:])
	} else {
		rem := msg[n*blockSize:]
		copy(last[:], rem)
		last[len(rem)] = 0x80 // padding: 0x80 || 0x00...
		xorInto(last[:], k2[:])
	}

	var x [blockSize]byte
	for i := 0; i < n; i++ {
		xorInto(x[:], msg[i*blockSize:(i+1)*blockSize])
		b.Encrypt(x[:], x[:])
	}
	xorInto(x[:], last[:])
	b.Encrypt(x[:], x[:])

	out := make([]byte, blockSize)
	copy(out, x[:])
	return out
}

// xorInto 计算 dst ^= src，按 dst 长度处理（src 必须不短于 dst）。
func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// CMACVerify 以常量时间比较校验 AES-CMAC。
func CMACVerify(key, msg, mac []byte) bool {
	got, err := CMAC(key, msg)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, mac) == 1
}
