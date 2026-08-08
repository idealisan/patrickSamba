package crypto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// KDF 的输入拼装必须是 [i]_32 || Label || 0x00 || Context || [L]_32。
// 这里用手工拼装的字节串独立算一遍 HMAC-SHA256 来交叉验证。
func TestKDF_InputLayout(t *testing.T) {
	ki := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	label := []byte("SMB2AESCMAC\x00")
	context := []byte("SmbSign\x00")

	var input []byte
	input = append(input, 0x00, 0x00, 0x00, 0x01) // [i]_32 = 1
	input = append(input, label...)
	input = append(input, 0x00)
	input = append(input, context...)
	input = append(input, 0x00, 0x00, 0x00, 0x80) // [L]_32 = 128

	h := hmac.New(sha256.New, ki)
	h.Write(input)
	want := h.Sum(nil)[:16]

	if got := KDF(ki, label, context, 128); !bytes.Equal(got, want) {
		t.Errorf("KDF = %x, want %x", got, want)
	}

	// L = 256 时 HMAC-SHA256 一次迭代刚好 32 字节。
	input[len(input)-2] = 0x01
	input[len(input)-1] = 0x00
	h.Reset()
	h.Write(input)
	if got := KDF(ki, label, context, 256); !bytes.Equal(got, h.Sum(nil)) {
		t.Errorf("KDF(256) mismatch")
	}
}

// Label / Context 大小写敏感且带结尾 NUL；"ServerIn " 结尾有空格。
// 这些常量一旦写错，与真实客户端的签名/加密就完全对不上，故单独钉住。
func TestKDFLabels(t *testing.T) {
	cases := []struct {
		got  []byte
		want string
	}{
		{labelSMB2AESCMAC, "SMB2AESCMAC\x00"},
		{contextSmbSign, "SmbSign\x00"},
		{labelSMB2APP, "SMB2APP\x00"},
		{contextSmbRpc, "SmbRpc\x00"},
		{labelSMB2AESCCM, "SMB2AESCCM\x00"},
		{contextServerOut, "ServerOut\x00"},
		{contextServerIn, "ServerIn \x00"},
		{labelSMBSigningKey, "SMBSigningKey\x00"},
		{labelSMBAppKey, "SMBAppKey\x00"},
		{labelSMBS2CCipherKey, "SMBS2CCipherKey\x00"},
		{labelSMBC2SCipherKey, "SMBC2SCipherKey\x00"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("label %q != %q", tc.got, tc.want)
		}
	}
}

func TestDerivedKeys(t *testing.T) {
	sessionKey := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	preauth := make([]byte, 64)
	for i := range preauth {
		preauth[i] = byte(i)
	}

	// 2.x 不派生，直接用 SessionKey。
	for _, d := range []uint16{DialectSMB202, DialectSMB210} {
		if got := SigningKey(d, sessionKey, nil); !bytes.Equal(got, sessionKey) {
			t.Errorf("SigningKey(%#04x) = %x, want session key", d, got)
		}
	}

	// 3.0 / 3.0.2 用固定 Label/Context，与 preauth 无关。
	a := SigningKey(DialectSMB300, sessionKey, preauth)
	b := SigningKey(DialectSMB302, sessionKey, nil)
	if !bytes.Equal(a, b) {
		t.Error("3.0 与 3.0.2 的 SigningKey 应当一致且不依赖 preauth")
	}
	if len(a) != 16 {
		t.Fatalf("SigningKey 长度 = %d", len(a))
	}

	// 3.1.1 依赖 preauth 哈希。
	c := SigningKey(DialectSMB311, sessionKey, preauth)
	if bytes.Equal(a, c) {
		t.Error("3.1.1 与 3.0 的 SigningKey 不应相同")
	}
	other := make([]byte, 64)
	if bytes.Equal(c, SigningKey(DialectSMB311, sessionKey, other)) {
		t.Error("3.1.1 SigningKey 必须随 preauth 变化")
	}

	// 四种密钥必须互不相同。
	keys := map[string][]byte{
		"sign": SigningKey(DialectSMB311, sessionKey, preauth),
		"app":  ApplicationKey(DialectSMB311, sessionKey, preauth),
		"s2c":  ServerOutKey(DialectSMB311, sessionKey, preauth, 16),
		"c2s":  ServerInKey(DialectSMB311, sessionKey, preauth, 16),
	}
	for n1, k1 := range keys {
		for n2, k2 := range keys {
			if n1 < n2 && bytes.Equal(k1, k2) {
				t.Errorf("%s 与 %s 派生出了相同的密钥", n1, n2)
			}
		}
	}

	// AES-256 场景：仅 3.1.1 允许 32 字节。
	if got := ServerOutKey(DialectSMB311, sessionKey, preauth, 32); len(got) != 32 {
		t.Errorf("AES-256 ServerOutKey 长度 = %d, want 32", len(got))
	}
	if got := ServerOutKey(DialectSMB300, sessionKey, preauth, 32); len(got) != 16 {
		t.Errorf("3.0 ServerOutKey 长度 = %d, want 16", len(got))
	}
	// S→C 与 C→S 在 3.0 下靠 Context 区分。
	if bytes.Equal(
		ServerOutKey(DialectSMB300, sessionKey, nil, 16),
		ServerInKey(DialectSMB300, sessionKey, nil, 16),
	) {
		t.Error("3.0 的 ServerOut/ServerIn 密钥不应相同")
	}
}
