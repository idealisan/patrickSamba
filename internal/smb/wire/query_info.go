package wire

import "fmt"

// ---------------------------------------------------------------------------
// QUERY_INFO Request（MS-SMB2 §2.2.37）
//
//	 0 StructureSize(2) = 41
//	 2 InfoType(1)
//	 3 FileInfoClass(1)
//	 4 OutputBufferLength(4)
//	 8 InputBufferOffset(2)     相对 SMB2 头起点
//	10 Reserved(2)
//	12 InputBufferLength(4)
//	16 AdditionalInformation(4)
//	20 Flags(4)
//	24 FileId(16)
//	40 Buffer
//
// QUERY_INFO Response（MS-SMB2 §2.2.38）
//
//	0 StructureSize(2) = 9
//	2 OutputBufferOffset(2)
//	4 OutputBufferLength(4)
//	8 Buffer
// ---------------------------------------------------------------------------

const (
	queryInfoRequestStructureSize = 41
	queryInfoRequestFixed         = 40

	queryInfoResponseStructureSize = 9
	queryInfoResponseFixed         = 8
)

// QueryInfoFlags 是 QUERY_INFO 的 Flags（MS-SMB2 §2.2.37），只对
// FileFullEaInformation 有意义。
type QueryInfoFlags uint32

const (
	RestartScansFlag      QueryInfoFlags = 0x00000001 // SL_RESTART_SCAN
	ReturnSingleEntryFlag QueryInfoFlags = 0x00000002 // SL_RETURN_SINGLE_ENTRY
	IndexSpecifiedFlag    QueryInfoFlags = 0x00000004 // SL_INDEX_SPECIFIED
)

// QueryInfoRequest 是 SMB2 QUERY_INFO Request（MS-SMB2 §2.2.37）。
type QueryInfoRequest struct {
	InfoType InfoType
	// FileInfoClass 按 InfoType 解释：InfoTypeFile 时是 FileInfoClass，
	// InfoTypeFileSystem 时是 FsInfoClass，其余忽略。
	FileInfoClass         uint8
	OutputBufferLength    uint32
	AdditionalInformation uint32
	Flags                 QueryInfoFlags
	FileID                FileID
	Input                 []byte
}

// FileClass 返回按 FileInformationClass 解释的类号。
func (r *QueryInfoRequest) FileClass() FileInfoClass { return FileInfoClass(r.FileInfoClass) }

// FsClass 返回按 FileSystemInformationClass 解释的类号。
func (r *QueryInfoRequest) FsClass() FsInfoClass { return FsInfoClass(r.FileInfoClass) }

// ParseQueryInfoRequest 解析 QUERY_INFO Request。b 是完整消息（含 64 字节头）。
func ParseQueryInfoRequest(b []byte) (*QueryInfoRequest, error) {
	body, err := msgBody(b, queryInfoRequestFixed, "QUERY_INFO Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, queryInfoRequestStructureSize); err != nil {
		return nil, fmt.Errorf("QUERY_INFO Request: %w", err)
	}
	r := &QueryInfoRequest{
		InfoType:              InfoType(body[2]),
		FileInfoClass:         body[3],
		OutputBufferLength:    le.Uint32(body[4:]),
		AdditionalInformation: le.Uint32(body[16:]),
		Flags:                 QueryInfoFlags(le.Uint32(body[20:])),
		FileID:                parseFileID(body[24:]),
	}
	// InputBufferOffset 相对 SMB2 头起点。
	r.Input, err = sliceAt(b, uint64(le.Uint16(body[8:])), uint64(le.Uint32(body[12:])))
	if err != nil {
		return nil, fmt.Errorf("QUERY_INFO Request InputBuffer: %w", err)
	}
	return r, nil
}

