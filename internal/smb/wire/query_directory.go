package wire

import (
	"bytes"
	"fmt"
)

// ---------------------------------------------------------------------------
// QUERY_DIRECTORY Request（MS-SMB2 §2.2.33）
//
//	 0 StructureSize(2) = 33
//	 2 FileInformationClass(1)
//	 3 Flags(1)
//	 4 FileIndex(4)            仅 SMB2_INDEX_SPECIFIED 时有意义
//	 8 FileId(16)
//	24 FileNameOffset(2)       相对 SMB2 头起点
//	26 FileNameLength(2)
//	28 OutputBufferLength(4)
//	32 Buffer                  搜索模式（通配符），空则等价于 "*"
//
// QUERY_DIRECTORY Response（MS-SMB2 §2.2.34）
//
//	0 StructureSize(2) = 9
//	2 OutputBufferOffset(2)    相对 SMB2 头起点
//	4 OutputBufferLength(4)
//	8 Buffer                   目录项链
// ---------------------------------------------------------------------------

const (
	queryDirectoryRequestStructureSize = 33
	queryDirectoryRequestFixed         = 32

	queryDirectoryResponseStructureSize = 9
	queryDirectoryResponseFixed         = 8
)

// QueryDirectoryRequest 是 SMB2 QUERY_DIRECTORY Request（MS-SMB2 §2.2.33）。
type QueryDirectoryRequest struct {
	FileInformationClass FileInfoClass
	Flags                QueryDirectoryFlags
	FileIndex            uint32
	FileID               FileID
	// FileName 是搜索模式，可能含通配符 `*` `?` 以及 DOS 特殊字符
	// `<`(DOS_STAR) `>`(DOS_QM) `"`(DOS_DOT)。空串按 "*" 处理。
	FileName           string
	OutputBufferLength uint32
}

// Restart 报告是否要求从头重新枚举（SMB2_RESTART_SCANS / SMB2_REOPEN）。
func (r *QueryDirectoryRequest) Restart() bool {
	return r.Flags&(RestartScans|ReopenQueryDirFlag) != 0
}

// SingleEntry 报告客户端是否只要一条结果（SMB2_RETURN_SINGLE_ENTRY）。
func (r *QueryDirectoryRequest) SingleEntry() bool {
	return r.Flags&ReturnSingleEntry != 0
}

// ParseQueryDirectoryRequest 解析 QUERY_DIRECTORY Request。b 是完整消息。
func ParseQueryDirectoryRequest(b []byte) (*QueryDirectoryRequest, error) {
	body, err := msgBody(b, queryDirectoryRequestFixed, "QUERY_DIRECTORY Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, queryDirectoryRequestStructureSize); err != nil {
		return nil, fmt.Errorf("QUERY_DIRECTORY Request: %w", err)
	}
	r := &QueryDirectoryRequest{
		FileInformationClass: FileInfoClass(body[2]),
		Flags:                QueryDirectoryFlags(body[3]),
		FileIndex:            le.Uint32(body[4:]),
		FileID:               parseFileID(body[8:]),
		OutputBufferLength:   le.Uint32(body[28:]),
	}
	off := uint64(le.Uint16(body[24:]))
	length := uint64(le.Uint16(body[26:]))
	r.FileName, err = DecodeUTF16LEAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("QUERY_DIRECTORY Request FileName: %w", err)
	}
	return r, nil
}

// Append 把 QUERY_DIRECTORY Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *QueryDirectoryRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, queryDirectoryRequestFixed)
	le.PutUint16(f[0:], queryDirectoryRequestStructureSize)
	f[2] = byte(r.FileInformationClass)
	f[3] = byte(r.Flags)
	le.PutUint32(f[4:], r.FileIndex)
	r.FileID.put(f[8:])
	le.PutUint32(f[28:], r.OutputBufferLength)

	n, err := u16(UTF16LELen(r.FileName), "QUERY_DIRECTORY FileName")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[24:], HeaderSize+queryDirectoryRequestFixed)
	le.PutUint16(f[26:], n)
	if n == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
		return dst, nil
	}
	dst, _ = AppendUTF16LE(dst, r.FileName)
	return dst, nil
}

