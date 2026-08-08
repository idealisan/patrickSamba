package wire

import "fmt"

// ---------------------------------------------------------------------------
// CLOSE Request（MS-SMB2 §2.2.15）
//
//	0 StructureSize(2) = 24
//	2 Flags(2)
//	4 Reserved(4)
//	8 FileId(16)
//
// CLOSE Response（MS-SMB2 §2.2.16）
//
//	 0 StructureSize(2) = 60
//	 2 Flags(2)
//	 4 Reserved(4)
//	 8 CreationTime(8)
//	16 LastAccessTime(8)
//	24 LastWriteTime(8)
//	32 ChangeTime(8)
//	40 AllocationSize(8)
//	48 EndofFile(8)
//	56 FileAttributes(4)
//
// 两者都没有可变部分，StructureSize 就是实际长度。
// ---------------------------------------------------------------------------

const (
	closeRequestStructureSize  = 24
	closeResponseStructureSize = 60
)

// CloseFlagPostQueryAttrib 表示客户端要求在关闭时回填文件属性
// （MS-SMB2 §2.2.15 SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB）。
const CloseFlagPostQueryAttrib uint16 = 0x0001

// CloseRequest 是 SMB2 CLOSE Request（MS-SMB2 §2.2.15）。
type CloseRequest struct {
	Flags  uint16
	FileID FileID
}

// PostQueryAttrib 报告客户端是否要求响应里带回属性。
func (r *CloseRequest) PostQueryAttrib() bool {
	return r.Flags&CloseFlagPostQueryAttrib != 0
}

// ParseCloseRequest 解析 CLOSE Request。b 是完整消息（含 64 字节头）。
func ParseCloseRequest(b []byte) (*CloseRequest, error) {
	body, err := msgBody(b, closeRequestStructureSize, "CLOSE Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, closeRequestStructureSize); err != nil {
		return nil, fmt.Errorf("CLOSE Request: %w", err)
	}
	return &CloseRequest{
		Flags:  le.Uint16(body[2:]),
		FileID: parseFileID(body[8:]),
	}, nil
}

// Append 把 CLOSE Request 报文体追加到 dst。
func (r *CloseRequest) Append(dst []byte) []byte {
	dst, f := grow(dst, closeRequestStructureSize)
	le.PutUint16(f[0:], closeRequestStructureSize)
	le.PutUint16(f[2:], r.Flags)
	r.FileID.put(f[8:])
	return dst
}

// CloseResponse 是 SMB2 CLOSE Response（MS-SMB2 §2.2.16）。
// 只有当请求置了 SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB 时属性字段才有意义，
// 否则必须全部为 0（服务端不得乱填）。
type CloseResponse struct {
	Flags          uint16
	CreationTime   uint64
	LastAccessTime uint64
	LastWriteTime  uint64
	ChangeTime     uint64
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes FileAttributes
}

// Append 把 CLOSE Response 报文体追加到 dst。
func (r *CloseResponse) Append(dst []byte) []byte {
	dst, f := grow(dst, closeResponseStructureSize)
	le.PutUint16(f[0:], closeResponseStructureSize)
	le.PutUint16(f[2:], r.Flags)
	le.PutUint64(f[8:], r.CreationTime)
	le.PutUint64(f[16:], r.LastAccessTime)
	le.PutUint64(f[24:], r.LastWriteTime)
	le.PutUint64(f[32:], r.ChangeTime)
	le.PutUint64(f[40:], r.AllocationSize)
	le.PutUint64(f[48:], r.EndOfFile)
	le.PutUint32(f[56:], uint32(r.FileAttributes))
	return dst
}

// ParseCloseResponse 解析 CLOSE Response（供测试与 Go 客户端使用）。
func ParseCloseResponse(b []byte) (*CloseResponse, error) {
	body, err := msgBody(b, closeResponseStructureSize, "CLOSE Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, closeResponseStructureSize); err != nil {
		return nil, fmt.Errorf("CLOSE Response: %w", err)
	}
	return &CloseResponse{
		Flags:          le.Uint16(body[2:]),
		CreationTime:   le.Uint64(body[8:]),
		LastAccessTime: le.Uint64(body[16:]),
		LastWriteTime:  le.Uint64(body[24:]),
		ChangeTime:     le.Uint64(body[32:]),
		AllocationSize: le.Uint64(body[40:]),
		EndOfFile:      le.Uint64(body[48:]),
		FileAttributes: FileAttributes(le.Uint32(body[56:])),
	}, nil
}
