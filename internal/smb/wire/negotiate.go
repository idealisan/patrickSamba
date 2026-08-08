package wire

import "fmt"

// ---------------------------------------------------------------------------
// 偏移基准约定（整个 wire 包统一）
//
// SMB2 里所有 *Offset 字段都是**相对该消息 SMB2 头起点**的偏移
// （MS-SMB2 §2.2.3 NegotiateContextOffset、§2.2.4 SecurityBufferOffset 等）。
// 因此：
//
//   - Parse*  的入参 b 是**完整消息**（b[0:64] 是 SMB2 头，b[64:] 是报文体）。
//     这样 offset 可以直接当作 b 的下标使用，无需调用方换算。
//   - Append* 只追加**报文体**，写入 offset 字段时用 HeaderSize + 体内偏移
//     （报文体永远紧跟在自己的头之后，复合链也是如此）。
// ---------------------------------------------------------------------------

// msgBody 返回完整消息 b 的报文体（跳过 64 字节头），并校验体至少有 fixed 字节。
func msgBody(b []byte, fixed int, what string) ([]byte, error) {
	if err := need(b, HeaderSize+fixed); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return b[HeaderSize:], nil
}

// NegotiateContextType 是 SMB 3.1.1 negotiate context 的类型（MS-SMB2 §2.2.3.1）。
type NegotiateContextType uint16

// MS-SMB2 §2.2.3.1 — SMB2 NEGOTIATE_CONTEXT ContextType
const (
	ContextPreauthIntegrityCapabilities NegotiateContextType = 0x0001 // SMB2_PREAUTH_INTEGRITY_CAPABILITIES
	ContextEncryptionCapabilities       NegotiateContextType = 0x0002 // SMB2_ENCRYPTION_CAPABILITIES
	ContextCompressionCapabilities      NegotiateContextType = 0x0003 // SMB2_COMPRESSION_CAPABILITIES
	ContextNetnameNegotiateContextID    NegotiateContextType = 0x0005 // SMB2_NETNAME_NEGOTIATE_CONTEXT_ID
	ContextTransportCapabilities        NegotiateContextType = 0x0006 // SMB2_TRANSPORT_CAPABILITIES
	ContextRDMATransformCapabilities    NegotiateContextType = 0x0007 // SMB2_RDMA_TRANSFORM_CAPABILITIES
	ContextSigningCapabilities          NegotiateContextType = 0x0008 // SMB2_SIGNING_CAPABILITIES
)

// MS-SMB2 §2.2.3.1.1 — HashAlgorithms，目前规范只定义了一个。
const HashAlgorithmSHA512 uint16 = 0x0001 // SHA-512

// MS-SMB2 §2.2.3.1.2 — Ciphers
const (
	CipherAES128CCM uint16 = 0x0001 // AES-128-CCM
	CipherAES128GCM uint16 = 0x0002 // AES-128-GCM
	CipherAES256CCM uint16 = 0x0003 // AES-256-CCM
	CipherAES256GCM uint16 = 0x0004 // AES-256-GCM
)

// MS-SMB2 §2.2.3.1.7 — SigningAlgorithms
const (
	SigningAlgorithmHMACSHA256 uint16 = 0x0000 // HMAC-SHA256
	SigningAlgorithmAESCMAC    uint16 = 0x0001 // AES-CMAC
	SigningAlgorithmAESGMAC    uint16 = 0x0002 // AES-GMAC
)

// MS-SMB2 §2.2.3.1.3 — CompressionAlgorithms
const (
	CompressionNone       uint16 = 0x0000
	CompressionLZNT1      uint16 = 0x0001
	CompressionLZ77       uint16 = 0x0002
	CompressionLZ77Huff   uint16 = 0x0003
	CompressionPatternV1  uint16 = 0x0004
	CompressionLZ4        uint16 = 0x0005
	CompressionChainedFlg uint32 = 0x00000001 // SMB2_COMPRESSION_CAPABILITIES_FLAG_CHAINED
)

