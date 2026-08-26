package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

// benchMsg 生成 n 字节的确定性伪随机消息（避免全零被任何优化路径特殊化）。
func benchMsg(b *testing.B, n int) []byte {
	msg := make([]byte, n)
	if _, err := rand.Read(msg); err != nil {
		b.Fatal(err)
	}
	return msg
}

func benchKey(b *testing.B, n int) []byte {
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		b.Fatal(err)
	}
	return k
}

func BenchmarkCMAC_64B(b *testing.B)         { benchCMAC(b, 64) }
func BenchmarkCMAC_1KiB(b *testing.B)        { benchCMAC(b, 1<<10) }
func BenchmarkCMAC_64KiB(b *testing.B)       { benchCMAC(b, 1<<16) }
func BenchmarkCMAC_1MiB(b *testing.B)        { benchCMAC(b, 1<<20) }
func BenchmarkCMAC_1MiBPlus512(b *testing.B) { benchCMAC(b, 1<<20+512) }

func benchCMAC(b *testing.B, n int) {
	key := benchKey(b, 16)
	msg := benchMsg(b, n)
	b.SetBytes(int64(n))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CMAC(key, msg); err != nil {
			b.Fatal(err)
		}
	}
}

// 对照组：签名算法协商的另一侧与加密路径的参照吞吐。
// 数字解读见 test/reports/perf-v050-analysis-draft.md。

func BenchmarkHMACSHA256_1MiB(b *testing.B) {
	key := benchKey(b, 16)
	msg := benchMsg(b, 1<<20)
	b.SetBytes(1 << 20)
	for i := 0; i < b.N; i++ {
		h := hmac.New(sha256.New, key)
		h.Write(msg)
		_ = h.Sum(nil)
	}
}

func BenchmarkGCMSeal_1MiB(b *testing.B) {
	block, err := aes.NewCipher(benchKey(b, 16))
	if err != nil {
		b.Fatal(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		b.Fatal(err)
	}
	msg := benchMsg(b, 1<<20)
	nonce := benchMsg(b, 12)
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.Seal(nil, nonce, msg, nil)
	}
}
