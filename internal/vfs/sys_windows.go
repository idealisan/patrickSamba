//go:build windows

package vfs

import (
	"errors"
	"os"
	"time"
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

// flushFileBuffers 调 FlushFileBuffers，并把「句柄没有写权限」这一平台事实
// 归一成「无可刷」。
//
// ⚠️ FlushFileBuffers 要求句柄带**写权限**：只读句柄与目录句柄都会拿到
// ERROR_ACCESS_DENIED。这两种句柄没有缓冲写要落盘（只读句柄写不了；目录句柄
// 本身没有数据流），所以这里的 ACCESS_DENIED 不是「刷失败」，而是「无可刷」。
// 不能把它当错误上报：SMB 的 FLUSH 在这两种句柄上必须成功 —— macOS 的
// Time Machine 会把 FLUSH 失败当作备份目标不可靠并**中止备份**，而 TM 会大量
// 建目录、也会 FLUSH 只读句柄。写句柄仍照常真刷，不影响
// TestFlushReallyCallsFullSync 钉的契约。
func flushFileBuffers(f *os.File) error {
	if err := f.Sync(); err != nil {
		if errors.Is(err, errAccessDenied) {
			return nil
		}
		return err
	}
	return nil
}

// platformFullSync：Windows 的 FlushFileBuffers 本来就是「刷到介质」的语义，
// 没有 macOS 那种两级刷盘的区分，所以两种强度落到同一个调用。
func platformFullSync(f *os.File) error { return flushFileBuffers(f) }

// platformSync 是普通强度的刷盘（SMB2 FLUSH 传 full=false，以及内部
// write-through 之外的收尾）。Windows 上与 full 强度同义，见上。
func platformSync(f *os.File) error { return flushFileBuffers(f) }

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

// openNoFollow 在 Windows 上是空操作，**不是缺口**。
//
// 「不跟随最后一跳」那件事改由 openhost_windows.go 的 CreateFileW 常开
// FILE_FLAG_OPEN_REPARSE_POINT 承担（flag 计算见 winopen.go 的
// winOpenParamsFor），不再需要调用方在这里传一个标志位。
const openNoFollow = 0

// platformSetCreateTime 用 SetFileTime 设置真实创建时间。
// 这是 Windows 相对 POSIX 的**原生优势**：NTFS 真的存了创建时间，
// 不需要旁路存储（AGENTS.md §5 P7）。
func platformSetCreateTime(f *os.File, host string, t time.Time) error {
	if f == nil {
		// 没有句柄就没法设置；调用方会忽略这个错误。
		return ErrNotSupported
	}
	ft := TimeToFiletime(t)
	wft := windows.Filetime{
		LowDateTime:  uint32(ft),
		HighDateTime: uint32(ft >> 32),
	}
	if err := windows.SetFileTime(windows.Handle(f.Fd()), &wft, nil, nil); err != nil {
		return mapError(err)
	}
	return nil
}

// platformSetDOSAttributes 用 SetFileAttributes 落地 DOS 属性位。
//
// 只改**可设置**的那几位（settableDOSAttributes），
// DIRECTORY / SPARSE / REPARSE 这些是文件系统的客观事实，必须原样保留 ——
// 清掉 DIRECTORY 位会让 Windows 认为这不再是目录。
func platformSetDOSAttributes(host string, attrs uint32) error {
	p, err := windows.UTF16PtrFromString(host)
	if err != nil {
		return ErrInvalidPath
	}
	cur, err := windows.GetFileAttributes(p)
	if err != nil {
		return mapError(err)
	}
	next := (cur &^ settableDOSAttributes) | (attrs & settableDOSAttributes)
	if next == 0 {
		next = windows.FILE_ATTRIBUTE_NORMAL
	}
	if next == cur {
		return nil
	}
	if err := windows.SetFileAttributes(p, next); err != nil {
		return mapError(err)
	}
	return nil
}
