package wire

import "fmt"

// ---------------------------------------------------------------------------
// TREE_CONNECT Request（MS-SMB2 §2.2.9）
//
//	0 StructureSize(2) = 9
//	2 Flags(2)        （3.1.1；更早的方言此处是 Reserved）
//	4 PathOffset(2)   （相对 SMB2 头起点）
//	6 PathLength(2)
//	8 Buffer          （UTF-16LE 的 \\server\share，无结尾 NUL）
// ---------------------------------------------------------------------------

const (
	treeConnectRequestStructureSize = 9
	treeConnectRequestFixed         = 8

	// treeConnectResponseStructureSize 是 §2.2.10 的 StructureSize，
	// 该结构无可变部分，固定 16 字节。
	treeConnectResponseStructureSize = 16
)

// TreeConnectFlags 是 SMB 3.1.1 TREE_CONNECT Request 的 Flags（MS-SMB2 §2.2.9）。
type TreeConnectFlags uint16

const (
	TreeConnectFlagClusterReconnect TreeConnectFlags = 0x0001 // SMB2_TREE_CONNECT_FLAG_CLUSTER_RECONNECT
	TreeConnectFlagRedirectToOwner  TreeConnectFlags = 0x0002 // SMB2_TREE_CONNECT_FLAG_REDIRECT_TO_OWNER
	TreeConnectFlagExtensionPresent TreeConnectFlags = 0x0004 // SMB2_TREE_CONNECT_FLAG_EXTENSION_PRESENT
)

// TreeConnectRequest 是 SMB2 TREE_CONNECT Request（MS-SMB2 §2.2.9）。
type TreeConnectRequest struct {
	Flags TreeConnectFlags
	// Path 形如 `\\SERVER\share`。服务端应忽略主机名部分，只按最后一段
	// 做**大小写不敏感**的 share 名匹配（protocol-notes §7）。
	Path string
}

// ShareName 返回 Path 最后一个反斜杠之后的部分，即 share 名。
// Path 不含反斜杠时原样返回。
func (r *TreeConnectRequest) ShareName() string {
	s := r.Path
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\\' || s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}

// ParseTreeConnectRequest 解析 TREE_CONNECT Request。b 是完整消息（含 64 字节头）。
func ParseTreeConnectRequest(b []byte) (*TreeConnectRequest, error) {
	body, err := msgBody(b, treeConnectRequestFixed, "TREE_CONNECT Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, treeConnectRequestStructureSize); err != nil {
		return nil, fmt.Errorf("TREE_CONNECT Request: %w", err)
	}
	r := &TreeConnectRequest{Flags: TreeConnectFlags(le.Uint16(body[2:]))}
	off := uint64(le.Uint16(body[4:]))
	length := uint64(le.Uint16(body[6:]))
	r.Path, err = DecodeUTF16LEAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("TREE_CONNECT Request Path: %w", err)
	}
	return r, nil
}

// Append 把 TREE_CONNECT Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *TreeConnectRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, treeConnectRequestFixed)
	le.PutUint16(f[0:], treeConnectRequestStructureSize)
	le.PutUint16(f[2:], uint16(r.Flags))
	n, err := u16(UTF16LELen(r.Path), "TREE_CONNECT Path")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[4:], HeaderSize+treeConnectRequestFixed)
	le.PutUint16(f[6:], n)
	dst, _ = AppendUTF16LE(dst, r.Path)
	return dst, nil
}

// ---------------------------------------------------------------------------
// TREE_CONNECT Response（MS-SMB2 §2.2.10）
//
//	 0 StructureSize(2) = 16
//	 2 ShareType(1)
//	 3 Reserved(1)
//	 4 ShareFlags(4)
//	 8 Capabilities(4)
//	12 MaximalAccess(4)
// ---------------------------------------------------------------------------

// TreeConnectResponse 是 SMB2 TREE_CONNECT Response（MS-SMB2 §2.2.10）。
type TreeConnectResponse struct {
	ShareType     ShareType
	ShareFlags    ShareFlags
	Capabilities  ShareCapabilities
	MaximalAccess uint32
}

// Append 把 TREE_CONNECT Response 报文体追加到 dst。
func (r *TreeConnectResponse) Append(dst []byte) []byte {
	dst, f := grow(dst, treeConnectResponseStructureSize)
	le.PutUint16(f[0:], treeConnectResponseStructureSize)
	f[2] = byte(r.ShareType)
	// f[3] Reserved
	le.PutUint32(f[4:], uint32(r.ShareFlags))
	le.PutUint32(f[8:], uint32(r.Capabilities))
	le.PutUint32(f[12:], r.MaximalAccess)
	return dst
}

// ParseTreeConnectResponse 解析 TREE_CONNECT Response（供测试与 Go 客户端使用）。
func ParseTreeConnectResponse(b []byte) (*TreeConnectResponse, error) {
	body, err := msgBody(b, treeConnectResponseStructureSize, "TREE_CONNECT Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, treeConnectResponseStructureSize); err != nil {
		return nil, fmt.Errorf("TREE_CONNECT Response: %w", err)
	}
	return &TreeConnectResponse{
		ShareType:     ShareType(body[2]),
		ShareFlags:    ShareFlags(le.Uint32(body[4:])),
		Capabilities:  ShareCapabilities(le.Uint32(body[8:])),
		MaximalAccess: le.Uint32(body[12:]),
	}, nil
}

// ---------------------------------------------------------------------------
// TREE_DISCONNECT（MS-SMB2 §2.2.11 / §2.2.12）
//
//	0 StructureSize(2) = 4
//	2 Reserved(2)
// ---------------------------------------------------------------------------

// treeDisconnectStructureSize 是 TREE_DISCONNECT 请求/响应的 StructureSize。
const treeDisconnectStructureSize = 4

// TreeDisconnectRequest 是 SMB2 TREE_DISCONNECT Request（MS-SMB2 §2.2.11）。
type TreeDisconnectRequest struct{}

// ParseTreeDisconnectRequest 解析 TREE_DISCONNECT Request。
func ParseTreeDisconnectRequest(b []byte) (*TreeDisconnectRequest, error) {
	if err := parseFixedOnly(b, treeDisconnectStructureSize, "TREE_DISCONNECT Request"); err != nil {
		return nil, err
	}
	return &TreeDisconnectRequest{}, nil
}

// Append 编码 TREE_DISCONNECT Request 报文体。
func (r *TreeDisconnectRequest) Append(dst []byte) []byte {
	return appendFixedOnly(dst, treeDisconnectStructureSize)
}

// TreeDisconnectResponse 是 SMB2 TREE_DISCONNECT Response（MS-SMB2 §2.2.12）。
type TreeDisconnectResponse struct{}

// Append 编码 TREE_DISCONNECT Response 报文体。
func (r *TreeDisconnectResponse) Append(dst []byte) []byte {
	return appendFixedOnly(dst, treeDisconnectStructureSize)
}

// ParseTreeDisconnectResponse 解析 TREE_DISCONNECT Response。
func ParseTreeDisconnectResponse(b []byte) (*TreeDisconnectResponse, error) {
	if err := parseFixedOnly(b, treeDisconnectStructureSize, "TREE_DISCONNECT Response"); err != nil {
		return nil, err
	}
	return &TreeDisconnectResponse{}, nil
}
