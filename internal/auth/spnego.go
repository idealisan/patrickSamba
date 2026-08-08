package auth

// SPNEGO（RFC 4178）与 GSS-API InitialContextToken（RFC 2743 §3.1）编解码。
//
// 只实现 SMB2 认证需要的最小子集：服务端发 negTokenInit2 宣告支持 NTLMSSP，
// 之后用裸 negTokenResp 交换 NTLM token。
//
// 这里手写 DER 的原因（AGENTS.md §4）：
//   - `negHints` 里的 `GeneralString` 标准库 encoding/asn1 **无法 Marshal**
//     （golang/go#18832），必须硬编码 tag 0x1B；
//   - SPNEGO 大量使用 context-specific 构造标签，用标准库反射式 API 反而更绕。
//
// 解析一律**先校验长度再切片**，任何畸形输入返回 ErrInvalidToken，绝不 panic。

import "errors"

// ASN.1 tag（DER）。
const (
	tagOID          = 0x06
	tagOctetString  = 0x04
	tagEnumerated   = 0x0A
	tagGeneralStr   = 0x1B // GeneralString，标准库编不出来
	tagSequence     = 0x30 // SEQUENCE, constructed
	tagApplication0 = 0x60 // [APPLICATION 0] constructed —— GSS InitialContextToken
	tagContext0     = 0xA0 // [0] constructed
	tagContext1     = 0xA1
	tagContext2     = 0xA2
	tagContext3     = 0xA3
)

// OID 的 DER content（不含 tag 与长度）。
var (
	// oidSPNEGO 是 1.3.6.1.5.5.2。
	oidSPNEGO = []byte{0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}
	// oidNTLMSSP 是 1.3.6.1.4.1.311.2.2.10。
	oidNTLMSSP = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a}
	// oidKerberos5 是 1.2.840.113554.1.2.2，客户端常把它排在 mechTypes 首位。
	oidKerberos5 = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}
	// oidMSKerberos5 是 1.2.840.48018.1.2.2（微软的 legacy Kerberos OID）。
	oidMSKerberos5 = []byte{0x2a, 0x86, 0x48, 0x82, 0xf7, 0x12, 0x01, 0x02, 0x02}
)

// negHintName 是 Windows 服务端在 negTokenInit2 里放的固定 hint 字符串。
// 客户端会忽略它，但缺了它某些客户端（老 macOS）会不高兴。
const negHintName = "not_defined_in_RFC4178@please_ignore"

// NegState 是 RFC 4178 §4.2.2 的 negState。
type NegState int

const (
	NegAcceptCompleted  NegState = 0
	NegAcceptIncomplete NegState = 1
	NegReject           NegState = 2
	NegRequestMIC       NegState = 3
)

// ErrDER 表示 DER 结构非法。对外统一暴露为 ErrInvalidToken。
var errDER = errors.New("auth: malformed DER")

// ---------------------------------------------------------------- DER 编码

