package auth

// NTLMv2 服务端校验（MS-NLMP §3.3.2 / §3.4）。
//
// 所有密码学原语来自 Go 标准库（crypto/md5、crypto/hmac、crypto/rc4）
// 与本包自实现的 MD4，**不调用任何系统密码库**（AGENTS.md C8）。

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"crypto/subtle"
	"encoding/binary"
	"strings"
	"time"
)

// NTProofStrLen 是 NTLMv2 响应里 NTProofStr 的长度（MS-NLMP §2.2.2.8）。
const NTProofStrLen = 16

// SessionKeyLen 是 NTLM ExportedSessionKey 的长度。
const SessionKeyLen = 16

// NTHash 计算 NT hash = MD4(UTF16LE(password))（MS-NLMP §3.3.1 NTOWFv1）。
//
// 口令**绝不落日志**，调用方拿到的 hash 也不应打印。
func NTHash(password string) [16]byte {
	return md4Sum(utf16le(password))
}

// NTOWFv2 = HMAC_MD5(NTHash, UTF16LE(UPPER(user) + domain))（MS-NLMP §3.3.2）。
//
// 注意：**用户名转大写，域名保持原样**。这是规范原文，不是笔误。
func NTOWFv2(ntHash [16]byte, user, domain string) [16]byte {
	h := hmac.New(md5.New, ntHash[:])
	h.Write(utf16le(strings.ToUpper(user)))
	h.Write(utf16le(domain))
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ComputeNTProofStr = HMAC_MD5(NTOWFv2, ServerChallenge || blob)（MS-NLMP §3.3.2）。
//
// blob 即 NtChallengeResponse 去掉前 16 字节 NTProofStr 后的剩余部分（temp）。
func ComputeNTProofStr(ntowf [16]byte, serverChallenge [8]byte, blob []byte) [16]byte {
	h := hmac.New(md5.New, ntowf[:])
	h.Write(serverChallenge[:])
	h.Write(blob)
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// SessionBaseKey = HMAC_MD5(NTOWFv2, NTProofStr)（MS-NLMP §3.3.2）。
func SessionBaseKey(ntowf [16]byte, ntProofStr [16]byte) [16]byte {
	h := hmac.New(md5.New, ntowf[:])
	h.Write(ntProofStr[:])
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// VerifyNTLMv2 校验 NtChallengeResponse。
//
// 返回 SessionBaseKey、blob（temp，供解析 AV_PAIR）与校验结果。
// 比较使用 crypto/subtle 常量时间比较（AGENTS.md §8）。
//
// 校验失败时 sessionBaseKey 无意义，调用方不得使用。
func VerifyNTLMv2(ntowf [16]byte, serverChallenge [8]byte, ntResponse []byte) (
	sessionBaseKey [16]byte, blob []byte, ok bool) {

	if len(ntResponse) < NTProofStrLen {
		return sessionBaseKey, nil, false
	}
	blob = ntResponse[NTProofStrLen:]
	want := ComputeNTProofStr(ntowf, serverChallenge, blob)
	if subtle.ConstantTimeCompare(want[:], ntResponse[:NTProofStrLen]) != 1 {
		return sessionBaseKey, blob, false
	}
	var proof [16]byte
	copy(proof[:], ntResponse[:NTProofStrLen])
	return SessionBaseKey(ntowf, proof), blob, true
}

// KeyExchangeKey 返回 NTLMv2 的 KeyExchangeKey。
//
// MS-NLMP §3.4.5.1 KXKEY：使用 NTLMv2（即 extended session security）时
// KeyExchangeKey 恒等于 SessionBaseKey。
func KeyExchangeKey(sessionBaseKey [16]byte) [16]byte { return sessionBaseKey }

// ExportedSessionKey 由 KeyExchangeKey 与 AUTHENTICATE 中的
// EncryptedRandomSessionKey 推出（MS-NLMP §3.4.5.1 / §3.1.5.1.2）。
//
//   - 未置 NTLMSSP_NEGOTIATE_KEY_EXCH：ExportedSessionKey = KeyExchangeKey
//   - 置位：ExportedSessionKey = RC4K(KeyExchangeKey, EncryptedRandomSessionKey)
//
// 这个值就是 SMB2 的 Session.SessionKey（MS-SMB2 §3.3.5.5.3）。
func ExportedSessionKey(kxKey [16]byte, flags NegotiateFlags, encrypted []byte) ([16]byte, error) {
	if !flags.Has(NegotiateKeyExch) || len(encrypted) == 0 {
		return kxKey, nil
	}
	var out [16]byte
	if len(encrypted) != SessionKeyLen {
		// 长度不对就当作非法 token，绝不越界切片。
		return out, ErrInvalidToken
	}
	c, err := rc4.NewCipher(kxKey[:])
	if err != nil {
		return out, err
	}
	c.XORKeyStream(out[:], encrypted)
	return out, nil
}

// ComputeMIC 计算 AUTHENTICATE_MESSAGE 的 MIC（MS-NLMP §3.1.5.1.2）：
//
//	MIC = HMAC_MD5(ExportedSessionKey,
//	               NEGOTIATE_MESSAGE || CHALLENGE_MESSAGE || AUTHENTICATE_MESSAGE)
//
// 其中 AUTHENTICATE_MESSAGE 的 MIC 字段在计算时必须为全零。
func ComputeMIC(exportedSessionKey [16]byte, negotiate, challenge, authenticate []byte) [MICSize]byte {
	h := hmac.New(md5.New, exportedSessionKey[:])
	h.Write(negotiate)
	h.Write(challenge)
	h.Write(authenticate)
	var out [MICSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ---------------------------------------------------------------- 时间戳

// unixToFiletime 把 Go 时间转为 FILETIME（1601-01-01 UTC 起的 100ns 数）。
//
// MS-DTYP §2.3.3。差值 116444736000000000 是 1601→1970 的 100ns 数。
const filetimeEpochDelta = 116444736000000000

func filetimeNow(t time.Time) uint64 {
	return uint64(t.UTC().UnixNano()/100) + filetimeEpochDelta
}

// encodeFiletime 把 FILETIME 编码为 8 字节**小端**（AV_PAIR MsvAvTimestamp）。
func encodeFiletime(v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return b[:]
}
