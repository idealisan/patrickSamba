//go:build windows

package vfs

import (
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// fillSysAttr 从 Win32FileAttributeData 补齐字段。
//
// Windows 宿主上「原生就有」而 POSIX 需要合成的：真实创建时间、DOS 属性位。
// 反过来「原生没有」而 POSIX 有的：uid / gid / mode ——
// 这三个交给 MetadataStore 旁路存储（AGENTS.md §5 P7），不在这里处理。
func fillSysAttr(fi fs.FileInfo, a *Attr) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return
	}
	// NTFS 原生的 DOS 属性位直接采信。attr.go 的 dosAttributes 会在此基础上
	// 补 READONLY（只读共享）等，不会把它清掉。
	a.FileAttributes = d.FileAttributes
	a.CreateTime = time.Unix(0, d.CreationTime.Nanoseconds())
	a.AccessTime = time.Unix(0, d.LastAccessTime.Nanoseconds())
	a.WriteTime = time.Unix(0, d.LastWriteTime.Nanoseconds())
	// Windows 没有 POSIX 的 ctime（inode 变更时间），用 mtime 顶替。
	a.ChangeTime = a.WriteTime
	// FileID 需要打开句柄才能取（见 fillSysAttrFromFile）。
	// 仅凭路径的 stat 拿不到，留 0 表示未知。
	a.NLink = 1
}

// fillSysAttrFromFile 用已打开的句柄补齐 FileID 与硬链接数。
//
// NTFS 的 FileIndex（64 位）在卷内稳定唯一，正是 SMB 的
// FileInternalInformation / QFid create context 需要的东西，
// 所以 Windows 上**不需要**旁路存储来伪造 FileID。
func fillSysAttrFromFile(f *os.File, a *Attr) {
	if f == nil {
		return
	}
	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &bhfi); err != nil {
		return
	}
	a.FileID = uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow)
	if bhfi.NumberOfLinks > 0 {
		a.NLink = bhfi.NumberOfLinks
	}
}

// statCreateTime 在 Windows 上是多余的（CreationTime 已在 fillSysAttr 里取到）。
func statCreateTime(string) (time.Time, bool) { return time.Time{}, false }

// fileAccessTime 从 FileInfo 取当前 atime（sticky write time 补偿用，
// 见 attr_linux.go 同名函数的说明）。
func fileAccessTime(fi fs.FileInfo) (time.Time, bool) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, d.LastAccessTime.Nanoseconds()), true
}
