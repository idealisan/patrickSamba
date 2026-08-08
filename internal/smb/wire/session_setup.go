package wire

import "fmt"

// ---------------------------------------------------------------------------
// SESSION_SETUP Request（MS-SMB2 §2.2.5）
//
//	 0  StructureSize(2) = 25
//	 2  Flags(1)                （3.x：SMB2_SESSION_FLAG_BINDING）
//	 3  SecurityMode(1)
//	 4  Capabilities(4)
//	 8  Channel(4)              （必须为 0）
//	12  SecurityBufferOffset(2) （相对 SMB2 头起点）
//	14  SecurityBufferLength(2)
//	16  PreviousSessionId(8)
//	24  Buffer                  （SPNEGO / 裸 NTLMSSP token）
// ---------------------------------------------------------------------------

const (
	// sessionSetupRequestStructureSize 是 §2.2.5 规定的 StructureSize
	// （固定部分 24 字节 + 1 字节可变部分占位）。
	sessionSetupRequestStructureSize = 25
	sessionSetupRequestFixed         = 24

	// sessionSetupResponseStructureSize 是 §2.2.6 规定的 StructureSize
	// （固定部分 8 字节 + 1 字节可变部分占位）。
	sessionSetupResponseStructureSize = 9
	sessionSetupResponseFixed         = 8
)

// SessionSetupRequest 是 SMB2 SESSION_SETUP Request（MS-SMB2 §2.2.5）。
type SessionSetupRequest struct {
	Flags             SessionSetupFlags
	SecurityMode      SecurityMode // 线上是 1 字节，位值与 NEGOTIATE 相同
	Capabilities      Capabilities
	Channel           uint32
	PreviousSessionID uint64
	// SecurityBuffer 是 GSS/SPNEGO token（可能是裸 NTLMSSP，见 protocol-notes §5）。
	SecurityBuffer []byte
}

// ParseSessionSetupRequest 解析 SESSION_SETUP Request。b 是完整消息（含 64 字节头）。
func ParseSessionSetupRequest(b []byte) (*SessionSetupRequest, error) {
	body, err := msgBody(b, sessionSetupRequestFixed, "SESSION_SETUP Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, sessionSetupRequestStructureSize); err != nil {
		return nil, fmt.Errorf("SESSION_SETUP Request: %w", err)
	}
	r := &SessionSetupRequest{
		Flags:             SessionSetupFlags(body[2]),
		SecurityMode:      SecurityMode(body[3]),
		Capabilities:      Capabilities(le.Uint32(body[4:])),
		Channel:           le.Uint32(body[8:]),
		PreviousSessionID: le.Uint64(body[16:]),
	}
	off := uint64(le.Uint16(body[12:]))
	length := uint64(le.Uint16(body[14:]))
	// SecurityBufferOffset 相对 SMB2 头起点，因此直接在完整消息 b 上取。
	r.SecurityBuffer, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("SESSION_SETUP Request SecurityBuffer: %w", err)
	}
	return r, nil
}

// Append 把 SESSION_SETUP Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *SessionSetupRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, sessionSetupRequestFixed)
	le.PutUint16(f[0:], sessionSetupRequestStructureSize)
	f[2] = byte(r.Flags)
	f[3] = byte(r.SecurityMode)
	le.PutUint32(f[4:], uint32(r.Capabilities))
	le.PutUint32(f[8:], r.Channel)
	le.PutUint64(f[16:], r.PreviousSessionID)

	n, err := u16(len(r.SecurityBuffer), "SecurityBuffer")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[12:], HeaderSize+sessionSetupRequestFixed)
	le.PutUint16(f[14:], n)
	return append(dst, r.SecurityBuffer...), nil
}

// ---------------------------------------------------------------------------
// SESSION_SETUP Response（MS-SMB2 §2.2.6）
//
//	0  StructureSize(2) = 9
//	2  SessionFlags(2)
//	4  SecurityBufferOffset(2)
//	6  SecurityBufferLength(2)
//	8  Buffer
//
// 注意（protocol-notes §5）：NTLM 两轮握手的第一轮，头里的 Status 是
// STATUS_MORE_PROCESSING_REQUIRED，但**必须回完整的 SESSION_SETUP 响应体**，
// 不能回 ERROR Response。
// ---------------------------------------------------------------------------

