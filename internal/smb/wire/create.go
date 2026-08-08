package wire

import "fmt"

// ---------------------------------------------------------------------------
// FileId（MS-SMB2 §2.2.14.1 SMB2_FILEID）—— 16 字节：Persistent(8) + Volatile(8)
// ---------------------------------------------------------------------------

// FileIDSize 是 SMB2_FILEID 的字节数。
const FileIDSize = 16

// FileID 是 SMB2 句柄标识（MS-SMB2 §2.2.14.1）。
type FileID struct {
	Persistent uint64
	Volatile   uint64
}

// CompoundFileID 是全 0xFF 的特殊 FileId：复合请求中表示「复用上一条
// CREATE 返回的句柄」（MS-SMB2 §3.2.4.1.4，macOS 大量使用）。
var CompoundFileID = FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}

// IsCompound 报告本 FileId 是否为全 0xFF 的复合占位值。
func (f FileID) IsCompound() bool { return f == CompoundFileID }

// String 便于日志输出。
func (f FileID) String() string {
	return fmt.Sprintf("FileId{%#016x,%#016x}", f.Persistent, f.Volatile)
}

// parseFileID 从 b 起始处读 16 字节 FileId。调用方必须先保证 len(b) >= 16。
func parseFileID(b []byte) FileID {
	return FileID{Persistent: le.Uint64(b[0:]), Volatile: le.Uint64(b[8:])}
}

// put 把 FileId 写入 b 起始的 16 字节。调用方必须先保证 len(b) >= 16。
func (f FileID) put(b []byte) {
	le.PutUint64(b[0:], f.Persistent)
	le.PutUint64(b[8:], f.Volatile)
}

// ---------------------------------------------------------------------------
// Create Context（MS-SMB2 §2.2.13.2）
//
//	 0 Next(4)          到下一个 context 的偏移，**相对本 context 起点**；0 表示最后一个
//	 4 NameOffset(2)    相对本 context 起点
//	 6 NameLength(2)
//	 8 Reserved(2)
//	10 DataOffset(2)    相对本 context 起点
//	12 DataLength(4)
//	16 Buffer           Name 与 Data，各自 8 字节对齐
//
// 注意这里的偏移基准是 **context 自身**，与报文里其它 offset 字段不同。
// ---------------------------------------------------------------------------

// createContextHeaderSize 是 create context 的固定头长度。
const createContextHeaderSize = 16

// maxCreateContexts 是单个 CREATE 允许的 context 条数上限，防御恶意构造的
// 环形 Next 链（AGENTS.md §8 资源限制）。真实客户端最多 6~8 条。
const maxCreateContexts = 64

// CreateContext 是一条 create context（MS-SMB2 §2.2.13.2）。
// Name 通常是 4 字节 ASCII（如 "MxAc"），也可能是 16 字节 GUID 形式。
type CreateContext struct {
	Name string
	Data []byte
}

// ParseCreateContexts 解析 create context 链。b 是 CreateContexts 区间本身
// （调用方已按 CreateContextsOffset/Length 切好）。
func ParseCreateContexts(b []byte) ([]CreateContext, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []CreateContext
	pos := uint64(0)
	for i := 0; ; i++ {
		if i >= maxCreateContexts {
			return nil, fmt.Errorf("%w: create context 数量超过上限 %d", ErrMalformed, maxCreateContexts)
		}
		h, err := sliceAt(b, pos, createContextHeaderSize)
		if err != nil {
			return nil, fmt.Errorf("CreateContext[%d] 头: %w", i, err)
		}
		next := uint64(le.Uint32(h[0:]))
		nameOff := uint64(le.Uint16(h[4:]))
		nameLen := uint64(le.Uint16(h[6:]))
		dataOff := uint64(le.Uint16(h[10:]))
		dataLen := uint64(le.Uint32(h[12:]))

		// 本 context 的可用区间：有 Next 时到 Next，否则到 b 末尾。
		end := uint64(len(b))
		if next != 0 {
			if next < createContextHeaderSize || pos+next > uint64(len(b)) || pos+next < pos {
				return nil, fmt.Errorf("%w: CreateContext[%d] Next=%d 非法", ErrMalformed, i, next)
			}
			end = pos + next
		}
		self := b[pos:end]

		name, err := sliceAt(self, nameOff, nameLen)
		if err != nil {
			return nil, fmt.Errorf("CreateContext[%d] Name: %w", i, err)
		}
		data, err := sliceAt(self, dataOff, dataLen)
		if err != nil {
			return nil, fmt.Errorf("CreateContext[%d] Data: %w", i, err)
		}
		out = append(out, CreateContext{Name: string(name), Data: data})

		if next == 0 {
			return out, nil
		}
		pos += next
	}
}

