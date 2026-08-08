package mdns

import (
	"testing"
	"time"
)

// 组播回环打开时内核会把自己发的报文送回本 socket。
// RFC 6762 §11 要求忽略它们，否则 probe 会与自己冲突、无限改名。
func TestSelfPacketSuppression(t *testing.T) {
	c := &conn{sent: make(map[uint64]time.Time)}

	own := []byte{0x00, 0x00, 0x00, 0x00, 0xde, 0xad, 0xbe, 0xef}
	other := []byte{0x00, 0x00, 0x00, 0x00, 0xca, 0xfe, 0xba, 0xbe}

	if c.isOwnPacket(own) {
		t.Fatal("还没发送过就被判定为自发报文")
	}

	c.noteSent(own)

	// 同一份报文会在每张网卡上各回环一次，必须能重复命中。
	for i := 0; i < 3; i++ {
		if !c.isOwnPacket(own) {
			t.Fatalf("第 %d 次回环没有被识别为自发报文", i+1)
		}
	}
	if c.isOwnPacket(other) {
		t.Fatal("别人的报文被误判为自发报文")
	}
}

// 超出时间窗口后指纹必须失效，否则一个"碰巧字节相同"的远端报文
// 会被永久屏蔽掉。
func TestSelfPacketSuppressionExpires(t *testing.T) {
	c := &conn{sent: make(map[uint64]time.Time)}

	b := []byte{0x01, 0x02, 0x03}
	c.noteSent(b)
	if !c.isOwnPacket(b) {
		t.Fatal("刚发送的报文应当被抑制")
	}

	// 把时间戳往前拨到窗口之外。
	c.sentMu.Lock()
	for k := range c.sent {
		c.sent[k] = time.Now().Add(-selfSuppressWindow - time.Second)
	}
	c.sentMu.Unlock()

	if c.isOwnPacket(b) {
		t.Fatal("过期指纹仍在抑制报文")
	}
}

// 过期条目必须被清理，否则长时间运行会无界增长。
func TestSelfPacketPrune(t *testing.T) {
	c := &conn{sent: make(map[uint64]time.Time)}

	c.noteSent([]byte("old"))
	c.sentMu.Lock()
	for k := range c.sent {
		c.sent[k] = time.Now().Add(-selfSuppressWindow - time.Second)
	}
	c.sentMu.Unlock()

	c.noteSent([]byte("new"))

	c.sentMu.Lock()
	n := len(c.sent)
	c.sentMu.Unlock()
	if n != 1 {
		t.Fatalf("过期条目没有被清理：len(sent)=%d，期望 1", n)
	}
}