// negotiateContextHeaderSize 是 SMB2_NEGOTIATE_CONTEXT 固定头长度
// （ContextType(2) + DataLength(2) + Reserved(4)，MS-SMB2 §2.2.3.1）。
const negotiateContextHeaderSize = 8

// NegotiateContext 是一条未解释的 negotiate context（MS-SMB2 §2.2.3.1）。
// Data 是 context 的裸载荷，具体结构用 Parse*Context 系列函数解释。
type NegotiateContext struct {
	Type NegotiateContextType
	Data []byte
}

// parseNegotiateContexts 从完整消息 b 的 off 处开始解析 count 条 context。
//
// off 相对消息起点（= SMB2 头起点）。context 之间 **8 字节对齐**，
// 这是 3.1.1 协商最容易踩的坑（MS-SMB2 §2.2.3.1）。
func parseNegotiateContexts(b []byte, off uint32, count uint16) ([]NegotiateContext, error) {
	if count == 0 {
		return nil, nil
	}
	pos := uint64(off)
	out := make([]NegotiateContext, 0, count)
	for i := 0; i < int(count); i++ {
		pos = uint64(align8(int(pos))) // 每条 context 起点 8 字节对齐
		hdr, err := sliceAt(b, pos, negotiateContextHeaderSize)
		if err != nil {
			return nil, fmt.Errorf("NegotiateContext[%d] 头: %w", i, err)
		}
		typ := NegotiateContextType(le.Uint16(hdr[0:]))
		dataLen := uint64(le.Uint16(hdr[2:]))
		// hdr[4:8] Reserved，规范要求忽略。
		data, err := sliceAt(b, pos+negotiateContextHeaderSize, dataLen)
		if err != nil {
			return nil, fmt.Errorf("NegotiateContext[%d] 数据: %w", i, err)
		}
		out = append(out, NegotiateContext{Type: typ, Data: data})
		pos += negotiateContextHeaderSize + dataLen
	}
	return out, nil
}

