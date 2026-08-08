package dcerpc

import (
	"encoding/hex"
	"fmt"
)

// UUID 是 16 字节的 DCE/RPC 接口或语法标识符（C706 §14.6 / MS-RPCE §2.2.2.5）。
// 内部以**规范线格式**存储：前 4 字节（Data1）小端、接着 2 字节（Data2）小端、
// 再 2 字节（Data3）小端，最后 8 字节（Data4）大端 —— 这是 PDU 里直接出现的字节序。
type UUID [16]byte

// ParseUUID 从形如 "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" 的字符串解析。
// 大括号可选。结果转换为 DCERPC 的**规范线格式**：Data1/Data2/Data3 以小端存储，
// Data4 以大端存储（DCE RPC UUID 规范；MS-RPCE §2.2.2.5）。这正是 PDU 中出现的字节序。
func ParseUUID(s string) (UUID, error) {
	var u UUID
	// 去掉花括号与短横线
	clean := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '{' || c == '}' || c == '-' {
			continue
		}
		clean = append(clean, c)
	}
	if len(clean) != 32 {
		return u, fmt.Errorf("dcerpc: UUID 字符串长度 %d 非法: %q", len(clean), s)
	}
	raw, err := hex.DecodeString(string(clean))
	if err != nil {
		return u, fmt.Errorf("dcerpc: UUID 十六进制解析失败 %q: %w", s, err)
	}
	// raw 是每字段大端的 16 字节；转成线格式（前三字段小端、Data4 大端）。
	u[0], u[1], u[2], u[3] = raw[3], raw[2], raw[1], raw[0] // Data1 LE
	u[4], u[5] = raw[5], raw[4]                             // Data2 LE
	u[6], u[7] = raw[7], raw[6]                             // Data3 LE
	copy(u[8:16], raw[8:16])                                // Data4 BE
	return u, nil
}

// mustParseUUID 用于编译期常量定义；解析失败直接 panic（仅在包初始化时调用）。
func mustParseUUID(s string) UUID {
	u, err := ParseUUID(s)
	if err != nil {
		panic(fmt.Sprintf("dcerpc: 内部 UUID 非法 %q: %v", s, err))
	}
	return u
}

// ParseUUIDBytes 从恰好 16 字节的线格式切片构造 UUID（不解析大小端）。
func ParseUUIDBytes(b []byte) UUID {
	var u UUID
	copy(u[:], b)
	return u
}

// String 还原为 "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" 形式（与输入格式一致）。
func (u UUID) String() string {
	raw := make([]byte, 16)
	raw[0], raw[1], raw[2], raw[3] = u[3], u[2], u[1], u[0] // Data1 还原大端
	raw[4], raw[5] = u[5], u[4]                             // Data2
	raw[6], raw[7] = u[7], u[6]                             // Data3
	copy(raw[8:16], u[8:16])                                // Data4
	h := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// Equal 比较两 UUID 是否相等。
func (u UUID) Equal(o UUID) bool {
	return u == o
}
