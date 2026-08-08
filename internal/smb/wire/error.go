package wire

import "fmt"

// ---------------------------------------------------------------------------
// ERROR Response（MS-SMB2 §2.2.2）
//
//	0 StructureSize(2) = 9
//	2 ErrorContextCount(1)  （3.1.1；更早方言为 Reserved）
//	3 Reserved(1)
//	4 ByteCount(4)          ErrorData 的字节数
//	8 ErrorData(变长)       ByteCount = 0 时**仍须有 1 字节占位**
//
// 最后这条是最容易踩的坑：StructureSize 9 = 固定 8 + 1 字节可变部分占位，
// 没有 ErrorData 时必须补一个 0 字节，否则 Windows/macOS 客户端会判定报文非法。
// ---------------------------------------------------------------------------

const (
	errorResponseStructureSize = 9
	errorResponseFixed         = 8
)

// ErrorResponse 是 SMB2 ERROR Response（MS-SMB2 §2.2.2）。
//
// NTSTATUS 本身在 SMB2 头的 Status 字段里，不在本结构中。
type ErrorResponse struct {
	// ErrorContextCount 仅 SMB 3.1.1 有意义；不使用 error context 时为 0。
	ErrorContextCount uint8
	// ErrorData 可为空。典型的非空场景：
	//   - STATUS_BUFFER_TOO_SMALL 时返回所需缓冲区大小（4 字节）
	//   - STATUS_STOPPED_ON_SYMLINK 时返回 Symbolic Link Error Response
	ErrorData []byte
}

// Append 把 ERROR Response 报文体追加到 dst。
func (r *ErrorResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, errorResponseFixed)
	le.PutUint16(f[0:], errorResponseStructureSize)
	f[2] = r.ErrorContextCount
	// f[3] Reserved
	n, err := u32(len(r.ErrorData), "ErrorData")
	if err != nil {
		return nil, err
	}
	le.PutUint32(f[4:], n)
	if n == 0 {
		// ByteCount = 0 时的 1 字节占位（MS-SMB2 §2.2.2 ErrorData）。
		dst, _ = grow(dst, 1)
		return dst, nil
	}
	return append(dst, r.ErrorData...), nil
}

// ParseErrorResponse 解析 ERROR Response（供测试与 Go 客户端使用）。
func ParseErrorResponse(b []byte) (*ErrorResponse, error) {
	body, err := msgBody(b, errorResponseFixed, "ERROR Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, errorResponseStructureSize); err != nil {
		return nil, fmt.Errorf("ERROR Response: %w", err)
	}
	r := &ErrorResponse{ErrorContextCount: body[2]}
	n := uint64(le.Uint32(body[4:]))
	r.ErrorData, err = sliceAt(body, errorResponseFixed, n)
	if err != nil {
		return nil, fmt.Errorf("ERROR Response ErrorData: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// ECHO（MS-SMB2 §2.2.28 / §2.2.29）
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
// ---------------------------------------------------------------------------

// echoStructureSize 是 ECHO 请求/响应的 StructureSize（MS-SMB2 §2.2.28）。
const echoStructureSize = 4

// EchoRequest 是 SMB2 ECHO Request（MS-SMB2 §2.2.28），无有效载荷。
type EchoRequest struct{}

// ParseEchoRequest 解析 ECHO Request。
func ParseEchoRequest(b []byte) (*EchoRequest, error) {
	if err := parseFixedOnly(b, echoStructureSize, "ECHO Request"); err != nil {
		return nil, err
	}
	return &EchoRequest{}, nil
}

// Append 编码 ECHO Request 报文体。
func (r *EchoRequest) Append(dst []byte) []byte {
	return appendFixedOnly(dst, echoStructureSize)
}

// EchoResponse 是 SMB2 ECHO Response（MS-SMB2 §2.2.29），无有效载荷。
type EchoResponse struct{}

// Append 编码 ECHO Response 报文体。
func (r *EchoResponse) Append(dst []byte) []byte {
	return appendFixedOnly(dst, echoStructureSize)
}

// ParseEchoResponse 解析 ECHO Response。
func ParseEchoResponse(b []byte) (*EchoResponse, error) {
	if err := parseFixedOnly(b, echoStructureSize, "ECHO Response"); err != nil {
		return nil, err
	}
	return &EchoResponse{}, nil
}

// ---------------------------------------------------------------------------
// CANCEL（MS-SMB2 §2.2.26）—— 只有请求，没有响应。
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
// ---------------------------------------------------------------------------

// cancelStructureSize 是 CANCEL Request 的 StructureSize（MS-SMB2 §2.2.26）。
const cancelStructureSize = 4

// CancelRequest 是 SMB2 CANCEL Request（MS-SMB2 §2.2.26）。
type CancelRequest struct{}

// ParseCancelRequest 解析 CANCEL Request。
func ParseCancelRequest(b []byte) (*CancelRequest, error) {
	if err := parseFixedOnly(b, cancelStructureSize, "CANCEL Request"); err != nil {
		return nil, err
	}
	return &CancelRequest{}, nil
}

// Append 编码 CANCEL Request 报文体。
func (r *CancelRequest) Append(dst []byte) []byte {
	return appendFixedOnly(dst, cancelStructureSize)
}
