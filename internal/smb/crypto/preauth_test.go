package crypto

import (
	"bytes"
	"crypto/sha512"
	"testing"
)

// 初始值必须是 64 字节全零（MS-SMB2 §3.1.5.2）。
func TestPreauthInitial(t *testing.T) {
	h := NewPreauthHash()
	if len(h) != PreauthHashSize || len(h) != 64 {
		t.Fatalf("长度 = %d, want 64", len(h))
	}
	if !bytes.Equal(h, make([]byte, 64)) {
		t.Error("初始值必须全零")
	}
}

// H_n = SHA-512(H_{n-1} || msg)，与手工计算一致。
func TestPreauthChain(t *testing.T) {
	m1 := []byte("negotiate request")
	m2 := []byte("negotiate response")

	want := sha512.Sum512(append(make([]byte, 64), m1...))
	got := UpdatePreauthHash(NewPreauthHash(), m1)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("第一步不符\n got: %x\nwant: %x", got, want)
	}

	want2 := sha512.Sum512(append(append([]byte(nil), want[:]...), m2...))
	got2 := UpdatePreauthHash(got, m2)
	if !bytes.Equal(got2, want2[:]) {
		t.Fatalf("第二步不符\n got: %x\nwant: %x", got2, want2)
	}

	// prev 长度不对时按全零处理。
	if !bytes.Equal(UpdatePreauthHash(nil, m1), want[:]) {
		t.Error("nil prev 应等价于全零")
	}
	if !bytes.Equal(UpdatePreauthHash([]byte{1, 2, 3}, m1), want[:]) {
		t.Error("长度不对的 prev 应等价于全零")
	}
}

// UpdatePreauthHash 不得修改传入的 prev（Connection 级的值必须保持不变）。
func TestPreauthNoMutation(t *testing.T) {
	prev := NewPreauthHash()
	snapshot := append([]byte(nil), prev...)
	_ = UpdatePreauthHash(prev, []byte("x"))
	if !bytes.Equal(prev, snapshot) {
		t.Error("prev 被修改了")
	}
}

// 累积器与 Clone：建立 Session 时从 Connection 级分叉，互不污染。
func TestPreauthAccumulatorClone(t *testing.T) {
	conn := NewPreauthAccumulator()
	conn.Update([]byte("NEGOTIATE req"))
	conn.Update([]byte("NEGOTIATE resp"))
	connValue := conn.Value()

	s1 := conn.Clone()
	s1.Update([]byte("SESSION_SETUP req 1 (会话 A)"))

	s2 := conn.Clone()
	s2.Update([]byte("SESSION_SETUP req 1 (会话 B)"))

	if !bytes.Equal(conn.Value(), connValue) {
		t.Error("Connection 级的值被会话污染了")
	}
	if bytes.Equal(s1.Value(), s2.Value()) {
		t.Error("两个会话喂了不同消息，哈希不应相同")
	}

	// Value 返回副本。
	v := conn.Value()
	v[0] ^= 0xFF
	if !bytes.Equal(conn.Value(), connValue) {
		t.Error("Value 返回的不是副本")
	}
}

// 完整的 3.1.1 序列：5 条消息累积后派生出的 SigningKey 应稳定且非零。
func TestPreauthDrivesSigningKey(t *testing.T) {
	acc := NewPreauthAccumulator()
	for _, m := range [][]byte{
		[]byte("1 NEGOTIATE Request"),
		[]byte("2 NEGOTIATE Response"),
		[]byte("3 SESSION_SETUP Request #1"),
		[]byte("4 SESSION_SETUP Response #1"),
		[]byte("5 SESSION_SETUP Request #2"),
	} {
		acc.Update(m)
	}
	sessionKey := bytes.Repeat([]byte{0x11}, 16)

	k1 := SigningKey(DialectSMB311, sessionKey, acc.Value())
	k2 := SigningKey(DialectSMB311, sessionKey, acc.Value())
	if !bytes.Equal(k1, k2) {
		t.Error("同样输入应派生出同样的密钥")
	}
	if len(k1) != 16 || bytes.Equal(k1, make([]byte, 16)) {
		t.Errorf("SigningKey = %x", k1)
	}
	// preauth 变了，密钥必须跟着变。
	acc.Update([]byte("multiplied"))
	if bytes.Equal(SigningKey(DialectSMB311, sessionKey, acc.Value()), k1) {
		t.Error("preauth 变化后密钥应不同")
	}
}

func TestPreauthAlgorithmID(t *testing.T) {
	// MS-SMB2 §2.2.3.1.1：目前唯一定义的算法 ID 是 SHA-512 = 0x0001。
	if PreauthHashAlgorithmSHA512 != 0x0001 {
		t.Errorf("算法 ID = 0x%04x", PreauthHashAlgorithmSHA512)
	}
}
