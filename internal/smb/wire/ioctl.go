package wire

import "fmt"

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
// 空缓冲区按 MS-SMB2 §2.2.31/§2.2.32 填 offset=0、count=0；两者都空时补
// 1 字节可变部分占位（StructureSize 比固定部分大 1）。
func appendIoctlBuffers(dst []byte, bodyStart, fixed, inFieldOff, outFieldOff int, input, output []byte) ([]byte, error) {
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
	if len(input) == 0 && len(output) == 0 {
		dst, _ = grow(dst, 1) // 1 字节可变部分占位
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
// ---------------------------------------------------------------------------

// FileAllocatedRangeBuffer 是一段已分配区间（MS-FSCC §2.3.21.1）。
type FileAllocatedRangeBuffer struct {
	FileOffset int64
	Length     int64
}

// AppendAllocatedRanges 把区间列表编码追加到 dst。
func AppendAllocatedRanges(dst []byte, ranges []FileAllocatedRangeBuffer) []byte {
	dst, b := grow(dst, len(ranges)*16)
	for i, r := range ranges {
		le.PutUint64(b[i*16:], uint64(r.FileOffset))
		le.PutUint64(b[i*16+8:], uint64(r.Length))
	}
	return dst
}

// ParseAllocatedRangesInput 解析 FSCTL_QUERY_ALLOCATED_RANGES 的输入
// （单个 FILE_ALLOCATED_RANGE_BUFFER，MS-FSCC §2.3.20）。
func ParseAllocatedRangesInput(data []byte) (FileAllocatedRangeBuffer, error) {
	var r FileAllocatedRangeBuffer
	if err := need(data, 16); err != nil {
		return r, fmt.Errorf("QUERY_ALLOCATED_RANGES Input: %w", err)
	}
	r.FileOffset = int64(le.Uint64(data[0:]))
	r.Length = int64(le.Uint64(data[8:]))
	return r, nil
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
