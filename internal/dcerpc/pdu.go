// Package dcerpc 实现 DCERPC 协议（C706 / MS-RPCE）的报文层与命名管道抽象。
//
// 本包只做二进制编解码与 "管道" 缓冲，不包含任何具体接口语义（srvsvc 见
// internal/dcerpc/srvsvc）。设计原则与 AGENTS.md P1 一致：报文层与状态层分离，
// 所有解析都先校验长度再切片，绝不 panic。
//
// 字节序说明（与 SMB2 不同）：DCERPC 是**自描述**的，整数字节序由公共头的
// packed_drep[0] 高 4 位决定（0x1 = 小端），不要硬编码。本包据此动态选择。
package dcerpc

import (
	"encoding/binary"
	"fmt"
)

// PTYPE —— PDU 类型（C706 §12.3 / MS-RPCE §2.2.2.1）。
const (
	PTYPERequest          uint8 = 0  // 客户端发起请求
	PTYPEPing             uint8 = 1  // 保留
	PTYPEResponse         uint8 = 2  // 服务端应答
	PTYPEFault            uint8 = 3  // 异常应答（含 NCA 状态码）
	PTYPEWorking          uint8 = 4  // 保留
	PTYPENocall           uint8 = 5  // 保留
	PTYPEReject           uint8 = 6  // 保留（非标准）
	PTYPEOrphaned         uint8 = 7  // 保留
	PTYPEBind             uint8 = 11 // 绑定请求
	PTYPEBindAck          uint8 = 12 // 绑定应答
	PTYPEBindNak          uint8 = 13 // 绑定拒绝
	PTYPEAlterContext     uint8 = 14 // 上下文变更请求
	PTYPEAlterContextResp uint8 = 15 // 上下文变更应答
)

// PFC 标志位（MS-RPCE §2.2.2.1 pfc_flags）。
const (
	PFCFirstFrag     uint8 = 0x01 // PFC_FIRST_FRAG
	PFCLastFrag      uint8 = 0x02 // PFC_LAST_FRAG
	PFCPendingCancel uint8 = 0x04 // PFC_PENDING_CANCEL / PFC_SUPPORT_HEADER_SIGN（版本相关）
	PFCReserved3     uint8 = 0x08 // 保留
	PFCConcMpx       uint8 = 0x10 // PFC_CONC_MPLEX
	PFCReserved5     uint8 = 0x20 // PFC_DID_NOT_EXECUTE
	PFCReserved7     uint8 = 0x40 // PFC_MAYBE
	PFCObjectUUID    uint8 = 0x80 // PFC_OBJECT_UUID
)

// NDR32 transfer syntax UUID（MS-RPCE §2.2.2.5 与 C706 §13.2.2）。
// 8a885d04-1ceb-11c9-9fe8-08002b104860, 版本 2.0。
var NDR32TransferSyntax = mustParseUUID("8a885d04-1ceb-11c9-9fe8-08002b104860")

// NDR32TransferVersion 是 NDR32 的 transfer syntax 版本号。
//
// p_syntax_id_t.if_version 是**一个 uint32**，低 16 位是 major、高 16 位是 minor
// （C706 §12.6.3.1）。NDR32 是 v2.0，因此值是 2 而不是 0x00020000
// ——真实抓包里 smbclient 发的就是 if_version = 0x00000002。
const NDR32TransferVersion uint32 = 0x00000002

// bindTimeFeaturePrefix 是「bind time feature negotiation」伪 transfer syntax
// 的前 8 个线格式字节，对应 UUID 6cb71c2c-9812-4540-xxxx-000000000000
// （MS-RPCE §3.3.1.5.3）。Data4 的前两字节携带客户端请求的特性位。
var bindTimeFeaturePrefix = [8]byte{0x2c, 0x1c, 0xb7, 0x6c, 0x12, 0x98, 0x40, 0x45}