// Append 把 QUERY_INFO Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *QueryInfoRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, queryInfoRequestFixed)
	le.PutUint16(f[0:], queryInfoRequestStructureSize)
	f[2] = byte(r.InfoType)
	f[3] = r.FileInfoClass
	le.PutUint32(f[4:], r.OutputBufferLength)
	le.PutUint32(f[16:], r.AdditionalInformation)
	le.PutUint32(f[20:], uint32(r.Flags))
	r.FileID.put(f[24:])

	if len(r.Input) == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
		return dst, nil
	}
	n, err := u32(len(r.Input), "QUERY_INFO InputBufferLength")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[8:], HeaderSize+queryInfoRequestFixed)
	le.PutUint32(f[12:], n)
	return append(dst, r.Input...), nil
}

// QueryInfoResponse 是 SMB2 QUERY_INFO Response（MS-SMB2 §2.2.38）。
//
// Buffer 是按 info class 编码好的结构（见本文件下半部分的 Encode 方法）。
// 缓冲区放不下时服务端应回 STATUS_BUFFER_OVERFLOW 或
// STATUS_INFO_LENGTH_MISMATCH，由 server 层判断。
type QueryInfoResponse struct {
	Buffer []byte
}

// Append 把 QUERY_INFO Response 报文体追加到 dst。
func (r *QueryInfoResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, queryInfoResponseFixed)
	le.PutUint16(f[0:], queryInfoResponseStructureSize)
	n, err := u32(len(r.Buffer), "QUERY_INFO OutputBufferLength")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[2:], HeaderSize+queryInfoResponseFixed)
	le.PutUint32(f[4:], n)
	if n == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
		return dst, nil
	}
	return append(dst, r.Buffer...), nil
}

// ParseQueryInfoResponse 解析 QUERY_INFO Response（供测试与 Go 客户端使用）。
func ParseQueryInfoResponse(b []byte) (*QueryInfoResponse, error) {
	body, err := msgBody(b, queryInfoResponseFixed, "QUERY_INFO Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, queryInfoResponseStructureSize); err != nil {
		return nil, fmt.Errorf("QUERY_INFO Response: %w", err)
	}
	buf, err := sliceAt(b, uint64(le.Uint16(body[2:])), uint64(le.Uint32(body[4:])))
	if err != nil {
		return nil, fmt.Errorf("QUERY_INFO Response OutputBuffer: %w", err)
	}
	return &QueryInfoResponse{Buffer: buf}, nil
}

// ---------------------------------------------------------------------------
// 文件信息类（MS-FSCC §2.4）
//
// 每个结构给 Encode() 与 Parse 一对函数。所有时间是 FILETIME。
// ---------------------------------------------------------------------------

// FileBasicInfo 是 FileBasicInformation（MS-FSCC §2.4.7），固定 40 字节。
type FileBasicInfo struct {
	CreationTime   uint64
	LastAccessTime uint64
	LastWriteTime  uint64
	ChangeTime     uint64
	FileAttributes FileAttributes
}

// FileBasicInfoSize 是 FileBasicInformation 的字节数。
// 注意 SET_INFO 时客户端可能只发 36 字节（不含尾部 Reserved），要容忍。
const FileBasicInfoSize = 40

// Encode 编码为 40 字节。
func (i FileBasicInfo) Encode() []byte {
	b := make([]byte, FileBasicInfoSize)
	le.PutUint64(b[0:], i.CreationTime)
	le.PutUint64(b[8:], i.LastAccessTime)
	le.PutUint64(b[16:], i.LastWriteTime)
	le.PutUint64(b[24:], i.ChangeTime)
	le.PutUint32(b[32:], uint32(i.FileAttributes))
	// b[36:40] Reserved
	return b
}

// ParseFileBasicInfo 解析 FileBasicInformation。允许 36 字节的短形式
// （某些客户端 SET_INFO 时省略尾部 Reserved）。
func ParseFileBasicInfo(data []byte) (FileBasicInfo, error) {
	var i FileBasicInfo
	if err := need(data, 36); err != nil {
		return i, fmt.Errorf("FileBasicInformation: %w", err)
	}
	i.CreationTime = le.Uint64(data[0:])
	i.LastAccessTime = le.Uint64(data[8:])
	i.LastWriteTime = le.Uint64(data[16:])
	i.ChangeTime = le.Uint64(data[24:])
	i.FileAttributes = FileAttributes(le.Uint32(data[32:]))
	return i, nil
}

