package crypto

import (
	"bytes"
	"crypto/aes"
	"testing"
)

// RFC 3610 §8 Packet Vectors —— AES-CCM 官方测试向量。
// 这些向量的 nonce = 13 字节、tag(M) = 8 字节。
func TestCCM_RFC3610(t *testing.T) {
	key := mustHex(t, "c0c1c2c3c4c5c6c7c8c9cacbcccdcecf")

	cases := []struct {
		name  string
		nonce string
		aad   string
		pt    string
		out   string // ciphertext || tag
	}{
		{
			name:  "Packet Vector #1",
			nonce: "00000003020100a0a1a2a3a4a5",
			aad:   "0001020304050607",
			pt:    "08090a0b0c0d0e0f101112131415161718191a1b1c1d1e",
			out: "588c979a61c663d2f066d0c2c0f98980" +
				"6d5f6b61dac384" + "17e8d12cfdf926e0",
		},
		{
			name:  "Packet Vector #2",
			nonce: "00000004030201a0a1a2a3a4a5",
			aad:   "0001020304050607",
			pt:    "08090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
			out: "72c91a36e135f8cf291ca894085c87e3" +
				"cc15c439c9e43a3b" + "a091d56e10400916",
		},
		{
			name:  "Packet Vector #3",
			nonce: "00000005040302a0a1a2a3a4a5",
			aad:   "0001020304050607",
			pt:    "08090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
			out: "51b1e5f44a197d1da46b0f8e2d282ae8" +
				"71e838bb64da8596574adaa76fbd9fb0c5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := aes.NewCipher(key)
			if err != nil {
				t.Fatal(err)
			}
			nonce := mustHex(t, tc.nonce)
			a, err := NewCCM(b, len(nonce), 8)
			if err != nil {
				t.Fatal(err)
			}
			got := a.Seal(nil, nonce, mustHex(t, tc.pt), mustHex(t, tc.aad))
			if want := mustHex(t, tc.out); !bytes.Equal(got, want) {
				t.Fatalf("Seal = %x\nwant   %x", got, want)
			}
			pt, err := a.Open(nil, nonce, got, mustHex(t, tc.aad))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if want := mustHex(t, tc.pt); !bytes.Equal(pt, want) {
				t.Fatalf("Open = %x, want %x", pt, want)
			}
		})
	}
}

// SMB3 使用 nonce 11 字节、tag 16 字节（MS-SMB2 §3.1.4.3）。
func TestCCM_SMB3Params(t *testing.T) {
	for _, keyLen := range []int{16, 32} {
		key := make([]byte, keyLen)
		for i := range key {
			key[i] = byte(i * 7)
		}
		b, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		a, err := NewCCM(b, 11, 16)
		if err != nil {
			t.Fatal(err)
		}
		if a.NonceSize() != 11 || a.Overhead() != 16 {
			t.Fatalf("NonceSize=%d Overhead=%d", a.NonceSize(), a.Overhead())
		}
		nonce := make([]byte, 11)
		aad := make([]byte, 32)
		for _, n := range []int{0, 1, 15, 16, 17, 4096} {
			pt := make([]byte, n)
			for i := range pt {
				pt[i] = byte(i)
			}
			ct := a.Seal(nil, nonce, pt, aad)
			if len(ct) != n+16 {
				t.Fatalf("len(ct) = %d, want %d", len(ct), n+16)
			}
			got, err := a.Open(nil, nonce, ct, aad)
			if err != nil {
				t.Fatalf("Open(n=%d): %v", n, err)
			}
			if !bytes.Equal(got, pt) {
				t.Fatalf("roundtrip mismatch at n=%d", n)
			}
			// 篡改密文/AAD 必须被拒绝。
			bad := append([]byte(nil), ct...)
			bad[len(bad)-1] ^= 1
			if _, err := a.Open(nil, nonce, bad, aad); err == nil {
				t.Fatal("Open accepted a tampered tag")
			}
			badAAD := append([]byte(nil), aad...)
			badAAD[0] ^= 1
			if _, err := a.Open(nil, nonce, ct, badAAD); err == nil {
				t.Fatal("Open accepted a tampered AAD")
			}
		}
	}
}

func TestCCM_BadParams(t *testing.T) {
	b, err := aes.NewCipher(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ nonce, tag int }{
		{6, 16}, {14, 16}, {11, 3}, {11, 18}, {11, 15},
	} {
		if _, err := NewCCM(b, tc.nonce, tc.tag); err == nil {
			t.Errorf("NewCCM(nonce=%d, tag=%d) accepted", tc.nonce, tc.tag)
		}
	}
	a, err := NewCCM(b, 11, 16)
	if err != nil {
		t.Fatal(err)
	}
	// 短于 tag 的密文不得 panic。
	if _, err := a.Open(nil, make([]byte, 11), make([]byte, 4), nil); err == nil {
		t.Error("Open accepted a truncated ciphertext")
	}
	if _, err := a.Open(nil, make([]byte, 5), make([]byte, 32), nil); err == nil {
		t.Error("Open accepted a wrong-size nonce")
	}
}
