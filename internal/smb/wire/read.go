package wire

import "fmt"

// ---------------------------------------------------------------------------
// READ Request（MS-SMB2 §2.2.19）
//
//	 0 StructureSize(2) = 49
//	 2 Padding(1)                 客户端期望的数据偏移，服务端可忽略
//	 3 Flags(1)                   3.0.2+：SMB2_READFLAG_READ_UNBUFFERED
//	 4 Length(4)                  要读的字节数
//	 8 Offset(8)                  文件内偏移
//	16 FileId(16)
//	32 MinimumCount(4)            少于该值则回 STATUS_END_OF_FILE
//	36 Channel(4)                 非 RDMA 一律 0
//	40 RemainingBytes(4)
//	44 ReadChannelInfoOffset(2)
//	46 ReadChannelInfoLength(2)
//	48 Buffer                     至少 1 字节（StructureSize 49 = 48 + 1）
// ---------------------------------------------------------------------------

const (
	readRequestStructureSize = 49
	readRequestFixed         = 48

	readResponseStructureSize = 17
	readResponseFixed         = 16
)

// READ Request 的 Flags 位（MS-SMB2 §2.2.19）。
const (
	ReadFlagUnbuffered    uint8 = 0x01 // SMB2_READFLAG_READ_UNBUFFERED（3.0.2+）
	ReadFlagRequestCompr  uint8 = 0x02 // SMB2_READFLAG_REQUEST_COMPRESSED（3.1.1）
	ReadResponseFlagCompr uint8 = 0x01 // SMB2_READFLAG_RESPONSE_NONE 之外的压缩标志（3.1.1）
)

// ReadRequest 是 SMB2 READ Request（MS-SMB2 §2.2.19）。
type ReadRequest struct {
	Padding        uint8
	Flags          uint8
	Length         uint32
	Offset         uint64
	FileID         FileID
	MinimumCount   uint32
	Channel        uint32
	RemainingBytes uint32
	// ReadChannelInfo 只在 RDMA 通道下有意义，本项目不支持 RDMA，仅保留原样。
	ReadChannelInfo []byte
}

// ParseReadRequest 解析 READ Request。b 是完整消息（含 64 字节头）。
func ParseReadRequest(b []byte) (*ReadRequest, error) {
	body, err := msgBody(b, readRequestFixed, "READ Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, readRequestStructureSize); err != nil {
		return nil, fmt.Errorf("READ Request: %w", err)
	}
	r := &ReadRequest{
		Padding:        body[2],
		Flags:          body[3],
		Length:         le.Uint32(body[4:]),
		Offset:         le.Uint64(body[8:]),
		FileID:         parseFileID(body[16:]),
		MinimumCount:   le.Uint32(body[32:]),
		Channel:        le.Uint32(body[36:]),
		RemainingBytes: le.Uint32(body[40:]),
	}
	off := uint64(le.Uint16(body[44:]))
	length := uint64(le.Uint16(body[46:]))
	r.ReadChannelInfo, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("READ Request ReadChannelInfo: %w", err)
	}
	return r, nil
}

// Append 把 READ Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *ReadRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, readRequestFixed)
	le.PutUint16(f[0:], readRequestStructureSize)
	f[2] = r.Padding
	f[3] = r.Flags
	le.PutUint32(f[4:], r.Length)
	le.PutUint64(f[8:], r.Offset)
	r.FileID.put(f[16:])
	le.PutUint32(f[32:], r.MinimumCount)
	le.PutUint32(f[36:], r.Channel)
	le.PutUint32(f[40:], r.RemainingBytes)

	if len(r.ReadChannelInfo) > 0 {
		n, err := u16(len(r.ReadChannelInfo), "ReadChannelInfo")
		if err != nil {
			return nil, err
		}
		le.PutUint16(f[44:], HeaderSize+readRequestFixed)
		le.PutUint16(f[46:], n)
		return append(dst, r.ReadChannelInfo...), nil
	}
	// StructureSize 49 = 固定 48 + 1 字节可变部分占位。
	dst, _ = grow(dst, 1)
	return dst, nil
}

// ---------------------------------------------------------------------------
// READ Response（MS-SMB2 §2.2.20）
//
//	 0 StructureSize(2) = 17
//	 2 DataOffset(1)     相对 SMB2 头起点（典型值 0x50 = 64+16）
//	 3 Reserved(1)
//	 4 DataLength(4)
//	 8 DataRemaining(4)
//	12 Reserved2(4)      3.1.1：Flags
//	16 Buffer
// ---------------------------------------------------------------------------

// ReadResponse 是 SMB2 READ Response（MS-SMB2 §2.2.20）。
type ReadResponse struct {
	DataRemaining uint32
	Flags         uint32
	Data          []byte
}

// Append 把 READ Response 报文体追加到 dst。
func (r *ReadResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, readResponseFixed)
	le.PutUint16(f[0:], readResponseStructureSize)
	// DataOffset 是 1 字节，因此数据必须紧跟固定部分：64 + 16 = 80 = 0x50。
	f[2] = HeaderSize + readResponseFixed
	n, err := u32(len(r.Data), "READ Data")
	if err != nil {
		return nil, err
	}
	le.PutUint32(f[4:], n)
	le.PutUint32(f[8:], r.DataRemaining)
	le.PutUint32(f[12:], r.Flags)
	if n == 0 {
		// 1 字节可变部分占位（StructureSize 17 = 16 + 1）。
		dst, _ = grow(dst, 1)
		return dst, nil
	}
	return append(dst, r.Data...), nil
}

// ParseReadResponse 解析 READ Response（供测试与 Go 客户端使用）。
func ParseReadResponse(b []byte) (*ReadResponse, error) {
	body, err := msgBody(b, readResponseFixed, "READ Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, readResponseStructureSize); err != nil {
		return nil, fmt.Errorf("READ Response: %w", err)
	}
	r := &ReadResponse{
		DataRemaining: le.Uint32(body[8:]),
		Flags:         le.Uint32(body[12:]),
	}
	off := uint64(body[2])
	length := uint64(le.Uint32(body[4:]))
	r.Data, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("READ Response Data: %w", err)
	}
	return r, nil
}
