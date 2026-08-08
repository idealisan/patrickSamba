package crypto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 构造一条最小的合法 SMB2 消息（64 字节头 + 一点 body）作为明文。
func fakeSMB2Message() []byte {
	msg := make([]byte, HeaderSize+8)
	copy(msg, []byte{0xFE, 'S', 'M', 'B'})
	binary.LittleEndian.PutUint16(msg[4:], HeaderSize)
	binary.LittleEndian.PutUint16(msg[0x0C:], 0x000D) // ECHO
	binary.LittleEndian.PutUint64(msg[0x18:], 42)
	return msg
}

// TRANSFORM_HEADER 布局 golden test（MS-SMB2 §2.2.41）。
func TestTransformHeaderLayout(t *testing.T) {
	h := &TransformHeader{
		OriginalMessageSize: 0x11223344,
		Flags:               TransformFlagEncrypted,
		SessionID:           0x0102030405060708,
	}
	for i := range h.Signature {
		h.Signature[i] = byte(0xA0 + i)
	}
	for i := range h.Nonce {
		h.Nonce[i] = byte(i)
	}
	b := h.Marshal()

	if len(b) != TransformHeaderSize {
		t.Fatalf("长度 = %d, want %d", len(b), TransformHeaderSize)
	}
	// ProtocolId 字节序列必须是 FD 53 4D 42（"\xFDSMB"）。
	if !bytes.Equal(b[:4], []byte{0xFD, 0x53, 0x4D, 0x42}) {
		t.Errorf("ProtocolId = % x", b[:4])
	}
	if !IsTransform(b) {
		t.Error("IsTransform 应为 true")
	}
	if !bytes.Equal(b[0x04:0x14], h.Signature[:]) {
		t.Error("Signature 偏移错")
	}
	if !bytes.Equal(b[0x14:0x24], h.Nonce[:]) {
		t.Error("Nonce 偏移错")
	}
	if binary.LittleEndian.Uint32(b[0x24:]) != 0x11223344 {
		t.Error("OriginalMessageSize 偏移错")
	}
	if binary.LittleEndian.Uint16(b[0x28:]) != 0 {
		t.Error("Reserved 必须为 0")
	}
	if binary.LittleEndian.Uint16(b[0x2A:]) != TransformFlagEncrypted {
		t.Error("Flags 偏移错")
	}
	if binary.LittleEndian.Uint64(b[0x2C:]) != 0x0102030405060708 {
		t.Error("SessionId 偏移错")
	}

	back, err := ParseTransformHeader(b)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if *back != *h {
		t.Errorf("回环不符\n got: %+v\nwant: %+v", back, h)
	}
}

// AAD 必须恰好是头的 0x14..0x33 共 32 字节。
func TestTransformAADRange(t *testing.T) {
	if aadOffset != 0x14 || aadOffset+aadLen-1 != 0x33 {
		t.Fatalf("AAD 范围 = 0x%02x..0x%02x, want 0x14..0x33",
			aadOffset, aadOffset+aadLen-1)
	}
	if aadOffset+aadLen != TransformHeaderSize {
		t.Fatalf("AAD 应一直覆盖到头末尾")
	}
}

