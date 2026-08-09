//go:build unix

package builtin

// inode_unix.go —— 从 os.FileInfo 里取 inode。
//
// stat 是 C9 明确允许的「普通文件系统」能力，取 st_ino 不需要任何可选特性。
// 但**不假设它一定可用**：ports.go 的调用方拿到 (0,false) 时会退到库分配的号。

import (
	"os"
	"syscall"
)

func inodeOf(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	ino := uint64(st.Ino)
	// 0 是「没有 inode 概念」的常见占位；≥ 2^63 会撞进库分配号段，
	// 两种都判为不可用，转走分配路径。
	if ino == 0 || ino >= idFallbackBase {
		return 0, false
	}
	return ino, true
}
