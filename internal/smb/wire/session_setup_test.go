package wire

import (
	"bytes"
	"testing"
)

func TestSessionSetupRequestRoundTrip(t *testing.T) {
	token := []byte("NTLMSSP\x00\x01\x00\x00\x00rest-of-token")
	r := &SessionSetupRequest{
		Flags:             SessionSetupFlagBinding,
		SecurityMode:      NegotiateSigningEnabled,
		Capabilities:      CapDFS,
		PreviousSessionID: 0x0102030405060708,
		SecurityBuffer:    token,
	}
	msg, err := r.Append(dummyHeader(CommandSessionSetup))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 25 {
		t.Errorf("StructureSize = %d, want 25", le.Uint16(body))
	}
	// SecurityBufferOffset 相对 SMB2 头起点 = 64 + 24 = 88。
	if got := le.Uint16(body[12:]); got != 88 {
		t.Errorf("SecurityBufferOffset = %d, want 88", got)
	}
	if got := le.Uint16(body[14:]); int(got) != len(token) {
		t.Errorf("SecurityBufferLength = %d, want %d", got, len(token))
	}

	got, err := ParseSessionSetupRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Flags != r.Flags || got.SecurityMode != r.SecurityMode ||
		got.Capabilities != r.Capabilities || got.PreviousSessionID != r.PreviousSessionID {
		t.Errorf("固定字段 round-trip 失败: %+v", got)
	}
	if !bytes.Equal(got.SecurityBuffer, token) {
		t.Errorf("SecurityBuffer = %q", got.SecurityBuffer)
	}
}

func TestSessionSetupResponseRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token []byte
		flags SessionFlags
	}{
		{"with token", []byte("\xa1\x30\x03challenge"), 0},
		{"guest empty", nil, SessionFlagIsGuest},
		{"null session", nil, SessionFlagIsNull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &SessionSetupResponse{SessionFlags: tc.flags, SecurityBuffer: tc.token}
			msg, err := r.Append(dummyHeader(CommandSessionSetup))
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if le.Uint16(msg[HeaderSize:]) != 9 {
				t.Errorf("StructureSize = %d, want 9", le.Uint16(msg[HeaderSize:]))
			}
			if got := le.Uint16(msg[HeaderSize+4:]); got != 72 {
				t.Errorf("SecurityBufferOffset = %d, want 72", got)
			}
			got, err := ParseSessionSetupResponse(msg)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.SessionFlags != tc.flags {
				t.Errorf("SessionFlags = %#x, want %#x", got.SessionFlags, tc.flags)
			}
			if !bytes.Equal(got.SecurityBuffer, tc.token) {
				t.Errorf("SecurityBuffer = %q, want %q", got.SecurityBuffer, tc.token)
			}
		})
	}
}

func TestSessionSetupParseErrors(t *testing.T) {
	r := &SessionSetupRequest{SecurityBuffer: []byte("abcd")}
	msg, err := r.Append(dummyHeader(CommandSessionSetup))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(msg); n++ {
		if _, err := ParseSessionSetupRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 字节应报错", n)
		}
	}
	// SecurityBufferLength 撒谎。
	bad := bytes.Clone(msg)
	le.PutUint16(bad[HeaderSize+14:], 0xFFFF)
	if _, err := ParseSessionSetupRequest(bad); err == nil {
		t.Error("越界的 SecurityBufferLength 应报错")
	}
	// StructureSize 错误。
	bad = bytes.Clone(msg)
	le.PutUint16(bad[HeaderSize:], 24)
	if _, err := ParseSessionSetupRequest(bad); err == nil {
		t.Error("错误的 StructureSize 应报错")
	}
}

func TestLogoffEchoTreeDisconnect(t *testing.T) {
	type codec struct {
		name  string
		enc   func([]byte) []byte
		dec   func([]byte) error
		wantN int
	}
	cases := []codec{
		{"logoff req", (&LogoffRequest{}).Append, func(b []byte) error { _, e := ParseLogoffRequest(b); return e }, 4},
		{"logoff resp", (&LogoffResponse{}).Append, func(b []byte) error { _, e := ParseLogoffResponse(b); return e }, 4},
		{"echo req", (&EchoRequest{}).Append, func(b []byte) error { _, e := ParseEchoRequest(b); return e }, 4},
		{"echo resp", (&EchoResponse{}).Append, func(b []byte) error { _, e := ParseEchoResponse(b); return e }, 4},
		{"cancel req", (&CancelRequest{}).Append, func(b []byte) error { _, e := ParseCancelRequest(b); return e }, 4},
		{"tdis req", (&TreeDisconnectRequest{}).Append, func(b []byte) error { _, e := ParseTreeDisconnectRequest(b); return e }, 4},
		{"tdis resp", (&TreeDisconnectResponse{}).Append, func(b []byte) error { _, e := ParseTreeDisconnectResponse(b); return e }, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := c.enc(dummyHeader(CommandEcho))
			if len(msg)-HeaderSize != c.wantN {
				t.Fatalf("体长度 = %d, want %d", len(msg)-HeaderSize, c.wantN)
			}
			if le.Uint16(msg[HeaderSize:]) != 4 {
				t.Errorf("StructureSize = %d, want 4", le.Uint16(msg[HeaderSize:]))
			}
			if err := c.dec(msg); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if err := c.dec(msg[:len(msg)-1]); err == nil {
				t.Error("截断应报错")
			}
		})
	}
}
