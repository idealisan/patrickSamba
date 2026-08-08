//go:build !unix && !windows

package vfs

import "syscall"

// mapErrno 在非 unix / 非 windows 平台（js/wasip1/plan9 等）上不做 errno
// 级别的映射，交给 io/fs 的通用判定兜底。
func mapErrno(syscall.Errno) error { return nil }
