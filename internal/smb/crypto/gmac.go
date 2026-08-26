package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"sync"
	"sync/atomic"
)

// String 返回签名算法的可读名（日志与测试诊断用）。
func (a SigningAlgorithm) String() string {
	switch a {
	case SigningHMACSHA256:
		return "hmac-sha256"
	case SigningAESCMAC:
		return "aes-cmac"
	case SigningAESGMAC:
		return "aes-gmac"
	default:
		return fmt.Sprintf("signing-alg(%d)", uint16(a))
	}
}

// gmacCache 以密钥字节为键缓存展开后的 GCM 实例（与 cmac.go 的 macCache
// 同型）：省掉每条报文签名/验签路径上的 AES 密钥扩展 —— 64 字节控制 PDU
// 上这笔固定开销比 GMAC 本体还贵（实测占一半以上）。构造后的
// cipher.AEAD 无可变状态，Seal/Open 可并发使用。
//
// 键空间 = 出现过的会话密钥种类；超过 macCacheMax 时整体换新，
// 与 macCache 相同的粗粒度限界策略（缓存只是加速结构，清空不影响正确性）。
var (
	gmacCache     sync.Map // string(key) -> cipher.AEAD
	gmacCacheSize atomic.Int64
)

func cachedGCM(key []byte) (cipher.AEAD, error) {
	ks := string(key)
	if v, ok := gmacCache.Load(ks); ok {
		return v.(cipher.AEAD), nil
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if gmacCacheSize.Add(1) > macCacheMax {
		// 粗粒度限界：整体重置。并发下的计数偏差无害。
		gmacCache = sync.Map{}
		gmacCacheSize.Store(0)
	}
	gmacCache.Store(ks, g)
	return g, nil
}

// GMAC 计算 AES-128-GMAC：GCM 的 AAD-only 口径（NIST SP 800-38D §5.2.1.2
// 定义 GMAC 为 P 为空串的 GCM；[RFC 4543 §3] 同口径）。
//
// 实现走标准库 cipher.NewGCM 的 Seal(nil, nonce, nil /*明文*/, data)：
// 明文为空时输出就是 16 字节认证 tag，数据全部作为 AAD 进 GHASH ——
// 这条路径在 amd64/arm64 上直接命中 AES-NI/加密扩展的 GCM 硬件加速，
// 这正是它比 CMAC 快的原因（性能移交项 M4）。展开后的 GCM 实例按密钥
// 缓存（见 gmacCache）。
//
// nonce 必须是 12 字节（96 位，SP 800-38D §8 的 IV 唯一性要求；
// SMB2 侧由 gmacNonce 按 MS-SMB2 §3.1.4.1 构造）。
// key 长度决定 AES-128/192/256；SMB 签名密钥恒为 16 字节。
//
// 返回值恰为 16 字节 tag（GCM 默认 tag 长度），可直接写入 SMB2 头的
// Signature 字段（MS-SMB2 §2.2.1.2）。
func GMAC(key, nonce, data []byte) ([]byte, error) {
	g, err := cachedGCM(key)
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, nonce, nil, data), nil
}