// appendNegotiateContexts 把 context 链追加到 dst。
//
// bodyStart 是本消息报文体在 dst 中的起点（用于计算 8 字节对齐）。
// 由于 SMB2 头固定 64 字节（8 的倍数），相对体起点对齐与相对头起点对齐等价。
// context 之间填充对齐，**最后一条之后不填充**。
func appendNegotiateContexts(dst []byte, bodyStart int, ctxs []NegotiateContext) ([]byte, error) {
	for i, c := range ctxs {
		if i > 0 {
			dst = padTo8(dst, bodyStart)
		}
		n, err := u16(len(c.Data), "NegotiateContext.Data")
		if err != nil {
			return nil, err
		}
		var h []byte
		dst, h = grow(dst, negotiateContextHeaderSize)
		le.PutUint16(h[0:], uint16(c.Type))
		le.PutUint16(h[2:], n)
		// h[4:8] Reserved = 0
		dst = append(dst, c.Data...)
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// NEGOTIATE Request（MS-SMB2 §2.2.3）
// ---------------------------------------------------------------------------

// negotiateRequestStructureSize 是 NEGOTIATE Request 的 StructureSize
// （MS-SMB2 §2.2.3，固定部分 36 字节，含 1 字节可变部分占位）。
const negotiateRequestStructureSize = 36

// NegotiateRequest 是 SMB2 NEGOTIATE Request（MS-SMB2 §2.2.3）。
type NegotiateRequest struct {
	SecurityMode SecurityMode
	Capabilities Capabilities
	ClientGUID   [16]byte
	// ClientStartTime 与 (NegotiateContextOffset/Count) 是 union：
	// 只有 Dialects 含 0x0311 时后者才有效，否则本字段为 ClientStartTime
	// （MS-SMB2 §2.2.3，实际客户端一律填 0）。
	ClientStartTime uint64
	Dialects        []Dialect
	Contexts        []NegotiateContext
}

// HasDialect 报告请求中是否包含方言 d。
func (r *NegotiateRequest) HasDialect(d Dialect) bool {
	for _, x := range r.Dialects {
		if x == d {
			return true
		}
	}
	return false
}

// ParseNegotiateRequest 解析 NEGOTIATE Request。b 是完整消息（含 64 字节头）。
func ParseNegotiateRequest(b []byte) (*NegotiateRequest, error) {
	body, err := msgBody(b, negotiateRequestStructureSize, "NEGOTIATE Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, negotiateRequestStructureSize); err != nil {
		return nil, fmt.Errorf("NEGOTIATE Request: %w", err)
	}
	// —— 字段偏移（MS-SMB2 §2.2.3）——
	//   0  StructureSize(2)=36
	//   2  DialectCount(2)
	//   4  SecurityMode(2)
	//   6  Reserved(2)
	//   8  Capabilities(4)
	//  12  ClientGuid(16)
	//  28  NegotiateContextOffset(4) + NegotiateContextCount(2) + Reserved2(2)
	//      或 ClientStartTime(8)
	//  36  Dialects(2 * DialectCount)
	r := &NegotiateRequest{
		SecurityMode: SecurityMode(le.Uint16(body[4:])),
		Capabilities: Capabilities(le.Uint32(body[8:])),
	}
	dc := int(le.Uint16(body[2:]))
	copy(r.ClientGUID[:], body[12:28])
	ctxOffset := le.Uint32(body[28:])
	ctxCount := le.Uint16(body[32:])
	r.ClientStartTime = le.Uint64(body[28:])

	// Dialects 数组：位于体内偏移 36，即消息内偏移 64+36。
	da, err := sliceAt(b, HeaderSize+negotiateRequestStructureSize, uint64(dc)*2)
	if err != nil {
		return nil, fmt.Errorf("NEGOTIATE Request Dialects: %w", err)
	}
	r.Dialects = make([]Dialect, dc)
	for i := range r.Dialects {
		r.Dialects[i] = Dialect(le.Uint16(da[i*2:]))
	}

	// negotiate context 只有在协商 3.1.1 时才存在。为了对不规范客户端宽容，
	// 只要 offset/count 都非 0 就尝试解析。
	if r.HasDialect(SMB311) && ctxOffset != 0 && ctxCount != 0 {
		r.ClientStartTime = 0
		r.Contexts, err = parseNegotiateContexts(b, ctxOffset, ctxCount)
		if err != nil {
			return nil, fmt.Errorf("NEGOTIATE Request: %w", err)
		}
	}
	return r, nil
}

// Append 把 NEGOTIATE Request 报文体追加到 dst（主要供测试与 Go 客户端使用）。
func (r *NegotiateRequest) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dc, err := u16(len(r.Dialects), "NEGOTIATE Dialects")
	if err != nil {
		return nil, err
	}
	dst, f := grow(dst, negotiateRequestStructureSize)
	le.PutUint16(f[0:], negotiateRequestStructureSize)
	le.PutUint16(f[2:], dc)
	le.PutUint16(f[4:], uint16(r.SecurityMode))
	le.PutUint32(f[8:], uint32(r.Capabilities))
	copy(f[12:], r.ClientGUID[:])

	for _, d := range r.Dialects {
		var db []byte
		dst, db = grow(dst, 2)
		le.PutUint16(db, uint16(d))
	}

	if len(r.Contexts) > 0 {
		dst = padTo8(dst, bodyStart)
		off, err := u32(HeaderSize+len(dst)-bodyStart, "NegotiateContextOffset")
		if err != nil {
			return nil, err
		}
		cc, err := u16(len(r.Contexts), "NegotiateContextCount")
		if err != nil {
			return nil, err
		}
		le.PutUint32(dst[bodyStart+28:], off)
		le.PutUint16(dst[bodyStart+32:], cc)
		dst, err = appendNegotiateContexts(dst, bodyStart, r.Contexts)
		if err != nil {
			return nil, err
		}
	} else {
		le.PutUint64(dst[bodyStart+28:], r.ClientStartTime)
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// NEGOTIATE Response（MS-SMB2 §2.2.4）
// ---------------------------------------------------------------------------

// negotiateResponseStructureSize 是 NEGOTIATE Response 的 StructureSize
// （MS-SMB2 §2.2.4：固定部分 64 字节 + 1 字节可变部分占位 = 65）。
const negotiateResponseStructureSize = 65

// negotiateResponseFixed 是响应固定部分的实际字节数。
const negotiateResponseFixed = 64

// NegotiateResponse 是 SMB2 NEGOTIATE Response（MS-SMB2 §2.2.4）。
//
// SystemTime / ServerStartTime 是 FILETIME（1601-01-01 起的 100ns 数）。
type NegotiateResponse struct {
	SecurityMode    SecurityMode
	DialectRevision Dialect
	ServerGUID      [16]byte
	Capabilities    Capabilities
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32
	SystemTime      uint64
	ServerStartTime uint64
	// SecurityBuffer 通常是 SPNEGO negTokenInit2。
	SecurityBuffer []byte
	// Contexts 仅在 DialectRevision == SMB311 时编码。
	Contexts []NegotiateContext
}

// Append 把 NEGOTIATE Response 报文体追加到 dst。
//
// dst 应当已经以本消息的 SMB2 头结尾；offset 字段按「报文体紧跟 64 字节头」
// 计算（见文件头的偏移基准约定）。
func (r *NegotiateResponse) Append(dst []byte) ([]byte, error) {
	bodyStart := len(dst)
	dst, f := grow(dst, negotiateResponseFixed)
	le.PutUint16(f[0:], negotiateResponseStructureSize)
	le.PutUint16(f[2:], uint16(r.SecurityMode))
	le.PutUint16(f[4:], uint16(r.DialectRevision))
	// f[6:8] NegotiateContextCount（3.1.1）/ Reserved，下面回填。
	copy(f[8:], r.ServerGUID[:])
	le.PutUint32(f[24:], uint32(r.Capabilities))
	le.PutUint32(f[28:], r.MaxTransactSize)
	le.PutUint32(f[32:], r.MaxReadSize)
	le.PutUint32(f[36:], r.MaxWriteSize)
	le.PutUint64(f[40:], r.SystemTime)
	le.PutUint64(f[48:], r.ServerStartTime)
	// f[56:58] SecurityBufferOffset、f[58:60] SecurityBufferLength、
	// f[60:64] NegotiateContextOffset / Reserved2，下面回填。

	if len(r.SecurityBuffer) > 0 {
		n, err := u16(len(r.SecurityBuffer), "SecurityBuffer")
		if err != nil {
			return nil, err
		}
		off, err := u16(HeaderSize+len(dst)-bodyStart, "SecurityBufferOffset")
		if err != nil {
			return nil, err
		}
		le.PutUint16(dst[bodyStart+56:], off)
		le.PutUint16(dst[bodyStart+58:], n)
		dst = append(dst, r.SecurityBuffer...)
	} else {
		// 无 SPNEGO token 时按规范填结构末尾偏移、长度 0。
		le.PutUint16(dst[bodyStart+56:], HeaderSize+negotiateResponseFixed)
	}

	if len(r.Contexts) > 0 {
		dst = padTo8(dst, bodyStart)
		off, err := u32(HeaderSize+len(dst)-bodyStart, "NegotiateContextOffset")
		if err != nil {
			return nil, err
		}
		cc, err := u16(len(r.Contexts), "NegotiateContextCount")
		if err != nil {
			return nil, err
		}
		le.PutUint16(dst[bodyStart+6:], cc)
		le.PutUint32(dst[bodyStart+60:], off)
		dst, err = appendNegotiateContexts(dst, bodyStart, r.Contexts)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// ParseNegotiateResponse 解析 NEGOTIATE Response（供测试与 Go 客户端使用）。
// b 是完整消息（含 64 字节头）。
func ParseNegotiateResponse(b []byte) (*NegotiateResponse, error) {
	body, err := msgBody(b, negotiateResponseFixed, "NEGOTIATE Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, negotiateResponseStructureSize); err != nil {
		return nil, fmt.Errorf("NEGOTIATE Response: %w", err)
	}
	r := &NegotiateResponse{
		SecurityMode:    SecurityMode(le.Uint16(body[2:])),
		DialectRevision: Dialect(le.Uint16(body[4:])),
		Capabilities:    Capabilities(le.Uint32(body[24:])),
		MaxTransactSize: le.Uint32(body[28:]),
		MaxReadSize:     le.Uint32(body[32:]),
		MaxWriteSize:    le.Uint32(body[36:]),
		SystemTime:      le.Uint64(body[40:]),
		ServerStartTime: le.Uint64(body[48:]),
	}
	copy(r.ServerGUID[:], body[8:24])
	ctxCount := le.Uint16(body[6:])
	secOff := uint64(le.Uint16(body[56:]))
	secLen := uint64(le.Uint16(body[58:]))
	ctxOff := le.Uint32(body[60:])

	r.SecurityBuffer, err = sliceAt(b, secOff, secLen)
	if err != nil {
		return nil, fmt.Errorf("NEGOTIATE Response SecurityBuffer: %w", err)
	}
	if r.DialectRevision == SMB311 && ctxOff != 0 && ctxCount != 0 {
		r.Contexts, err = parseNegotiateContexts(b, ctxOff, ctxCount)
		if err != nil {
			return nil, fmt.Errorf("NEGOTIATE Response: %w", err)
		}
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 各 negotiate context 的载荷结构
// ---------------------------------------------------------------------------

// PreauthIntegrityCapabilities 是 SMB2_PREAUTH_INTEGRITY_CAPABILITIES 的载荷
// （MS-SMB2 §2.2.3.1.1）：
//
//	HashAlgorithmCount(2) + SaltLength(2) + HashAlgorithms(2*N) + Salt
type PreauthIntegrityCapabilities struct {
	HashAlgorithms []uint16
	Salt           []byte
}

// ParsePreauthIntegrityCapabilities 解析 context 载荷。
func ParsePreauthIntegrityCapabilities(data []byte) (*PreauthIntegrityCapabilities, error) {
	if err := need(data, 4); err != nil {
		return nil, fmt.Errorf("PREAUTH_INTEGRITY_CAPABILITIES: %w", err)
	}
	n := uint64(le.Uint16(data[0:]))
	saltLen := uint64(le.Uint16(data[2:]))
	algs, err := sliceAt(data, 4, n*2)
	if err != nil {
		return nil, fmt.Errorf("PREAUTH HashAlgorithms: %w", err)
	}
	salt, err := sliceAt(data, 4+n*2, saltLen)
	if err != nil {
		return nil, fmt.Errorf("PREAUTH Salt: %w", err)
	}
	p := &PreauthIntegrityCapabilities{
		HashAlgorithms: make([]uint16, n),
		Salt:           salt,
	}
	for i := range p.HashAlgorithms {
		p.HashAlgorithms[i] = le.Uint16(algs[i*2:])
	}
	return p, nil
}

// Encode 把载荷编码为 context 的 Data。
func (p *PreauthIntegrityCapabilities) Encode() ([]byte, error) {
	n, err := u16(len(p.HashAlgorithms), "HashAlgorithmCount")
	if err != nil {
		return nil, err
	}
	sl, err := u16(len(p.Salt), "SaltLength")
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+len(p.HashAlgorithms)*2, 4+len(p.HashAlgorithms)*2+len(p.Salt))
	le.PutUint16(out[0:], n)
	le.PutUint16(out[2:], sl)
	for i, a := range p.HashAlgorithms {
		le.PutUint16(out[4+i*2:], a)
	}
	return append(out, p.Salt...), nil
}

// EncryptionCapabilities 是 SMB2_ENCRYPTION_CAPABILITIES 的载荷
// （MS-SMB2 §2.2.3.1.2）：CipherCount(2) + Ciphers(2*N)。
type EncryptionCapabilities struct {
	Ciphers []uint16
}

// ParseEncryptionCapabilities 解析 context 载荷。
func ParseEncryptionCapabilities(data []byte) (*EncryptionCapabilities, error) {
	return parseU16List(data, "ENCRYPTION_CAPABILITIES", func(v []uint16) *EncryptionCapabilities {
		return &EncryptionCapabilities{Ciphers: v}
	})
}

// Encode 把载荷编码为 context 的 Data。
func (e *EncryptionCapabilities) Encode() ([]byte, error) {
	return encodeU16List(e.Ciphers, "CipherCount")
}

// SigningCapabilities 是 SMB2_SIGNING_CAPABILITIES 的载荷
// （MS-SMB2 §2.2.3.1.7）：SigningAlgorithmCount(2) + SigningAlgorithms(2*N)。
type SigningCapabilities struct {
	SigningAlgorithms []uint16
}

// ParseSigningCapabilities 解析 context 载荷。
func ParseSigningCapabilities(data []byte) (*SigningCapabilities, error) {
	return parseU16List(data, "SIGNING_CAPABILITIES", func(v []uint16) *SigningCapabilities {
		return &SigningCapabilities{SigningAlgorithms: v}
	})
}

// Encode 把载荷编码为 context 的 Data。
func (s *SigningCapabilities) Encode() ([]byte, error) {
	return encodeU16List(s.SigningAlgorithms, "SigningAlgorithmCount")
}

// CompressionCapabilities 是 SMB2_COMPRESSION_CAPABILITIES 的载荷
// （MS-SMB2 §2.2.3.1.3）：
//
//	CompressionAlgorithmCount(2) + Padding(2) + Flags(4) + CompressionAlgorithms(2*N)
type CompressionCapabilities struct {
	Flags                 uint32
	CompressionAlgorithms []uint16
}

// ParseCompressionCapabilities 解析 context 载荷。
func ParseCompressionCapabilities(data []byte) (*CompressionCapabilities, error) {
	if err := need(data, 8); err != nil {
		return nil, fmt.Errorf("COMPRESSION_CAPABILITIES: %w", err)
	}
	n := uint64(le.Uint16(data[0:]))
	c := &CompressionCapabilities{Flags: le.Uint32(data[4:])}
	algs, err := sliceAt(data, 8, n*2)
	if err != nil {
		return nil, fmt.Errorf("COMPRESSION_CAPABILITIES 算法表: %w", err)
	}
	c.CompressionAlgorithms = make([]uint16, n)
	for i := range c.CompressionAlgorithms {
		c.CompressionAlgorithms[i] = le.Uint16(algs[i*2:])
	}
	return c, nil
}

// Encode 把载荷编码为 context 的 Data。
func (c *CompressionCapabilities) Encode() ([]byte, error) {
	n, err := u16(len(c.CompressionAlgorithms), "CompressionAlgorithmCount")
	if err != nil {
		return nil, err
	}
	out := make([]byte, 8+len(c.CompressionAlgorithms)*2)
	le.PutUint16(out[0:], n)
	le.PutUint32(out[4:], c.Flags)
	for i, a := range c.CompressionAlgorithms {
		le.PutUint16(out[8+i*2:], a)
	}
	return out, nil
}

// ParseNetnameContext 解析 SMB2_NETNAME_NEGOTIATE_CONTEXT_ID 的载荷
// （MS-SMB2 §2.2.3.1.4）：整个 Data 就是 UTF-16LE 的服务器名，服务端应忽略它。
func ParseNetnameContext(data []byte) (string, error) {
	return DecodeUTF16LE(data)
}

// parseU16List 解析「Count(2) + uint16 数组」这种常见载荷。
func parseU16List[T any](data []byte, what string, mk func([]uint16) T) (T, error) {
	var zero T
	if err := need(data, 2); err != nil {
		return zero, fmt.Errorf("%s: %w", what, err)
	}
	n := uint64(le.Uint16(data[0:]))
	raw, err := sliceAt(data, 2, n*2)
	if err != nil {
		return zero, fmt.Errorf("%s 数组: %w", what, err)
	}
	v := make([]uint16, n)
	for i := range v {
		v[i] = le.Uint16(raw[i*2:])
	}
	return mk(v), nil
}

// encodeU16List 编码「Count(2) + uint16 数组」。
func encodeU16List(v []uint16, what string) ([]byte, error) {
	n, err := u16(len(v), what)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 2+len(v)*2)
	le.PutUint16(out[0:], n)
	for i, x := range v {
		le.PutUint16(out[2+i*2:], x)
	}
	return out, nil
}