// QueryDirectoryResponse 是 SMB2 QUERY_DIRECTORY Response（MS-SMB2 §2.2.34）。
// Buffer 是编码好的目录项链（用 DirEntryWriter 生成）。
type QueryDirectoryResponse struct {
	Buffer []byte
}

// Append 把 QUERY_DIRECTORY Response 报文体追加到 dst。
func (r *QueryDirectoryResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, queryDirectoryResponseFixed)
	le.PutUint16(f[0:], queryDirectoryResponseStructureSize)
	n, err := u32(len(r.Buffer), "QUERY_DIRECTORY OutputBuffer")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[2:], HeaderSize+queryDirectoryResponseFixed)
	le.PutUint32(f[4:], n)
	if n == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
		return dst, nil
	}
	return append(dst, r.Buffer...), nil
}

// ParseQueryDirectoryResponse 解析 QUERY_DIRECTORY Response（供测试与客户端使用）。
func ParseQueryDirectoryResponse(b []byte) (*QueryDirectoryResponse, error) {
	body, err := msgBody(b, queryDirectoryResponseFixed, "QUERY_DIRECTORY Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, queryDirectoryResponseStructureSize); err != nil {
		return nil, fmt.Errorf("QUERY_DIRECTORY Response: %w", err)
	}
	off := uint64(le.Uint16(body[2:]))
	length := uint64(le.Uint32(body[4:]))
	buf, err := sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("QUERY_DIRECTORY Response OutputBuffer: %w", err)
	}
	return &QueryDirectoryResponse{Buffer: buf}, nil
}

// ---------------------------------------------------------------------------
// 目录项（MS-FSCC §2.4）
//
// 公共前缀（除 FileNamesInformation 外都有）：
//
//	 0 NextEntryOffset(4)   相对本条目起点；最后一条为 0
//	 4 FileIndex(4)
//	 8 CreationTime(8)
//	16 LastAccessTime(8)
//	24 LastWriteTime(8)
//	32 ChangeTime(8)
//	40 EndOfFile(8)
//	48 AllocationSize(8)
//	56 FileAttributes(4)
//	60 FileNameLength(4)
//
// 之后按 information class 追加不同字段，最后是 FileName（UTF-16LE，无 NUL）。
// 条目之间 **8 字节对齐**。
// ---------------------------------------------------------------------------

// 各 information class 的固定部分长度（不含 FileName）。
const (
	dirInfoFixedDirectory     = 64  // FileDirectoryInformation        (MS-FSCC §2.4.10)
	dirInfoFixedFullDirectory = 68  // FileFullDirectoryInformation    (MS-FSCC §2.4.14)
	dirInfoFixedIDFullDir     = 80  // FileIdFullDirectoryInformation  (MS-FSCC §2.4.18)
	dirInfoFixedBothDirectory = 94  // FileBothDirectoryInformation    (MS-FSCC §2.4.8)
	dirInfoFixedIDBothDir     = 104 // FileIdBothDirectoryInformation  (MS-FSCC §2.4.17)
	dirInfoFixedNames         = 12  // FileNamesInformation            (MS-FSCC §2.4.26)
)

// shortNameFieldSize 是 8.3 短名字段的固定字节数（12 个 UTF-16 码元）。
const shortNameFieldSize = 24

