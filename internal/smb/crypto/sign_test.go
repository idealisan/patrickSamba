package crypto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// 构造一条最小的 SMB2 消息（64 字节头 + body）。
func testMessage(body int) []byte {
	msg := make([]byte, HeaderSize+body)
	copy(msg, []byte{0xFE, 'S', 'M', 'B'})
	binary.LittleEndian.PutUint16(msg[4:], 64)   // StructureSize
	binary.LittleEndian.PutUint16(msg[0x0C:], 1) // Command = SESSION_SETUP
	binary.LittleEndian.PutUint64(msg[0x18:], 0x0102030405060708)
	for i := HeaderSize; i < len(msg); i++ {
		msg[i] = byte(i)
	}
	return msg
}

func TestSign_HMACSHA256(t *testing.T) {
	key := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	msg := testMessage(32)

	if err := Sign(DialectSMB210, key, msg); err != nil {
		t.Fatal(err)
	}
	if !IsSigned(msg) {
		t.Error("Sign 未置 SMB2_FLAGS_SIGNED")
	}

	// 独立复算：签名字段清零 + SIGNED 置位后对整条消息做 HMAC-SHA256 取前 16 字节。
	ref := append([]byte(nil), msg...)
	for i := 0; i < SignatureSize; i++ {
		ref[0x30+i] = 0
	}
	h := hmac.New(sha256.New, key)
	h.Write(ref)
	if want := h.Sum(nil)[:16]; !bytes.Equal(msg[0x30:0x40], want) {
		t.Errorf("signature = %x, want %x", msg[0x30:0x40], want)
	}

	if err := Verify(DialectSMB210, key, msg); err != nil {
		t.Errorf("Verify: %v", err)
	}
	// Verify 必须原样恢复 Signature 字段。
	if err := Verify(DialectSMB210, key, msg); err != nil {
		t.Errorf("Verify 第二次: %v", err)
	}
}

func TestSignVerify_AllAlgorithms(t *testing.T) {
	key := mustHex(t, "0f0e0d0c0b0a09080706050403020100")
	for _, alg := range []SigningAlgorithm{SigningHMACSHA256, SigningAESCMAC, SigningAESGMAC} {
		msg := testMessage(72)
		if err := SignWith(alg, key, msg); err != nil {
			t.Fatalf("alg %d: %v", alg, err)
		}
		if err := VerifyWith(alg, key, msg); err != nil {
			t.Errorf("alg %d: Verify: %v", alg, err)
		}
		// 篡改 body 必须被发现。
		msg[HeaderSize] ^= 1
		if err := VerifyWith(alg, key, msg); err == nil {
			t.Errorf("alg %d: 篡改后仍校验通过", alg)
		}
		msg[HeaderSize] ^= 1
		// 篡改签名必须被发现。
		msg[0x30] ^= 1
		if err := VerifyWith(alg, key, msg); err == nil {
			t.Errorf("alg %d: 错误签名仍校验通过", alg)
		}
	}
}

// AES-GMAC 的 nonce 依赖 MessageId、方向位与 CANCEL 位。
func TestGMACNonce(t *testing.T) {
	msg := testMessage(0)
	n := gmacNonce(msg)
	if !bytes.Equal(n[:8], msg[0x18:0x20]) {
		t.Errorf("nonce 前 8 字节应为 MessageId")
	}
	if n[8] != 0x00 || n[9] != 0 || n[10] != 0 || n[11] != 0 {
		t.Errorf("客户端非 CANCEL 消息的 nonce[8:] = %x, want 0", n[8:])
	}

	binary.LittleEndian.PutUint32(msg[0x10:], flagsServerToRedir)
	if n := gmacNonce(msg); n[8] != 0x01 {
		t.Errorf("服务端消息 nonce[8] = %#x, want 0x01", n[8])
	}

	binary.LittleEndian.PutUint16(msg[0x0C:], commandCancel)
	if n := gmacNonce(msg); n[8] != 0x03 {
		t.Errorf("服务端 CANCEL 消息 nonce[8] = %#x, want 0x03", n[8])
	}
}

func TestSign_ShortMessage(t *testing.T) {
	key := make([]byte, 16)
	for _, n := range []int{0, 1, 63} {
		if err := Sign(DialectSMB300, key, make([]byte, n)); err != ErrShortMessage {
			t.Errorf("Sign(len=%d) err = %v, want ErrShortMessage", n, err)
		}
		if err := Verify(DialectSMB300, key, make([]byte, n)); err != ErrShortMessage {
			t.Errorf("Verify(len=%d) err = %v, want ErrShortMessage", n, err)
		}
	}
	if err := SignWith(SigningAlgorithm(0xFFFF), key, testMessage(0)); err != ErrSigningAlgorithm {
		t.Errorf("未知算法 err = %v", err)
	}
}

func TestSigningAlgorithmForDialect(t *testing.T) {
	cases := map[uint16]SigningAlgorithm{
		DialectSMB202: SigningHMACSHA256,
		DialectSMB210: SigningHMACSHA256,
		DialectSMB300: SigningAESCMAC,
		DialectSMB302: SigningAESCMAC,
		DialectSMB311: SigningAESCMAC,
	}
	for d, want := range cases {
		if got := SigningAlgorithmForDialect(d); got != want {
			t.Errorf("dialect %#04x -> %d, want %d", d, got, want)
		}
	}
}
