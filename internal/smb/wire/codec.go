package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// 解码错误。所有 Decode 方法在输入不可信时都必须返回 error 而不是 panic。
var (
	// ErrTruncated 表示缓冲区长度不足以容纳所需字段。
	ErrTruncated = errors.New("smb2 wire: 报文被截断")
	// ErrStructureSize 表示 StructureSize 字段与规范固定值不符。
	ErrStructureSize = errors.New("smb2 wire: StructureSize 非法")
	// ErrInvalidOffset 表示 Offset/Length 组合越界或溢出。
	ErrInvalidOffset = errors.New("smb2 wire: Offset/Length 越界")
	// ErrProtocolID 表示 ProtocolId 魔数不匹配。
	ErrProtocolID = errors.New("smb2 wire: ProtocolId 非法")
	// ErrMalformed 表示报文结构自相矛盾。
	ErrMalformed = errors.New("smb2 wire: 报文格式错误")
)

// le 是 SMB2 报文体统一使用的字节序：**小端**（MS-SMB2 §2.1）。
// 唯一的例外是 Direct TCP 的 4 字节长度前缀（大端），那在传输层处理。
var le = binary.LittleEndian

// need 校验 b 至少有 n 字节，避免裸切片 panic。
func need(b []byte, n int) error {
	if n < 0 || len(b) < n {
		return fmt.Errorf("%w: 需要 %d 字节, 实际 %d", ErrTruncated, n, len(b))
	}
	return nil
}

// sliceAt 从 b 中安全取出 [off, off+length) 区间。
//
// off/length 来自网络，可能是任意 uint32，因此全程用 uint64 运算，
// 先判溢出与越界再切片（AGENTS.md §8）。
func sliceAt(b []byte, off, length uint64) ([]byte, error) {
	if length == 0 {
		// 长度为 0 时 offset 允许是任意值（很多客户端填 0 或填结构尾部），
		// 直接返回空切片。
		return nil, nil
	}
	end := off + length
	if end < off { // uint64 回绕
		return nil, fmt.Errorf("%w: off=%d length=%d 溢出", ErrInvalidOffset, off, length)
	}
	if end > uint64(len(b)) {
		return nil, fmt.Errorf("%w: off=%d length=%d 超出缓冲区 %d", ErrInvalidOffset, off, length, len(b))
	}
	return b[off:end], nil
}

// checkStructureSize 校验结构体首 2 字节的 StructureSize 是否等于 want。
//
// 注意 SMB2 的坑：**可变长结构的 StructureSize 是"固定部分大小 + 1"**
// （例如 CREATE Request 是 57 而非 56），want 传规范里写的那个值即可。
func checkStructureSize(b []byte, want uint16) error {
	if err := need(b, 2); err != nil {
		return err
	}
	if got := le.Uint16(b); got != want {
		return fmt.Errorf("%w: 期望 %d, 实际 %d", ErrStructureSize, want, got)
	}
	return nil
}

// grow 在 dst 尾部扩展 n 字节并返回 (新切片, 新增区间的子切片)。
// 新增区间已清零，便于直接按偏移写字段。
func grow(dst []byte, n int) ([]byte, []byte) {
	base := len(dst)
	if cap(dst)-base >= n {
		dst = dst[:base+n]
		clear(dst[base:])
	} else {
		dst = append(dst, make([]byte, n)...)
	}
	return dst, dst[base:]
}

// align8 返回 v 向上取整到 8 的倍数。SMB2 中 NegotiateContext、
// CreateContext、目录条目链都要求 8 字节对齐。
func align8(v int) int { return (v + 7) &^ 7 }

// padTo8 在 dst 尾部补 0 直到相对 base 的长度是 8 的倍数。
// base 通常是某个结构体在整条消息中的起点。
func padTo8(dst []byte, base int) []byte {
	n := align8(len(dst)-base) - (len(dst) - base)
	if n > 0 {
		dst, _ = grow(dst, n)
	}
	return dst
}

// u16 安全地把 int 转成 uint16，超界返回错误（防止编码时字段回绕）。
func u16(n int, what string) (uint16, error) {
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("%w: %s 长度 %d 超出 uint16", ErrMalformed, what, n)
	}
	return uint16(n), nil
}

// u32 安全地把 int 转成 uint32。
func u32(n int, what string) (uint32, error) {
	if n < 0 || uint64(n) > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %s 长度 %d 超出 uint32", ErrMalformed, what, n)
	}
	return uint32(n), nil
}
