package auth

import "encoding/binary"

// MD4（RFC 1320）。
//
// Go 标准库没有 MD4，`golang.org/x/crypto/md4` 已 deprecated 且会引入额外依赖，
// 因此这里按 RFC 1320 §3 自实现约 80 行。NTLM 的 NT hash 定义为
// MD4(UTF16LE(password))（MS-NLMP §3.3.1），除此之外本项目不使用 MD4。
//
// 输入长度域为 **小端** 64 位比特数（RFC 1320 §3.2）。

// md4Size 是 MD4 摘要长度。
const md4Size = 16

// md4Sum 返回 data 的 MD4 摘要。
func md4Sum(data []byte) [md4Size]byte {
	// RFC 1320 §3.3 初始链接变量（小端存储的 0x01234567 等）。
	s := [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}

	// 整块处理。
	n := len(data) &^ 63
	for i := 0; i < n; i += 64 {
		md4Block(&s, data[i:i+64])
	}

	// RFC 1320 §3.1 填充：追加 0x80，补 0 至长度 ≡ 56 (mod 64)，
	// 再追加 64 位小端比特长度。剩余不足两块，用固定缓冲避免分配。
	var tail [128]byte
	rem := copy(tail[:], data[n:])
	tail[rem] = 0x80
	tailLen := 64
	if rem >= 56 {
		tailLen = 128
	}
	binary.LittleEndian.PutUint64(tail[tailLen-8:], uint64(len(data))*8)
	for i := 0; i < tailLen; i += 64 {
		md4Block(&s, tail[i:i+64])
	}

	var out [md4Size]byte
	for i, v := range s {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

// md4Block 处理一个 64 字节分组（RFC 1320 §3.4）。
func md4Block(s *[4]uint32, p []byte) {
	var x [16]uint32
	for i := range x {
		x[i] = binary.LittleEndian.Uint32(p[i*4:])
	}

	a, b, c, d := s[0], s[1], s[2], s[3]

	// Round 1：F(x,y,z) = (x AND y) OR (NOT(x) AND z)，s 依次 3,7,11,19。
	for i := 0; i < 16; i += 4 {
		a = rotl32(a+md4F(b, c, d)+x[i+0], 3)
		d = rotl32(d+md4F(a, b, c)+x[i+1], 7)
		c = rotl32(c+md4F(d, a, b)+x[i+2], 11)
		b = rotl32(b+md4F(c, d, a)+x[i+3], 19)
	}

	// Round 2：G(x,y,z) = (x AND y) OR (x AND z) OR (y AND z)，加常量 0x5A827999，
	// s 依次 3,5,9,13，k 顺序为 0,4,8,12, 1,5,9,13, ...
	for i := 0; i < 4; i++ {
		a = rotl32(a+md4G(b, c, d)+x[i+0]+0x5a827999, 3)
		d = rotl32(d+md4G(a, b, c)+x[i+4]+0x5a827999, 5)
		c = rotl32(c+md4G(d, a, b)+x[i+8]+0x5a827999, 9)
		b = rotl32(b+md4G(c, d, a)+x[i+12]+0x5a827999, 13)
	}

	// Round 3：H(x,y,z) = x XOR y XOR z，加常量 0x6ED9EBA1，s 依次 3,9,11,15。
	round3 := [4]int{0, 2, 1, 3}
	for _, i := range round3 {
		a = rotl32(a+md4H(b, c, d)+x[i+0]+0x6ed9eba1, 3)
		d = rotl32(d+md4H(a, b, c)+x[i+8]+0x6ed9eba1, 9)
		c = rotl32(c+md4H(d, a, b)+x[i+4]+0x6ed9eba1, 11)
		b = rotl32(b+md4H(c, d, a)+x[i+12]+0x6ed9eba1, 15)
	}

	s[0] += a
	s[1] += b
	s[2] += c
	s[3] += d
}

func md4F(x, y, z uint32) uint32 { return (x & y) | (^x & z) }
func md4G(x, y, z uint32) uint32 { return (x & y) | (x & z) | (y & z) }
func md4H(x, y, z uint32) uint32 { return x ^ y ^ z }

func rotl32(v uint32, n uint) uint32 { return v<<n | v>>(32-n) }
