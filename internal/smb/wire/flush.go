package wire

import "fmt"

// ---------------------------------------------------------------------------
// FLUSH（MS-SMB2 §2.2.17 / §2.2.18）
//
// Request：
//
//	0 StructureSize(2) = 24
//	2 Reserved1(2)
//	4 Reserved2(4)
//	8 FileId(16)
//
// Response：
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
//
// 注意（protocol-notes §3）：**不要**对 FLUSH 回 STATUS_NOT_SUPPORTED，
// macOS 与 Office 会据此判定挂载不可用。
// ---------------------------------------------------------------------------

const (
	flushRequestStructureSize  = 24
	flushResponseStructureSize = 4
)

// FlushRequest 是 SMB2 FLUSH Request（MS-SMB2 §2.2.17）。
type FlushRequest struct {
	FileID FileID
}

// ParseFlushRequest 解析 FLUSH Request。b 是完整消息（含 64 字节头）。
func ParseFlushRequest(b []byte) (*FlushRequest, error) {
	body, err := msgBody(b, flushRequestStructureSize, "FLUSH Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, flushRequestStructureSize); err != nil {
		return nil, fmt.Errorf("FLUSH Request: %w", err)
	}
	return &FlushRequest{FileID: parseFileID(body[8:])}, nil
}

// Append 把 FLUSH Request 报文体追加到 dst。
func (r *FlushRequest) Append(dst []byte) []byte {
	dst, f := grow(dst, flushRequestStructureSize)
	le.PutUint16(f[0:], flushRequestStructureSize)
	r.FileID.put(f[8:])
	return dst
}

// FlushResponse 是 SMB2 FLUSH Response（MS-SMB2 §2.2.18），无有效载荷。
type FlushResponse struct{}

// Append 把 FLUSH Response 报文体追加到 dst。
func (r *FlushResponse) Append(dst []byte) []byte {
	return appendFixedOnly(dst, flushResponseStructureSize)
}

// ParseFlushResponse 解析 FLUSH Response。
func ParseFlushResponse(b []byte) (*FlushResponse, error) {
	if err := parseFixedOnly(b, flushResponseStructureSize, "FLUSH Response"); err != nil {
		return nil, err
	}
	return &FlushResponse{}, nil
}
