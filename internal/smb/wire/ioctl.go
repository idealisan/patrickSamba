package wire

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// IOCTL Request（MS-SMB2 §2.2.31）
//
//	 0 StructureSize(2) = 57
//	 2 Reserved(2)
//	 4 CtlCode(4)
//	 8 FileId(16)
//	24 InputOffset(4)          相对 SMB2 头起点
//	28 InputCount(4)
//	32 MaxInputResponse(4)
//	36 OutputOffset(4)         相对 SMB2 头起点
//	40 OutputCount(4)          请求里通常为 0
//	44 MaxOutputResponse(4)
//	48 Flags(4)
//	52 Reserved2(4)
//	56 Buffer
//
// IOCTL Response（MS-SMB2 §2.2.32）
//
//	 0 StructureSize(2) = 49
//	 2 Reserved(2)
//	 4 CtlCode(4)
//	 8 FileId(16)
//	24 InputOffset(4)
//	28 InputCount(4)
//	32 OutputOffset(4)
//	36 OutputCount(4)
//	40 Flags(4)
//	44 Reserved2(4)
//	48 Buffer
// ---------------------------------------------------------------------------

const (
	ioctlRequestStructureSize = 57
	ioctlRequestFixed         = 56

	ioctlResponseStructureSize = 49
	ioctlResponseFixed         = 48
)

// CtlCode 是 FSCTL/IOCTL 控制码（MS-SMB2 §2.2.31）。
type CtlCode uint32

// 本项目会遇到的控制码。取值见 MS-SMB2 §2.2.31 与 MS-FSCC §2.3。
const (
	// FSCTLDFSGetReferrals 是 DFS 转介查询。不做 DFS 时回
	// STATUS_NOT_FOUND，且 NEGOTIATE 不要声明 CAP_DFS
	// （见 docs/protocol-notes.md §7）。
	FSCTLDFSGetReferrals CtlCode = 0x00060194

	// FSCTLPipeTransceive 用于命名管道（srvsvc 等）上的 DCERPC 往返。
	FSCTLPipeTransceive CtlCode = 0x0011C017
	FSCTLPipePeek       CtlCode = 0x0011400C
	FSCTLPipeWait       CtlCode = 0x00110018

	// FSCTLValidateNegotiateInfo ⚠️ SMB 3.0/3.0.2 + 签名时 Windows 会在
	// TREE_CONNECT 之后立刻发送，**响应错误会导致客户端立即 TCP RESET**。
	// 要么按 §2.2.31.4 正确回显，要么回 STATUS_NOT_SUPPORTED（客户端会容忍）。
	FSCTLValidateNegotiateInfo CtlCode = 0x00140204

	// FSCTLGetReparsePoint / FSCTLSetReparsePoint 用于符号链接与挂载点。
	FSCTLGetReparsePoint CtlCode = 0x000900A8
	FSCTLSetReparsePoint CtlCode = 0x000900A4

	// FSCTLSetSparse / FSCTLSetZeroData / FSCTLQueryAllocatedRanges
	// 是稀疏文件相关，Time Machine 的 .sparsebundle 会用到。
	FSCTLSetSparse            CtlCode = 0x000900C4
	FSCTLSetZeroData          CtlCode = 0x000980C8
	FSCTLQueryAllocatedRanges CtlCode = 0x000940CF

	// FSCTLDFSGetReferralsEx / FSCTLFileLevelTrim / FSCTLLmrRequestResiliency
	// 目前不实现，列出来是为了识别后回 STATUS_NOT_SUPPORTED。
	FSCTLDFSGetReferralsEx     CtlCode = 0x000601B0
	FSCTLFileLevelTrim         CtlCode = 0x00098208
	FSCTLLmrRequestResiliency  CtlCode = 0x001401D4
	FSCTLQueryNetworkInterface CtlCode = 0x001401FC
	FSCTLSrvCopychunk          CtlCode = 0x001440F2
	FSCTLSrvCopychunkWrite     CtlCode = 0x001480F2
	FSCTLSrvRequestResumeKey   CtlCode = 0x00140078
	FSCTLSrvEnumerateSnapshots CtlCode = 0x00144064
	FSCTLSrvReadHash           CtlCode = 0x001441bb
)

