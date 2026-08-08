package auth

// NTLM (NTLMSSP) 报文层 —— MS-NLMP。
//
// 全部字段一律 **小端**（MS-NLMP §2.2）。
// 变长字段用 payload 三元组描述：Len(2) || MaxLen(2) || BufferOffset(4)，
// 偏移相对**消息起点**。
//
// 本文件只做 []byte ↔ struct，不含任何状态与 IO（对应 AGENTS.md P1）。

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// Signature 与消息类型（MS-NLMP §2.2.1）。
var ntlmSignature = [8]byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

// MessageType（MS-NLMP §2.2.1）。
const (
	MsgTypeNegotiate    uint32 = 0x00000001
	MsgTypeChallenge    uint32 = 0x00000002
	MsgTypeAuthenticate uint32 = 0x00000003
)

// 各消息的固定头长度（不含 payload）。
const (
	// NEGOTIATE：Signature(8)+Type(4)+Flags(4)+Domain(8)+Workstation(8)+Version(8)
	negotiateHeaderSize = 40
	// CHALLENGE：Signature(8)+Type(4)+TargetName(8)+Flags(4)+ServerChallenge(8)
	//            +Reserved(8)+TargetInfo(8)+Version(8)
	challengeHeaderSize = 56
	// AUTHENTICATE：Signature(8)+Type(4)+Lm(8)+Nt(8)+Domain(8)+User(8)
	//               +Workstation(8)+EncryptedRandomSessionKey(8)+Flags(4)
	//               = 64；再加 Version(8) = 72；再加 MIC(16) = 88。
	authenticateMinSize     = 64
	authenticateVersionSize = 72
	authenticateMICSize     = 88
	// MICSize 是 AUTHENTICATE 消息里 MIC 字段长度。
	MICSize = 16
	// micOffset 是 MIC 字段在 AUTHENTICATE 消息中的偏移。
	micOffset = 72
)

// maxNTLMMessageSize 是可接受的单条 NTLM 消息上限（防御性资源限制，AGENTS.md §8）。
const maxNTLMMessageSize = 64 * 1024

// NegotiateFlags 是 NTLM 协商标志（MS-NLMP §2.2.2.5）。
type NegotiateFlags uint32

// NTLMSSP_NEGOTIATE_* （MS-NLMP §2.2.2.5，位序按规范的 A..W 反向排列后的数值）。
const (
	NegotiateUnicode                 NegotiateFlags = 0x00000001
	NegotiateOEM                     NegotiateFlags = 0x00000002
	NegotiateRequestTarget           NegotiateFlags = 0x00000004
	NegotiateSign                    NegotiateFlags = 0x00000010
	NegotiateSeal                    NegotiateFlags = 0x00000020
	NegotiateDatagram                NegotiateFlags = 0x00000040
	NegotiateLMKey                   NegotiateFlags = 0x00000080
	NegotiateNTLM                    NegotiateFlags = 0x00000200
	NegotiateAnonymous               NegotiateFlags = 0x00000800
	NegotiateOEMDomainSupplied       NegotiateFlags = 0x00001000
	NegotiateOEMWorkstationSupplied  NegotiateFlags = 0x00002000
	NegotiateAlwaysSign              NegotiateFlags = 0x00008000
	NegotiateTargetTypeDomain        NegotiateFlags = 0x00010000
	NegotiateTargetTypeServer        NegotiateFlags = 0x00020000
	NegotiateExtendedSessionSecurity NegotiateFlags = 0x00080000
	NegotiateIdentify                NegotiateFlags = 0x00100000
	RequestNonNTSessionKey           NegotiateFlags = 0x00400000
	NegotiateTargetInfo              NegotiateFlags = 0x00800000
	NegotiateVersion                 NegotiateFlags = 0x02000000
	Negotiate128                     NegotiateFlags = 0x20000000
	NegotiateKeyExch                 NegotiateFlags = 0x40000000
	Negotiate56                      NegotiateFlags = 0x80000000
)

// Has 报告是否置了给定标志位。
func (f NegotiateFlags) Has(x NegotiateFlags) bool { return f&x != 0 }

// AvID 是 AV_PAIR 的标识（MS-NLMP §2.2.2.1）。
type AvID uint16

const (
	MsvAvEOL             AvID = 0x0000
	MsvAvNbComputerName  AvID = 0x0001
	MsvAvNbDomainName    AvID = 0x0002
	MsvAvDnsComputerName AvID = 0x0003
	MsvAvDnsDomainName   AvID = 0x0004
	MsvAvDnsTreeName     AvID = 0x0005
	MsvAvFlags           AvID = 0x0006
	MsvAvTimestamp       AvID = 0x0007
	MsvAvSingleHost      AvID = 0x0008
	MsvAvTargetName      AvID = 0x0009
	MsvAvChannelBindings AvID = 0x000A
)

