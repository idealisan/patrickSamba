package crypto

import (
	"crypto/rand"
	"testing"
)

// GMAC 签名基准：与 CMAC_* 同口径（bench_test.go），用于量化
// AES-GMAC（GCM 硬件路径）相对 AES-CMAC 的签名吞吐差异。
func benchGMACSign(b *testing.B, n int) {
	b.Helper()
	key := make([]byte, 16)
	msg := make([]byte, n)
	rand.Read(key)
	rand.Read(msg)
	b.SetBytes(int64(n))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := SignWith(SigningAESGMAC, key, msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGMACSign_64B(b *testing.B)         { benchGMACSign(b, 64) }
func BenchmarkGMACSign_1KiB(b *testing.B)        { benchGMACSign(b, 1<<10) }
func BenchmarkGMACSign_64KiB(b *testing.B)       { benchGMACSign(b, 1<<16) }
func BenchmarkGMACSign_1MiB(b *testing.B)        { benchGMACSign(b, 1<<20) }
func BenchmarkGMACSign_1MiBPlus512(b *testing.B) { benchGMACSign(b, 1<<20+512) }
