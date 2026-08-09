//go:build !windows

package vfs

import "os"

// openHostFile 是「打开宿主文件」的统一接缝（openHostFile seam）。
//
// 把 vfs 内所有直接落盘的宿主文件打开收口到这唯一一个函数，是为了让
// Windows 平台将来能在这里统一换成 CreateFileW（见 openhost_windows.go 与
// winopen.go 的 winOpenParamsFor），而不必在 openDir / openFile / 目录快照 /
// 大小写扫描 / AppleDouble 各处各写一份 SMB 语义（disposition / attr-only /
// 截断）——那会把同一套语义复制两份、两份各自漂移，违反 AGENTS.md §5 P7
// 「平台差异用 build tag 隔离，不要让兼容层污染主路径」。
//
// 本文件（unix）只是 os.OpenFile 的薄包，零行为变化；现有用例直接回归。
func openHostFile(host string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(host, flag, perm)
}
