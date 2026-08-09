package auth

// NTLM 会话安全：签名/封装密钥派生与 GSS_GetMIC（MS-NLMP §3.4.4 / §3.4.5.2 / §3.4.5.3）。
//
// 本文件存在的唯一理由是 SPNEGO 的 mechListMIC（RFC 4178 §5）——
// SMB2 自身的报文签名与加密走 internal/smb/crypto，不使用 NTLM 的 MAC。
//
// 所有密码学原语来自 Go 标准库（crypto/md5、crypto/hmac、crypto/rc4），
// **不调用任何系统密码库**（AGENTS.md C8）。

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"errors"
)

// SIGNKEY（MS-NLMP §3.4.5.2）与 SEALKEY（§3.4.5.3）的 magic constant。
//
// **末尾的 NUL 是常量的一部分**（规范伪码里显式写了 0x00），
// 漏掉它会导出完全不同的密钥 —— 这是本处最常见的踩坑点。
const (
	clientSignMagic = "session key to client-to-server signing key magic constant\x00"
	serverSignMagic = "session key to server-to-client signing key magic constant\x00"
	clientSealMagic = "session key to client-to-server sealing key magic constant\x00"
	serverSealMagic = "session key to server-to-client sealing key magic constant\x00"
)

// SignatureLen 是 NTLMSSP_MESSAGE_SIGNATURE 的长度（MS-NLMP §2.2.2.9）。
const SignatureLen = 16

// signatureVersion 是签名结构的 Version 字段，恒为 1（小端）。
const signatureVersion uint32 = 0x00000001

// ErrNoSessionSecurity 表示未协商 NTLMSSP_NEGOTIATE_EXTENDED_SESSIONSECURITY，
// 此时 MS-NLMP §3.4.5.2 规定 SignKey 为 NIL，无法生成 HMAC 型签名。
var ErrNoSessionSecurity = errors.New("auth: extended session security not negotiated")

// SignKey 派生**单向**签名密钥（MS-NLMP §3.4.5.2 SIGNKEY）。
//
// client 为 true 取 client-to-server 方向（校验客户端送来的 MIC 时用），
// false 取 server-to-client 方向（服务端自己签名时用）。
//
// 未协商 EXTENDED_SESSIONSECURITY 时返回 nil（规范里 SignKey 为 NIL）。
func SignKey(exportedSessionKey []byte, flags NegotiateFlags, client bool) []byte {
	if !flags.Has(NegotiateExtendedSessionSecurity) {
		return nil
	}
	magic := serverSignMagic
	if client {
		magic = clientSignMagic
	}
	h := md5.New()
	h.Write(exportedSessionKey)
	h.Write([]byte(magic))
	return h.Sum(nil)
}

// SealKey 派生**单向**封装密钥（MS-NLMP §3.4.5.3 SEALKEY）。
//
// 只实现 EXTENDED_SESSIONSECURITY 分支：先按协商的密钥强度截断
// ExportedSessionKey（128 → 全 16 字节，56 → 前 7 字节，否则前 5 字节），
// 再与方向相关的 magic constant 一起做 MD5。
//
// 未协商 EXTENDED_SESSIONSECURITY 时返回 nil。
func SealKey(exportedSessionKey []byte, flags NegotiateFlags, client bool) []byte {
	if !flags.Has(NegotiateExtendedSessionSecurity) {
		return nil
	}
	n := len(exportedSessionKey)
	switch {
	case flags.Has(Negotiate128):
		// 用全长密钥。
	case flags.Has(Negotiate56):
		n = 7
	default:
		n = 5
	}
	if n > len(exportedSessionKey) {
		n = len(exportedSessionKey)
	}
	magic := serverSealMagic
	if client {
		magic = clientSealMagic
	}
	h := md5.New()
	h.Write(exportedSessionKey[:n])
	h.Write([]byte(magic))
	return h.Sum(nil)
}

// SigningContext 是 NTLM 会话安全的**单向**状态（MS-NLMP §3.4）。
//
// 客户端→服务端与服务端→客户端各持一个：RC4 封装句柄是**有状态**的流，
// 每次使用都会推进 keystream，绝不能每条消息新建。序号同样是单向独立计数。
//
// 非并发安全，由单个会话串行使用。
type SigningContext struct {
	signKey []byte
	// seal 只在协商了 NTLMSSP_NEGOTIATE_KEY_EXCH 时非空。
	seal   *rc4.Cipher
	seqNum uint32
}

// NewSigningContext 由 ExportedSessionKey 与协商标志创建单向会话安全状态。
//
// client 为 true 表示 client-to-server 方向。
// 未协商 EXTENDED_SESSIONSECURITY 时返回 ErrNoSessionSecurity。
func NewSigningContext(exportedSessionKey []byte, flags NegotiateFlags, client bool) (*SigningContext, error) {
	sign := SignKey(exportedSessionKey, flags, client)
	if sign == nil {
		return nil, ErrNoSessionSecurity
	}
	s := &SigningContext{signKey: sign}
	// MS-NLMP §3.4.4.2：只有置了 KEY_EXCH 才会用 RC4 再加密一次 checksum。
	if flags.Has(NegotiateKeyExch) {
		c, err := rc4.NewCipher(SealKey(exportedSessionKey, flags, client))
		if err != nil {
			return nil, err
		}
		s.seal = c
	}
	return s, nil
}

// MIC 计算一条消息的 NTLMSSP_MESSAGE_SIGNATURE 并递增序号
// （MS-NLMP §3.4.4.2 With Extended Session Security，即 GSS_GetMIC 的输出）：
//
//	Version(4B 小端 = 1) || Checksum(8B) || SeqNum(4B 小端)
//	Checksum = HMAC_MD5(SignKey, SeqNum_le32 || Message)[0:8]
//	若协商了 KEY_EXCH：Checksum = RC4(SealHandle, Checksum)
func (s *SigningContext) MIC(message []byte) [SignatureLen]byte {
	var out [SignatureLen]byte
	binary.LittleEndian.PutUint32(out[0:4], signatureVersion)

	var seq [4]byte
	binary.LittleEndian.PutUint32(seq[:], s.seqNum)

	h := hmac.New(md5.New, s.signKey)
	h.Write(seq[:])
	h.Write(message)
	copy(out[4:12], h.Sum(nil)[:8])

	if s.seal != nil {
		s.seal.XORKeyStream(out[4:12], out[4:12])
	}
	copy(out[12:16], seq[:])

	s.seqNum++
	return out
}