var ctlCodeNames = map[CtlCode]string{
	FSCTLDFSGetReferrals:       "FSCTL_DFS_GET_REFERRALS",
	FSCTLPipeTransceive:        "FSCTL_PIPE_TRANSCEIVE",
	FSCTLPipePeek:              "FSCTL_PIPE_PEEK",
	FSCTLPipeWait:              "FSCTL_PIPE_WAIT",
	FSCTLValidateNegotiateInfo: "FSCTL_VALIDATE_NEGOTIATE_INFO",
	FSCTLGetReparsePoint:       "FSCTL_GET_REPARSE_POINT",
	FSCTLSetReparsePoint:       "FSCTL_SET_REPARSE_POINT",
	FSCTLSetSparse:             "FSCTL_SET_SPARSE",
	FSCTLSetZeroData:           "FSCTL_SET_ZERO_DATA",
	FSCTLQueryAllocatedRanges:  "FSCTL_QUERY_ALLOCATED_RANGES",
	FSCTLDFSGetReferralsEx:     "FSCTL_DFS_GET_REFERRALS_EX",
	FSCTLFileLevelTrim:         "FSCTL_FILE_LEVEL_TRIM",
	FSCTLLmrRequestResiliency:  "FSCTL_LMR_REQUEST_RESILIENCY",
	FSCTLQueryNetworkInterface: "FSCTL_QUERY_NETWORK_INTERFACE_INFO",
	FSCTLSrvCopychunk:          "FSCTL_SRV_COPYCHUNK",
	FSCTLSrvCopychunkWrite:     "FSCTL_SRV_COPYCHUNK_WRITE",
	FSCTLSrvRequestResumeKey:   "FSCTL_SRV_REQUEST_RESUME_KEY",
	FSCTLSrvEnumerateSnapshots: "FSCTL_SRV_ENUMERATE_SNAPSHOTS",
	FSCTLSrvReadHash:           "FSCTL_SRV_READ_HASH",
}

func (c CtlCode) String() string {
	if s, ok := ctlCodeNames[c]; ok {
		return s
	}
	return fmt.Sprintf("FSCTL(%#08x)", uint32(c))
}

// IoctlFlags 是 IOCTL Request/Response 的 Flags 字段（MS-SMB2 §2.2.31）。
type IoctlFlags uint32

// SMB2_0_IOCTL_IS_FSCTL：置位表示这是文件系统控制码而非设备 IOCTL。
const IoctlIsFSCTL IoctlFlags = 0x00000001

// IoctlRequest 是 SMB2 IOCTL Request（MS-SMB2 §2.2.31）。
type IoctlRequest struct {
	CtlCode CtlCode
	// FileID 对 FSCTL_DFS_GET_REFERRALS / FSCTL_VALIDATE_NEGOTIATE_INFO
	// 等无句柄的控制码是全 0xFF（见 §2.2.31）。
	FileID            FileID
	MaxInputResponse  uint32
	MaxOutputResponse uint32
	Flags             IoctlFlags
	Input             []byte
	// Output 在请求里几乎总是空的，保留字段以便完整往返。
	Output []byte
}

// IsFSCTL 报告 SMB2_0_IOCTL_IS_FSCTL 是否置位。
func (r *IoctlRequest) IsFSCTL() bool { return r.Flags&IoctlIsFSCTL != 0 }

// ParseIoctlRequest 解析 IOCTL Request。b 是完整消息（含 64 字节头）。
func ParseIoctlRequest(b []byte) (*IoctlRequest, error) {
	body, err := msgBody(b, ioctlRequestFixed, "IOCTL Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, ioctlRequestStructureSize); err != nil {
		return nil, fmt.Errorf("IOCTL Request: %w", err)
	}
	r := &IoctlRequest{
		CtlCode:           CtlCode(le.Uint32(body[4:])),
		FileID:            parseFileID(body[8:]),
		MaxInputResponse:  le.Uint32(body[32:]),
		MaxOutputResponse: le.Uint32(body[44:]),
		Flags:             IoctlFlags(le.Uint32(body[48:])),
	}
	// InputOffset / OutputOffset 相对 SMB2 头起点，直接在完整消息上取。
	r.Input, err = sliceAt(b, uint64(le.Uint32(body[24:])), uint64(le.Uint32(body[28:])))
	if err != nil {
		return nil, fmt.Errorf("IOCTL Request Input: %w", err)
	}
	r.Output, err = sliceAt(b, uint64(le.Uint32(body[36:])), uint64(le.Uint32(body[40:])))
	if err != nil {
		return nil, fmt.Errorf("IOCTL Request Output: %w", err)
	}
	return r, nil
}

