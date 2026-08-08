package crypto

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// RFC 4493 §4 Test Vectors —— AES-128 CMAC。
func TestCMAC_RFC4493(t *testing.T) {
	key := mustHex(t, "2b7e151628aed2a6abf7158809cf4f3c")

	// RFC 4493 §4 给出的中间量：K1、K2。
	b, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := cmacSubkeys(b)
	if want := mustHex(t, "fbeed618357133667c85e08f7236a8de"); !bytes.Equal(k1[:], want) {
		t.Errorf("K1 = %x, want %x", k1, want)
	}
	if want := mustHex(t, "f7ddac306ae266ccf90bc11ee46d513b"); !bytes.Equal(k2[:], want) {
		t.Errorf("K2 = %x, want %x", k2, want)
	}

	cases := []struct {
		name string
		msg  string
		mac  string
	}{
		{
			name: "Example 1: len 0",
			msg:  "",
			mac:  "bb1d6929e95937287fa37d129b756746",
		},
		{
			name: "Example 2: len 16",
			msg:  "6bc1bee22e409f96e93d7e117393172a",
			mac:  "070a16b46b4d4144f79bdd9dd04a287c",
		},
		{
			name: "Example 3: len 40",
			msg: "6bc1bee22e409f96e93d7e117393172a" +
				"ae2d8a571e03ac9c9eb76fac45af8e51" +
				"30c81c46a35ce411",
			mac: "dfa66747de9ae63030ca32611497c827",
		},
		{
			name: "Example 4: len 64",
			msg: "6bc1bee22e409f96e93d7e117393172a" +
				"ae2d8a571e03ac9c9eb76fac45af8e51" +
				"30c81c46a35ce411e5fbc1191a0a52ef" +
				"f69f2445df4f9b17ad2b417be66c3710",
			mac: "51f0bebf7e3b9d92fc49741779363cfe",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CMAC(key, mustHex(t, tc.msg))
			if err != nil {
				t.Fatal(err)
			}
			if want := mustHex(t, tc.mac); !bytes.Equal(got, want) {
				t.Errorf("CMAC = %x, want %x", got, want)
			}
			if !CMACVerify(key, mustHex(t, tc.msg), mustHex(t, tc.mac)) {
				t.Error("CMACVerify = false, want true")
			}
		})
	}
}

// NIST SP 800-38B Appendix D.3 —— AES-256 CMAC。
func TestCMAC_SP80038B_AES256(t *testing.T) {
	key := mustHex(t, "603deb1015ca71be2b73aef0857d7781"+
		"1f352c073b6108d72d9810a30914dff4")

	cases := []struct{ msg, mac string }{
		{"", "028962f61b7bf89efc6b551f4667d983"},
		{"6bc1bee22e409f96e93d7e117393172a", "28a7023f452e8f82bd4bf28d8c37c35c"},
		{
			"6bc1bee22e409f96e93d7e117393172a" +
				"ae2d8a571e03ac9c9eb76fac45af8e51" +
				"30c81c46a35ce411",
			"aaf3d8f1de5640c232f5b169b9c911e6",
		},
		{
			"6bc1bee22e409f96e93d7e117393172a" +
				"ae2d8a571e03ac9c9eb76fac45af8e51" +
				"30c81c46a35ce411e5fbc1191a0a52ef" +
				"f69f2445df4f9b17ad2b417be66c3710",
			"e1992190549f6ed5696a2c056c315410",
		},
	}
	for _, tc := range cases {
		got, err := CMAC(key, mustHex(t, tc.msg))
		if err != nil {
			t.Fatal(err)
		}
		if want := mustHex(t, tc.mac); !bytes.Equal(got, want) {
			t.Errorf("CMAC(len=%d) = %x, want %x", len(tc.msg)/2, got, want)
		}
	}
}

func TestCMACVerify_Rejects(t *testing.T) {
	key := mustHex(t, "2b7e151628aed2a6abf7158809cf4f3c")
	if CMACVerify(key, []byte("hello"), make([]byte, 16)) {
		t.Error("CMACVerify accepted a wrong MAC")
	}
	if CMACVerify([]byte("short key"), []byte("hello"), make([]byte, 16)) {
		t.Error("CMACVerify accepted an invalid key")
	}
}