// DirEntry 是一条与 information class 无关的目录项数据。
// 编码时按目标 class 取用其中需要的字段。时间均为 FILETIME。
type DirEntry struct {
	FileIndex      uint32
	CreationTime   uint64
	LastAccessTime uint64
	LastWriteTime  uint64
	ChangeTime     uint64
	EndOfFile      uint64
	AllocationSize uint64
	FileAttributes FileAttributes
	EaSize         uint32
	// FileID 是 64 位稳定文件号，用于 FileId*DirectoryInformation。
	FileID uint64
	// ShortName 是 8.3 短名，可为空（现代客户端不依赖它）。
	ShortName string
	// ShortNameRaw 非 nil 时**直接覆盖** ShortName 的 24 字节字段（不做 UTF-16
	// 编码），并把 ShortNameLength 填成 len(ShortNameRaw)；此时 ShortName 被忽略。
	// 超过 24 字节返回错误。
	//
	// 用途：Apple 的 AAPL readdir_attr 把 FileIdBothDirectoryInformation 里这
	// 24 字节挪作他用（Samba vfs_fruit `readdir_attr` 的布局）：
	//
	//	[0:8]   资源派生（AFP_Resource）大小，**小端** uint64
	//	[8:24]  压缩后的 16 字节 FinderInfo
	//
	// 同时 ShortNameLength 填 24（Samba 是 `SSVAL(p, 0, 24)`，即长度字节 24、
	// 后面的 Reserved1 为 0）。
	//
	// 解析侧会填充本字段：取 ShortName 字段前 ShortNameLength 个字节的副本
	// （ShortNameLength 为 0 时留 nil），因此「解析→再编码」是无损的。
	ShortNameRaw []byte
	// Name 是文件名（不含路径）。
	Name string
}

// DirInfoFixedSize 返回该 information class 的固定部分长度。
// class 不被支持时返回 (0, false)。
func DirInfoFixedSize(class FileInfoClass) (int, bool) {
	switch class {
	case FileDirectoryInformation:
		return dirInfoFixedDirectory, true
	case FileFullDirectoryInformation:
		return dirInfoFixedFullDirectory, true
	case FileIdFullDirectoryInformation:
		return dirInfoFixedIDFullDir, true
	case FileBothDirectoryInformation:
		return dirInfoFixedBothDirectory, true
	case FileIdBothDirectoryInformation:
		return dirInfoFixedIDBothDir, true
	case FileNamesInformation:
		return dirInfoFixedNames, true
	default:
		return 0, false
	}
}

// DirEntrySize 返回该目录项按 class 编码后的字节数（未对齐）。
func DirEntrySize(class FileInfoClass, e DirEntry) (int, error) {
	fixed, ok := DirInfoFixedSize(class)
	if !ok {
		return 0, fmt.Errorf("%w: 不支持的目录信息类 %d", ErrMalformed, class)
	}
	return fixed + UTF16LELen(e.Name), nil
}

// AppendDirEntry 按 class 把一条目录项追加到 dst（不做对齐、不回填 NextEntryOffset，
// 这两件事由 DirEntryWriter 负责）。
func AppendDirEntry(dst []byte, class FileInfoClass, e DirEntry) ([]byte, error) {
	fixed, ok := DirInfoFixedSize(class)
	if !ok {
		return nil, fmt.Errorf("%w: 不支持的目录信息类 %d", ErrMalformed, class)
	}
	nameLen, err := u32(UTF16LELen(e.Name), "目录项 FileName")
	if err != nil {
		return nil, err
	}

	dst, f := grow(dst, fixed)
	// NextEntryOffset 留 0，由调用方回填。
	le.PutUint32(f[4:], e.FileIndex)

	if class == FileNamesInformation {
		// MS-FSCC §2.4.26：只有 NextEntryOffset + FileIndex + FileNameLength。
		le.PutUint32(f[8:], nameLen)
		dst, _ = AppendUTF16LE(dst, e.Name)
		return dst, nil
	}

	le.PutUint64(f[8:], e.CreationTime)
	le.PutUint64(f[16:], e.LastAccessTime)
	le.PutUint64(f[24:], e.LastWriteTime)
	le.PutUint64(f[32:], e.ChangeTime)
	le.PutUint64(f[40:], e.EndOfFile)
	le.PutUint64(f[48:], e.AllocationSize)
	le.PutUint32(f[56:], uint32(e.FileAttributes))
	le.PutUint32(f[60:], nameLen)

	switch class {
	case FileDirectoryInformation:
		// 无附加字段。
	case FileFullDirectoryInformation:
		le.PutUint32(f[64:], e.EaSize)
	case FileIdFullDirectoryInformation:
		le.PutUint32(f[64:], e.EaSize)
		// f[68:72] Reserved
		le.PutUint64(f[72:], e.FileID)
	case FileBothDirectoryInformation:
		// MS-FSCC §2.4.8：64 EaSize(4) 68 ShortNameLength(1) 69 Reserved1(1)
		//                 70 ShortName(24) → 固定部分 94 字节。
		le.PutUint32(f[64:], e.EaSize)
		if err := putShortName(f[68:], e); err != nil {
			return nil, err
		}
	case FileIdBothDirectoryInformation:
		// MS-FSCC §2.4.17：在 §2.4.8 的基础上多了
		//                 94 Reserved2(2) 96 FileId(8) → 固定部分 104 字节。
		// ⚠️ 68 后面的 Reserved1(1) 与 94 处的 Reserved2(2) 是最容易漏写/多写的两处。
		le.PutUint32(f[64:], e.EaSize)
		if err := putShortName(f[68:], e); err != nil {
			return nil, err
		}
		// f[94:96] Reserved2
		le.PutUint64(f[96:], e.FileID)
	}
	dst, _ = AppendUTF16LE(dst, e.Name)
	return dst, nil
}