// FileStandardInfo 是 FileStandardInformation（MS-FSCC §2.4.41），固定 24 字节。
type FileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// FileStandardInfoSize 是 FileStandardInformation 的字节数。
const FileStandardInfoSize = 24

// Encode 编码为 24 字节。
func (i FileStandardInfo) Encode() []byte {
	b := make([]byte, FileStandardInfoSize)
	le.PutUint64(b[0:], uint64(i.AllocationSize))
	le.PutUint64(b[8:], uint64(i.EndOfFile))
	le.PutUint32(b[16:], i.NumberOfLinks)
	b[20] = boolByte(i.DeletePending)
	b[21] = boolByte(i.Directory)
	// b[22:24] Reserved
	return b
}

// ParseFileStandardInfo 解析 FileStandardInformation。
func ParseFileStandardInfo(data []byte) (FileStandardInfo, error) {
	var i FileStandardInfo
	if err := need(data, 22); err != nil {
		return i, fmt.Errorf("FileStandardInformation: %w", err)
	}
	i.AllocationSize = int64(le.Uint64(data[0:]))
	i.EndOfFile = int64(le.Uint64(data[8:]))
	i.NumberOfLinks = le.Uint32(data[16:])
	i.DeletePending = data[20] != 0
	i.Directory = data[21] != 0
	return i, nil
}

// FileInternalInfoSize 是 FileInternalInformation（MS-FSCC §2.4.20）的字节数。
const FileInternalInfoSize = 8

// EncodeFileInternalInfo 编码 FileInternalInformation（64 位文件号）。
func EncodeFileInternalInfo(indexNumber uint64) []byte {
	b := make([]byte, FileInternalInfoSize)
	le.PutUint64(b, indexNumber)
	return b
}

// FileEaInfoSize 是 FileEaInformation（MS-FSCC §2.4.12）的字节数。
const FileEaInfoSize = 4

// EncodeFileEaInfo 编码 FileEaInformation（EA 总长度，不支持 EA 时填 0）。
func EncodeFileEaInfo(eaSize uint32) []byte {
	b := make([]byte, FileEaInfoSize)
	le.PutUint32(b, eaSize)
	return b
}

// FileAccessInfoSize 是 FileAccessInformation（MS-FSCC §2.4.1）的字节数。
const FileAccessInfoSize = 4

// EncodeFileAccessInfo 编码 FileAccessInformation（授予的访问掩码）。
func EncodeFileAccessInfo(access Access) []byte {
	b := make([]byte, FileAccessInfoSize)
	le.PutUint32(b, uint32(access))
	return b
}

// FilePositionInfoSize 是 FilePositionInformation（MS-FSCC §2.4.35）的字节数。
const FilePositionInfoSize = 8

// EncodeFilePositionInfo 编码 FilePositionInformation（当前字节偏移）。
func EncodeFilePositionInfo(offset int64) []byte {
	b := make([]byte, FilePositionInfoSize)
	le.PutUint64(b, uint64(offset))
	return b
}

// ParseFilePositionInfo 解析 FilePositionInformation。
func ParseFilePositionInfo(data []byte) (int64, error) {
	if err := need(data, FilePositionInfoSize); err != nil {
		return 0, fmt.Errorf("FilePositionInformation: %w", err)
	}
	return int64(le.Uint64(data)), nil
}

// FileModeInfoSize 是 FileModeInformation（MS-FSCC §2.4.26）的字节数。
const FileModeInfoSize = 4

// EncodeFileModeInfo 编码 FileModeInformation（CreateOptions 的子集）。
func EncodeFileModeInfo(mode uint32) []byte {
	b := make([]byte, FileModeInfoSize)
	le.PutUint32(b, mode)
	return b
}

// FileAlignmentInfoSize 是 FileAlignmentInformation（MS-FSCC §2.4.3）的字节数。
const FileAlignmentInfoSize = 4

// FileByteAlignment 表示无对齐要求（MS-FSCC §2.4.3）。
const FileByteAlignment uint32 = 0x00000000