// SessionSetupResponse 是 SMB2 SESSION_SETUP Response（MS-SMB2 §2.2.6）。
type SessionSetupResponse struct {
	SessionFlags   SessionFlags
	SecurityBuffer []byte
}

// Append 把 SESSION_SETUP Response 报文体追加到 dst。
func (r *SessionSetupResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, sessionSetupResponseFixed)
	le.PutUint16(f[0:], sessionSetupResponseStructureSize)
	le.PutUint16(f[2:], uint16(r.SessionFlags))

	n, err := u16(len(r.SecurityBuffer), "SecurityBuffer")
	if err != nil {
		return nil, err
	}
	// 即使长度为 0 也填结构末尾偏移，Windows 对 0 偏移不友好。
	le.PutUint16(f[4:], HeaderSize+sessionSetupResponseFixed)
	le.PutUint16(f[6:], n)
	return append(dst, r.SecurityBuffer...), nil
}

// ParseSessionSetupResponse 解析 SESSION_SETUP Response（供测试与 Go 客户端使用）。
func ParseSessionSetupResponse(b []byte) (*SessionSetupResponse, error) {
	body, err := msgBody(b, sessionSetupResponseFixed, "SESSION_SETUP Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, sessionSetupResponseStructureSize); err != nil {
		return nil, fmt.Errorf("SESSION_SETUP Response: %w", err)
	}
	r := &SessionSetupResponse{SessionFlags: SessionFlags(le.Uint16(body[2:]))}
	off := uint64(le.Uint16(body[4:]))
	length := uint64(le.Uint16(body[6:]))
	r.SecurityBuffer, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("SESSION_SETUP Response SecurityBuffer: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// LOGOFF（MS-SMB2 §2.2.7 / §2.2.8）—— 请求与响应结构相同：
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
// ---------------------------------------------------------------------------

// logoffStructureSize 是 LOGOFF 请求/响应的 StructureSize（MS-SMB2 §2.2.7）。
const logoffStructureSize = 4

// LogoffRequest 是 SMB2 LOGOFF Request（MS-SMB2 §2.2.7），无有效载荷。
type LogoffRequest struct{}

// ParseLogoffRequest 解析 LOGOFF Request。
func ParseLogoffRequest(b []byte) (*LogoffRequest, error) {
	if err := parseFixedOnly(b, logoffStructureSize, "LOGOFF Request"); err != nil {
		return nil, err
	}
	return &LogoffRequest{}, nil
}

// Append 编码 LOGOFF Request 报文体。
func (r *LogoffRequest) Append(dst []byte) []byte {
	return appendFixedOnly(dst, logoffStructureSize)
}

// LogoffResponse 是 SMB2 LOGOFF Response（MS-SMB2 §2.2.8），无有效载荷。
type LogoffResponse struct{}

// Append 编码 LOGOFF Response 报文体。
func (r *LogoffResponse) Append(dst []byte) []byte {
	return appendFixedOnly(dst, logoffStructureSize)
}

// ParseLogoffResponse 解析 LOGOFF Response。
func ParseLogoffResponse(b []byte) (*LogoffResponse, error) {
	if err := parseFixedOnly(b, logoffStructureSize, "LOGOFF Response"); err != nil {
		return nil, err
	}
	return &LogoffResponse{}, nil
}

// parseFixedOnly 校验「StructureSize(2) + Reserved(2)」这类 4 字节无载荷结构。
func parseFixedOnly(b []byte, size uint16, what string) error {
	body, err := msgBody(b, int(size), what)
	if err != nil {
		return err
	}
	if err := checkStructureSize(body, size); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// appendFixedOnly 编码「StructureSize(2) + Reserved(2)」这类 4 字节无载荷结构。
func appendFixedOnly(dst []byte, size uint16) []byte {
	dst, f := grow(dst, int(size))
	le.PutUint16(f[0:], size)
	return dst
}
