package wire

import "fmt"

// ---------------------------------------------------------------------------
// WRITE Request（MS-SMB2 §2.2.21）
//
//	 0 StructureSize(2) = 49
//	 2 DataOffset(2)              相对 SMB2 头起点
//	 4 Length(4)                  数据字节数
//	 8 Offset(8)                  文件内偏移
//	16 FileId(16)
//	32 Channel(4)
//	36 RemainingBytes(4)
//	40 WriteChannelInfoOffset(2)
//	42 WriteChannelInfoLength(2)
//	44 Flags(4)
//	48 Buffer
// ---------------------------------------------------------------------------

const (
	writeRequestStructureSize = 49
	writeRequestFixed         = 48

	writeResponseStructureSize = 17
	writeResponseFixed         = 16
)

// WRITE Request 的 Flags 位（MS-SMB2 §2.2.21）。
const (
	WriteFlagWriteThrough  uint32 = 0x00000001 // SMB2_WRITEFLAG_WRITE_THROUGH
	WriteFlagWriteUnbuffer uint32 = 0x00000002 // SMB2_WRITEFLAG_WRITE_UNBUFFERED（3.0.2+）
)

// WriteRequest 是 SMB2 WRITE Request（MS-SMB2 §2.2.21）。
type WriteRequest struct {
	Offset         uint64
	FileID         FileID
	Channel        uint32
	RemainingBytes uint32
	Flags          uint32
	// Data 是要写入的数据，直接指向输入缓冲区，调用方在缓冲区被复用前必须用完。
	Data []byte
	// WriteChannelInfo 只在 RDMA 通道下有意义。
	WriteChannelInfo []byte
}

// ParseWriteRequest 解析 WRITE Request。b 是完整消息（含 64 字节头）。
func ParseWriteRequest(b []byte) (*WriteRequest, error) {
	body, err := msgBody(b, writeRequestFixed, "WRITE Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, writeRequestStructureSize); err != nil {
		return nil, fmt.Errorf("WRITE Request: %w", err)
	}
	r := &WriteRequest{
		Offset:         le.Uint64(body[8:]),
		FileID:         parseFileID(body[16:]),
		Channel:        le.Uint32(body[32:]),
		RemainingBytes: le.Uint32(body[36:]),
		Flags:          le.Uint32(body[44:]),
	}
	dataOff := uint64(le.Uint16(body[2:]))
	dataLen := uint64(le.Uint32(body[4:]))
	r.Data, err = sliceAt(b, dataOff, dataLen)
	if err != nil {
		return nil, fmt.Errorf("WRITE Request Data: %w", err)
	}
	ciOff := uint64(le.Uint16(body[40:]))
	ciLen := uint64(le.Uint16(body[42:]))
	r.WriteChannelInfo, err = sliceAt(b, ciOff, ciLen)
	if err != nil {
		return nil, fmt.Errorf("WRITE Request WriteChannelInfo: %w", err)
	}
	return r, nil
}

// Append 把 WRITE Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *WriteRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, writeRequestFixed)
	le.PutUint16(f[0:], writeRequestStructureSize)
	le.PutUint64(f[8:], r.Offset)
	r.FileID.put(f[16:])
	le.PutUint32(f[32:], r.Channel)
	le.PutUint32(f[36:], r.RemainingBytes)
	le.PutUint32(f[44:], r.Flags)

	n, err := u32(len(r.Data), "WRITE Data")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[2:], HeaderSize+writeRequestFixed)
	le.PutUint32(f[4:], n)
	if n == 0 {
		// 1 字节可变部分占位（StructureSize 49 = 48 + 1）。
		dst, _ = grow(dst, 1)
		return dst, nil
	}
	return append(dst, r.Data...), nil
}

// ---------------------------------------------------------------------------
// WRITE Response（MS-SMB2 §2.2.22）
//
//	 0 StructureSize(2) = 17
//	 2 Reserved(2)
//	 4 Count(4)                   实际写入字节数
//	 8 Remaining(4)
//	12 WriteChannelInfoOffset(2)
//	14 WriteChannelInfoLength(2)
//	16 Buffer（RDMA 通道信息，本实现不支持，长度为 0）
// ---------------------------------------------------------------------------

// WriteResponse 是 SMB2 WRITE Response（MS-SMB2 §2.2.22）。
type WriteResponse struct {
	Count     uint32
	Remaining uint32
}

// Append 把 WRITE Response 报文体追加到 dst。
func (r *WriteResponse) Append(dst []byte) []byte {
	dst, f := grow(dst, writeResponseFixed)
	le.PutUint16(f[0:], writeResponseStructureSize)
	le.PutUint32(f[4:], r.Count)
	le.PutUint32(f[8:], r.Remaining)
	// 体就是 16 字节，比 StructureSize(17) 少 1，**不补占位字节**：
	// 真实 Samba 4.22 的 WRITE Response 是 80 字节（64 头 + 16 体），
	// 见 testdata/capture/create-read-write/018-s2c-WRITE.bin。
	// 只有 ERROR Response 被 MS-SMB2 §2.2.2 明文要求补满 1 字节。
	return dst
}

// ParseWriteResponse 解析 WRITE Response（供测试与 Go 客户端使用）。
func ParseWriteResponse(b []byte) (*WriteResponse, error) {
	body, err := msgBody(b, writeResponseFixed, "WRITE Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, writeResponseStructureSize); err != nil {
		return nil, fmt.Errorf("WRITE Response: %w", err)
	}
	return &WriteResponse{
		Count:     le.Uint32(body[4:]),
		Remaining: le.Uint32(body[8:]),
	}, nil
}