// MsvAvFlags 的位（MS-NLMP §2.2.2.1）。
const (
	// AvFlagMICPresent：AUTHENTICATE_MESSAGE 里带了 MIC 字段，服务端**必须**校验。
	AvFlagMICPresent uint32 = 0x00000002
	// AvFlagAccountIsGuest：客户端账户是 guest。
	AvFlagAccountIsGuest uint32 = 0x00000004
)

// AvPair 是一条 AV_PAIR：AvId(2) || AvLen(2) || Value。
type AvPair struct {
	ID    AvID
	Value []byte
}

// AvPairs 是 AV_PAIR 列表（不含结尾的 MsvAvEOL，编码时自动补）。
type AvPairs []AvPair

var (
	// ErrNTLMTruncated 表示消息被截断或长度字段越界。
	ErrNTLMTruncated = errors.New("auth: NTLM message truncated")
	// ErrNTLMSignature 表示缺少 "NTLMSSP\\0" 魔数。
	ErrNTLMSignature = errors.New("auth: bad NTLMSSP signature")
	// ErrNTLMMessageType 表示消息类型不是期望值。
	ErrNTLMMessageType = errors.New("auth: unexpected NTLM message type")
	// ErrNTLMTooLarge 表示消息超过 maxNTLMMessageSize。
	ErrNTLMTooLarge = errors.New("auth: NTLM message too large")
)

// Encode 把 AV_PAIR 列表编码为字节串，自动追加 MsvAvEOL 终止对。
func (p AvPairs) Encode() []byte {
	n := 4 // EOL
	for _, av := range p {
		n += 4 + len(av.Value)
	}
	out := make([]byte, 0, n)
	var hdr [4]byte
	for _, av := range p {
		binary.LittleEndian.PutUint16(hdr[0:], uint16(av.ID))
		binary.LittleEndian.PutUint16(hdr[2:], uint16(len(av.Value)))
		out = append(out, hdr[:]...)
		out = append(out, av.Value...)
	}
	return append(out, 0, 0, 0, 0) // MsvAvEOL, AvLen=0
}

// ParseAvPairs 解析 AV_PAIR 列表。遇到 MsvAvEOL 结束，EOL 不进入结果。
//
// 先校验长度再切片，任何越界都返回 ErrNTLMTruncated（不 panic）。
func ParseAvPairs(b []byte) (AvPairs, error) {
	var out AvPairs
	for {
		if len(b) < 4 {
			// 缺少 EOL 也当作截断，避免默默接受畸形输入。
			return nil, ErrNTLMTruncated
		}
		id := AvID(binary.LittleEndian.Uint16(b[0:]))
		n := int(binary.LittleEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			return nil, ErrNTLMTruncated
		}
		if id == MsvAvEOL {
			return out, nil
		}
		v := make([]byte, n)
		copy(v, b[4:4+n])
		out = append(out, AvPair{ID: id, Value: v})
		b = b[4+n:]
	}
}

// Get 返回第一个匹配 id 的值。
func (p AvPairs) Get(id AvID) ([]byte, bool) {
	for _, av := range p {
		if av.ID == id {
			return av.Value, true
		}
	}
	return nil, false
}

// Version 是 NTLM 的 VERSION 结构（MS-NLMP §2.2.2.10），仅用于调试展示。
type Version struct {
	Major        uint8
	Minor        uint8
	Build        uint16
	NTLMRevision uint8
}

// NTLMRevisionCurrent 即 NTLMSSP_REVISION_W2K3 = 0x0F（MS-NLMP §2.2.2.10）。
const NTLMRevisionCurrent uint8 = 0x0F

func (v Version) encode(dst []byte) {
	_ = dst[7]
	dst[0] = v.Major
	dst[1] = v.Minor
	binary.LittleEndian.PutUint16(dst[2:], v.Build)
	dst[4], dst[5], dst[6] = 0, 0, 0
	dst[7] = v.NTLMRevision
}

func decodeVersion(b []byte) Version {
	if len(b) < 8 {
		return Version{}
	}
	return Version{
		Major:        b[0],
		Minor:        b[1],
		Build:        binary.LittleEndian.Uint16(b[2:]),
		NTLMRevision: b[7],
	}
}

// ---------------------------------------------------------------- payload 三元组

