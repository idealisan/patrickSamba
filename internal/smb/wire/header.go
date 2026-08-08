package wire

import "fmt"

// SMB2 Packet Header（MS-SMB2 §2.2.1.2 "SMB2 Packet Header - SYNC" 与
// §2.2.1.1 "SMB2 Packet Header - ASYNC"）。
//
// 布局（**全部小端**，唯一的大端是 Direct TCP 的 4 字节长度前缀）：
//
//	0x00  4   ProtocolId                = 0xFE 'S' 'M' 'B'
//	0x04  2   StructureSize             = 64（固定）
//	0x06  2   CreditCharge              （2.0.2 保留为 0）
//	0x08  4   Status(响应) / ChannelSequence(2)+Reserved(2)(3.x 请求)
//	0x0C  2   Command
//	0x0E  2   CreditRequest / CreditResponse
//	0x10  4   Flags
//	0x14  4   NextCommand               （复合链偏移，相对本头起点）
//	0x18  8   MessageId
//	0x20  4   Reserved                  ┐ SYNC
//	0x24  4   TreeId                    ┘
//	0x20  8   AsyncId                     ASYNC（Flags 含 ASYNC_COMMAND）
//	0x28  8   SessionId
//	0x30  16  Signature
const (
	// HeaderSize 是 SMB2 报文头的固定长度（MS-SMB2 §2.2.1.2 StructureSize）。
	HeaderSize = 64
	// headerStructureSize 是头部 StructureSize 字段的固定值。
	headerStructureSize = 64

	// SignatureOffset 是 Signature 字段在头中的偏移，签名计算时需要先清零该区间
	// （MS-SMB2 §3.1.4.1）。
	SignatureOffset = 0x30
	// SignatureSize 是 Signature 字段长度。
	SignatureSize = 16
)

// ProtocolID 是 SMB2 报文的 4 字节魔数 0xFE 'S' 'M' 'B'（MS-SMB2 §2.2.1.2）。
var ProtocolID = [4]byte{0xFE, 'S', 'M', 'B'}

// SMB1ProtocolID 是 SMB1 报文的魔数 0xFF 'S' 'M' 'B'（MS-CIFS §2.2.3.1）。
// 只用于识别 SMB1 多协议协商入口，本包不实现 SMB1 报文体。
var SMB1ProtocolID = [4]byte{0xFF, 'S', 'M', 'B'}

// Header 是解析后的 SMB2 报文头。
//
// Status 字段是偏移 0x08 处 4 字节的**原始值**：
//   - 响应中它是 NTSTATUS；
//   - SMB 3.x 的请求中它是 ChannelSequence(2) + Reserved(2)（MS-SMB2 §2.2.1.2）。
//
// 保存原始 4 字节可以保证 Parse/Append 严格 round-trip；需要按 3.x 语义访问时
// 用 ChannelSequence / SetChannelSequence。
//
// AsyncID 与 (Reserved, TreeID) 是 0x20 处 8 字节的 union，由
// FlagAsyncCommand 决定当前用哪一种解释。
type Header struct {
	CreditCharge uint16
	Status       uint32
	Command      Command
	// Credits 在请求中是 CreditRequest，在响应中是 CreditResponse。
	Credits     uint16
	Flags       Flags
	NextCommand uint32
	MessageID   uint64

	// AsyncID 仅在 Flags 含 FlagAsyncCommand 时有效。
	AsyncID uint64
	// Reserved / TreeID 仅在同步头（不含 FlagAsyncCommand）时有效。
	Reserved uint32
	TreeID   uint32

	SessionID uint64
	Signature [SignatureSize]byte
}

// ChannelSequence 返回 SMB 3.x 请求中 0x08 处的 ChannelSequence 字段
// （低 16 位，MS-SMB2 §2.2.1.2）。
func (h Header) ChannelSequence() uint16 { return uint16(h.Status) }

// SetChannelSequence 设置 SMB 3.x 请求中的 ChannelSequence，高 16 位 Reserved 清零。
func (h *Header) SetChannelSequence(v uint16) { h.Status = uint32(v) }

// IsResponse 报告本头是否为服务端到客户端的响应（SMB2_FLAGS_SERVER_TO_REDIR）。
func (h Header) IsResponse() bool { return h.Flags.Has(FlagServerToRedir) }

// IsAsync 报告本头是否为 ASYNC 头（0x20 处 8 字节为 AsyncId）。
func (h Header) IsAsync() bool { return h.Flags.Has(FlagAsyncCommand) }

// IsSigned 报告 SMB2_FLAGS_SIGNED 是否置位。
func (h Header) IsSigned() bool { return h.Flags.Has(FlagSigned) }

// IsRelated 报告 SMB2_FLAGS_RELATED_OPERATIONS 是否置位（复合链中复用
// 前一条的 SessionId/TreeId/FileId）。
func (h Header) IsRelated() bool { return h.Flags.Has(FlagRelatedOps) }

