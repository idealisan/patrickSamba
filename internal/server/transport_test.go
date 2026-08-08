package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
)

// pipePair 造一对内存连接，用于不依赖真实网络的传输层测试。
func pipePair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() {
		_ = c.Close()
		_ = s.Close()
	})
	return c, s
}

func TestReadFrameRoundTrip(t *testing.T) {
	c, s := pipePair(t)
	ct := NewTransport(c, 0)
	st := NewTransport(s, 0)

	payload := []byte{0xFE, 'S', 'M', 'B', 0x40, 0x00, 0x01, 0x02}

	errCh := make(chan error, 1)
	go func() { errCh <- ct.WriteFrame(payload) }()

	got, err := st.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %x want %x", got, payload)
	}
}

// TestFrameHeaderIsBigEndian 固定字节向量校验：长度前缀必须是大端。
// 这是最容易写反的地方（SMB2 报文体是小端）。
func TestFrameHeaderIsBigEndian(t *testing.T) {
	c, s := pipePair(t)
	ct := NewTransport(c, 0)

	// 0x000102 = 258 字节载荷。
	payload := make([]byte, 258)
	for i := range payload {
		payload[i] = byte(i)
	}

	go func() { _ = ct.WriteFrame(payload) }()

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(s, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	want := []byte{0x00, 0x00, 0x01, 0x02}
	if !bytes.Equal(hdr, want) {
		t.Fatalf("header = %x, want %x (Zero(1)+Length(3,大端))", hdr, want)
	}

	body := make([]byte, 258)
	if _, err := io.ReadFull(s, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("body mismatch")
	}
}

func TestReadFrameRejectsNonZeroFirstByte(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 0)

	// 0x81 是 NBSS SESSION REQUEST，我们只做 Direct TCP，必须拒绝。
	go func() { _, _ = c.Write([]byte{0x81, 0x00, 0x00, 0x44}) }()

	if _, err := st.ReadFrame(); !errors.Is(err, ErrBadStreamHeader) {
		t.Fatalf("err = %v, want ErrBadStreamHeader", err)
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 1024)

	// 声明 0x00FFFF (65535) 字节，超过 1024 上限。
	go func() { _, _ = c.Write([]byte{0x00, 0x00, 0xFF, 0xFF}) }()

	if _, err := st.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameRejectsZeroLength(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 0)

	go func() { _, _ = c.Write([]byte{0x00, 0x00, 0x00, 0x00}) }()

	if _, err := st.ReadFrame(); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("err = %v, want ErrEmptyFrame", err)
	}
}

// TestReadFrameTruncatedBody 校验 body 读到一半断开时返回 ErrUnexpectedEOF，
// 与对端优雅关闭（io.EOF）区分开。
func TestReadFrameTruncatedBody(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 0)

	go func() {
		_, _ = c.Write([]byte{0x00, 0x00, 0x00, 0x10}) // 声明 16 字节
		_, _ = c.Write([]byte{0x01, 0x02, 0x03})       // 只给 3 字节
		_ = c.Close()
	}()

	if _, err := st.ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestReadFrameCleanEOF 对端在帧边界关闭时必须是 io.EOF（正常断连，不报错）。
func TestReadFrameCleanEOF(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 0)

	go func() { _ = c.Close() }()

	if _, err := st.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// TestReadFrameSplitAcrossWrites 校验严格「读满头 → 读满 body」，
// 不依赖对端一次写完（TCP 会任意切分）。
func TestReadFrameSplitAcrossWrites(t *testing.T) {
	c, s := pipePair(t)
	st := NewTransport(s, 0)

	payload := bytes.Repeat([]byte{0xAB}, 300)
	go func() {
		_, _ = c.Write([]byte{0x00, 0x00}) // 头的前半
		_, _ = c.Write([]byte{0x01, 0x2C}) // 头的后半：300
		_, _ = c.Write(payload[:100])      // body 分三次
		_, _ = c.Write(payload[100:250])   //
		_, _ = c.Write(payload[250:])      //
	}()

	got, err := st.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch after split writes")
	}
}

func TestNewTransportClampsMaxFrame(t *testing.T) {
	c, _ := pipePair(t)
	if got := NewTransport(c, 0).MaxFrameSize(); got != DefaultMaxFrameSize {
		t.Fatalf("maxFrame = %d, want %d", got, DefaultMaxFrameSize)
	}
	if got := NewTransport(c, -1).MaxFrameSize(); got != DefaultMaxFrameSize {
		t.Fatalf("maxFrame = %d, want %d", got, DefaultMaxFrameSize)
	}
	if got := NewTransport(c, MaxDirectTCPLength+1).MaxFrameSize(); got != DefaultMaxFrameSize {
		t.Fatalf("maxFrame = %d, want %d", got, DefaultMaxFrameSize)
	}
	if got := NewTransport(c, 4096).MaxFrameSize(); got != 4096 {
		t.Fatalf("maxFrame = %d, want 4096", got)
	}
}