// 四种算法的加解密回环。
func TestTransformRoundTrip(t *testing.T) {
	plain := fakeSMB2Message()
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	for _, c := range []Cipher{CipherAES128CCM, CipherAES128GCM, CipherAES256CCM, CipherAES256GCM} {
		key := bytes.Repeat([]byte{0x5A}, c.KeySize())

		enc, err := Encrypt(c, key, nonce, 0xDEADBEEF, plain)
		if err != nil {
			t.Fatalf("%v Encrypt: %v", c, err)
		}
		if len(enc) != TransformHeaderSize+len(plain) {
			t.Errorf("%v 密文长度 = %d, want %d", c, len(enc), TransformHeaderSize+len(plain))
		}
		// 密文不得等于明文。
		if bytes.Equal(enc[TransformHeaderSize:], plain) {
			t.Errorf("%v 密文与明文相同", c)
		}
		// Nonce 字段里超出算法长度的尾部必须为 0。
		h, err := ParseTransformHeader(enc)
		if err != nil {
			t.Fatalf("%v 解析头: %v", c, err)
		}
		for i := c.NonceSize(); i < 16; i++ {
			if h.Nonce[i] != 0 {
				t.Errorf("%v Nonce[%d] 应为 0", c, i)
			}
		}
		if h.OriginalMessageSize != uint32(len(plain)) {
			t.Errorf("%v OriginalMessageSize = %d", c, h.OriginalMessageSize)
		}

		got, err := Decrypt(c, key, enc)
		if err != nil {
			t.Fatalf("%v Decrypt: %v", c, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("%v 回环明文不符", c)
		}
	}
}

// 任何一个比特被篡改都必须认证失败。
func TestTransformTamper(t *testing.T) {
	plain := fakeSMB2Message()
	nonce := bytes.Repeat([]byte{0x01}, 16)

	for _, c := range []Cipher{CipherAES128CCM, CipherAES128GCM} {
		key := bytes.Repeat([]byte{0x5A}, c.KeySize())
		enc, err := Encrypt(c, key, nonce, 1, plain)
		if err != nil {
			t.Fatal(err)
		}

		// 篡改 Signature / Nonce / SessionId / 密文各一处。
		for _, pos := range []int{0x04, 0x14, 0x2C, TransformHeaderSize} {
			bad := append([]byte(nil), enc...)
			bad[pos] ^= 0x01
			if _, err := Decrypt(c, key, bad); err == nil {
				t.Errorf("%v 篡改偏移 0x%02x 后仍解密成功", c, pos)
			}
		}
		// 换密钥必须失败。
		other := bytes.Repeat([]byte{0x5B}, c.KeySize())
		if _, err := Decrypt(c, other, enc); err != ErrTransformDecrypt {
			t.Errorf("%v 错误密钥应返回 ErrTransformDecrypt, got %v", c, err)
		}
	}
}

// 参数与畸形输入的防御。
func TestTransformErrors(t *testing.T) {
	plain := fakeSMB2Message()
	nonce := bytes.Repeat([]byte{1}, 16)

	if _, err := Encrypt(Cipher(0x9999), bytes.Repeat([]byte{1}, 16), nonce, 1, plain); err != ErrCipherUnsupported {
		t.Errorf("未知算法应返回 ErrCipherUnsupported, got %v", err)
	}
	if _, err := Encrypt(CipherAES128CCM, bytes.Repeat([]byte{1}, 15), nonce, 1, plain); err != ErrTransformKeySize {
		t.Errorf("密钥长度不对应返回 ErrTransformKeySize, got %v", err)
	}
	if _, err := Encrypt(CipherAES256GCM, bytes.Repeat([]byte{1}, 16), nonce, 1, plain); err != ErrTransformKeySize {
		t.Errorf("AES-256 需要 32 字节密钥, got %v", err)
	}
	if _, err := Encrypt(CipherAES128CCM, bytes.Repeat([]byte{1}, 16), []byte{1, 2}, 1, plain); err != ErrTransformHeader {
		t.Errorf("nonce 过短应被拒绝, got %v", err)
	}

	key := bytes.Repeat([]byte{1}, 16)
	if _, err := ParseTransformHeader(nil); err != ErrTransformHeader {
		t.Error("空输入应被拒绝")
	}
	if _, err := ParseTransformHeader(make([]byte, TransformHeaderSize)); err != ErrTransformHeader {
		t.Error("魔数不对应被拒绝")
	}
	// 头完整但没有密文，且 OriginalMessageSize 撒谎。
	enc, err := Encrypt(CipherAES128CCM, key, nonce, 1, plain)
	if err != nil {
		t.Fatal(err)
	}
	short := append([]byte(nil), enc[:TransformHeaderSize+4]...)
	if _, err := Decrypt(CipherAES128CCM, key, short); err != ErrTransformHeader {
		t.Errorf("长度不一致应返回 ErrTransformHeader, got %v", err)
	}
	// 截断到头都不完整。
	for i := 0; i < TransformHeaderSize; i++ {
		if _, err := Decrypt(CipherAES128CCM, key, enc[:i]); err == nil {
			t.Errorf("截断到 %d 字节仍成功", i)
		}
	}
}

// nonce 计数器必须单调递增且不重复（含 64 位进位）。
func TestNonceCounter(t *testing.T) {
	var n NonceCounter
	seen := make(map[[16]byte]bool)
	var prev [16]byte
	for i := 0; i < 1000; i++ {
		v := n.Next()
		if seen[v] {
			t.Fatalf("第 %d 个 nonce 重复: % x", i, v)
		}
		seen[v] = true
		if bytes.Equal(v[:], make([]byte, 16)) {
			t.Fatal("不得产生全零 nonce")
		}
		prev = v
	}
	if binary.LittleEndian.Uint64(prev[0:]) != 1000 {
		t.Errorf("低 64 位 = %d, want 1000", binary.LittleEndian.Uint64(prev[0:]))
	}

	// 低位回绕时高位进位。
	n2 := NonceCounter{low: ^uint64(0) - 1}
	_ = n2.Next() // low = MaxUint64
	v := n2.Next()
	if binary.LittleEndian.Uint64(v[0:]) != 0 || binary.LittleEndian.Uint64(v[8:]) != 1 {
		t.Errorf("进位错: low=%d high=%d",
			binary.LittleEndian.Uint64(v[0:]), binary.LittleEndian.Uint64(v[8:]))
	}
}

func TestCipherMetadata(t *testing.T) {
	cases := []struct {
		c        Cipher
		key, non int
		name     string
	}{
		{CipherAES128CCM, 16, 11, "AES-128-CCM"},
		{CipherAES128GCM, 16, 12, "AES-128-GCM"},
		{CipherAES256CCM, 32, 11, "AES-256-CCM"},
		{CipherAES256GCM, 32, 12, "AES-256-GCM"},
		{Cipher(0), 0, 0, "unknown-cipher"},
	}
	for _, c := range cases {
		if c.c.KeySize() != c.key || c.c.NonceSize() != c.non || c.c.String() != c.name {
			t.Errorf("%v: key=%d nonce=%d name=%s",
				uint16(c.c), c.c.KeySize(), c.c.NonceSize(), c.c.String())
		}
	}
}
