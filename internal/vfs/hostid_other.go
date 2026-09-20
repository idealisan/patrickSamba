//go:build !windows

package vfs

import "os"

// hostid_other.go —— POSIX 侧 hostIdentity 的空实现。
//
// POSIX 的 lstat 已经给出 st_blocks（真实分配长度）与 st_nlink（硬链接数），
// 两者都由 fillSysAttr 直接填好了，不需要再查一次。FileID 走 oscap 的
// StableFileID 能力（native 给 inode，builtin 给旁路分配号），同样不经这里。
//
// 于是返回 ok=false 表示「没有额外信息要补」，调用方保留原值。

func hostIdentityFromFile(*os.File) (hostIdentity, bool) { return hostIdentity{}, false }

func hostIdentityAt(string) (hostIdentity, bool) { return hostIdentity{}, false }