// Append 把 IOCTL Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *IoctlRequest) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dst, f := grow(dst, ioctlRequestFixed)
	le.PutUint16(f[0:], ioctlRequestStructureSize)
	le.PutUint32(f[4:], uint32(r.CtlCode))
	r.FileID.put(f[8:])
	le.PutUint32(f[32:], r.MaxInputResponse)
	le.PutUint32(f[44:], r.MaxOutputResponse)
	le.PutUint32(f[48:], uint32(r.Flags))

	dst, err := appendIoctlBuffers(dst, bodyStart, ioctlRequestFixed, 24, 36, r.Input, r.Output)
	if err != nil {
		return nil, fmt.Errorf("IOCTL Request: %w", err)
	}
	return dst, nil
}

// IoctlResponse 是 SMB2 IOCTL Response（MS-SMB2 §2.2.32）。
type IoctlResponse struct {
	CtlCode CtlCode
	FileID  FileID
	Flags   IoctlFlags
	Input   []byte
	Output  []byte
}

// Append 把 IOCTL Response 报文体追加到 dst。
func (r *IoctlResponse) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dst, f := grow(dst, ioctlResponseFixed)
	le.PutUint16(f[0:], ioctlResponseStructureSize)
	le.PutUint32(f[4:], uint32(r.CtlCode))
	r.FileID.put(f[8:])
	le.PutUint32(f[40:], uint32(r.Flags))

	dst, err := appendIoctlBuffers(dst, bodyStart, ioctlResponseFixed, 24, 32, r.Input, r.Output)
	if err != nil {
		return nil, fmt.Errorf("IOCTL Response: %w", err)
	}
	return dst, nil
}

// ParseIoctlResponse 解析 IOCTL Response（供测试与 Go 客户端使用）。
func ParseIoctlResponse(b []byte) (*IoctlResponse, error) {
	body, err := msgBody(b, ioctlResponseFixed, "IOCTL Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, ioctlResponseStructureSize); err != nil {
		return nil, fmt.Errorf("IOCTL Response: %w", err)
	}
	r := &IoctlResponse{
		CtlCode: CtlCode(le.Uint32(body[4:])),
		FileID:  parseFileID(body[8:]),
		Flags:   IoctlFlags(le.Uint32(body[40:])),
	}
	r.Input, err = sliceAt(b, uint64(le.Uint32(body[24:])), uint64(le.Uint32(body[28:])))
	if err != nil {
		return nil, fmt.Errorf("IOCTL Response Input: %w", err)
	}
	r.Output, err = sliceAt(b, uint64(le.Uint32(body[32:])), uint64(le.Uint32(body[36:])))
	if err != nil {
		return nil, fmt.Errorf("IOCTL Response Output: %w", err)
	}
	return r, nil
}