// AppendCreateContexts 把 context 链编码追加到 dst，返回新切片。
//
// 每个 context 起点、其 Name 与 Data 都按 8 字节对齐（对齐基准是 dst 中的
// blobStart，调用方须保证 blobStart 相对 SMB2 头起点也是 8 的倍数）。
func AppendCreateContexts(dst []byte, ctxs []CreateContext) ([]byte, error) {
	blobStart := len(dst)
	for i, c := range ctxs {
		ctxStart := len(dst)
		var h []byte
		dst, h = grow(dst, createContextHeaderSize)

		nameLen, err := u16(len(c.Name), "CreateContext.Name")
		if err != nil {
			return nil, err
		}
		dataLen, err := u32(len(c.Data), "CreateContext.Data")
		if err != nil {
			return nil, err
		}
		// Name 紧跟固定头（16 已是 8 的倍数，无需额外填充）。
		le.PutUint16(h[4:], createContextHeaderSize)
		le.PutUint16(h[6:], nameLen)
		dst = append(dst, c.Name...)

		if dataLen > 0 {
			dst = padTo8(dst, ctxStart) // Data 相对本 context 起点 8 字节对齐
			off, err := u16(len(dst)-ctxStart, "CreateContext.DataOffset")
			if err != nil {
				return nil, err
			}
			// h 可能因 append 扩容而失效，改用 dst 下标回填。
			le.PutUint16(dst[ctxStart+10:], off)
			le.PutUint32(dst[ctxStart+12:], dataLen)
			dst = append(dst, c.Data...)
		}

		if i != len(ctxs)-1 {
			dst = padTo8(dst, blobStart)
			next, err := u32(len(dst)-ctxStart, "CreateContext.Next")
			if err != nil {
				return nil, err
			}
			le.PutUint32(dst[ctxStart:], next)
		}
	}
	return dst, nil
}