// EncodeFileAlignmentInfo 编码 FileAlignmentInformation。
func EncodeFileAlignmentInfo(requirement uint32) []byte {
	b := make([]byte, FileAlignmentInfoSize)
	le.PutUint32(b, requirement)
	return b
}

// EncodeFileNameInfo 编码 FileNameInformation / FileAlternateNameInformation /
// FileNormalizedNameInformation（MS-FSCC §2.4.27）：
//
//	0 FileNameLength(4)
//	4 FileName(UTF-16LE)
func EncodeFileNameInfo(name string) []byte {
	nameBytes := EncodeUTF16LE(name)
	b := make([]byte, 4+len(nameBytes))
	le.PutUint32(b, uint32(len(nameBytes)))
	copy(b[4:], nameBytes)
	return b
}

// ParseFileNameInfo 解析 FileNameInformation 族。
func ParseFileNameInfo(data []byte) (string, error) {
	if err := need(data, 4); err != nil {
		return "", fmt.Errorf("FileNameInformation: %w", err)
	}
	return DecodeUTF16LEAt(data, 4, uint64(le.Uint32(data)))
}

// FileNetworkOpenInfo 是 FileNetworkOpenInformation（MS-FSCC §2.4.29），
// 固定 56 字节。macOS 与 Windows 常用它一次拿到全部基本属性。
type FileNetworkOpenInfo struct {
	CreationTime   uint64
	LastAccessTime uint64
	LastWriteTime  uint64
	ChangeTime     uint64
	AllocationSize int64
	EndOfFile      int64
	FileAttributes FileAttributes
}

// FileNetworkOpenInfoSize 是 FileNetworkOpenInformation 的字节数。
const FileNetworkOpenInfoSize = 56

// Encode 编码为 56 字节。
func (i FileNetworkOpenInfo) Encode() []byte {
	b := make([]byte, FileNetworkOpenInfoSize)
	le.PutUint64(b[0:], i.CreationTime)
	le.PutUint64(b[8:], i.LastAccessTime)
	le.PutUint64(b[16:], i.LastWriteTime)
	le.PutUint64(b[24:], i.ChangeTime)
	le.PutUint64(b[32:], uint64(i.AllocationSize))
	le.PutUint64(b[40:], uint64(i.EndOfFile))
	le.PutUint32(b[48:], uint32(i.FileAttributes))
	// b[52:56] Reserved
	return b
}

// ParseFileNetworkOpenInfo 解析 FileNetworkOpenInformation。
func ParseFileNetworkOpenInfo(data []byte) (FileNetworkOpenInfo, error) {
	var i FileNetworkOpenInfo
	if err := need(data, 52); err != nil {
		return i, fmt.Errorf("FileNetworkOpenInformation: %w", err)
	}
	i.CreationTime = le.Uint64(data[0:])
	i.LastAccessTime = le.Uint64(data[8:])
	i.LastWriteTime = le.Uint64(data[16:])
	i.ChangeTime = le.Uint64(data[24:])
	i.AllocationSize = int64(le.Uint64(data[32:]))
	i.EndOfFile = int64(le.Uint64(data[40:]))
	i.FileAttributes = FileAttributes(le.Uint32(data[48:]))
	return i, nil
}

// FileAttributeTagInfoSize 是 FileAttributeTagInformation（MS-FSCC §2.4.6）的字节数。
const FileAttributeTagInfoSize = 8

// EncodeFileAttributeTagInfo 编码 FileAttributeTagInformation。
// 非重解析点时 reparseTag 填 0。
func EncodeFileAttributeTagInfo(attrs FileAttributes, reparseTag uint32) []byte {
	b := make([]byte, FileAttributeTagInfoSize)
	le.PutUint32(b[0:], uint32(attrs))
	le.PutUint32(b[4:], reparseTag)
	return b
}

// FileIDInfoSize 是 FileIdInformation（MS-FSCC §2.4.43）的字节数。
const FileIDInfoSize = 24

