//go:build !linux && !darwin && !windows

package vfs

// 非目标平台（AGENTS.md C7 只要求 linux/darwin/windows）的兜底：
// 没有空洞探测能力，一律报「整段已分配」。多报是安全的 ——
// 客户端最多多读一遍零，不会丢数据。

import "os"

func platformAllocatedRanges(_ *os.File, off, end int64) ([]Range, error) {
	return wholeRange(off, end), nil
}

// 语义与 POSIX 一致，理由见 sparse_unix.go 的 platformSetSparse。
func platformSetSparse(_ *os.File, v bool) error {
	if !v {
		return ErrNotSupported
	}
	return nil
}
