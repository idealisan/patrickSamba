package crypto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// KDF 的输入拼装必须是 [i]_32 || Label || 0x00 || Context || [L]_32。
// 这里用手工拼装的字节串独立算一遍 HMAC-SHA256 来交叉验证。
//
// 注意：本测试只能证明"实现与它自己的描述一致"，**证明不了描述本身是对的**
// —— 如果 Label 后面到底要不要再加一个 0x00 分隔符搞错了，实现和测试会一起错。
// 真正的锚点是下面 TestDerivedKeys_SambaVectors 那组跨实现向量。
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

// TestDerivedKeys_SambaVectors 是**跨实现**的已知答案测试。
//
// 向量来自与真实 Samba 4 客户端（smbclient）的实际握手：用
//
//	smbclient //127.0.0.1/pub -U alice%pass -m SMB3_11 \
//	    --option="debug encryption=yes" -d5
//
// 让 smbclient 打印它**自己独立派生**的 Session/Signing/App/ServerIn/ServerOut
// 密钥，再与我们用同一个 SessionKey + preauth hash 算出的结果逐字节比对。
//
// 这组向量的价值在于它来自另一套独立实现：只要 Label 少一个 NUL、
// 分隔符 0x00 多一个少一个、"ServerIn " 漏掉结尾空格、或者 S2C/C2S 方向搞反，
// 这里立刻就会红。MS-SMB2 §3.1.4.2 本身没有给 worked example，
// 抓真实客户端是唯一可行的钉法（AGENTS.md §3 / §9）。
//
// Samba 的命名与我们的对应关系（**方向极易搞反**）：
//
//	Samba "ServerIn Key"  = C2S = 服务端**解密**用 = ServerInKey()
//	Samba "ServerOut Key" = S2C = 服务端**加密**用 = ServerOutKey()
func TestDerivedKeys_SambaVectors(t *testing.T) {
	cases := []struct {
		name       string
		dialect    uint16
		keyLen     int
		sessionKey string
		preauth    string
		sign       string
		app        string
		serverIn   string
		serverOut  string
	}{
		{
			// 协商结果 cipher=AES-128-GCM。
			name:       "3.1.1 AES-128",
			dialect:    DialectSMB311,
			keyLen:     16,
			sessionKey: "4a8f04462de7ae80d0a0567c40e79ef0",
			preauth: "3e9167552e7990b0f7aee088d35965caa969f03162602c5ffe4fa58272a36eca" +
				"9a6219187a1c6acca417aab4f66f426baa43ba6144091c348c648a724f79c433",
			sign:      "27eea77a8c485435d23b447834f73018",
			app:       "9e079a85ddba552a5f0da1014e51ff0a",
			serverIn:  "b343dd5198cb43fcf66ede5dc0a94170",
			serverOut: "91c8dcf53dd40439af08cf0d3958942c",
		},
		{
			// 协商结果 cipher=AES-256-GCM，走 L=256 的分支。
			name:       "3.1.1 AES-256",
			dialect:    DialectSMB311,
			keyLen:     32,
			sessionKey: "b4e755302eec96d67ff18816fc04d5a3",
			preauth: "071c668a8b20b31d2f7144ad3ff720e1200f53ea11b425672528c4ea502a1fdd" +
				"fb11aa4b4c8218330502546eb434fd9c0a6bff8dfe9e98fd409fb4096b6118e4",
			sign:      "99143824488064bc1e898e35dc4009e5",
			app:       "54160243fb5b8ebbc50bd1c17ca214f5",
			serverIn:  "6acdc61f5ee81f38a186e4fc2057d4a2ef8ccbf45160b437af4938b45a7b8959",
			serverOut: "00d4806f6c3d8620c719e39164abb4bf8b43aa98f1caeb3313124eae8fe39127",
		},
		{
			// 3.0.2 走另一整套 Label/Context（"SMB2AESCMAC"/"SmbSign"、
			// "SMB2APP"/"SmbRpc"、"SMB2AESCCM"/"ServerOut"/"ServerIn "），
			// 与 preauth hash 无关。这条路径此前没有任何互操作测试覆盖。
			name:       "3.0.2",
			dialect:    DialectSMB302,
			keyLen:     16,
			sessionKey: "f75f5aa54972ae7b06e1b400222d8833",
			sign:       "22d874e060e0108434a7c8cd35b7a0b4",
			app:        "4cbb6c92ba1f413e0c58450a8e5d85c9",
			serverIn:   "2e223d7b7001772828d65191e3411654",
			serverOut:  "4bc9db157d6afcafe11d7dce53577811",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk := mustHex(t, tc.sessionKey)
			var ph []byte
			if tc.preauth != "" {
				ph = mustHex(t, tc.preauth)
			}
			check := func(what string, got []byte, want string) {
				t.Helper()
				if w := mustHex(t, want); !bytes.Equal(got, w) {
					t.Errorf("%s = %x, want %x（Samba 独立派生值）", what, got, w)
				}
			}
			check("SigningKey", SigningKey(tc.dialect, sk, ph), tc.sign)
			check("ApplicationKey", ApplicationKey(tc.dialect, sk, ph), tc.app)
			check("ServerInKey", ServerInKey(tc.dialect, sk, ph, tc.keyLen), tc.serverIn)
			check("ServerOutKey", ServerOutKey(tc.dialect, sk, ph, tc.keyLen), tc.serverOut)
		})
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
