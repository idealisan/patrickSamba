package wire

import (
	"bytes"
	"testing"
)

func TestTreeConnectRequestRoundTrip(t *testing.T) {
	r := &TreeConnectRequest{Path: `\\127.0.0.1\share`}
	msg, err := r.Append(dummyHeader(CommandTreeConnect))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 9 {
		t.Errorf("StructureSize = %d, want 9", le.Uint16(body))
	}
	// PathOffset 相对头起点 = 64 + 8 = 72。
	if got := le.Uint16(body[4:]); got != 72 {
		t.Errorf("PathOffset = %d, want 72", got)
	}
	if got := le.Uint16(body[6:]); int(got) != UTF16LELen(r.Path) {
		t.Errorf("PathLength = %d, want %d", got, UTF16LELen(r.Path))
	}
	got, err := ParseTreeConnectRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Path != r.Path {
		t.Errorf("Path = %q, want %q", got.Path, r.Path)
	}
	if got.ShareName() != "share" {
		t.Errorf("ShareName = %q, want share", got.ShareName())
	}
}

func TestTreeConnectShareName(t *testing.T) {
	cases := map[string]string{
		`\\SERVER\Data`:      "Data",
		`\\server\IPC$`:      "IPC$",
		`\\a.b.c.d\my share`: "my share",
		`share`:              "share",
		`\\server\`:          "",
	}
	for path, want := range cases {
		r := &TreeConnectRequest{Path: path}
		if got := r.ShareName(); got != want {
			t.Errorf("ShareName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTreeConnectResponseRoundTrip(t *testing.T) {
	r := &TreeConnectResponse{
		ShareType:     ShareTypeDisk,
		ShareFlags:    ShareFlagManualCaching | ShareFlagForceLevelIIOplock,
		Capabilities:  0,
		MaximalAccess: MaximalAccessReadWrite,
	}
	msg := r.Append(dummyHeader(CommandTreeConnect))
	if len(msg)-HeaderSize != 16 {
		t.Fatalf("体长度 = %d, want 16", len(msg)-HeaderSize)
	}
	got, err := ParseTreeConnectResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *got != *r {
		t.Errorf("round-trip 不一致: %+v", got)
	}
	if _, err := ParseTreeConnectResponse(msg[:len(msg)-1]); err == nil {
		t.Error("截断应报错")
	}
}

func TestTreeConnectPathTruncated(t *testing.T) {
	r := &TreeConnectRequest{Path: `\\host\s`}
	msg, err := r.Append(dummyHeader(CommandTreeConnect))
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(msg)
	le.PutUint16(bad[HeaderSize+6:], 0xFFFE) // PathLength 撒谎
	if _, err := ParseTreeConnectRequest(bad); err == nil {
		t.Error("越界 PathLength 应报错")
	}
	// 奇数长度的 UTF-16 必须报错而不是 panic。
	bad = bytes.Clone(msg)
	le.PutUint16(bad[HeaderSize+6:], 3)
	if _, err := ParseTreeConnectRequest(bad); err == nil {
		t.Error("奇数 PathLength 应报错")
	}
}

func TestErrorResponse(t *testing.T) {
	t.Run("empty error data 占位", func(t *testing.T) {
		r := &ErrorResponse{}
		msg, err := r.Append(dummyHeader(CommandCreate))
		if err != nil {
			t.Fatal(err)
		}
		body := msg[HeaderSize:]
		// StructureSize 9 = 固定 8 + 1 字节占位，因此体长必须是 9。
		if len(body) != 9 {
			t.Fatalf("体长度 = %d, want 9（ByteCount=0 时仍需 1 字节占位）", len(body))
		}
		if le.Uint16(body) != 9 {
			t.Errorf("StructureSize = %d, want 9", le.Uint16(body))
		}
		if le.Uint32(body[4:]) != 0 {
			t.Errorf("ByteCount = %d, want 0", le.Uint32(body[4:]))
		}
		if body[8] != 0 {
			t.Errorf("占位字节 = %#x, want 0", body[8])
		}
	})

	t.Run("with error data", func(t *testing.T) {
		r := &ErrorResponse{ErrorData: []byte{0x00, 0x10, 0x00, 0x00}} // 例：所需缓冲区大小
		msg, err := r.Append(dummyHeader(CommandQueryInfo))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseErrorResponse(msg)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !bytes.Equal(got.ErrorData, r.ErrorData) {
			t.Errorf("ErrorData = %x", got.ErrorData)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		r := &ErrorResponse{ErrorData: []byte{1, 2, 3}}
		msg, err := r.Append(dummyHeader(CommandRead))
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n < len(msg); n++ {
			if _, err := ParseErrorResponse(msg[:n]); err == nil {
				t.Fatalf("截断到 %d 应报错", n)
			}
		}
	})
}