// EncodeFileIDInfo 编码 FileIdInformation：VolumeSerialNumber(8) + FileId(16)。
// 只有 64 位文件号时高 8 字节填 0。
func EncodeFileIDInfo(volumeSerial uint64, fileID uint64) []byte {
	b := make([]byte, FileIDInfoSize)
	le.PutUint64(b[0:], volumeSerial)
	le.PutUint64(b[8:], fileID)
	return b
}

// FileAllInfo 是 FileAllInformation（MS-FSCC §2.4.2）。
// 它是若干子结构的拼接，固定部分 96 字节 + 文件名。
type FileAllInfo struct {
	Basic                FileBasicInfo
	Standard             FileStandardInfo
	IndexNumber          uint64
	EaSize               uint32
	AccessFlags          Access
	CurrentByteOffset    int64
	Mode                 uint32
	AlignmentRequirement uint32
	// Name 是文件名（含前导反斜杠的相对路径形式，如 `\dir\file.txt`）。
	Name string
}

// fileAllInfoFixed 是 FileAllInformation 里 FileNameInformation 之前的字节数：
// Basic(40) + Standard(24) + Internal(8) + Ea(4) + Access(4) + Position(8)
// + Mode(4) + Alignment(4) = 96。
const fileAllInfoFixed = 96

// Encode 编码 FileAllInformation。
func (i FileAllInfo) Encode() []byte {
	nameBytes := EncodeUTF16LE(i.Name)
	b := make([]byte, fileAllInfoFixed+4+len(nameBytes))
	copy(b[0:], i.Basic.Encode())
	copy(b[40:], i.Standard.Encode())
	le.PutUint64(b[64:], i.IndexNumber)
	le.PutUint32(b[72:], i.EaSize)
	le.PutUint32(b[76:], uint32(i.AccessFlags))
	le.PutUint64(b[80:], uint64(i.CurrentByteOffset))
	le.PutUint32(b[88:], i.Mode)
	le.PutUint32(b[92:], i.AlignmentRequirement)
	le.PutUint32(b[96:], uint32(len(nameBytes)))
	copy(b[100:], nameBytes)
	return b
}

// ParseFileAllInfo 解析 FileAllInformation（供测试与 Go 客户端使用）。
func ParseFileAllInfo(data []byte) (FileAllInfo, error) {
	var i FileAllInfo
	if err := need(data, fileAllInfoFixed+4); err != nil {
		return i, fmt.Errorf("FileAllInformation: %w", err)
	}
	var err error
	if i.Basic, err = ParseFileBasicInfo(data[0:40]); err != nil {
		return i, err
	}
	if i.Standard, err = ParseFileStandardInfo(data[40:64]); err != nil {
		return i, err
	}
	i.IndexNumber = le.Uint64(data[64:])
	i.EaSize = le.Uint32(data[72:])
	i.AccessFlags = Access(le.Uint32(data[76:]))
	i.CurrentByteOffset = int64(le.Uint64(data[80:]))
	i.Mode = le.Uint32(data[88:])
	i.AlignmentRequirement = le.Uint32(data[92:])
	i.Name, err = DecodeUTF16LEAt(data, fileAllInfoFixed+4, uint64(le.Uint32(data[96:])))
	if err != nil {
		return i, fmt.Errorf("FileAllInformation FileName: %w", err)
	}
	return i, nil
}

// FileStreamInfo 是一条 FileStreamInformation（MS-FSCC §2.4.44）。
// Apple 扩展与 Time Machine 会用到 alternate data stream 枚举。
type FileStreamInfo struct {
	StreamSize           int64
	StreamAllocationSize int64
	// Name 是流名，形如 `::$DATA` 或 `:AFP_Resource:$DATA`。
	Name string
}

