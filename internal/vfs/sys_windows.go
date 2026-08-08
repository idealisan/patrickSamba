//go:build windows

package vfs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGetDiskFreeSpaceW 手动绑定 —— x/sys/windows 只导出了
// GetDiskFreeSpaceEx，没有导出给出簇粒度的 GetDiskFreeSpaceW。
// NewLazySystemDLL 是纯 Go 的动态符号解析，不违反 AGENTS.md C2
// （kernel32 是 Windows 的系统 API 面，不是第三方动态库）。
var procGetDiskFreeSpaceW = windows.NewLazySystemDLL("kernel32.dll").
	NewProc("GetDiskFreeSpaceW")

// platformStatFS 用 GetDiskFreeSpaceEx + GetDiskFreeSpaceW 取容量信息。
//
// 两个 API 各取所需：
//   - GetDiskFreeSpaceEx 给出**字节数**且能正确处理 > 2TB 的卷与磁盘配额；
//   - GetDiskFreeSpaceW 给出扇区/簇的粒度，SMB 的
//     FileFsSizeInformation 需要 SectorsPerAllocationUnit/BytesPerSector。
func platformStatFS(path string, info *FSInfo) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ErrInvalidPath
	}

	var cluster uint64 = 4096 // NTFS 默认簇大小，取不到时的兜底
	var sectorsPerCluster, bytesPerSector, freeClusters, totalClusters uint32
	r, _, _ := procGetDiskFreeSpaceW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&sectorsPerCluster)),
		uintptr(unsafe.Pointer(&bytesPerSector)),
		uintptr(unsafe.Pointer(&freeClusters)),
		uintptr(unsafe.Pointer(&totalClusters)),
	)
	if r != 0 && sectorsPerCluster > 0 && bytesPerSector > 0 {
		cluster = uint64(sectorsPerCluster) * uint64(bytesPerSector)
	}
	info.BlockSize = uint32(cluster)

	var freeAvail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &totalBytes, &totalFree); err != nil {
		return mapError(err)
	}
	info.TotalBlocks = totalBytes / cluster
	info.FreeBlocks = totalFree / cluster
	info.AvailBlocks = freeAvail / cluster
	info.MaxComponentLen = MaxComponentLen
	return nil
}

// platformFullSync：Windows 的 FlushFileBuffers 本来就是「刷到介质」的语义，
// 没有 macOS 那种两级刷盘的区分。
func platformFullSync(f *os.File) error {
	return f.Sync()
}

// fsctlSetSparse 与 fsctlSetZeroData 的控制码（winioctl.h）。
//
//	FSCTL_SET_SPARSE    = CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 49, METHOD_BUFFERED, FILE_SPECIAL_ACCESS)
//	FSCTL_SET_ZERO_DATA = CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 50, METHOD_BUFFERED, FILE_WRITE_DATA)
const (
	fsctlSetSparse   uint32 = 0x000900C4
	fsctlSetZeroData uint32 = 0x000980C8
)

// fileZeroDataInformation 对应 winioctl.h 的 FILE_ZERO_DATA_INFORMATION。
type fileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

// platformPunchHole 用 FSCTL_SET_ZERO_DATA 打洞。
//
// 前提是文件已被标记为 sparse（FSCTL_SET_SPARSE），否则 NTFS 会老老实实
// 写一片零而不是释放簇。这里每次都先标记一次 —— 幂等，且比记状态可靠。
func platformPunchHole(f *os.File, off, length int64) error {
	if length <= 0 {
		return nil
	}
	h := windows.Handle(f.Fd())
	var ret uint32
	// 标记稀疏失败不致命（例如卷是 FAT32），继续尝试 ZERO_DATA。
	_ = windows.DeviceIoControl(h, fsctlSetSparse, nil, 0, nil, 0, &ret, nil)

	zd := fileZeroDataInformation{FileOffset: off, BeyondFinalZero: off + length}
	err := windows.DeviceIoControl(h, fsctlSetZeroData,
		(*byte)(unsafe.Pointer(&zd)), uint32(unsafe.Sizeof(zd)),
		nil, 0, &ret, nil)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// platformPreallocate：NTFS 上设置 EOF 即完成分配，没有独立的预分配 API。
// 上层用 SetEndOfFile/Truncate 就够了，这里返回不支持让调用方走通用路径。
func platformPreallocate(*os.File, int64, int64) error {
	return ErrNotSupported
}

// openNoFollow：Windows 的 CreateFile 没有 O_NOFOLLOW 对应物。
// NTFS 的重解析点（junction / symlink）需要 FILE_FLAG_OPEN_REPARSE_POINT，
// 而 Go 的 os.OpenFile 不暴露它，故这里为 0。
//
// 影响面有限：Windows 上创建符号链接默认需要管理员权限或开发者模式，
// 共享内出现攻击者可控软链的前提本就不成立。
// TODO: 若要在 Windows 上严格化，需绕开 os.OpenFile 直接调 CreateFileW。
const openNoFollow = 0
