package wire

import (
	"fmt"
	"unicode/utf16"
)

// SMB2 线上的所有字符串都是 **UTF-16LE**，无 BOM、通常无结尾 NUL
// （MS-SMB2 §2.2.9 TREE_CONNECT Path、§2.2.13 CREATE Name 等）。

// DecodeUTF16LE 把 UTF-16LE 字节序列转成 Go string。
//
// 长度为奇数时返回错误（不能静默丢弃半个码元，那会掩盖协议错误）。
// 未配对的代理项由 utf16.Decode 替换为 U+FFFD，不报错 —— 真实客户端
// 偶尔会发出这种串，拒绝服务不划算。
func DecodeUTF16LE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("%w: UTF-16LE 长度 %d 为奇数", ErrMalformed, len(b))
	}
	if len(b) == 0 {
		return "", nil
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = le.Uint16(b[i*2:]) // 小端
	}
	return string(utf16.Decode(u)), nil
}

// DecodeUTF16LEAt 从 b 中按 (offset, length) 取出一段 UTF-16LE 字符串。
// offset 相对 b 的起点。length 为 0 时返回空串。
func DecodeUTF16LEAt(b []byte, off, length uint64) (string, error) {
	s, err := sliceAt(b, off, length)
	if err != nil {
		return "", err
	}
	return DecodeUTF16LE(s)
}

// EncodeUTF16LE 把 Go string 编码为 UTF-16LE 字节（无结尾 NUL）。
func EncodeUTF16LE(s string) []byte {
	if s == "" {
		return nil
	}
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		le.PutUint16(b[i*2:], v) // 小端
	}
	return b
}

// AppendUTF16LE 把 s 的 UTF-16LE 编码追加到 dst，返回新切片与写入字节数。
func AppendUTF16LE(dst []byte, s string) ([]byte, int) {
	if s == "" {
		return dst, 0
	}
	u := utf16.Encode([]rune(s))
	dst, buf := grow(dst, len(u)*2)
	for i, v := range u {
		le.PutUint16(buf[i*2:], v) // 小端
	}
	return dst, len(u) * 2
}

// UTF16LELen 返回 s 编码为 UTF-16LE 后的字节数（BMP 字符 2 字节，
// 增补平面字符 4 字节）。
func UTF16LELen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 4
		} else {
			n += 2
		}
	}
	return n
}
