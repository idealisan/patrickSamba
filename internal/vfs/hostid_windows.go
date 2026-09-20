//go:build windows

package vfs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostid_windows.go —— hostIdentity 的 Windows 取值侧。
//
// 为什么需要它：Windows 的 os.Stat 只给出 Win32FileAttributeData，
// **没有**真实分配长度（要 FILE_STANDARD_INFO）、也没有硬链接数与文件索引
// （要已打开的句柄）。缺了这三项，vfs 侧会：
//   - 把分配长度当成「逻辑长度向上取整」，于是 SPARSE 位（Alloc < Size）
//     在 Windows 上永远报不出来；
//   - 硬链接数恒为 1，用量统计的硬链接去重（usage.go 的 `NLink > 1` 门槛）
//     永不生效，配额账目把硬链接重复计数。
//
// 全部是「按路径/句柄查一次 Win32」的薄封装，判定与合并在 attr.go。

// fileStandardInfo 对应 winbase.h 的 FILE_STANDARD_INFO。
//
// x/sys@v0.47.0 只导出了 InfoClass 常量（FileStandardInfo = 1），没导出这个
// 结构体，按定义手写。字段偏移与 C 侧一致：
//
//	AllocationSize@0、EndOfFile@8、NumberOfLinks@16、
//	DeletePending@20、Directory@21（BOOLEAN 各占 1 字节，尾部补齐到 24）
type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// hostIdentityFromHandle 从一个已打开的句柄取补充信息。
func hostIdentityFromHandle(h windows.Handle) (hostIdentity, bool) {
	var info fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return hostIdentity{}, false
	}
	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
		return hostIdentity{}, false
	}
	return hostIdentity{
		Alloc:    info.AllocationSize,
		HasAlloc: true,
		NLink:    info.NumberOfLinks,
		FileID:   uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow),
	}, true
}

// hostIdentityFromFile 复用调用方已经打开的句柄，不再开一次。
func hostIdentityFromFile(f *os.File) (hostIdentity, bool) {
	if f == nil {
		return hostIdentity{}, false
	}
	return hostIdentityFromHandle(windows.Handle(f.Fd()))
}

// hostIdentityAt 按路径查一次。句柄路径调用方（local_handle.go）应当优先用
// hostIdentityFromFile，别再开一次。
//
// ShareMode 给全（含 DELETE），并且带 FILE_FLAG_BACKUP_SEMANTICS
// （不带它打不开目录句柄）—— 我们这次纯查询不能挡住别人的改名/删除。
func hostIdentityAt(path string) (hostIdentity, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return hostIdentity{}, false
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return hostIdentity{}, false
	}
	defer windows.CloseHandle(h)
	return hostIdentityFromHandle(h)
}