// field 描述一个 payload 三元组。
type field struct {
	Len    uint16
	MaxLen uint16
	Offset uint32
}

func readField(b []byte, off int) field {
	return field{
		Len:    binary.LittleEndian.Uint16(b[off:]),
		MaxLen: binary.LittleEndian.Uint16(b[off+2:]),
		Offset: binary.LittleEndian.Uint32(b[off+4:]),
	}
}

func writeField(b []byte, off int, n int, payloadOff int) {
	binary.LittleEndian.PutUint16(b[off:], uint16(n))
	binary.LittleEndian.PutUint16(b[off+2:], uint16(n))
	binary.LittleEndian.PutUint32(b[off+4:], uint32(payloadOff))
}

// slice 按三元组取出 payload，越界返回 ErrNTLMTruncated。
func (f field) slice(msg []byte) ([]byte, error) {
	if f.Len == 0 {
		return nil, nil
	}
	end := uint64(f.Offset) + uint64(f.Len)
	if uint64(f.Offset) > uint64(len(msg)) || end > uint64(len(msg)) {
		return nil, ErrNTLMTruncated
	}
	out := make([]byte, f.Len)
	copy(out, msg[f.Offset:end])
	return out, nil
}

// ---------------------------------------------------------------- 字符串编解码

// utf16le 把 Go 字符串编码为 UTF-16LE 字节串。
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

