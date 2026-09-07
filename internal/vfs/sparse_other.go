//go:build !linux && !darwin && !windows

package vfs

// ⚠️ **本文件当前没有调用点 —— 改这里不会改变任何行为。**
//
// 本文件的 platformAllocatedRanges / platformSetSparse 曾打算作为"非目标平台
// （AGENTS.md C7 只要求 linux/darwin/windows）的兜底"，但真实路径后来整体
// 迁到了 oscap 层：
//
//	vfs/optional.go → fs.caps.Sparse() → oscap/native/sparse_linux.go
//	                                   → oscap/native/sparse_windows.go
//	                                   → oscap/builtin/sparse.go（无原生能力时）
//
// "没有空洞探测能力就一律报整段已分配"这个兜底语义，现在由
// oscap/builtin/sparse.go 承担（多报是安全的：客户端最多多读一遍零，不会丢数据）。
//
// 保留本文件只为不违反项目禁止删除的约定（AGENTS.md §7.5），**不要照着它
// 判断运行期行为**，也不要往这里加新逻辑 —— 加了不会生效。

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