// putShortName 写入 ShortNameLength(1) + Reserved1(1) + ShortName(24)。
//
// e.ShortNameRaw 非 nil 时原样写入这 24 字节（AAPL readdir_attr 会用），
// 否则把 e.ShortName 编码成 UTF-16LE；超长的短名直接截断
// （8.3 名最多 12 个 UTF-16 码元）。
func putShortName(f []byte, e DirEntry) error {
	if e.ShortNameRaw != nil {
		if len(e.ShortNameRaw) > shortNameFieldSize {
			return fmt.Errorf("%w: ShortNameRaw %d 字节超过 %d",
				ErrMalformed, len(e.ShortNameRaw), shortNameFieldSize)
		}
		f[0] = byte(len(e.ShortNameRaw))
		copy(f[2:], e.ShortNameRaw)
		return nil
	}
	if e.ShortName == "" {
		return nil
	}
	b := EncodeUTF16LE(e.ShortName)
	if len(b) > shortNameFieldSize {
		b = b[:shortNameFieldSize]
	}
	f[0] = byte(len(b))
	copy(f[2:], b)
	return nil
}

// DirEntryWriter 按 information class 生成目录项链，负责 **8 字节对齐**、
// NextEntryOffset 回填、以及 OutputBufferLength 上限控制。
//
// 用法：
//
//	w := NewDirEntryWriter(class, int(req.OutputBufferLength))
//	for _, e := range entries {
//	    if ok, err := w.Add(e); err != nil { ... } else if !ok { break } // 放不下
//	}
//	resp := &QueryDirectoryResponse{Buffer: w.Bytes()}
type DirEntryWriter struct {
	class     FileInfoClass
	max       int
	buf       []byte
	lastStart int // 最后一条目录项的起点，-1 表示还没有条目
	count     int
}

// NewDirEntryWriter 创建写入器。max 是 OutputBufferLength 上限（字节）。
func NewDirEntryWriter(class FileInfoClass, max int) *DirEntryWriter {
	return &DirEntryWriter{class: class, max: max, lastStart: -1}
}

