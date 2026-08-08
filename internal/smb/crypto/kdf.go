package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// SMB2 方言常量（MS-SMB2 §2.2.3 SMB2 NEGOTIATE Request — Dialects）。
//
// 本包刻意不 import internal/smb/wire（避免循环依赖），调用方传 uint16 即可。
const (
	DialectSMB202 uint16 = 0x0202
	DialectSMB210 uint16 = 0x0210
	DialectSMB300 uint16 = 0x0300
	DialectSMB302 uint16 = 0x0302
	DialectSMB311 uint16 = 0x0311
)

// SessionKeyLen 是 SMB2 Session.SessionKey 的固定长度（MS-SMB2 §3.2.5.3.1）：
// 认证子系统给出的 key 不足 16 字节补零，超过则截断。
const SessionKeyLen = 16

// SP800-108 KDF 的 Label / Context 常量。
//
// 全部**大小写敏感**，且除 3.1.1 的 Context（preauth 哈希）外都带结尾 NUL。
// 出处：MS-SMB2 §3.1.4.2 Generating Cryptographic Keys。
var (
	// SMB 3.0 / 3.0.2
	labelSMB2AESCMAC = []byte("SMB2AESCMAC\x00")
	contextSmbSign   = []byte("SmbSign\x00")
	labelSMB2APP     = []byte("SMB2APP\x00")
	contextSmbRpc    = []byte("SmbRpc\x00")
	labelSMB2AESCCM  = []byte("SMB2AESCCM\x00")
	contextServerOut = []byte("ServerOut\x00")
	// 注意 "ServerIn " 结尾有一个**空格**，再跟 NUL —— 这是规范原文，不是笔误。
	contextServerIn = []byte("ServerIn \x00")

	// SMB 3.1.1（Context 为 preauth integrity hash）
	labelSMBSigningKey   = []byte("SMBSigningKey\x00")
	labelSMBAppKey       = []byte("SMBAppKey\x00")
	labelSMBS2CCipherKey = []byte("SMBS2CCipherKey\x00")
	labelSMBC2SCipherKey = []byte("SMBC2SCipherKey\x00")
)

// KDF 实现 NIST SP 800-108 的 KDF in Counter Mode，PRF = HMAC-SHA256，
// 计数器 r = 32 位，计数器位于最前。
//
// 单次迭代的输入为：
//
//	[i]_32(大端) || Label || 0x00 || Context || [L]_32(大端)
//
// lBits 是期望输出的**比特**数（SMB 用 128 或 256）。
func KDF(ki, label, context []byte, lBits int) []byte {
	if lBits <= 0 {
		return nil
	}
	outLen := (lBits + 7) / 8

	var fixed []byte
	fixed = append(fixed, label...)
	fixed = append(fixed, 0x00)
	fixed = append(fixed, context...)
	var lbuf [4]byte
	binary.BigEndian.PutUint32(lbuf[:], uint32(lBits))
	fixed = append(fixed, lbuf[:]...)

	out := make([]byte, 0, outLen+sha256.Size)
	var ctr [4]byte
	for i := uint32(1); len(out) < outLen; i++ {
		binary.BigEndian.PutUint32(ctr[:], i)
		h := hmac.New(sha256.New, ki)
		h.Write(ctr[:])
		h.Write(fixed)
		out = h.Sum(out)
	}
	return out[:outLen]
}

// normalizeSessionKey 把认证得到的 session key 规整为 16 字节
// （不足补零、超长截断），MS-SMB2 §3.2.5.3.1 / §3.3.5.5.3。
func normalizeSessionKey(sessionKey []byte) []byte {
	k := make([]byte, SessionKeyLen)
	copy(k, sessionKey)
	return k
}

// SigningKey 派生 Session.SigningKey（16 字节）。
//
//   - 3.1.1：Label "SMBSigningKey"，Context = preauth integrity hash
//   - 3.0 / 3.0.2：Label "SMB2AESCMAC"，Context "SmbSign"
//   - 2.x：不派生，直接使用 Session.SessionKey（HMAC-SHA256 签名）
func SigningKey(dialect uint16, sessionKey, preauthHash []byte) []byte {
	ki := normalizeSessionKey(sessionKey)
	switch {
	case dialect >= DialectSMB311:
		return KDF(ki, labelSMBSigningKey, preauthHash, 128)
	case dialect >= DialectSMB300:
		return KDF(ki, labelSMB2AESCMAC, contextSmbSign, 128)
	default:
		return ki
	}
}

// ApplicationKey 派生 Session.ApplicationKey（16 字节）。
// 2.x 无此密钥，返回规整后的 SessionKey。
func ApplicationKey(dialect uint16, sessionKey, preauthHash []byte) []byte {
	ki := normalizeSessionKey(sessionKey)
	switch {
	case dialect >= DialectSMB311:
		return KDF(ki, labelSMBAppKey, preauthHash, 128)
	case dialect >= DialectSMB300:
		return KDF(ki, labelSMB2APP, contextSmbRpc, 128)
	default:
		return ki
	}
}

// ServerOutKey 派生**服务端发往客户端（S→C）**方向的加密密钥，
// 即服务端的 Session.ServerOutKey / 客户端的 ServerOut 解密密钥。
//
// keyLen 取 16（AES-128-CCM/GCM）或 32（AES-256-CCM/GCM，仅 3.1.1）。
// 非 3.1.1 方言恒为 16 字节。
func ServerOutKey(dialect uint16, sessionKey, preauthHash []byte, keyLen int) []byte {
	return cipherKey(dialect, sessionKey, preauthHash, keyLen,
		labelSMBS2CCipherKey, contextServerOut)
}

// ServerInKey 派生**客户端发往服务端（C→S）**方向的密钥，
// 服务端用它**解密**收到的 TRANSFORM 报文。
func ServerInKey(dialect uint16, sessionKey, preauthHash []byte, keyLen int) []byte {
	return cipherKey(dialect, sessionKey, preauthHash, keyLen,
		labelSMBC2SCipherKey, contextServerIn)
}

func cipherKey(dialect uint16, sessionKey, preauthHash []byte, keyLen int,
	label311, context30x []byte) []byte {
	ki := normalizeSessionKey(sessionKey)
	if dialect >= DialectSMB311 {
		if keyLen != 32 {
			keyLen = 16
		}
		return KDF(ki, label311, preauthHash, keyLen*8)
	}
	// 3.0 / 3.0.2 只有 AES-128-CCM，密钥恒为 16 字节。
	return KDF(ki, labelSMB2AESCCM, context30x, 128)
}
