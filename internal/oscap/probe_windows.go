//go:build windows

package oscap

import (
	"os"

	"golang.org/x/sys/windows"
)

// fsctlSetSparse 对应 winioctl.h 的 FSCTL_SET_SPARSE
// （MS-FSCC §2.3.69）。NTFS/ReFS 支持，FAT32/exFAT 会失败。
const fsctlSetSparse = 0x000900C4

func probeNative(c Capability, o Options) bool {
	switch c {
	case CapXattr:
		// Windows 没有 POSIX 扩展属性。
		//
		// 别把 NTFS 的 ADS 当成 xattr 的等价物拿来"支持"这一项：
		// 二者语义不同（ADS 是可任意大的数据流，xattr 是小块属性，
		// 且枚举方式、大小限制、命名规则都不一样）。真要在 Windows 上
		// 提供 xattr 语义，那是 builtin 用旁路存储做的事。
		return false

	case CapNamedStream:
		return probeADS(o.Root)

	case CapSparseFile:
		return probeSparseWindows(o.Root)

	case CapStableFileID:
		return probeFileIndex(o.Root)

	case CapCreationTime:
		// Win32 的文件时间三元组本来就含 CreationTime，FAT 与 NTFS 都有。
		// 只确认 root 可 stat，避免对一个根本不存在的路径报 true。
		_, err := os.Stat(o.Root)
		return err == nil

	case CapDOSAttributes:
		// DOS 属性位是 FAT 时代就有的东西，Windows 上任何文件系统都支持。
		return probeFileAttributes(o.Root)
	}
	return false
}

// probeADS 探测 alternate data stream。
//
// 用真实打开一个流来判定，而不是靠「看起来像 NTFS」：
// 挂在 Windows 上的 FAT32/exFAT 外置盘会在这里失败，而那恰恰是
// AGENTS.md §1.2 点名要覆盖的场景。
func probeADS(root string) bool {
	return withProbeFile(root, func(f *os.File) bool {
		// 流随宿主文件一起被删除，无需单独清理。
		s, err := os.OpenFile(f.Name()+":stupidsambacapprobe",
			os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return false
		}
		_ = s.Close()
		return true
	})
}

// probeSparseWindows 探测稀疏文件支持。
//
// FSCTL_SET_SPARSE 是 NTFS 打洞（FSCTL_SET_ZERO_DATA）的前置条件：
// 没有它，写零就是老老实实写一片零，簇一个都不会被释放。
func probeSparseWindows(root string) bool {
	return withProbeFile(root, func(f *os.File) bool {
		var ret uint32
		err := windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse,
			nil, 0, nil, 0, &ret, nil)
		return err == nil
	})
}

// probeFileIndex 探测能否拿到稳定的文件 ID。
//
// NTFS 的 file reference number 稳定且跨重命名不变；FAT 上
// GetFileInformationByHandle 返回的索引是 0（"文件系统不支持"的表示法），
// 那种值绝不能拿来当 FileID —— 所有对象都会撞成同一个。
func probeFileIndex(root string) bool {
	return withProbeFile(root, func(f *os.File) bool {
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
			return false
		}
		return info.FileIndexHigh != 0 || info.FileIndexLow != 0
	})
}

// probeFileAttributes 确认能读到 root 的 DOS 属性位。
func probeFileAttributes(root string) bool {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	return err == nil && attrs != windows.INVALID_FILE_ATTRIBUTES
}