// Add 追加一条目录项。
//
// 返回 (false, nil) 表示剩余空间不足，缓冲区保持不变，调用方应停止本轮枚举
// 并把该条目留到下一次 QUERY_DIRECTORY。
func (w *DirEntryWriter) Add(e DirEntry) (bool, error) {
	size, err := DirEntrySize(w.class, e)
	if err != nil {
		return false, err
	}
	// 新条目要放在 8 字节对齐处：先算上一条尾部需要的填充。
	start := len(w.buf)
	if w.lastStart >= 0 {
		start = align8(len(w.buf))
	}
	if start+size > w.max {
		return false, nil
	}
	if pad := start - len(w.buf); pad > 0 {
		w.buf, _ = grow(w.buf, pad)
	}
	if w.lastStart >= 0 {
		// 回填上一条的 NextEntryOffset（相对上一条起点）。
		le.PutUint32(w.buf[w.lastStart:], uint32(start-w.lastStart))
	}
	w.buf, err = AppendDirEntry(w.buf, w.class, e)
	if err != nil {
		return false, err
	}
	w.lastStart = start
	w.count++
	return true, nil
}

// Count 返回已写入的条目数。
func (w *DirEntryWriter) Count() int { return w.count }

// Len 返回当前缓冲区字节数。
func (w *DirEntryWriter) Len() int { return len(w.buf) }

// Bytes 返回目录项链。最后一条的 NextEntryOffset 已是 0，且尾部不补对齐填充。
func (w *DirEntryWriter) Bytes() []byte { return w.buf }

// ParseDirEntries 解析目录项链（供测试与 Go 客户端使用）。
func ParseDirEntries(b []byte, class FileInfoClass) ([]DirEntry, error) {
	fixed, ok := DirInfoFixedSize(class)
	if !ok {
		return nil, fmt.Errorf("%w: 不支持的目录信息类 %d", ErrMalformed, class)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var out []DirEntry
	pos := uint64(0)
	for {
		f, err := sliceAt(b, pos, uint64(fixed))
		if err != nil {
			return nil, fmt.Errorf("目录项[%d]: %w", len(out), err)
		}
		next := uint64(le.Uint32(f[0:]))
		e := DirEntry{FileIndex: le.Uint32(f[4:])}
		var nameLen uint64
		if class == FileNamesInformation {
			nameLen = uint64(le.Uint32(f[8:]))
		} else {
			e.CreationTime = le.Uint64(f[8:])
			e.LastAccessTime = le.Uint64(f[16:])
			e.LastWriteTime = le.Uint64(f[24:])
			e.ChangeTime = le.Uint64(f[32:])
			e.EndOfFile = le.Uint64(f[40:])
			e.AllocationSize = le.Uint64(f[48:])
			e.FileAttributes = FileAttributes(le.Uint32(f[56:]))
			nameLen = uint64(le.Uint32(f[60:]))
			switch class {
			case FileFullDirectoryInformation:
				e.EaSize = le.Uint32(f[64:])
			case FileIdFullDirectoryInformation:
				e.EaSize = le.Uint32(f[64:])
				e.FileID = le.Uint64(f[72:])
			case FileBothDirectoryInformation:
				e.EaSize = le.Uint32(f[64:])
				e.ShortName, _ = DecodeUTF16LE(f[70 : 70+min(int(f[68]), shortNameFieldSize)])
				e.ShortNameRaw = bytes.Clone(f[70 : 70+min(int(f[68]), shortNameFieldSize)])
			case FileIdBothDirectoryInformation:
				e.EaSize = le.Uint32(f[64:])
				e.ShortName, _ = DecodeUTF16LE(f[70 : 70+min(int(f[68]), shortNameFieldSize)])
				e.ShortNameRaw = bytes.Clone(f[70 : 70+min(int(f[68]), shortNameFieldSize)])
				e.FileID = le.Uint64(f[96:])
			}
		}
		e.Name, err = DecodeUTF16LEAt(b, pos+uint64(fixed), nameLen)
		if err != nil {
			return nil, fmt.Errorf("目录项[%d] FileName: %w", len(out), err)
		}
		out = append(out, e)

		if next == 0 {
			return out, nil
		}
		if next < uint64(fixed) || pos+next <= pos || pos+next > uint64(len(b)) {
			return nil, fmt.Errorf("%w: 目录项[%d] NextEntryOffset=%d 非法", ErrMalformed, len(out)-1, next)
		}
		pos += next
	}
}