// FindCreateContext 在链中查找指定名字的 context，返回其 Data。
// 名字比较是**大小写敏感**的 4 字节 ASCII 精确匹配。
func FindCreateContext(ctxs []CreateContext, name string) ([]byte, bool) {
	for _, c := range ctxs {
		if c.Name == name {
			return c.Data, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// CREATE Request（MS-SMB2 §2.2.13）
//
//	 0 StructureSize(2) = 57
//	 2 SecurityFlags(1)        保留，必须为 0
//	 3 RequestedOplockLevel(1)
//	 4 ImpersonationLevel(4)
//	 8 SmbCreateFlags(8)       保留
//	16 Reserved(8)
//	24 DesiredAccess(4)
//	28 FileAttributes(4)
//	32 ShareAccess(4)
//	36 CreateDisposition(4)
//	40 CreateOptions(4)
//	44 NameOffset(2)           相对 SMB2 头起点
//	46 NameLength(2)
//	48 CreateContextsOffset(4) 相对 SMB2 头起点
//	52 CreateContextsLength(4)
//	56 Buffer
// ---------------------------------------------------------------------------

const (
	createRequestStructureSize = 57
	createRequestFixed         = 56

	createResponseStructureSize = 89
	createResponseFixed         = 88
)

// CreateRequest 是 SMB2 CREATE Request（MS-SMB2 §2.2.13）。
type CreateRequest struct {
	RequestedOplockLevel OplockLevel
	ImpersonationLevel   ImpersonationLevel
	DesiredAccess        Access
	FileAttributes       FileAttributes
	ShareAccess          ShareAccess
	CreateDisposition    CreateDisposition
	CreateOptions        CreateOptions
	// Name 是**相对共享根的路径，无前导反斜杠**，根目录为空串
	// （protocol-notes §8）。分隔符是反斜杠。
	Name     string
	Contexts []CreateContext
}

// ParseCreateRequest 解析 CREATE Request。b 是完整消息（含 64 字节头）。
func ParseCreateRequest(b []byte) (*CreateRequest, error) {
	body, err := msgBody(b, createRequestFixed, "CREATE Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, createRequestStructureSize); err != nil {
		return nil, fmt.Errorf("CREATE Request: %w", err)
	}
	r := &CreateRequest{
		RequestedOplockLevel: OplockLevel(body[3]),
		ImpersonationLevel:   ImpersonationLevel(le.Uint32(body[4:])),
		DesiredAccess:        Access(le.Uint32(body[24:])),
		FileAttributes:       FileAttributes(le.Uint32(body[28:])),
		ShareAccess:          ShareAccess(le.Uint32(body[32:])),
		CreateDisposition:    CreateDisposition(le.Uint32(body[36:])),
		CreateOptions:        CreateOptions(le.Uint32(body[40:])),
	}
	nameOff := uint64(le.Uint16(body[44:]))
	nameLen := uint64(le.Uint16(body[46:]))
	ctxOff := uint64(le.Uint32(body[48:]))
	ctxLen := uint64(le.Uint32(body[52:]))

	r.Name, err = DecodeUTF16LEAt(b, nameOff, nameLen)
	if err != nil {
		return nil, fmt.Errorf("CREATE Request Name: %w", err)
	}
	if ctxLen > 0 {
		blob, err := sliceAt(b, ctxOff, ctxLen)
		if err != nil {
			return nil, fmt.Errorf("CREATE Request CreateContexts: %w", err)
		}
		r.Contexts, err = ParseCreateContexts(blob)
		if err != nil {
			return nil, fmt.Errorf("CREATE Request: %w", err)
		}
	}
	return r, nil
}

// Append 把 CREATE Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *CreateRequest) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dst, f := grow(dst, createRequestFixed)
	le.PutUint16(f[0:], createRequestStructureSize)
	f[3] = byte(r.RequestedOplockLevel)
	le.PutUint32(f[4:], uint32(r.ImpersonationLevel))
	le.PutUint32(f[24:], uint32(r.DesiredAccess))
	le.PutUint32(f[28:], uint32(r.FileAttributes))
	le.PutUint32(f[32:], uint32(r.ShareAccess))
	le.PutUint32(f[36:], uint32(r.CreateDisposition))
	le.PutUint32(f[40:], uint32(r.CreateOptions))

	// Name：紧跟固定部分。NameLength 为 0 时（根目录）NameOffset 仍填结构末尾。
	nameLen, err := u16(UTF16LELen(r.Name), "CREATE Name")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[44:], HeaderSize+createRequestFixed)
	le.PutUint16(f[46:], nameLen)
	dst, _ = AppendUTF16LE(dst, r.Name)

	if len(r.Contexts) > 0 {
		dst = padTo8(dst, bodyStart)
		off, err := u32(HeaderSize+len(dst)-bodyStart, "CreateContextsOffset")
		if err != nil {
			return nil, err
		}
		blobStart := len(dst)
		dst, err = AppendCreateContexts(dst, r.Contexts)
		if err != nil {
			return nil, err
		}
		n, err := u32(len(dst)-blobStart, "CreateContextsLength")
		if err != nil {
			return nil, err
		}
		le.PutUint32(dst[bodyStart+48:], off)
		le.PutUint32(dst[bodyStart+52:], n)
	} else if nameLen == 0 {
		// 文件名与 context 都为空（打开共享根目录就是这种情况）时补 1 字节占位，
		// 使体长等于 StructureSize 57。真实客户端确实这么发：
		// testdata/capture/*/019-c2s-CREATE.bin（smbclient）与
		// testdata/capture/gosmb2/009-c2s-CREATE.bin（go-smb2）都是 57 字节体。
		dst, _ = grow(dst, 1)
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// CREATE Response（MS-SMB2 §2.2.14）
//
//	 0 StructureSize(2) = 89
//	 2 OplockLevel(1)
//	 3 Flags(1)                3.x：SMB2_CREATE_FLAG_REPARSEPOINT
//	 4 CreateAction(4)
//	 8 CreationTime(8)
//	16 LastAccessTime(8)
//	24 LastWriteTime(8)
//	32 ChangeTime(8)
//	40 AllocationSize(8)
//	48 EndofFile(8)
//	56 FileAttributes(4)
//	60 Reserved2(4)
//	64 FileId(16)
//	80 CreateContextsOffset(4) 相对 SMB2 头起点
//	84 CreateContextsLength(4)
//	88 Buffer
// ---------------------------------------------------------------------------

// CreateResponseFlagReparsePoint 是 CREATE Response 的 Flags 位
// （MS-SMB2 §2.2.14，仅 3.x）。
const CreateResponseFlagReparsePoint uint8 = 0x01

// CreateResponse 是 SMB2 CREATE Response（MS-SMB2 §2.2.14）。
// 各时间字段是 FILETIME。
type CreateResponse struct {
	OplockLevel    OplockLevel
	Flags          uint8
	CreateAction   CreateAction
	CreationTime   uint64
	LastAccessTime uint64
	LastWriteTime  uint64
	ChangeTime     uint64
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes FileAttributes
	FileID         FileID
	Contexts       []CreateContext
}

// Append 把 CREATE Response 报文体追加到 dst。
func (r *CreateResponse) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dst, f := grow(dst, createResponseFixed)
	le.PutUint16(f[0:], createResponseStructureSize)
	f[2] = byte(r.OplockLevel)
	f[3] = r.Flags
	le.PutUint32(f[4:], uint32(r.CreateAction))
	le.PutUint64(f[8:], r.CreationTime)
	le.PutUint64(f[16:], r.LastAccessTime)
	le.PutUint64(f[24:], r.LastWriteTime)
	le.PutUint64(f[32:], r.ChangeTime)
	le.PutUint64(f[40:], r.AllocationSize)
	le.PutUint64(f[48:], r.EndOfFile)
	le.PutUint32(f[56:], uint32(r.FileAttributes))
	r.FileID.put(f[64:])

	if len(r.Contexts) > 0 {
		// 固定部分 88 字节，加上 64 字节头是 152，已是 8 的倍数；
		// 仍显式对齐一次以防将来结构变化。
		dst = padTo8(dst, bodyStart)
		off, err := u32(HeaderSize+len(dst)-bodyStart, "CreateContextsOffset")
		if err != nil {
			return nil, err
		}
		blobStart := len(dst)
		dst, err = AppendCreateContexts(dst, r.Contexts)
		if err != nil {
			return nil, err
		}
		n, err := u32(len(dst)-blobStart, "CreateContextsLength")
		if err != nil {
			return nil, err
		}
		le.PutUint32(dst[bodyStart+80:], off)
		le.PutUint32(dst[bodyStart+84:], n)
	}
	// 无 context 时**不补**占位字节：体就是 88 字节，比 StructureSize(89) 少 1。
	// 这不是笔误 —— MS-SMB2 只在 §2.2.2 ERROR Response 里明文要求
	// 「ByteCount 为 0 时 ErrorData 仍占 1 字节」，其余响应没有这条要求。
	// 真实 Samba 4.22 的 CREATE Response 就是 152 字节（64 头 + 88 体），
	// 见 testdata/capture/*/0??-s2c-CREATE.bin。既然 Windows 客户端天天连 Samba，
	// 这个长度是被大规模验证过的（AGENTS.md §9：以真实实现行为为准）。
	return dst, nil
}

// ParseCreateResponse 解析 CREATE Response（供测试与 Go 客户端使用）。
func ParseCreateResponse(b []byte) (*CreateResponse, error) {
	body, err := msgBody(b, createResponseFixed, "CREATE Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, createResponseStructureSize); err != nil {
		return nil, fmt.Errorf("CREATE Response: %w", err)
	}
	r := &CreateResponse{
		OplockLevel:    OplockLevel(body[2]),
		Flags:          body[3],
		CreateAction:   CreateAction(le.Uint32(body[4:])),
		CreationTime:   le.Uint64(body[8:]),
		LastAccessTime: le.Uint64(body[16:]),
		LastWriteTime:  le.Uint64(body[24:]),
		ChangeTime:     le.Uint64(body[32:]),
		AllocationSize: le.Uint64(body[40:]),
		EndOfFile:      le.Uint64(body[48:]),
		FileAttributes: FileAttributes(le.Uint32(body[56:])),
		FileID:         parseFileID(body[64:]),
	}
	ctxOff := uint64(le.Uint32(body[80:]))
	ctxLen := uint64(le.Uint32(body[84:]))
	if ctxLen > 0 {
		blob, err := sliceAt(b, ctxOff, ctxLen)
		if err != nil {
			return nil, fmt.Errorf("CREATE Response CreateContexts: %w", err)
		}
		r.Contexts, err = ParseCreateContexts(blob)
		if err != nil {
			return nil, fmt.Errorf("CREATE Response: %w", err)
		}
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 常用 create context 载荷
// ---------------------------------------------------------------------------

// MaximalAccessContext 是 "MxAc" 响应载荷（MS-SMB2 §2.2.14.2.5）：
// QueryStatus(4) + MaximalAccess(4)。
type MaximalAccessContext struct {
	QueryStatus   uint32
	MaximalAccess uint32
}

// Encode 编码 MxAc 响应载荷。
func (m MaximalAccessContext) Encode() []byte {
	out := make([]byte, 8)
	le.PutUint32(out[0:], m.QueryStatus)
	le.PutUint32(out[4:], m.MaximalAccess)
	return out
}

// ParseMaximalAccessContext 解析 MxAc 载荷（响应形态；请求形态可能为空或 8 字节 Timestamp）。
func ParseMaximalAccessContext(data []byte) (MaximalAccessContext, error) {
	var m MaximalAccessContext
	if err := need(data, 8); err != nil {
		return m, fmt.Errorf("MxAc: %w", err)
	}
	m.QueryStatus = le.Uint32(data[0:])
	m.MaximalAccess = le.Uint32(data[4:])
	return m, nil
}

// DiskIDContext 是 "QFid" 响应载荷（MS-SMB2 §2.2.14.2.9）：
// DiskFileId(8) + VolumeId(8) + Reserved(16)，共 32 字节。
type DiskIDContext struct {
	DiskFileID uint64
	VolumeID   uint64
}

// Encode 编码 QFid 响应载荷。
func (d DiskIDContext) Encode() []byte {
	out := make([]byte, 32)
	le.PutUint64(out[0:], d.DiskFileID)
	le.PutUint64(out[8:], d.VolumeID)
	return out
}

// AllocationSizeContext 解析 "AlSi" 载荷（MS-SMB2 §2.2.13.2.2）：AllocationSize(8)。
func AllocationSizeContext(data []byte) (uint64, error) {
	if err := need(data, 8); err != nil {
		return 0, fmt.Errorf("AlSi: %w", err)
	}
	return le.Uint64(data), nil
}

// TimewarpContext 解析 "TWrp" 载荷（MS-SMB2 §2.2.13.2.7）：Timestamp(8) FILETIME。
func TimewarpContext(data []byte) (uint64, error) {
	if err := need(data, 8); err != nil {
		return 0, fmt.Errorf("TWrp: %w", err)
	}
	return le.Uint64(data), nil
}