// ParseHeader 解析 64 字节 SMB2 报文头。b 不含 Direct TCP 长度前缀。
//
// 只校验 ProtocolId 与 StructureSize，其余字段原样返回 —— 语义校验
// （方言、命令是否允许等）属于状态层的职责。
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if err := need(b, HeaderSize); err != nil {
		return h, fmt.Errorf("SMB2 Header: %w", err)
	}
	if b[0] != ProtocolID[0] || b[1] != ProtocolID[1] || b[2] != ProtocolID[2] || b[3] != ProtocolID[3] {
		return h, fmt.Errorf("%w: 期望 FE 53 4D 42, 实际 %02X %02X %02X %02X",
			ErrProtocolID, b[0], b[1], b[2], b[3])
	}
	if got := le.Uint16(b[4:]); got != headerStructureSize {
		return h, fmt.Errorf("%w: Header StructureSize 期望 64, 实际 %d", ErrStructureSize, got)
	}

	h.CreditCharge = le.Uint16(b[0x06:])
	h.Status = le.Uint32(b[0x08:])
	h.Command = Command(le.Uint16(b[0x0C:]))
	h.Credits = le.Uint16(b[0x0E:])
	h.Flags = Flags(le.Uint32(b[0x10:]))
	h.NextCommand = le.Uint32(b[0x14:])
	h.MessageID = le.Uint64(b[0x18:])
	if h.Flags.Has(FlagAsyncCommand) {
		h.AsyncID = le.Uint64(b[0x20:])
	} else {
		h.Reserved = le.Uint32(b[0x20:])
		h.TreeID = le.Uint32(b[0x24:])
	}
	h.SessionID = le.Uint64(b[0x28:])
	copy(h.Signature[:], b[SignatureOffset:SignatureOffset+SignatureSize])
	return h, nil
}

// Append 把头编码为 64 字节追加到 dst，返回新切片。
//
// Flags 含 FlagAsyncCommand 时 0x20 处写 AsyncID，否则写 Reserved + TreeID。
func (h Header) Append(dst []byte) []byte {
	dst, b := grow(dst, HeaderSize)
	copy(b[0:], ProtocolID[:])
	le.PutUint16(b[0x04:], headerStructureSize)
	le.PutUint16(b[0x06:], h.CreditCharge)
	le.PutUint32(b[0x08:], h.Status)
	le.PutUint16(b[0x0C:], uint16(h.Command))
	le.PutUint16(b[0x0E:], h.Credits)
	le.PutUint32(b[0x10:], uint32(h.Flags))
	le.PutUint32(b[0x14:], h.NextCommand)
	le.PutUint64(b[0x18:], h.MessageID)
	if h.Flags.Has(FlagAsyncCommand) {
		le.PutUint64(b[0x20:], h.AsyncID)
	} else {
		le.PutUint32(b[0x20:], h.Reserved)
		le.PutUint32(b[0x24:], h.TreeID)
	}
	le.PutUint64(b[0x28:], h.SessionID)
	copy(b[SignatureOffset:], h.Signature[:])
	return dst
}

// Reply 由请求头派生出响应头：沿用 Command / MessageId / SessionId / TreeId，
// 置 SMB2_FLAGS_SERVER_TO_REDIR，清掉 NextCommand、Signature 与仅请求有意义的
// 标志位（RELATED_OPERATIONS、SIGNED 由调用方按需重新设置）。
//
// 注意：Status 与 Credits 由调用方填。credit 至少要给 1，否则客户端会挂起
// （见 docs/protocol-notes.md §12）。
func (h Header) Reply() Header {
	r := Header{
		CreditCharge: h.CreditCharge,
		Command:      h.Command,
		Flags:        FlagServerToRedir | (h.Flags & FlagPriorityMask),
		MessageID:    h.MessageID,
		TreeID:       h.TreeID,
		SessionID:    h.SessionID,
	}
	if h.Flags.Has(FlagDFSOperations) {
		r.Flags |= FlagDFSOperations
	}
	return r
}

// IsSMB2 报告 b 是否以 SMB2 魔数开头。
func IsSMB2(b []byte) bool {
	return len(b) >= 4 && b[0] == ProtocolID[0] && b[1] == ProtocolID[1] &&
		b[2] == ProtocolID[2] && b[3] == ProtocolID[3]
}

// IsSMB1 报告 b 是否以 SMB1 魔数开头（用于识别多协议协商入口）。
func IsSMB1(b []byte) bool {
	return len(b) >= 4 && b[0] == SMB1ProtocolID[0] && b[1] == SMB1ProtocolID[1] &&
		b[2] == SMB1ProtocolID[2] && b[3] == SMB1ProtocolID[3]
}
