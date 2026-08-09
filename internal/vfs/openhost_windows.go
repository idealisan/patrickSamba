//go:build windows

package vfs

import "os"

// openHostFile 是「打开宿主文件」的统一接缝（openHostFile seam）。
// 总说明见 openhost_unix.go 顶部：为什么要把所有宿主文件打开收口到这里。
//
// 第一步（本版本）实现：直接转调 os.OpenFile，**行为与原先完全一致**。
//
// TODO(第二步): 在这里换成 CreateFileW 路径 ——
//  1. p, err := winOpenParamsFor(flag, perm)            // 见 winopen.go
//     算出 Access / ShareMode / CreateDisposition / dwFlagsAndAttributes；
//  2. h, err := windows.CreateFileW(...)；
//  3. f := os.NewFile(uintptr(h), host)。
//
// 第二步**暂缓**：整个方案的存亡假设「os.NewFile(handle) 返回的 *os.File
// 能正常支撑 Seek / ReadAt / WriteAt / Truncate」无法用任何静态手段验证，
// 本项目没有 Windows runner，写下去等于赌一个没法验的假设。详见 PR 描述的
// known gap 一节。另见 sys_windows.go 的 openNoFollow = 0（Windows 上符号
// 链接逃逸防护缺口，同样待第二步用 FILE_FLAG_OPEN_REPARSE_POINT 补）。
func openHostFile(host string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(host, flag, perm)
}
