package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// RFC 1320 附录 A.5 MDDriver 的官方测试向量。
func TestMD4RFC1320Vectors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "31d6cfe0d16ae931b73c59d7e0c089c0"},
		{"a", "bde52cb31de33e46245e05fbdbd6fb24"},
		{"abc", "a448017aaf21d8525fc10ae87aa6729d"},
		{"message digest", "d9130a8164549fe818874806e1c7014b"},
		{"abcdefghijklmnopqrstuvwxyz", "d79e1c308aa5bbcdeea8ed63df412da9"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
			"043f8582f241db351ce627e153e7f0e4"},
		{"12345678901234567890123456789012345678901234567890123456789012345678901234567890",
			"e33b4ddc9c38f2199c3e7b164fcc0536"},
	}
	for _, c := range cases {
		got := md4Sum([]byte(c.in))
		if hex.EncodeToString(got[:]) != c.want {
			t.Errorf("md4(%q) = %x, want %s", c.in, got, c.want)
		}
	}
}

// 填充分支覆盖说明：上面的官方向量里
//   - 62 字节串（rem ≥ 56）走两块尾部分支；
//   - 80 字节串（n=64, rem=16）走多分组 + 单块尾部分支。
//
// 这里只额外确认各种长度都不会 panic、长度正确。
func TestMD4NoPanicAllLengths(t *testing.T) {
	for n := 0; n <= 200; n++ {
		got := md4Sum([]byte(strings.Repeat("a", n)))
		if len(got) != md4Size {
			t.Fatalf("n=%d 摘要长度 %d", n, len(got))
		}
	}
}

// NT hash：MD4(UTF16LE(password))。MS-NLMP §4.2.1 的口令是 "Password"。
func TestNTHashMSNLMP(t *testing.T) {
	// MS-NLMP §4.2.2.1.2 / §4.2.3.1.1：
	// NTOWFv1("Password") = a4f49c406510bdcab6824ee7c30fd852
	got := NTHash("Password")
	want := "a4f49c406510bdcab6824ee7c30fd852"
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("NTHash(Password) = %x, want %s", got, want)
	}
}