// derLen 编码 DER 长度（短型 / 长型）。
func derLen(n int) []byte {
	switch {
	case n < 0x80:
		return []byte{byte(n)}
	case n <= 0xFF:
		return []byte{0x81, byte(n)}
	case n <= 0xFFFF:
		return []byte{0x82, byte(n >> 8), byte(n)}
	default:
		return []byte{0x83, byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

// derTLV 包一层 tag + 长度 + 内容。
func derTLV(tag byte, content []byte) []byte {
	l := derLen(len(content))
	out := make([]byte, 0, 1+len(l)+len(content))
	out = append(out, tag)
	out = append(out, l...)
	return append(out, content...)
}

// ---------------------------------------------------------------- DER 解析

// derNext 从 b 中读出一个 TLV，返回 tag、内容与剩余字节。
func derNext(b []byte) (tag byte, content, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, errDER
	}
	tag = b[0]
	// 多字节 tag（0x1F）在 SPNEGO 里不会出现，直接拒绝。
	if tag&0x1F == 0x1F {
		return 0, nil, nil, errDER
	}
	i := 1
	l := int(b[i])
	i++
	if l&0x80 != 0 {
		n := l & 0x7F
		// 长度域超过 4 字节说明是畸形或超大报文，拒绝。
		if n == 0 || n > 4 || len(b) < i+n {
			return 0, nil, nil, errDER
		}
		l = 0
		for j := 0; j < n; j++ {
			l = l<<8 | int(b[i+j])
		}
		i += n
		if l < 0 {
			return 0, nil, nil, errDER
		}
	}
	if l > len(b)-i {
		return 0, nil, nil, errDER
	}
	return tag, b[i : i+l], b[i+l:], nil
}

// ---------------------------------------------------------------- negTokenInit2

// NegTokenInit2 构造服务端在 SMB2 NEGOTIATE Response 里宣告的 SPNEGO token。
//
// 结构（RFC 2743 §3.1 + RFC 4178 §4.2.1 + MS-SPNG §2.2.1）：
//
//	[APPLICATION 0] IMPLICIT SEQUENCE {
//	    thisMech           OID 1.3.6.1.5.5.2
//	    innerContextToken  [0] NegTokenInit2 {
//	        mechTypes [0] SEQUENCE OF OID { NTLMSSP }
//	        negHints  [3] SEQUENCE { hintName [0] GeneralString }
//	    }
//	}
func NegTokenInit2(mechs [][]byte) []byte {
	var mechList []byte
	for _, m := range mechs {
		mechList = append(mechList, derTLV(tagOID, m)...)
	}
	mechTypes := derTLV(tagContext0, derTLV(tagSequence, mechList))

	// negHints —— hintName 必须是 GeneralString（tag 0x1B），
	// 标准库 encoding/asn1 无法 Marshal，这里手工拼。
	hint := derTLV(tagContext0, derTLV(tagGeneralStr, []byte(negHintName)))
	negHints := derTLV(tagContext3, derTLV(tagSequence, hint))

	inner := derTLV(tagContext0, derTLV(tagSequence, append(mechTypes, negHints...)))

	body := append(derTLV(tagOID, oidSPNEGO), inner...)
	return derTLV(tagApplication0, body)
}

// ---------------------------------------------------------------- negTokenResp

// NegTokenResp 构造服务端回给客户端的 negTokenResp（裸 [1] 包裹，无 GSS 外壳）。
//
//	NegTokenResp ::= SEQUENCE {
//	    negState      [0] ENUMERATED OPTIONAL
//	    supportedMech [1] MechType    OPTIONAL
//	    responseToken [2] OCTET STRING OPTIONAL
//	    mechListMIC   [3] OCTET STRING OPTIONAL
//	}
//
// supportedMech 只在**第一次**回应时携带（RFC 4178 §4.2.2）；
// mech 传 nil 表示不带。
func NegTokenResp(state NegState, mech, responseToken, mechListMIC []byte) []byte {
	var body []byte
	body = append(body, derTLV(tagContext0, derTLV(tagEnumerated, []byte{byte(state)}))...)
	if len(mech) > 0 {
		body = append(body, derTLV(tagContext1, derTLV(tagOID, mech))...)
	}
	if len(responseToken) > 0 {
		body = append(body, derTLV(tagContext2, derTLV(tagOctetString, responseToken))...)
	}
	if len(mechListMIC) > 0 {
		body = append(body, derTLV(tagContext3, derTLV(tagOctetString, mechListMIC))...)
	}
	return derTLV(tagContext1, derTLV(tagSequence, body))
}

// ---------------------------------------------------------------- 客户端 token 解析

// SPNEGOToken 是从客户端 token 中提取出的信息。
type SPNEGOToken struct {
	// Raw 为 true 表示这是裸 NTLMSSP（没有 SPNEGO 外壳）。
	Raw bool
	// Init 为 true 表示这是 negTokenInit（客户端的第一个 token）。
	Init bool
	// MechTypes 是客户端 negTokenInit 里列出的机制 OID（DER content，不含 tag）。
	MechTypes [][]byte
	// MechTypesDER 是 mechTypes 字段 **[0] 内部那层 SEQUENCE 的完整 DER**，
	// 计算 mechListMIC 时需要原样使用（RFC 4178 §5）。
	MechTypesDER []byte
	// Token 是内层的 NTLM 消息（mechToken 或 responseToken 的内容）。
	Token []byte
	// MechListMIC 是客户端携带的 mechListMIC（可能为空）。
	MechListMIC []byte
	// State 是客户端 negTokenResp 里的 negState，未携带时为 -1。
	State NegState
}

// HasMech 报告 mechTypes 里是否包含给定 OID。
func (t *SPNEGOToken) HasMech(oid []byte) bool {
	for _, m := range t.MechTypes {
		if len(m) == len(oid) && string(m) == string(oid) {
			return true
		}
	}
	return false
}

// ParseSPNEGO 解析客户端送来的 GSS/SPNEGO token。
//
// 支持三种形态：
//  1. 裸 NTLMSSP（buffer 以 "NTLMSSP\0" 开头）—— 部分客户端与 SMB1 会这么发；
//  2. GSS InitialContextToken（0x60）包裹的 negTokenInit / negTokenInit2；
//  3. 裸 negTokenResp（0xA1）。
func ParseSPNEGO(in []byte) (*SPNEGOToken, error) {
	if len(in) == 0 {
		return nil, ErrInvalidToken
	}
	if IsNTLMSSP(in) {
		return &SPNEGOToken{Raw: true, Token: in, State: -1}, nil
	}

	switch in[0] {
	case tagApplication0:
		return parseInitialContextToken(in)
	case tagContext1:
		return parseNegTokenResp(in)
	case tagContext0:
		// 少数实现直接发裸 negTokenInit（无 GSS 外壳）。
		t, content, _, err := derNext(in)
		if err != nil || t != tagContext0 {
			return nil, ErrInvalidToken
		}
		return parseNegTokenInitBody(content)
	default:
		return nil, ErrInvalidToken
	}
}

func parseInitialContextToken(in []byte) (*SPNEGOToken, error) {
	tag, content, _, err := derNext(in)
	if err != nil || tag != tagApplication0 {
		return nil, ErrInvalidToken
	}
	// thisMech
	tag, oid, rest, err := derNext(content)
	if err != nil || tag != tagOID {
		return nil, ErrInvalidToken
	}
	if string(oid) != string(oidSPNEGO) {
		// 不是 SPNEGO（例如裸 Kerberos GSS token），我们不支持。
		return nil, ErrMechUnsupported
	}
	tag, inner, _, err := derNext(rest)
	if err != nil {
		return nil, ErrInvalidToken
	}
	switch tag {
	case tagContext0:
		return parseNegTokenInitBody(inner)
	case tagContext1:
		return parseNegTokenRespBody(inner)
	default:
		return nil, ErrInvalidToken
	}
}

// parseNegTokenInitBody 解析 [0] 内部的 SEQUENCE。
func parseNegTokenInitBody(inner []byte) (*SPNEGOToken, error) {
	tag, seq, _, err := derNext(inner)
	if err != nil || tag != tagSequence {
		return nil, ErrInvalidToken
	}
	out := &SPNEGOToken{Init: true, State: -1}
	for len(seq) > 0 {
		var t byte
		var content []byte
		t, content, seq, err = derNext(seq)
		if err != nil {
			return nil, ErrInvalidToken
		}
		switch t {
		case tagContext0: // mechTypes
			mt, mtSeq, _, err := derNext(content)
			if err != nil || mt != tagSequence {
				return nil, ErrInvalidToken
			}
			// mechListMIC 的计算范围是这层 SEQUENCE 的完整 DER。
			out.MechTypesDER = derTLV(tagSequence, mtSeq)
			for len(mtSeq) > 0 {
				var ot byte
				var oid []byte
				ot, oid, mtSeq, err = derNext(mtSeq)
				if err != nil {
					return nil, ErrInvalidToken
				}
				if ot == tagOID {
					cp := make([]byte, len(oid))
					copy(cp, oid)
					out.MechTypes = append(out.MechTypes, cp)
				}
			}
		case tagContext2: // mechToken
			ot, tok, _, err := derNext(content)
			if err != nil || ot != tagOctetString {
				return nil, ErrInvalidToken
			}
			out.Token = tok
		case tagContext3:
			// negTokenInit 的 [3] 在 RFC 4178 里是 mechListMIC，
			// 但 MS-SPNG 的 negTokenInit2 把它用作 negHints。
			// 用内容的 tag 区分：OCTET STRING 才是 MIC。
			if ot, mic, _, err := derNext(content); err == nil && ot == tagOctetString {
				out.MechListMIC = mic
			}
		case tagContext1: // reqFlags，忽略
		}
	}
	return out, nil
}

func parseNegTokenResp(in []byte) (*SPNEGOToken, error) {
	tag, content, _, err := derNext(in)
	if err != nil || tag != tagContext1 {
		return nil, ErrInvalidToken
	}
	return parseNegTokenRespBody(content)
}

func parseNegTokenRespBody(inner []byte) (*SPNEGOToken, error) {
	tag, seq, _, err := derNext(inner)
	if err != nil || tag != tagSequence {
		return nil, ErrInvalidToken
	}
	out := &SPNEGOToken{State: -1}
	for len(seq) > 0 {
		var t byte
		var content []byte
		t, content, seq, err = derNext(seq)
		if err != nil {
			return nil, ErrInvalidToken
		}
		switch t {
		case tagContext0: // negState
			et, v, _, err := derNext(content)
			if err != nil || et != tagEnumerated || len(v) != 1 {
				return nil, ErrInvalidToken
			}
			out.State = NegState(v[0])
		case tagContext1: // supportedMech
			ot, oid, _, err := derNext(content)
			if err != nil || ot != tagOID {
				return nil, ErrInvalidToken
			}
			cp := make([]byte, len(oid))
			copy(cp, oid)
			out.MechTypes = append(out.MechTypes, cp)
		case tagContext2: // responseToken
			ot, tok, _, err := derNext(content)
			if err != nil || ot != tagOctetString {
				return nil, ErrInvalidToken
			}
			out.Token = tok
		case tagContext3: // mechListMIC
			ot, mic, _, err := derNext(content)
			if err != nil || ot != tagOctetString {
				return nil, ErrInvalidToken
			}
			out.MechListMIC = mic
		}
	}
	return out, nil
}