// appendIoctlBuffers 追加 Input/Output 并回填两组 offset/count。
// inFieldOff / outFieldOff 是固定部分内 offset 字段的下标（count 紧随其后 4 字节）。
//
// 关于「空缓冲区的 offset 填什么」，MS-SMB2 §2.2.31/§2.2.32 的字面意思像是填 0，
// 但真实 Samba 4.22 的行为分两种情况（抓包实测，见 testdata/capture/）：
//
//	至少有一个缓冲区非空 → 两个 offset 都指向**可变部分起点**（64 + 固定部分长度），
//	                       空的那个 count 填 0 但 offset 照样非 0。
//	    negotiate-smb202/009-c2s-IOCTL.bin：InputCount=26 InputOffset=0x78，
//	                                        OutputCount=0  OutputOffset=0x78
//	    negotiate-smb202/010-s2c-IOCTL.bin：InputCount=0  InputOffset=0x70，
//	                                        OutputCount=24 OutputOffset=0x70
//
//	两个都空           → 两个 offset 都填 0，并补 1 字节占位（体长 = StructureSize）。
//	    query-info/035-c2s-IOCTL.bin：体 57 字节，两个 offset 都是 0
//
// 按 AGENTS.md §9「以真实实现行为为准」跟随 Samba。解析侧对各种写法都兼容
// （count 为 0 时根本不会去取字节）。
func appendIoctlBuffers(dst []byte, bodyStart, fixed, inFieldOff, outFieldOff int, input, output []byte) ([]byte, error) {
	if len(input) == 0 && len(output) == 0 {
		// offset/count 保持 0，补 1 字节占位。
		dst, _ = grow(dst, 1)
		return dst, nil
	}

	// bufStart 是可变部分起点（相对本消息 SMB2 头），空的那一个填它。
	bufStart, err := u32(HeaderSize+fixed, "IOCTL BufferOffset")
	if err != nil {
		return nil, err
	}
	le.PutUint32(dst[bodyStart+inFieldOff:], bufStart)
	le.PutUint32(dst[bodyStart+outFieldOff:], bufStart)

	if len(input) > 0 {
		n, err := u32(len(input), "IOCTL InputCount")
		if err != nil {
			return nil, err
		}
		off, err := u32(HeaderSize+len(dst)-bodyStart, "IOCTL InputOffset")
		if err != nil {
			return nil, err
		}
		le.PutUint32(dst[bodyStart+inFieldOff:], off)
		le.PutUint32(dst[bodyStart+inFieldOff+4:], n)
		dst = append(dst, input...)
	}
	if len(output) > 0 {
		// Output 紧跟 Input，两者之间按 8 字节对齐（Windows 如此，客户端也容忍不对齐）。
		dst = padTo8(dst, bodyStart)
		n, err := u32(len(output), "IOCTL OutputCount")
		if err != nil {
			return nil, err
		}
		off, err := u32(HeaderSize+len(dst)-bodyStart, "IOCTL OutputOffset")
		if err != nil {
			return nil, err
		}
		le.PutUint32(dst[bodyStart+outFieldOff:], off)
		le.PutUint32(dst[bodyStart+outFieldOff+4:], n)
		dst = append(dst, output...)
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// FSCTL_VALIDATE_NEGOTIATE_INFO（MS-SMB2 §2.2.31.4 / §2.2.32.6）
//
// 请求与响应结构不同：请求带方言列表，响应只回单个协商结果。
// 服务端必须逐字段与 NEGOTIATE 阶段的值比对，不一致要断连（§3.3.5.15.12）。
// ---------------------------------------------------------------------------

// ValidateNegotiateInfoRequest 是 FSCTL_VALIDATE_NEGOTIATE_INFO 的输入
// （MS-SMB2 §2.2.31.4）。
//
//	 0 Capabilities(4)
//	 4 Guid(16)
//	20 SecurityMode(2)
//	22 DialectCount(2)
//	24 Dialects(2*n)
type ValidateNegotiateInfoRequest struct {
	Capabilities Capabilities
	ClientGUID   [16]byte
	SecurityMode SecurityMode
	Dialects     []Dialect
}

const validateNegotiateInfoRequestFixed = 24

// ParseValidateNegotiateInfoRequest 解析 IOCTL 的 Input 缓冲区。
func ParseValidateNegotiateInfoRequest(data []byte) (*ValidateNegotiateInfoRequest, error) {
	if err := need(data, validateNegotiateInfoRequestFixed); err != nil {
		return nil, fmt.Errorf("VALIDATE_NEGOTIATE_INFO Request: %w", err)
	}
	r := &ValidateNegotiateInfoRequest{
		Capabilities: Capabilities(le.Uint32(data[0:])),
		SecurityMode: SecurityMode(le.Uint16(data[20:])),
	}
	copy(r.ClientGUID[:], data[4:20])
	n := int(le.Uint16(data[22:]))
	body, err := sliceAt(data, validateNegotiateInfoRequestFixed, uint64(n)*2)
	if err != nil {
		return nil, fmt.Errorf("VALIDATE_NEGOTIATE_INFO Dialects: %w", err)
	}
	r.Dialects = make([]Dialect, n)
	for i := range r.Dialects {
		r.Dialects[i] = Dialect(le.Uint16(body[i*2:]))
	}
	return r, nil
}

// Encode 编码 FSCTL_VALIDATE_NEGOTIATE_INFO 的输入（供测试与 Go 客户端使用）。
func (r *ValidateNegotiateInfoRequest) Encode() ([]byte, error) {
	n, err := u16(len(r.Dialects), "VALIDATE_NEGOTIATE_INFO DialectCount")
	if err != nil {
		return nil, err
	}
	b := make([]byte, validateNegotiateInfoRequestFixed+len(r.Dialects)*2)
	le.PutUint32(b[0:], uint32(r.Capabilities))
	copy(b[4:], r.ClientGUID[:])
	le.PutUint16(b[20:], uint16(r.SecurityMode))
	le.PutUint16(b[22:], n)
	for i, d := range r.Dialects {
		le.PutUint16(b[validateNegotiateInfoRequestFixed+i*2:], uint16(d))
	}
	return b, nil
}

// ValidateNegotiateInfoResponse 是 FSCTL_VALIDATE_NEGOTIATE_INFO 的输出
// （MS-SMB2 §2.2.32.6），固定 24 字节。
//
//	 0 Capabilities(4)
//	 4 Guid(16)
//	20 SecurityMode(2)
//	22 Dialect(2)
//
// 这些值必须是**服务端在 NEGOTIATE 响应里发过的原值**，否则客户端断连。
type ValidateNegotiateInfoResponse struct {
	Capabilities Capabilities
	ServerGUID   [16]byte
	SecurityMode SecurityMode
	Dialect      Dialect
}

// ValidateNegotiateInfoResponseSize 是 §2.2.32.6 的固定长度。
const ValidateNegotiateInfoResponseSize = 24

// Encode 编码 FSCTL_VALIDATE_NEGOTIATE_INFO 的输出。
func (r *ValidateNegotiateInfoResponse) Encode() []byte {
	b := make([]byte, ValidateNegotiateInfoResponseSize)
	le.PutUint32(b[0:], uint32(r.Capabilities))
	copy(b[4:], r.ServerGUID[:])
	le.PutUint16(b[20:], uint16(r.SecurityMode))
	le.PutUint16(b[22:], uint16(r.Dialect))
	return b
}

// ParseValidateNegotiateInfoResponse 解析输出（供测试与 Go 客户端使用）。
func ParseValidateNegotiateInfoResponse(data []byte) (*ValidateNegotiateInfoResponse, error) {
	if err := need(data, ValidateNegotiateInfoResponseSize); err != nil {
		return nil, fmt.Errorf("VALIDATE_NEGOTIATE_INFO Response: %w", err)
	}
	r := &ValidateNegotiateInfoResponse{
		Capabilities: Capabilities(le.Uint32(data[0:])),
		SecurityMode: SecurityMode(le.Uint16(data[20:])),
		Dialect:      Dialect(le.Uint16(data[22:])),
	}
	copy(r.ServerGUID[:], data[4:20])
	return r, nil
}

// ---------------------------------------------------------------------------
// FSCTL_QUERY_ALLOCATED_RANGES（MS-FSCC §2.3.20 / §2.3.21）
// 稀疏文件用；Time Machine 的 .sparsebundle 会查询。
//
// FILE_ALLOCATED_RANGE_BUFFER（§2.3.20 请求 / §2.3.21 响应元素）固定 16 字节，
// 全部**小端**：
//
//	0 FileOffset(8)  INT64，必须 >= 0
//	8 Length(8)      INT64，必须 >= 0
//
// 请求侧是**单个**结构（描述客户端关心的查询窗口），
// 响应侧是**数组**（0..n 个区间，按 FileOffset 递增，互不重叠）。
// 响应为空数组是合法的：表示该窗口内没有任何已分配数据（全是洞）。
// ---------------------------------------------------------------------------

// FileAllocatedRangeBufferSize 是 FILE_ALLOCATED_RANGE_BUFFER 的固定长度
// （MS-FSCC §2.3.20）。响应缓冲区大小 = 区间数 × 该值。
const FileAllocatedRangeBufferSize = 16

// FileAllocatedRangeBuffer 是一段已分配区间（MS-FSCC §2.3.20 / §2.3.21）。
type FileAllocatedRangeBuffer struct {
	FileOffset int64
	Length     int64
}

// AppendAllocatedRanges 把区间列表编码追加到 dst
// （FSCTL_QUERY_ALLOCATED_RANGES 的输出，MS-FSCC §2.3.21）。
// ranges 为空时不追加任何字节 —— 这是"整段都是洞"的合法应答。
func AppendAllocatedRanges(dst []byte, ranges []FileAllocatedRangeBuffer) []byte {
	dst, b := grow(dst, len(ranges)*FileAllocatedRangeBufferSize)
	for i, r := range ranges {
		le.PutUint64(b[i*FileAllocatedRangeBufferSize:], uint64(r.FileOffset))
		le.PutUint64(b[i*FileAllocatedRangeBufferSize+8:], uint64(r.Length))
	}
	return dst
}

// ParseAllocatedRangesInput 解析 FSCTL_QUERY_ALLOCATED_RANGES 的输入
// （单个 FILE_ALLOCATED_RANGE_BUFFER，MS-FSCC §2.3.20）。
//
// 按 §2.3.20 处理规则，FileOffset 与 Length 都必须是非负 INT64，
// 且相加不得溢出，否则回 STATUS_INVALID_PARAMETER。
func ParseAllocatedRangesInput(data []byte) (FileAllocatedRangeBuffer, error) {
	var r FileAllocatedRangeBuffer
	if err := need(data, FileAllocatedRangeBufferSize); err != nil {
		return r, fmt.Errorf("QUERY_ALLOCATED_RANGES Input: %w", err)
	}
	r.FileOffset = int64(le.Uint64(data[0:]))
	r.Length = int64(le.Uint64(data[8:]))
	if r.FileOffset < 0 || r.Length < 0 || r.FileOffset+r.Length < r.FileOffset {
		return r, fmt.Errorf("%w: QUERY_ALLOCATED_RANGES 区间非法 offset=%d length=%d",
			ErrMalformed, r.FileOffset, r.Length)
	}
	return r, nil
}

// ParseAllocatedRanges 解析 FSCTL_QUERY_ALLOCATED_RANGES 的输出数组
// （MS-FSCC §2.3.21，供测试与 Go 客户端使用）。
//
// 长度必须是 16 的整数倍；空输入返回 (nil, nil)。
func ParseAllocatedRanges(data []byte) ([]FileAllocatedRangeBuffer, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data)%FileAllocatedRangeBufferSize != 0 {
		return nil, fmt.Errorf("%w: QUERY_ALLOCATED_RANGES 输出 %d 字节不是 %d 的整数倍",
			ErrMalformed, len(data), FileAllocatedRangeBufferSize)
	}
	out := make([]FileAllocatedRangeBuffer, len(data)/FileAllocatedRangeBufferSize)
	for i := range out {
		f := data[i*FileAllocatedRangeBufferSize:]
		out[i] = FileAllocatedRangeBuffer{
			FileOffset: int64(le.Uint64(f[0:])),
			Length:     int64(le.Uint64(f[8:])),
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// FSCTL_SET_SPARSE（MS-FSCC §2.3.69）
//
// FILE_SET_SPARSE_BUFFER：
//
//	0 SetSparse(1)  BOOLEAN，0 = 清除稀疏标记，非 0 = 置稀疏标记
//
// ⚠️ 关键行为（§2.3.69）：**输入缓冲区为空时视为 TRUE**。
// macOS 与 Windows 都会发不带输入的 FSCTL_SET_SPARSE 来把文件标为稀疏，
// 若按"缺省 false"处理，Time Machine 的 .sparsebundle band 文件就不会稀疏化。
// ---------------------------------------------------------------------------

// FileSetSparseBufferSize 是 FILE_SET_SPARSE_BUFFER 的长度（MS-FSCC §2.3.69）。
const FileSetSparseBufferSize = 1

// ParseSetSparseInput 解析 FSCTL_SET_SPARSE 的输入缓冲区。
//
// 空输入返回 true（见上方注释）。多余字节按 §2.3.69 忽略：
// 只取第 1 字节，非 0 即为 TRUE。
func ParseSetSparseInput(data []byte) (bool, error) {
	if len(data) == 0 {
		return true, nil
	}
	return data[0] != 0, nil
}

// EncodeSetSparseInput 编码 FILE_SET_SPARSE_BUFFER（供测试与 Go 客户端使用）。
func EncodeSetSparseInput(setSparse bool) []byte {
	b := make([]byte, FileSetSparseBufferSize)
	if setSparse {
		b[0] = 1
	}
	return b
}

// ---------------------------------------------------------------------------
// FSCTL_SRV_ENUMERATE_SNAPSHOTS / SRV_SNAPSHOT_ARRAY
// （MS-SMB2 §2.2.32.2，处理规则 §3.3.5.15.1）
//
// 全部**小端**：
//
//	 0 NumberOfSnapShots(4)          该卷上的快照总数
//	 4 NumberOfSnapShotsReturned(4)  本次返回的条数；装不下时为 0
//	 8 SnapShotArraySize(4)          SnapShots 数组字节数；
//	                                 装不下时为"全部装下所需字节数"
//	12 SnapShots(变长)               UTF-16LE 的 @GMT 标签，
//	                                 以 UNICODE NUL 分隔、两个 UNICODE NUL 结尾
//
// ── 关于"零快照时 SnapShots 到底几个字节"（这一点规范文本两可，已查实现定案）──
//
// 1. MS-SMB2 附录 A 注 <68>（§2.2.32.2）：
//    "Windows-based SMB2 server will place 2 extra bytes set to zero in the
//     SRV_SNAPSHOT_ARRAY response, if NumberOfSnapShotsReturned is zero."
//    → Windows 服务端在 Returned=0 时补 **2** 个零字节（总长 14）。
//
// 2. Samba `vfswrap_fsctl` / FSCTL_GET_SHADOW_COPY_DATA
//    （source3/modules/vfs_default.c，master 分支实测阅读）：
//        labels_data_count = num_volumes * 2 * sizeof(SHADOW_COPY_LABEL) + 2;
//        if (!labels) *out_len = 16; else *out_len = 12 + labels_data_count;
//    → SnapShotArraySize **总是** n*50 + 2，零快照即 2，与注 <68> 一致；
//      而 MaxOutputResponse == 16 时输出**补齐到 16 字节**。
//
// 3. 但 Samba **客户端**（smbclient 走的
//    source3/libsmb/cli_smb2_fnum.c:cli_smb2_shadow_copy_data_fnum_recv）
//    硬性要求 `out_output_buffer.length >= 16`，否则回
//    NT_STATUS_INVALID_NETWORK_RESPONSE。smbclient 的 `allinfo` 用
//    get_names=true（MaxOutputResponse = CLI_BUFFER_SIZE = 64K），
//    所以只回 14 字节会被它判为坏响应 —— 这是 Samba 自身服务端/客户端的不一致。
//
// 结论（按 AGENTS.md §9「以真实客户端行为为准」）：
//   **Encode 输出最少 16 字节**（12 头 + 4 个零字节），
//   SnapShotArraySize 仍按 n*50+2 上报（零快照 = 2）。
//   16 字节是 Windows 14 字节形式的超集，smbclient 与 Linux cifs 都能接受。
//
// 每个标签占**固定 50 字节槽位**：@GMT-YYYY.MM.DD-HH.MM.SS 共 24 字符
// （48 字节）+ 1 个 UTF-16 NUL。Samba 客户端就是按 50 字节定长步进解析的
// （`src = data + 12 + i * 2 * sizeof(SHADOW_COPY_LABEL)`），
// 与规范「NUL 分隔」的说法在 @GMT 定长格式下完全等价。
// ---------------------------------------------------------------------------

const (
	// SrvSnapshotArrayHeaderSize 是 SRV_SNAPSHOT_ARRAY 的固定头长度。
	SrvSnapshotArrayHeaderSize = 12

	// SrvSnapshotArrayMinSize 是本实现输出的最小长度，见上方第 3 点。
	// 也正是 §3.3.5.15.1 里 MaxOutputResponse 的下限。
	SrvSnapshotArrayMinSize = 16

	// GMTTokenLen 是 @GMT-YYYY.MM.DD-HH.MM.SS 的字符数（MS-SMB2 §2.2.32.2）。
	GMTTokenLen = 24

	// SnapshotLabelSize 是每个标签在 SnapShots 数组里占的字节数：
	// 24 个字符 + 1 个 UTF-16 NUL，共 50 字节。
	SnapshotLabelSize = (GMTTokenLen + 1) * 2

	// snapshotArrayTerminator 是数组末尾额外的那个 UTF-16 NUL。
	snapshotArrayTerminator = 2
)

// SrvSnapshotArraySize 返回装下 n 个 @GMT 标签所需的 SnapShots 数组字节数
// （即 SnapShotArraySize 字段应填的值）。n = 0 时为 2，与 Windows 注 <68>
// 和 Samba 的 labels_data_count 一致。
func SrvSnapshotArraySize(n int) uint32 {
	return uint32(n)*SnapshotLabelSize + snapshotArrayTerminator
}

// SrvSnapshotArray 是 FSCTL_SRV_ENUMERATE_SNAPSHOTS 的输出（MS-SMB2 §2.2.32.2）。
//
// NumberOfSnapShotsReturned 不单独存字段，它恒等于 len(SnapShots)。
type SrvSnapshotArray struct {
	// NumberOfSnapShots 是卷上的快照总数（可以大于 len(SnapShots)）。
	NumberOfSnapShots uint32
	// SnapShotArraySize 是「装下全部标签所需的字节数」，用 SrvSnapshotArraySize 算。
	SnapShotArraySize uint32
	// SnapShots 是本次真正返回的 @GMT 标签。为空表示只回计数（装不下或没有快照）。
	SnapShots []string
}

// NewSrvSnapshotArray 按 MS-SMB2 §3.3.5.15.1 组装应答。
//
// all 是 Share.SnapshotList 里的全部 @GMT 标签，maxOutput 是请求的
// MaxOutputResponse（调用方需先按 §3.3.5.15.1 校验它 >= 16，
// 否则应回 STATUS_INVALID_PARAMETER，本函数不做这个判断）。
//
// 没有快照、或全部标签装不进 maxOutput 时：Returned = 0、SnapShots 为空，
// 但 SnapShotArraySize 仍是「全部装下所需的字节数」，好让客户端加大缓冲重试。
//
// 本项目不做卷影副本，命令层直接 NewSrvSnapshotArray(nil, maxOutput) 即可，
// 回「0 个快照」而不是 STATUS_INVALID_DEVICE_REQUEST。
func NewSrvSnapshotArray(all []string, maxOutput uint32) SrvSnapshotArray {
	a := SrvSnapshotArray{
		NumberOfSnapShots: uint32(len(all)),
		SnapShotArraySize: SrvSnapshotArraySize(len(all)),
	}
	need := uint64(SrvSnapshotArrayHeaderSize) + uint64(a.SnapShotArraySize)
	if len(all) > 0 && need <= uint64(maxOutput) {
		a.SnapShots = all
	}
	return a
}

// Encode 编码 SRV_SNAPSHOT_ARRAY。
//
// 长度 = 12 + len(SnapShots)*50 + 2，且**不小于 16**（见类型上方注释第 3 点）。
// 每个标签写进自己的 50 字节槽位，槽位剩余字节保持 0，天然构成 NUL 结尾；
// 数组末尾那 2 个零字节也由 make 的零值提供。
//
// 超过 24 个字符的标签会被截到 24 字符（槽位的 NUL 位不可侵占）。
// 合法的 @GMT 标签恒为 24 字符，只有调用方传了非法值才会触发。
func (a *SrvSnapshotArray) Encode() []byte {
	size := SrvSnapshotArrayHeaderSize + len(a.SnapShots)*SnapshotLabelSize
	if len(a.SnapShots) > 0 {
		size += snapshotArrayTerminator
	}
	if size < SrvSnapshotArrayMinSize {
		size = SrvSnapshotArrayMinSize
	}
	b := make([]byte, size)
	le.PutUint32(b[0:], a.NumberOfSnapShots)
	le.PutUint32(b[4:], uint32(len(a.SnapShots)))
	le.PutUint32(b[8:], a.SnapShotArraySize)

	off := SrvSnapshotArrayHeaderSize
	for _, s := range a.SnapShots {
		label := EncodeUTF16LE(s)
		if len(label) > SnapshotLabelSize-2 {
			label = label[:SnapshotLabelSize-2]
		}
		copy(b[off:], label)
		off += SnapshotLabelSize
	}
	return b
}

// ParseSrvSnapshotArray 解析 SRV_SNAPSHOT_ARRAY（供测试与 Go 客户端使用）。
//
// 校验与 Samba 客户端（cli_smb2_shadow_copy_data_fnum_recv）一致：
// 总长至少 16 字节，且 Returned 条标签必须真的在缓冲区内。
func ParseSrvSnapshotArray(data []byte) (*SrvSnapshotArray, error) {
	if err := need(data, SrvSnapshotArrayMinSize); err != nil {
		return nil, fmt.Errorf("SRV_SNAPSHOT_ARRAY: %w", err)
	}
	a := &SrvSnapshotArray{
		NumberOfSnapShots: le.Uint32(data[0:]),
		SnapShotArraySize: le.Uint32(data[8:]),
	}
	returned := le.Uint32(data[4:])
	if returned == 0 {
		return a, nil
	}
	body, err := sliceAt(data, SrvSnapshotArrayHeaderSize, uint64(returned)*SnapshotLabelSize)
	if err != nil {
		return nil, fmt.Errorf("SRV_SNAPSHOT_ARRAY SnapShots: %w", err)
	}
	a.SnapShots = make([]string, returned)
	for i := range a.SnapShots {
		slot := body[i*SnapshotLabelSize : (i+1)*SnapshotLabelSize]
		s, err := DecodeUTF16LE(slot)
		if err != nil {
			return nil, fmt.Errorf("SRV_SNAPSHOT_ARRAY SnapShots[%d]: %w", i, err)
		}
		// 槽位用 NUL 补齐，取到第一个 NUL 为止。
		if k := strings.IndexByte(s, 0); k >= 0 {
			s = s[:k]
		}
		a.SnapShots[i] = s
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// FSCTL_SET_ZERO_DATA（MS-FSCC §2.3.79）
// ---------------------------------------------------------------------------

// FileZeroDataInformation 是打洞区间 [FileOffset, BeyondFinalZero)。
type FileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

// ParseZeroDataInput 解析 FSCTL_SET_ZERO_DATA 的输入。
func ParseZeroDataInput(data []byte) (FileZeroDataInformation, error) {
	var z FileZeroDataInformation
	if err := need(data, 16); err != nil {
		return z, fmt.Errorf("SET_ZERO_DATA Input: %w", err)
	}
	z.FileOffset = int64(le.Uint64(data[0:]))
	z.BeyondFinalZero = int64(le.Uint64(data[8:]))
	if z.FileOffset < 0 || z.BeyondFinalZero < z.FileOffset {
		return z, fmt.Errorf("%w: SET_ZERO_DATA 区间非法 [%d,%d)", ErrMalformed, z.FileOffset, z.BeyondFinalZero)
	}
	return z, nil
}
