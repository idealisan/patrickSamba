//go:build !unix

package builtin

// inode_other.go —— 非 Unix 平台（Windows、wasm 等）没有可从 os.FileInfo 直接
// 取到的 inode，一律走库分配号。
//
// Windows 上真正的稳定 ID 要 GetFileInformationByHandle / FILE_ID_INFO，
// 那是 native 适配器的活 —— builtin 的前提是「只用普通文件」，
// 在这里去调平台 API 会把两侧的边界搞混。

import "os"

func inodeOf(os.FileInfo) (uint64, bool) { return 0, false }
