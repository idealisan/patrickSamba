package wire

import "fmt"

// ---------------------------------------------------------------------------
// SET_INFO Request（MS-SMB2 §2.2.39）
//
//	 0 StructureSize(2) = 33
//	 2 InfoType(1)
//	 3 FileInfoClass(1)
//	 4 BufferLength(4)
//	 8 BufferOffset(2)         相对 SMB2 头起点
//	10 Reserved(2)
//	12 AdditionalInformation(4)
//	16 FileId(16)
//	32 Buffer
//
// SET_INFO Response（MS-SMB2 §2.2.40）：只有 StructureSize(2) = 2。
// ---------------------------------------------------------------------------

const (
	setInfoRequestStructureSize = 33
	setInfoRequestFixed         = 32

	setInfoResponseStructureSize = 2
)

// SetInfoRequest 是 SMB2 SET_INFO Request（MS-SMB2 §2.2.39）。
type SetInfoRequest struct {
	InfoType InfoType
	// FileInfoClass 按 InfoType 解释，同 QUERY_INFO。
	FileInfoClass         uint8
	AdditionalInformation uint32
	FileID                FileID
	Buffer                []byte
}

// FileClass 返回按 FileInformationClass 解释的类号。
func (r *SetInfoRequest) FileClass() FileInfoClass { return FileInfoClass(r.FileInfoClass) }

// FsClass 返回按 FileSystemInformationClass 解释的类号。
func (r *SetInfoRequest) FsClass() FsInfoClass { return FsInfoClass(r.FileInfoClass) }

// ParseSetInfoRequest 解析 SET_INFO Request。b 是完整消息（含 64 字节头）。
func ParseSetInfoRequest(b []byte) (*SetInfoRequest, error) {
	body, err := msgBody(b, setInfoRequestFixed, "SET_INFO Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, setInfoRequestStructureSize); err != nil {
		return nil, fmt.Errorf("SET_INFO Request: %w", err)
	}
	r := &SetInfoRequest{
		InfoType:              InfoType(body[2]),
		FileInfoClass:         body[3],
		AdditionalInformation: le.Uint32(body[12:]),
		FileID:                parseFileID(body[16:]),
	}
	// BufferOffset 相对 SMB2 头起点。
	r.Buffer, err = sliceAt(b, uint64(le.Uint16(body[8:])), uint64(le.Uint32(body[4:])))
	if err != nil {
		return nil, fmt.Errorf("SET_INFO Request Buffer: %w", err)
	}
	return r, nil
}

// Append 把 SET_INFO Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *SetInfoRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, setInfoRequestFixed)
	le.PutUint16(f[0:], setInfoRequestStructureSize)
	f[2] = byte(r.InfoType)
	f[3] = r.FileInfoClass
	le.PutUint32(f[12:], r.AdditionalInformation)
	r.FileID.put(f[16:])

	if len(r.Buffer) == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
		return dst, nil
	}
	n, err := u32(len(r.Buffer), "SET_INFO BufferLength")
	if err != nil {
		return nil, err
	}
	le.PutUint32(f[4:], n)
	le.PutUint16(f[8:], HeaderSize+setInfoRequestFixed)
	return append(dst, r.Buffer...), nil
}

// SetInfoResponse 是 SMB2 SET_INFO Response（MS-SMB2 §2.2.40），无可变部分。
type SetInfoResponse struct{}

// Append 把 SET_INFO Response 报文体（2 字节）追加到 dst。
func (r *SetInfoResponse) Append(dst []byte) []byte {
	dst, f := grow(dst, 2)
	le.PutUint16(f, setInfoResponseStructureSize)
	return dst
}

// ParseSetInfoResponse 解析 SET_INFO Response（供测试与 Go 客户端使用）。
func ParseSetInfoResponse(b []byte) (*SetInfoResponse, error) {
	body, err := msgBody(b, 2, "SET_INFO Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, setInfoResponseStructureSize); err != nil {
		return nil, fmt.Errorf("SET_INFO Response: %w", err)
	}
	return &SetInfoResponse{}, nil
}

// ---------------------------------------------------------------------------
// SET_INFO 载荷（MS-FSCC §2.4）
// ---------------------------------------------------------------------------

// FileRenameInfo 是 FileRenameInformation（MS-FSCC §2.4.37.2，SMB2 用 Type 2）：
//
//	 0 ReplaceIfExists(1)
//	 1 Reserved(7)
//	 8 RootDirectory(8)     SMB2 里必须为 0
//	16 FileNameLength(4)
//	20 FileName(UTF-16LE)   相对 share 根的路径，**不含**前导反斜杠
type FileRenameInfo struct {
	ReplaceIfExists bool
	RootDirectory   uint64
	FileName        string
}

// fileRenameInfoFixed 是 FileRenameInformation 里 FileName 之前的字节数。
const fileRenameInfoFixed = 20