// fromUTF16LE 解码 UTF-16LE 字节串。长度为奇数时丢弃末尾半个码元。
func fromUTF16LE(b []byte) string {
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// decodeString 按 flags 里的 UNICODE 位决定用 UTF-16LE 还是 OEM(当作 Latin-1/ASCII)。
func decodeString(b []byte, flags NegotiateFlags) string {
	if flags.Has(NegotiateUnicode) {
		return fromUTF16LE(b)
	}
	return string(b)
}

// ---------------------------------------------------------------- 消息公共头

// MessageType 返回 NTLM 消息的类型，同时校验魔数与最小长度。
func MessageType(b []byte) (uint32, error) {
	if len(b) > maxNTLMMessageSize {
		return 0, ErrNTLMTooLarge
	}
	if len(b) < 12 {
		return 0, ErrNTLMTruncated
	}
	for i, c := range ntlmSignature {
		if b[i] != c {
			return 0, ErrNTLMSignature
		}
	}
	return binary.LittleEndian.Uint32(b[8:]), nil
}

// IsNTLMSSP 报告 buffer 是否以 "NTLMSSP\0" 开头（裸 NTLM，无 SPNEGO 外壳）。
func IsNTLMSSP(b []byte) bool {
	if len(b) < len(ntlmSignature) {
		return false
	}
	for i, c := range ntlmSignature {
		if b[i] != c {
			return false
		}
	}
	return true
}

func putHeader(b []byte, msgType uint32) {
	copy(b, ntlmSignature[:])
	binary.LittleEndian.PutUint32(b[8:], msgType)
}

// ---------------------------------------------------------------- NEGOTIATE

// NegotiateMessage 是 NTLM NEGOTIATE_MESSAGE（Type 1，MS-NLMP §2.2.1.1）。
type NegotiateMessage struct {
	Flags       NegotiateFlags
	Domain      string
	Workstation string
	Version     Version
}

// ParseNegotiateMessage 解析 Type 1。
//
// 注意：真实客户端（含 Windows）可能只发到 Flags 为止（16 字节）就结束，
// 因此 Domain/Workstation/Version 都按"可选"处理。
func ParseNegotiateMessage(b []byte) (*NegotiateMessage, error) {
	t, err := MessageType(b)
	if err != nil {
		return nil, err
	}
	if t != MsgTypeNegotiate {
		return nil, ErrNTLMMessageType
	}
	if len(b) < 16 {
		return nil, ErrNTLMTruncated
	}
	m := &NegotiateMessage{Flags: NegotiateFlags(binary.LittleEndian.Uint32(b[12:]))}
	if len(b) >= 24 {
		f := readField(b, 16)
		v, err := f.slice(b)
		if err != nil {
			return nil, err
		}
		// Type 1 的 Domain/Workstation 恒为 OEM 字符串（MS-NLMP §2.2.1.1）。
		m.Domain = string(v)
	}
	if len(b) >= 32 {
		f := readField(b, 24)
		v, err := f.slice(b)
		if err != nil {
			return nil, err
		}
		m.Workstation = string(v)
	}
	if len(b) >= negotiateHeaderSize && m.Flags.Has(NegotiateVersion) {
		m.Version = decodeVersion(b[32:])
	}
	return m, nil
}

// Marshal 编码 Type 1。服务端不需要它，主要供测试与将来的客户端侧使用。
func (m *NegotiateMessage) Marshal() []byte {
	domain := []byte(m.Domain)
	ws := []byte(m.Workstation)
	out := make([]byte, negotiateHeaderSize+len(domain)+len(ws))
	putHeader(out, MsgTypeNegotiate)
	binary.LittleEndian.PutUint32(out[12:], uint32(m.Flags))
	off := negotiateHeaderSize
	writeField(out, 16, len(domain), off)
	copy(out[off:], domain)
	off += len(domain)
	writeField(out, 24, len(ws), off)
	copy(out[off:], ws)
	m.Version.encode(out[32:])
	return out
}

// ---------------------------------------------------------------- CHALLENGE

// ChallengeMessage 是 NTLM CHALLENGE_MESSAGE（Type 2，MS-NLMP §2.2.1.2）。
type ChallengeMessage struct {
	TargetName      string
	Flags           NegotiateFlags
	ServerChallenge [8]byte
	TargetInfo      AvPairs
	Version         Version
}

// Marshal 编码 Type 2。
//
// 布局（全部小端）：固定头 56 字节 + payload。
// payload 顺序为 TargetName、TargetInfo（与 Windows 实际报文一致）。
func (m *ChallengeMessage) Marshal() []byte {
	flags := m.Flags
	var name []byte
	if flags.Has(NegotiateUnicode) {
		name = utf16le(m.TargetName)
	} else {
		name = []byte(m.TargetName)
	}
	var info []byte
	if len(m.TargetInfo) > 0 {
		info = m.TargetInfo.Encode()
	}

	out := make([]byte, challengeHeaderSize+len(name)+len(info))
	putHeader(out, MsgTypeChallenge)

	off := challengeHeaderSize
	writeField(out, 12, len(name), off)
	copy(out[off:], name)
	off += len(name)

	binary.LittleEndian.PutUint32(out[20:], uint32(flags))
	copy(out[24:], m.ServerChallenge[:])
	// out[32:40] Reserved 恒为 0。
	writeField(out, 40, len(info), off)
	copy(out[off:], info)

	m.Version.encode(out[48:])
	return out
}

// ParseChallengeMessage 解析 Type 2（服务端不需要，供测试与客户端侧使用）。
func ParseChallengeMessage(b []byte) (*ChallengeMessage, error) {
	t, err := MessageType(b)
	if err != nil {
		return nil, err
	}
	if t != MsgTypeChallenge {
		return nil, ErrNTLMMessageType
	}
	if len(b) < challengeHeaderSize {
		return nil, ErrNTLMTruncated
	}
	m := &ChallengeMessage{Flags: NegotiateFlags(binary.LittleEndian.Uint32(b[20:]))}
	nameBytes, err := readField(b, 12).slice(b)
	if err != nil {
		return nil, err
	}
	m.TargetName = decodeString(nameBytes, m.Flags)
	copy(m.ServerChallenge[:], b[24:32])
	infoBytes, err := readField(b, 40).slice(b)
	if err != nil {
		return nil, err
	}
	if len(infoBytes) > 0 {
		if m.TargetInfo, err = ParseAvPairs(infoBytes); err != nil {
			return nil, err
		}
	}
	m.Version = decodeVersion(b[48:])
	return m, nil
}

// ---------------------------------------------------------------- AUTHENTICATE

// AuthenticateMessage 是 NTLM AUTHENTICATE_MESSAGE（Type 3，MS-NLMP §2.2.1.3）。
type AuthenticateMessage struct {
	LMResponse                []byte
	NTResponse                []byte
	DomainName                string
	UserName                  string
	Workstation               string
	EncryptedRandomSessionKey []byte
	Flags                     NegotiateFlags
	Version                   Version

	// MICPresent 表示消息里存在 MIC 字段（由 payload 最小偏移 ≥ 88 判定）。
	MICPresent bool
	// MIC 是 16 字节校验值，MICPresent 为 false 时无意义。
	MIC [MICSize]byte

	// raw 是消息原始字节的副本，MIC 校验需要（要把 MIC 字段清零后重算）。
	raw []byte
}

// ParseAuthenticateMessage 解析 Type 3。
//
// MIC 是否存在无法从标志位判断，MS-NLMP §2.2.1.3 的做法是看 payload 的
// 最小偏移：≥ 88 说明固定头里给 MIC 留了位置。
func ParseAuthenticateMessage(b []byte) (*AuthenticateMessage, error) {
	t, err := MessageType(b)
	if err != nil {
		return nil, err
	}
	if t != MsgTypeAuthenticate {
		return nil, ErrNTLMMessageType
	}
	if len(b) < authenticateMinSize {
		return nil, ErrNTLMTruncated
	}

	lmF := readField(b, 12)
	ntF := readField(b, 20)
	domF := readField(b, 28)
	userF := readField(b, 36)
	wsF := readField(b, 44)
	keyF := readField(b, 52)

	m := &AuthenticateMessage{
		Flags: NegotiateFlags(binary.LittleEndian.Uint32(b[60:])),
	}
	if m.LMResponse, err = lmF.slice(b); err != nil {
		return nil, err
	}
	if m.NTResponse, err = ntF.slice(b); err != nil {
		return nil, err
	}
	dom, err := domF.slice(b)
	if err != nil {
		return nil, err
	}
	user, err := userF.slice(b)
	if err != nil {
		return nil, err
	}
	ws, err := wsF.slice(b)
	if err != nil {
		return nil, err
	}
	if m.EncryptedRandomSessionKey, err = keyF.slice(b); err != nil {
		return nil, err
	}
	m.DomainName = decodeString(dom, m.Flags)
	m.UserName = decodeString(user, m.Flags)
	m.Workstation = decodeString(ws, m.Flags)

	// payload 最小偏移决定固定头实际长度。
	minOff := len(b)
	for _, f := range []field{lmF, ntF, domF, userF, wsF, keyF} {
		if f.Len > 0 && int(f.Offset) < minOff {
			minOff = int(f.Offset)
		}
	}
	if minOff >= authenticateVersionSize && len(b) >= authenticateVersionSize {
		m.Version = decodeVersion(b[64:])
	}
	if minOff >= authenticateMICSize && len(b) >= authenticateMICSize {
		m.MICPresent = true
		copy(m.MIC[:], b[micOffset:micOffset+MICSize])
	}

	m.raw = make([]byte, len(b))
	copy(m.raw, b)
	return m, nil
}

// Raw 返回消息原始字节（只读，调用方不得修改）。
func (m *AuthenticateMessage) Raw() []byte { return m.raw }

// rawWithZeroMIC 返回把 MIC 字段清零后的消息副本，用于 MIC 校验。
func (m *AuthenticateMessage) rawWithZeroMIC() []byte {
	out := make([]byte, len(m.raw))
	copy(out, m.raw)
	if m.MICPresent && len(out) >= authenticateMICSize {
		for i := micOffset; i < micOffset+MICSize; i++ {
			out[i] = 0
		}
	}
	return out
}

// IsAnonymous 报告这是不是匿名（null session）认证。
//
// MS-NLMP §3.2.5.1.2：UserName 为空且 NtChallengeResponse 长度为 0
// （某些客户端会带一个长度为 1 的 LM 响应）。
func (m *AuthenticateMessage) IsAnonymous() bool {
	if m.Flags.Has(NegotiateAnonymous) {
		return true
	}
	return m.UserName == "" && len(m.NTResponse) == 0
}

// Marshal 编码 Type 3（供测试与客户端侧使用）。
//
// 布局：固定头（含 Version 与 MIC 时为 88 字节）+ payload。
// payload 顺序 Domain、User、Workstation、LM、NT、SessionKey，与 Windows 一致。
func (m *AuthenticateMessage) Marshal() []byte {
	enc := func(s string) []byte {
		if m.Flags.Has(NegotiateUnicode) {
			return utf16le(s)
		}
		return []byte(s)
	}
	dom, user, ws := enc(m.DomainName), enc(m.UserName), enc(m.Workstation)

	hdr := authenticateVersionSize
	if m.MICPresent {
		hdr = authenticateMICSize
	}
	total := hdr + len(dom) + len(user) + len(ws) +
		len(m.LMResponse) + len(m.NTResponse) + len(m.EncryptedRandomSessionKey)
	out := make([]byte, total)
	putHeader(out, MsgTypeAuthenticate)
	binary.LittleEndian.PutUint32(out[60:], uint32(m.Flags))
	m.Version.encode(out[64:])
	if m.MICPresent {
		copy(out[micOffset:], m.MIC[:])
	}

	off := hdr
	put := func(fieldOff int, v []byte) {
		writeField(out, fieldOff, len(v), off)
		copy(out[off:], v)
		off += len(v)
	}
	put(28, dom)
	put(36, user)
	put(44, ws)
	put(12, m.LMResponse)
	put(20, m.NTResponse)
	put(52, m.EncryptedRandomSessionKey)
	return out
}
