package wire

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
// CANCEL（MS-SMB2 §2.2.30）—— 只有请求，**没有任何响应**。
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
//
// 服务端收到后要去找 MessageId（异步时是 AsyncId）匹配的挂起请求，
// 让那条请求以 STATUS_CANCELLED 收尾；CANCEL 本身不产生回包
// （§3.3.5.16）。§2.2.26 是 LOCK Request，别弄混。
// ---------------------------------------------------------------------------

// cancelStructureSize 是 CANCEL Request 的 StructureSize（MS-SMB2 §2.2.30）。
const cancelStructureSize = 4

// CancelRequest 是 SMB2 CANCEL Request（MS-SMB2 §2.2.30）。
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