// IsBindTimeFeature 报告该 transfer syntax 是否为 bind time feature negotiation，
// 并返回客户端请求的特性位（MS-RPCE §2.2.2.14）。
func IsBindTimeFeature(u UUID) (uint16, bool) {
	for i, b := range bindTimeFeaturePrefix {
		if u[i] != b {
			return 0, false
		}
	}
	for _, b := range u[10:16] {
		if b != 0 {
			return 0, false
		}
	}
	return uint16(u[8]) | uint16(u[9])<<8, true
}

// Header 是 DCERPC 公共头（16 字节，MS-RPCE §2.2.2.1 / C706 §12.4）。
type Header struct {
	Version      uint8   // rpc_vers，固定 5
	VersionMinor uint8   // rpc_vers_minor，固定 0
	PType        uint8   // PTYPE
	PFCFlags     uint8   // pfc_flags
	PackedDrep   [4]byte // packed data representation
	FragLength   uint16  // frag_length（含公共头）
	AuthLength   uint16  // auth_length
	CallID       uint32  // call_id（配对 request/response）
}

// ByteOrder 由 packed_drep[0] 高 4 位决定（C706 §14.1.1）：
// 0x0 = 大端，0x1 = 小端。实际实现里 Windows/Samba 永远发 0x10（小端 NDR32）。
func (h Header) ByteOrder() binary.ByteOrder {
	if (h.PackedDrep[0]>>4)&0x0F == 0 {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// PDU 是解析后的完整 DCERPC 报文；body 字段按 PType 选择性填充。
type PDU struct {
	Header
	// request / response / fault 共用
	AllocHint uint32
	ContextID uint16
	Opnum     uint16
	CancelCnt uint8  // response/fault 的 cancel_count
	Stub      []byte // request/response 的 stub 数据

	// bind
	MaxXmitFrag  uint16
	MaxRecvFrag  uint16
	AssocGroupID uint32
	ContextElems []ContextElem

	// bind_ack
	SecAddr string
	Results []Result

	// fault
	Status uint32 // NCA 状态码
}

// ContextElem 是 bind 的 p_context_elem 项（C706 §12.6.3.2）。
//
// 线格式字段顺序是 p_cont_id(2) → n_transfer_syn(1) → reserved(1) →
// abstract_syntax(20) → transfer_syntaxes[n]，**abstract_syntax 不在最前面**。
type ContextElem struct {
	ContextID          uint16
	AbstractSyntaxUUID UUID
	AbstractSyntaxVer  uint32
	TransferSyntaxes   []TransferSyntax
}

// TransferSyntax 是 transfer syntax 项。
type TransferSyntax struct {
	UUID    UUID
	Version uint32
}

// Result 是 bind_ack 的 p_result 项。
type Result struct {
	Result uint16 // 0 = 接受（acceptance）
	Reason uint16 // 0
	Syntax TransferSyntax
}

// p_result 接受码（C706 §12.6.3.1 / MS-RPCE §2.2.2.4）。
const (
	ResultAcceptance     uint16 = 0
	ResultUserReject     uint16 = 1
	ResultProviderReject uint16 = 2
	// ResultNegotiateAck 用于回应 bind time feature negotiation
	// （MS-RPCE §3.3.1.5.3），此时 reason 字段承载服务端支持的特性位。
	ResultNegotiateAck uint16 = 3
)

// p_provider_reason 拒绝原因（C706 §12.6.3.1）。
const (
	ReasonNotSpecified          uint16 = 0
	ReasonAbstractSyntaxUnsup   uint16 = 1 // abstract_syntax_not_supported
	ReasonTransferSyntaxesUnsup uint16 = 2 // proposed_transfer_syntaxes_not_supported
)

// NegotiateResults 按客户端提出的 context 列表逐项给出协商结果
// （C706 §12.6.3.1：p_result_list 的项数必须与 p_context_elem 一一对应）。
//
// iface 是本端支持的抽象接口 UUID。规则：
//   - transfer syntax 是 bind time feature negotiation → negotiate_ack，
//     reason 回本端支持的特性位（我们两个都不支持，回 0）；
//   - abstract syntax 不认识 → provider_rejection / abstract_syntax_not_supported；
//   - 没有 NDR32 → provider_rejection / proposed_transfer_syntaxes_not_supported；
//   - 否则 acceptance + NDR32。
//
// 第二个返回值表示是否至少有一个 context 被接受；全不接受时调用方应回 bind_nak。
func NegotiateResults(elems []ContextElem, iface UUID) ([]Result, bool) {
	ndr32 := TransferSyntax{UUID: NDR32TransferSyntax, Version: NDR32TransferVersion}
	results := make([]Result, 0, len(elems))
	accepted := false

	for _, e := range elems {
		// 特性协商上下文优先判断：它的 abstract syntax 仍然是目标接口，
		// 区分点在 transfer syntax。
		feature := false
		hasNDR32 := false
		for _, ts := range e.TransferSyntaxes {
			if _, ok := IsBindTimeFeature(ts.UUID); ok {
				feature = true
			}
			if ts.UUID.Equal(NDR32TransferSyntax) {
				hasNDR32 = true
			}
		}

		switch {
		case feature:
			// 本端不支持安全上下文复用与 orphan 保活，特性位回 0。
			results = append(results, Result{Result: ResultNegotiateAck})
		case !e.AbstractSyntaxUUID.Equal(iface):
			results = append(results, Result{
				Result: ResultProviderReject,
				Reason: ReasonAbstractSyntaxUnsup,
			})
		case !hasNDR32:
			results = append(results, Result{
				Result: ResultProviderReject,
				Reason: ReasonTransferSyntaxesUnsup,
			})
		default:
			results = append(results, Result{Result: ResultAcceptance, Syntax: ndr32})
			accepted = true
		}
	}
	return results, accepted
}

// 公共头固定 16 字节（MS-RPCE §2.2.2.1）。
const headerSize = 16

// 各类 PDU 体最小长度（不含 16 字节头），用于长度校验。
const (
	requestBodyMin  = 8  // alloc_hint(4)+context_id(2)+opnum(2)
	responseBodyMin = 8  // alloc_hint(4)+context_id(2)+cancel_count(1)+reserved(1)
	bindBodyMin     = 12 // max_xmit(2)+max_recv(2)+assoc(4)+n_ctx(1)+reserved(1)+reserved(2)
	bindAckBodyMin  = 10 // max_xmit(2)+max_recv(2)+assoc(4)+sec_addr_len(2)
)

// ParsePDU 解析一个完整（单分片）DCERPC PDU。先做整体长度校验，再按 PType 细化。
func ParsePDU(b []byte) (*PDU, error) {
	if len(b) < headerSize {
		return nil, fmt.Errorf("dcerpc: PDU 长度 %d < 公共头 %d 字节", len(b), headerSize)
	}
	h := Header{
		Version:      b[0],
		VersionMinor: b[1],
		PType:        b[2],
		PFCFlags:     b[3],
		FragLength:   binary.LittleEndian.Uint16(b[8:10]),
		AuthLength:   binary.LittleEndian.Uint16(b[10:12]),
		CallID:       binary.LittleEndian.Uint32(b[12:16]),
	}
	copy(h.PackedDrep[:], b[4:8])

	if h.Version != 5 {
		return nil, fmt.Errorf("dcerpc: 不支持的 rpc_vers=%d（仅支持 5）", h.Version)
	}
	if int(h.FragLength) != len(b) {
		return nil, fmt.Errorf("dcerpc: frag_length=%d 与实际长度 %d 不符", h.FragLength, len(b))
	}
	if int(h.AuthLength) > len(b) {
		return nil, fmt.Errorf("dcerpc: auth_length=%d 超出报文长度 %d", h.AuthLength, len(b))
	}

	pdu := &PDU{Header: h}
	body := b[headerSize:]
	order := h.ByteOrder()

	switch h.PType {
	case PTYPERequest:
		if len(body) < requestBodyMin {
			return nil, fmt.Errorf("dcerpc: request body 长度 %d < %d", len(body), requestBodyMin)
		}
		pdu.AllocHint = order.Uint32(body[0:4])
		pdu.ContextID = order.Uint16(body[4:6])
		pdu.Opnum = order.Uint16(body[6:8])
		stubEnd := len(body)
		if h.AuthLength > 0 {
			stubEnd = len(body) - int(h.AuthLength)
		}
		if stubEnd < requestBodyMin {
			return nil, fmt.Errorf("dcerpc: request stub 越界")
		}
		pdu.Stub = body[requestBodyMin:stubEnd]
	case PTYPEResponse:
		if len(body) < responseBodyMin {
			return nil, fmt.Errorf("dcerpc: response body 长度 %d < %d", len(body), responseBodyMin)
		}
		pdu.AllocHint = order.Uint32(body[0:4])
		pdu.ContextID = order.Uint16(body[4:6])
		pdu.CancelCnt = body[6]
		stubEnd := len(body)
		if h.AuthLength > 0 {
			stubEnd = len(body) - int(h.AuthLength)
		}
		pdu.Stub = body[responseBodyMin:stubEnd]
	case PTYPEBind:
		p, err := parseBind(body, order)
		if err != nil {
			return nil, err
		}
		*pdu = *p
		pdu.Header = h
	case PTYPEBindAck:
		p, err := parseBindAck(body, order)
		if err != nil {
			return nil, err
		}
		*pdu = *p
		pdu.Header = h
	case PTYPEBindNak:
		if len(body) < 2 {
			return nil, fmt.Errorf("dcerpc: bind_nak body 太短")
		}
		pdu.Status = uint32(order.Uint16(body[0:2]))
	case PTYPEFault:
		if len(body) < 12 {
			return nil, fmt.Errorf("dcerpc: fault body 长度 %d < 12", len(body))
		}
		pdu.AllocHint = order.Uint32(body[0:4])
		pdu.ContextID = order.Uint16(body[4:6])
		pdu.CancelCnt = body[6]
		pdu.Status = order.Uint32(body[8:12])
	case PTYPEAlterContext, PTYPEAlterContextResp:
		// 本实现暂不主动发起，但能解析公共头以确认类型。
		_ = body
	default:
		return nil, fmt.Errorf("dcerpc: 未知 PTYPE=%d", h.PType)
	}
	return pdu, nil
}

// parseBind 解析 PTYPEBind 的 body（C706 §12.6.3.2 / MS-RPCE §2.2.2.5）。
func parseBind(body []byte, order binary.ByteOrder) (*PDU, error) {
	if len(body) < bindBodyMin {
		return nil, fmt.Errorf("dcerpc: bind body 长度 %d < %d", len(body), bindBodyMin)
	}
	pdu := &PDU{}
	pdu.MaxXmitFrag = order.Uint16(body[0:2])
	pdu.MaxRecvFrag = order.Uint16(body[2:4])
	pdu.AssocGroupID = order.Uint32(body[4:8])
	nCtx := int(body[8])
	off := 12
	elems := make([]ContextElem, 0, nCtx)
	for i := 0; i < nCtx; i++ {
		// p_cont_id(2) + n_transfer_syn(1) + reserved(1) + abstract_syntax(20)
		if off+24 > len(body) {
			return nil, fmt.Errorf("dcerpc: bind 第 %d 个 context_elem 越界", i)
		}
		elem := ContextElem{
			ContextID:          order.Uint16(body[off : off+2]),
			AbstractSyntaxUUID: ParseUUIDBytes(body[off+4 : off+20]),
			AbstractSyntaxVer:  order.Uint32(body[off+20 : off+24]),
		}
		nXfer := int(body[off+2])
		off += 24
		for j := 0; j < nXfer; j++ {
			if off+20 > len(body) {
				return nil, fmt.Errorf("dcerpc: bind 第 %d 个 context 的 transfer[%d] 越界", i, j)
			}
			ts := TransferSyntax{
				UUID:    ParseUUIDBytes(body[off : off+16]),
				Version: order.Uint32(body[off+16 : off+20]),
			}
			elem.TransferSyntaxes = append(elem.TransferSyntaxes, ts)
			off += 20
		}
		elems = append(elems, elem)
	}
	pdu.ContextElems = elems
	return pdu, nil
}

// parseBindAck 解析 PTYPEBindAck 的 body（MS-RPCE §2.2.2.4）。
func parseBindAck(body []byte, order binary.ByteOrder) (*PDU, error) {
	if len(body) < bindAckBodyMin {
		return nil, fmt.Errorf("dcerpc: bind_ack body 长度 %d < %d", len(body), bindAckBodyMin)
	}
	pdu := &PDU{}
	pdu.MaxXmitFrag = order.Uint16(body[0:2])
	pdu.MaxRecvFrag = order.Uint16(body[2:4])
	pdu.AssocGroupID = order.Uint32(body[4:8])
	secLen := int(order.Uint16(body[8:10]))
	off := 10
	if off+secLen > len(body) {
		return nil, fmt.Errorf("dcerpc: bind_ack sec_addr 越界 secLen=%d", secLen)
	}
	raw := body[off : off+secLen]
	pdu.SecAddr = cstr(raw)
	off += secLen
	off = align4(off)
	if off+4 > len(body) {
		return nil, fmt.Errorf("dcerpc: bind_ack p_result_list 头部越界")
	}
	nRes := int(body[off])
	off += 4
	results := make([]Result, 0, nRes)
	for i := 0; i < nRes; i++ {
		if off+24 > len(body) {
			return nil, fmt.Errorf("dcerpc: bind_ack 第 %d 个 result 越界", i)
		}
		r := Result{
			Result: order.Uint16(body[off : off+2]),
			Reason: order.Uint16(body[off+2 : off+4]),
			Syntax: TransferSyntax{
				UUID:    ParseUUIDBytes(body[off+4 : off+20]),
				Version: order.Uint32(body[off+20 : off+24]),
			},
		}
		results = append(results, r)
		off += 24
	}
	pdu.Results = results
	return pdu, nil
}

// ---- 构造函数（服务端生成响应） ----

// NewHeader 构造一个默认小端 NDR32 公共头。
func NewHeader(ptype uint8, callID uint32) Header {
	var drep [4]byte
	drep[0] = 0x10 // 高 4 位 = 1 → 小端（C706 §14.1.1）
	return Header{
		Version:      5,
		VersionMinor: 0,
		PType:        ptype,
		PFCFlags:     PFCLastFrag | PFCFirstFrag,
		PackedDrep:   drep,
		CallID:       callID,
	}
}

// MarshalBindAck 构造 bind_ack（C706 §12.6.4.4 / MS-RPCE §2.2.2.4）。
//
// secAddr 形如 `\PIPE\srvsvc`，**不要**自带 NUL —— 本函数负责补上，
// 且 p_len 计入这个 NUL（Samba 的 IDL 是 `[value(strlen(x)+1)] uint16 size`）。
// results 必须与客户端 bind 的 p_context_elem 一一对应，见 NegotiateResults。
func MarshalBindAck(callID uint32, secAddr string, results []Result) []byte {
	// port_any_t：NUL 结尾的字符串，p_len 含 NUL（C706 §12.6.4.4）。
	secBytes := append([]byte(secAddr), 0)

	// p_result_list: n_results(1)+reserved(1)+reserved2(2)+n*[result(2)+reason(2)+syntax(20)]
	resBlock := make([]byte, 0, 4+24*len(results))
	resBlock = append(resBlock, byte(len(results)), 0, 0, 0)
	for _, r := range results {
		resBlock = append(resBlock, le16(r.Result)...)
		resBlock = append(resBlock, le16(r.Reason)...)
		resBlock = append(resBlock, r.Syntax.UUID[:]...)
		resBlock = append(resBlock, le32(r.Syntax.Version)...)
	}

	const maxFrag = 5840
	body := make([]byte, 0, 64)
	body = append(body, le16(maxFrag)...)
	body = append(body, le16(maxFrag)...)
	body = append(body, le32(0)...) // assoc_group_id（0 = 服务端自选）
	body = append(body, le16(uint16(len(secBytes)))...)
	body = append(body, secBytes...)
	body = pad4(body)
	body = append(body, resBlock...)

	h := NewHeader(PTYPEBindAck, callID)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	binary.LittleEndian.PutUint16(out[8:10], h.FragLength)
	binary.LittleEndian.PutUint32(out[12:16], callID)
	out = append(out, body...)
	return out
}

// MarshalBindNak 构造 bind_nak（MS-RPCE §2.2.2.6），reason 为 provider 拒绝原因。
func MarshalBindNak(callID uint32, reason uint16) []byte {
	body := append(le16(reason), le16(0)...) // reason + protocol-major-version(0)
	h := NewHeader(PTYPEBindNak, callID)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	binary.LittleEndian.PutUint16(out[8:10], h.FragLength)
	binary.LittleEndian.PutUint32(out[12:16], callID)
	out = append(out, body...)
	return out
}

// MarshalResponse 用给定 stub 构造 response PDU（小端 NDR32）。
func MarshalResponse(callID uint32, stub []byte) []byte {
	body := make([]byte, 0, 8+len(stub))
	body = append(body, le32(uint32(len(stub)))...) // alloc_hint
	body = append(body, le16(0)...)                 // context_id
	body = append(body, 0)                          // cancel_count
	body = append(body, 0)                          // reserved
	body = append(body, stub...)

	h := NewHeader(PTYPEResponse, callID)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	binary.LittleEndian.PutUint16(out[8:10], h.FragLength)
	binary.LittleEndian.PutUint32(out[12:16], callID)
	out = append(out, body...)
	return out
}

// MarshalRequest 构造 request PDU（小端 NDR32），用于测试与本仓库可能的 Go 客户端。
func MarshalRequest(callID uint32, contextID uint16, opnum uint16, stub []byte) []byte {
	body := make([]byte, 0, 8+len(stub))
	body = append(body, le32(uint32(len(stub)))...) // alloc_hint
	body = append(body, le16(contextID)...)
	body = append(body, le16(opnum)...)
	body = append(body, stub...)

	h := NewHeader(PTYPERequest, callID)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	binary.LittleEndian.PutUint16(out[8:10], h.FragLength)
	binary.LittleEndian.PutUint32(out[12:16], callID)
	out = append(out, body...)
	return out
}

// MarshalFault 构造 fault PDU（MS-RPCE §2.2.2.12），status 为 NCA 状态码。
func MarshalFault(callID uint32, status uint32) []byte {
	body := make([]byte, 0, 12)
	body = append(body, le32(12)...)     // alloc_hint
	body = append(body, le16(0)...)      // context_id
	body = append(body, 0)               // cancel_count
	body = append(body, 0)               // reserved
	body = append(body, le32(status)...) // status（NCA 码）

	h := NewHeader(PTYPEFault, callID)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	binary.LittleEndian.PutUint16(out[8:10], h.FragLength)
	binary.LittleEndian.PutUint32(out[12:16], callID)
	out = append(out, body...)
	return out
}

// putHeader 把公共头写入 buf 前 16 字节（frag_length/auth_length/call_id 由调用者回填）。
func putHeader(buf []byte, h Header) {
	buf[0] = h.Version
	buf[1] = h.VersionMinor
	buf[2] = h.PType
	buf[3] = h.PFCFlags
	copy(buf[4:8], h.PackedDrep[:])
}

// ---- 小工具 ----

func le16(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

func align4(n int) int { return (n + 3) &^ 3 }
func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// cstr 去掉结尾 NUL 返回 C 字符串内容。
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
