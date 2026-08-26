package crypto

import (
	"crypto/aes"
	"crypto/cipher"
)

// GMAC 计算 AES-128-GMAC：GCM 的 AAD-only 口径（NIST SP 800-38D §5.2.1.2
// 定义 GMAC 为 P 为空串的 GCM；[RFC 4543 §3] 同口径）。
//
// 实现走标准库 cipher.NewGCM 的 Seal(nil, nonce, nil /*明文*/, data)：
// 明文为空时输出就是 16 字节认证 tag，数据全部作为 AAD 进 GHASH ——
// 这条路径在 amd64/arm64 上直接命中 AES-NI/加密扩展的 GCM 硬件加速，
// 这正是它比 CMAC 快的原因（性能移交项 M4）。
//
// nonce 必须是 12 字节（96 位，SP 800-38D §8 的 IV 唯一性要求；
// SMB2 侧由 gmacNonce 按 MS-SMB2 §3.1.4.1 构造）。
// key 长度决定 AES-128/192/256；SMB 签名密钥恒为 16 字节。
//
// 返回值恰为 16 字节 tag（GCM 默认 tag 长度），可直接写入 SMB2 头的
// Signature 字段（MS-SMB2 §2.2.1.2）。
func GMAC(key, nonce, data []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, nonce, nil, data), nil
}