// AppendStreamInfoChain 编码 FileStreamInformation 链（8 字节对齐，
// 最后一条 NextEntryOffset=0）。空列表返回空切片，调用方应回
// STATUS_OBJECT_NAME_NOT_FOUND。
func AppendStreamInfoChain(dst []byte, streams []FileStreamInfo) []byte {
	base := len(dst)
	for idx, s := range streams {
		start := len(dst)
		nameBytes := EncodeUTF16LE(s.Name)

		var f []byte
		dst, f = grow(dst, streamInfoFixed)
		// f[0:4] NextEntryOffset 由下一轮回填。
		le.PutUint32(f[4:], uint32(len(nameBytes)))
		le.PutUint64(f[8:], uint64(s.StreamSize))
		le.PutUint64(f[16:], uint64(s.StreamAllocationSize))
		dst = append(dst, nameBytes...)

		if idx != len(streams)-1 {
			dst = padTo8(dst, base)
			le.PutUint32(dst[start:], uint32(len(dst)-start))
		}
	}
	return dst
}

// streamInfoFixed 是 FileStreamInformation 里 StreamName 之前的字节数：
// NextEntryOffset(4) + StreamNameLength(4) + StreamSize(8) + StreamAllocationSize(8)。
const streamInfoFixed = 24

// ParseStreamInfoChain 解析 FileStreamInformation 链（供测试与 Go 客户端使用）。
func ParseStreamInfoChain(b []byte) ([]FileStreamInfo, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []FileStreamInfo
	pos := uint64(0)
	for {
		f, err := sliceAt(b, pos, streamInfoFixed)
		if err != nil {
			return nil, fmt.Errorf("FileStreamInformation[%d]: %w", len(out), err)
		}
		next := uint64(le.Uint32(f[0:]))
		s := FileStreamInfo{
			StreamSize:           int64(le.Uint64(f[8:])),
			StreamAllocationSize: int64(le.Uint64(f[16:])),
		}
		s.Name, err = DecodeUTF16LEAt(b, pos+streamInfoFixed, uint64(le.Uint32(f[4:])))
		if err != nil {
			return nil, fmt.Errorf("FileStreamInformation[%d] StreamName: %w", len(out), err)
		}
		out = append(out, s)

		if next == 0 {
			return out, nil
		}
		if next < streamInfoFixed || pos+next > uint64(len(b)) {
			return nil, fmt.Errorf("%w: FileStreamInformation[%d] NextEntryOffset=%d 非法",
				ErrMalformed, len(out)-1, next)
		}
		pos += next
	}
}

// ---------------------------------------------------------------------------
// 文件系统信息类（MS-FSCC §2.5）
// ---------------------------------------------------------------------------

// FsVolumeInfo 是 FileFsVolumeInformation（MS-FSCC §2.5.9）。
type FsVolumeInfo struct {
	VolumeCreationTime uint64
	VolumeSerialNumber uint32
	SupportsObjects    bool
	Label              string
}

// Encode 编码 FileFsVolumeInformation。
func (i FsVolumeInfo) Encode() []byte {
	label := EncodeUTF16LE(i.Label)
	b := make([]byte, 18+len(label))
	le.PutUint64(b[0:], i.VolumeCreationTime)
	le.PutUint32(b[8:], i.VolumeSerialNumber)
	le.PutUint32(b[12:], uint32(len(label)))
	b[16] = boolByte(i.SupportsObjects)
	// b[17] Reserved
	copy(b[18:], label)
	return b
}

// FsSizeInfo 是 FileFsSizeInformation（MS-FSCC §2.5.8），固定 24 字节。
type FsSizeInfo struct {
	TotalAllocationUnits     int64
	AvailableAllocationUnits int64
	SectorsPerAllocationUnit uint32
	BytesPerSector           uint32
}

// FsSizeInfoSize 是 FileFsSizeInformation 的字节数。
const FsSizeInfoSize = 24

// Encode 编码为 24 字节。
func (i FsSizeInfo) Encode() []byte {
	b := make([]byte, FsSizeInfoSize)
	le.PutUint64(b[0:], uint64(i.TotalAllocationUnits))
	le.PutUint64(b[8:], uint64(i.AvailableAllocationUnits))
	le.PutUint32(b[16:], i.SectorsPerAllocationUnit)
	le.PutUint32(b[20:], i.BytesPerSector)
	return b
}