// ParseFileRenameInfo 解析 FileRenameInformation。
func ParseFileRenameInfo(data []byte) (FileRenameInfo, error) {
	var i FileRenameInfo
	if err := need(data, fileRenameInfoFixed); err != nil {
		return i, fmt.Errorf("FileRenameInformation: %w", err)
	}
	i.ReplaceIfExists = data[0] != 0
	i.RootDirectory = le.Uint64(data[8:])
	name, err := DecodeUTF16LEAt(data, fileRenameInfoFixed, uint64(le.Uint32(data[16:])))
	if err != nil {
		return i, fmt.Errorf("FileRenameInformation FileName: %w", err)
	}
	i.FileName = name
	return i, nil
}

// Encode 编码 FileRenameInformation（供测试与 Go 客户端使用）。
func (i FileRenameInfo) Encode() []byte {
	name := EncodeUTF16LE(i.FileName)
	b := make([]byte, fileRenameInfoFixed+len(name))
	b[0] = boolByte(i.ReplaceIfExists)
	le.PutUint64(b[8:], i.RootDirectory)
	le.PutUint32(b[16:], uint32(len(name)))
	copy(b[fileRenameInfoFixed:], name)
	return b
}

// FileLinkInfo 是 FileLinkInformation（MS-FSCC §2.4.21.2），布局同 rename。
type FileLinkInfo struct {
	ReplaceIfExists bool
	RootDirectory   uint64
	FileName        string
}

// ParseFileLinkInfo 解析 FileLinkInformation。
func ParseFileLinkInfo(data []byte) (FileLinkInfo, error) {
	r, err := ParseFileRenameInfo(data)
	if err != nil {
		return FileLinkInfo{}, fmt.Errorf("FileLinkInformation: %w", err)
	}
	return FileLinkInfo(r), nil
}

// Encode 编码 FileLinkInformation。
func (i FileLinkInfo) Encode() []byte { return FileRenameInfo(i).Encode() }

// FileDispositionInfoSize 是 FileDispositionInformation（MS-FSCC §2.4.11）的字节数。
const FileDispositionInfoSize = 1

// ParseFileDispositionInfo 解析 FileDispositionInformation，返回 DeletePending。
// 置位表示句柄关闭时删除文件。
func ParseFileDispositionInfo(data []byte) (bool, error) {
	if err := need(data, FileDispositionInfoSize); err != nil {
		return false, fmt.Errorf("FileDispositionInformation: %w", err)
	}
	return data[0] != 0, nil
}

// EncodeFileDispositionInfo 编码 FileDispositionInformation。
func EncodeFileDispositionInfo(deletePending bool) []byte {
	return []byte{boolByte(deletePending)}
}

// FileEndOfFileInfoSize 是 FileEndOfFileInformation（MS-FSCC §2.4.13）的字节数。
const FileEndOfFileInfoSize = 8

// ParseFileEndOfFileInfo 解析 FileEndOfFileInformation（截断/扩展文件长度）。
// 负值非法。
func ParseFileEndOfFileInfo(data []byte) (int64, error) {
	if err := need(data, FileEndOfFileInfoSize); err != nil {
		return 0, fmt.Errorf("FileEndOfFileInformation: %w", err)
	}
	v := int64(le.Uint64(data))
	if v < 0 {
		return 0, fmt.Errorf("%w: FileEndOfFileInformation 长度为负 %d", ErrMalformed, v)
	}
	return v, nil
}

// EncodeFileEndOfFileInfo 编码 FileEndOfFileInformation。
func EncodeFileEndOfFileInfo(eof int64) []byte {
	b := make([]byte, FileEndOfFileInfoSize)
	le.PutUint64(b, uint64(eof))
	return b
}

// FileAllocationInfoSize 是 FileAllocationInformation（MS-FSCC §2.4.4）的字节数。
const FileAllocationInfoSize = 8

// ParseFileAllocationInfo 解析 FileAllocationInformation（预分配大小）。
func ParseFileAllocationInfo(data []byte) (int64, error) {
	if err := need(data, FileAllocationInfoSize); err != nil {
		return 0, fmt.Errorf("FileAllocationInformation: %w", err)
	}
	v := int64(le.Uint64(data))
	if v < 0 {
		return 0, fmt.Errorf("%w: FileAllocationInformation 大小为负 %d", ErrMalformed, v)
	}
	return v, nil
}

// FileValidDataLengthInfoSize 是 FileValidDataLengthInformation（MS-FSCC §2.4.45）的字节数。
const FileValidDataLengthInfoSize = 8

// ParseFileValidDataLengthInfo 解析 FileValidDataLengthInformation。
func ParseFileValidDataLengthInfo(data []byte) (int64, error) {
	if err := need(data, FileValidDataLengthInfoSize); err != nil {
		return 0, fmt.Errorf("FileValidDataLengthInformation: %w", err)
	}
	return int64(le.Uint64(data)), nil
}

// ParseFileShortNameInfo 解析 FileShortNameInformation（MS-FSCC §2.4.40），
// 布局同 FileNameInformation。不支持 8.3 短名时回 STATUS_NOT_SUPPORTED。
func ParseFileShortNameInfo(data []byte) (string, error) {
	s, err := ParseFileNameInfo(data)
	if err != nil {
		return "", fmt.Errorf("FileShortNameInformation: %w", err)
	}
	return s, nil
}
