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
	"encoding/binary"
	"sync"
	"sync/atomic"
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
	st, err := cachedMAC(key)
	if err != nil {
		return nil, err
	}
	return st.sum(msg), nil
}

// macState 是按密钥缓存的 CMAC 计算状态：展开后的分组密码与 RFC 4493 子钥。
// 构造完成后字段全部只读，方法可并发使用（签名/验签可能来自不同 goroutine）。
type macState struct {
	b      cipher.Block
	k1, k2 [blockSize]byte
}

// macCache 以密钥字节为键缓存 macState，省掉每条报文的密钥扩展与子钥推导
// （实测占小消息 CMAC 成本的一半上下；控制 PDU 绝大多数 <1KiB）。
//
// 键空间 = 出现过的会话密钥种类，受认证与连接数天然约束；但会话密钥
// 逐会话随机，长期运行下仍会无界增长，因此超过上限时整体换新 ——
// 缓存只是加速结构，清空不影响任何正确性。
var (
	macCache     sync.Map // string(key) -> *macState
	macCacheSize atomic.Int64
	macCacheMax  = int64(4096) // ≈1.2 MB 上限，足够覆盖全部合法并发会话还有余
)

func cachedMAC(key []byte) (*macState, error) {
	ks := string(key)
	if v, ok := macCache.Load(ks); ok {
		return v.(*macState), nil
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	k1, k2 := cmacSubkeys(b)
	st := &macState{b: b, k1: k1, k2: k2}
	if macCacheSize.Add(1) > macCacheMax {
		// 粗粒度限界：整体重置。存在并发下的计数偏差，无害。
		macCache = sync.Map{}
		macCacheSize.Store(0)
	}
	macCache.Store(ks, st)
	return st, nil
}

// sum 按 RFC 4493 计算 msg 的 CMAC，返回新分配的 16 字节 MAC。
func (st *macState) sum(msg []byte) []byte {
	out := make([]byte, blockSize)
	st.sumInto(out[:0], msg)
	return out
}

// sumInto 把 msg 的 CMAC 追加到 dst 末尾（dst 原内容不动）。
func (st *macState) sumInto(dst, msg []byte) []byte {
	return appendMAC(dst, st.b, st.k1, st.k2, msg)
}

// CMACWithBlock 用已构造好的分组密码计算 AES-CMAC，便于复用 cipher.Block
// 避免在热路径（每条报文签名）上重复做密钥扩展。
//
// b 的分组长度必须是 16 字节（AES 恒满足）。
//
// 实现说明（性能）：CMAC 的链式定义 X_i = E(X_{i-1} ⊕ M_i) 与 IV=0 的
// CBC-MAC 在末块处理之前完全同构，因此除末块外的整段数据交给
// cipher.NewCBCEncrypter 批量处理 —— amd64 上是 AES-NI 的分批汇编路径，
// 摊薄逐块接口调用的开销；末块按 RFC 4493 §2.4 单独异或子钥后加密一次。
// 输入 msg 不会被修改（验签路径要求原样保留），批量处理在复用的暂存
// 缓冲上进行，见 cbcMacChunks。
func CMACWithBlock(b cipher.Block, msg []byte) []byte {
	k1, k2 := cmacSubkeys(b)
	var dst []byte
	return appendMAC(dst, b, k1, k2, msg)
}

// appendMAC 是 CMAC 链式核心的公共体：给定分组密码与子钥，把 msg 的
// CMAC 追加到 dst。CMACWithBlock 与 (macState).sumInto 共用。
func appendMAC(dst []byte, b cipher.Block, k1, k2 [blockSize]byte, msg []byte) []byte {
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
	if n > 0 {
		if n <= cmacDirectBlocks {
			for i := 0; i < n; i++ {
				xorInto(x[:], msg[i*blockSize:(i+1)*blockSize])
				b.Encrypt(x[:], x[:])
			}
		} else {
			x = cbcMacChunks(b, msg[:n*blockSize])
		}
	}
	xorInto(x[:], last[:])
	b.Encrypt(x[:], x[:])

	return append(dst, x[:]...)
}

// cmacChunk 是批量 CBC-MAC 每次提交的暂存缓冲大小。
// 64 KiB 对 SMB2 报文（≤1 MiB+512B）意味着最多 17 次 CryptBlocks 调用，
// 暂存内存常驻可控，且让硬件路径每次都有足够长的分组串可吃。
const cmacChunk = 64 << 10

// cmacDirectBlocks 是「直接循环」与「批量 CBC」的路径分界（块数）。
// 微基准交叉点在 4~60 块之间：64 B 消息批量路径慢 ~39%（池与
// NewCBCEncrypter 的固定开销），1 KiB（64 块）起批量稳定反超 ~11%+。
const cmacDirectBlocks = 32

// cmacScratch 复用批量处理的暂存缓冲。CMAC 在每条报文的签名/验签路径上
// 各跑一次，若按消息长度分配会制造稳定的 GC 压力（实测堆 profile 中
// 密码学缓冲占比显著）。
var cmacScratch = sync.Pool{
	New: func() any { b := make([]byte, cmacChunk); return &b },
}

// cbcMacChunks 计算 body（长度必须是 blockSize 整数倍）的 IV=0 CBC-MAC 链值。
func cbcMacChunks(b cipher.Block, body []byte) [blockSize]byte {
	var chain [blockSize]byte

	bp := cmacScratch.Get().(*[]byte)
	buf := *bp
	defer cmacScratch.Put(bp)

	for off := 0; off < len(body); {
		n := len(body) - off
		if n > cmacChunk {
			n = cmacChunk
		}
		src := body[off : off+n]
		off += n

		work := buf[:n]
		copy(work, src)
		// 每段的 IV 取上一段的链值；CryptBlocks 就地覆盖 work，
		// 末 16 字节即本段结束时的链值。
		mode := cipher.NewCBCEncrypter(b, chain[:])
		mode.CryptBlocks(work, work)
		copy(chain[:], work[n-blockSize:])
	}
	return chain
}

// xorInto 计算 dst ^= src，按 dst 长度处理（src 必须不短于 dst）。
//
// 按 8 字节字处理，尾部不足一字的部分退回逐字节 —— XOR 出现在
// CMAC/CCM 的每分组热路径上，字长运算省掉约一半的循环开销。
func xorInto(dst, src []byte) {
	i := 0
	for ; i+8 <= len(dst); i += 8 {
		l := binary.LittleEndian.Uint64(dst[i:])
		r := binary.LittleEndian.Uint64(src[i:])
		binary.LittleEndian.PutUint64(dst[i:], l^r)
	}
	for ; i < len(dst); i++ {
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