// FsFullSizeInfo 是 FileFsFullSizeInformation（MS-FSCC §2.5.4），固定 32 字节。
// Time Machine 依赖它正确上报卷容量。
type FsFullSizeInfo struct {
	TotalAllocationUnits           int64
	CallerAvailableAllocationUnits int64
	ActualAvailableAllocationUnits int64
	SectorsPerAllocationUnit       uint32
	BytesPerSector                 uint32
}

// FsFullSizeInfoSize 是 FileFsFullSizeInformation 的字节数。
const FsFullSizeInfoSize = 32

// Encode 编码为 32 字节。
func (i FsFullSizeInfo) Encode() []byte {
	b := make([]byte, FsFullSizeInfoSize)
	le.PutUint64(b[0:], uint64(i.TotalAllocationUnits))
	le.PutUint64(b[8:], uint64(i.CallerAvailableAllocationUnits))
	le.PutUint64(b[16:], uint64(i.ActualAvailableAllocationUnits))
	le.PutUint32(b[24:], i.SectorsPerAllocationUnit)
	le.PutUint32(b[28:], i.BytesPerSector)
	return b
}

// FsDeviceInfo 是 FileFsDeviceInformation（MS-FSCC §2.5.10），固定 8 字节。
type FsDeviceInfo struct {
	DeviceType      uint32
	Characteristics uint32
}

// FsDeviceInfoSize 是 FileFsDeviceInformation 的字节数。
const FsDeviceInfoSize = 8

// Encode 编码为 8 字节。
func (i FsDeviceInfo) Encode() []byte {
	b := make([]byte, FsDeviceInfoSize)
	le.PutUint32(b[0:], i.DeviceType)
	le.PutUint32(b[4:], i.Characteristics)
	return b
}

// FsAttributeInfo 是 FileFsAttributeInformation（MS-FSCC §2.5.1）。
type FsAttributeInfo struct {
	Attributes                 uint32
	MaximumComponentNameLength int32
	FileSystemName             string
}

// Encode 编码 FileFsAttributeInformation。
func (i FsAttributeInfo) Encode() []byte {
	name := EncodeUTF16LE(i.FileSystemName)
	b := make([]byte, 12+len(name))
	le.PutUint32(b[0:], i.Attributes)
	le.PutUint32(b[4:], uint32(i.MaximumComponentNameLength))
	le.PutUint32(b[8:], uint32(len(name)))
	copy(b[12:], name)
	return b
}

// FsSectorSizeInfo 是 FileFsSectorSizeInformation（MS-FSCC §2.5.7），固定 28 字节。
type FsSectorSizeInfo struct {
	LogicalBytesPerSector                                 uint32
	PhysicalBytesPerSectorForAtomicity                    uint32
	PhysicalBytesPerSectorForPerformance                  uint32
	FileSystemEffectivePhysicalBytesPerSectorForAtomicity uint32
	Flags                                                 uint32
	ByteOffsetForSectorAlignment                          uint32
	ByteOffsetForPartitionAlignment                       uint32
}

// FsSectorSizeInfoSize 是 FileFsSectorSizeInformation 的字节数。
const FsSectorSizeInfoSize = 28

// Encode 编码为 28 字节。
func (i FsSectorSizeInfo) Encode() []byte {
	b := make([]byte, FsSectorSizeInfoSize)
	le.PutUint32(b[0:], i.LogicalBytesPerSector)
	le.PutUint32(b[4:], i.PhysicalBytesPerSectorForAtomicity)
	le.PutUint32(b[8:], i.PhysicalBytesPerSectorForPerformance)
	le.PutUint32(b[12:], i.FileSystemEffectivePhysicalBytesPerSectorForAtomicity)
	le.PutUint32(b[16:], i.Flags)
	le.PutUint32(b[20:], i.ByteOffsetForSectorAlignment)
	le.PutUint32(b[24:], i.ByteOffsetForPartitionAlignment)
	return b
}

// FsObjectIDInfoSize 是 FileFsObjectIdInformation（MS-FSCC §2.5.6）的字节数。
const FsObjectIDInfoSize = 64

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
