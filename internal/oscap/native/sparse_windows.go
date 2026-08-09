//go:build windows

package native

// sparse_windows.go —— oscap.SparseFile 的原生实现：NTFS 的稀疏文件 FSCTL。
//
// 走 golang.org/x/sys/windows 的 DeviceIoControl / SetFileInformationByHandle，
// 都是纯 Go 的 syscall 封装，不需要 CGO（AGENTS.md C1）。

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// winioctl.h 的控制码。数值不要改写成 CTL_CODE 宏的展开式 ——
// 写成常量并注明出处，是本项目对「写死的常量必须注明规范章节」的做法。
const (
	// FSCTL_SET_SPARSE = CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 49, METHOD_BUFFERED,
	// FILE_SPECIAL_ACCESS)，MS-FSCC §2.3.69。
	fsctlSetSparse uint32 = 0x000900C4

	// FSCTL_SET_ZERO_DATA = CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 50,
	// METHOD_BUFFERED, FILE_WRITE_DATA)，MS-FSCC §2.3.71。
	fsctlSetZeroData uint32 = 0x000980C8

	// FSCTL_QUERY_ALLOCATED_RANGES = CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 51,
	// METHOD_NEITHER, FILE_READ_DATA)，MS-FSCC §2.3.21。
	fsctlQueryAllocatedRanges uint32 = 0x000940CF
)

// fileSetSparseBuffer 对应 winioctl.h 的 FILE_SET_SPARSE_BUFFER。
// SetSparse 是 BOOLEAN（1 字节，不是 Go 的 bool 也不是 4 字节 BOOL）。
type fileSetSparseBuffer struct {
	SetSparse byte
}

// fileZeroDataInformation 对应 FILE_ZERO_DATA_INFORMATION（MS-FSCC §2.3.71.1）。
// 置零区间是 [FileOffset, BeyondFinalZero)，**右开**。
type fileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

// fileAllocatedRangeBuffer 对应 FILE_ALLOCATED_RANGE_BUFFER（MS-FSCC §2.3.21.1）。
// 输入是一个，输出是数组。
type fileAllocatedRangeBuffer struct {
	FileOffset int64
	Length     int64
}

// fileAllocationInfo 对应 FILE_ALLOCATION_INFO（SetFileInformationByHandle 的
// FileAllocationInfo 类）。
type fileAllocationInfo struct {
	AllocationSize int64
}

// queryRangesBatch 是一次 DeviceIoControl 最多取回的区间数。
// 128 个 = 2 KiB 输出缓冲，对 .sparsebundle 的 band（8 MiB 上下）足够；
// 不够时下面的循环会带着新的起点继续问。
const queryRangesBatch = 128

// winSparse 实现 oscap.SparseFile。
type winSparse struct {
	readOnly bool
}

var _ oscap.SparseFile = (*winSparse)(nil)

// PunchHole 先把文件标记为稀疏，再用 FSCTL_SET_ZERO_DATA 释放区间。
//
// **两步缺一不可**：没有 FSCTL_SET_SPARSE，SET_ZERO_DATA 就是老老实实写一片零，
// 簇一个都不会被释放 —— 那正好是 Time Machine 场景里最不能接受的结果
// （备份卷只涨不落），而且从返回码上完全看不出区别。
// 所以标记失败时如实报错，绝不「反正读回来是零」地继续。
func (s *winSparse) PunchHole(ref oscap.Ref, off, length int64) error {
	if err := checkWindow(off, length); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	h, cleanup, err := winHandle(ref, windows.GENERIC_WRITE)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := setSparseFlag(h, true); err != nil {
		return err
	}
	in := fileZeroDataInformation{FileOffset: off, BeyondFinalZero: off + length}
	var ret uint32
	return mapWinErr(windows.DeviceIoControl(h, fsctlSetZeroData,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &ret, nil))
}

// Preallocate 用 FileAllocationInfo 把分配长度抬到 off+length，EOF 不变。
//
// ⚠️ **必须先比 EOF**：FileAllocationInfo 的值小于当前 EOF 时，NTFS 会把文件
// **截断**到那个长度 —— 一个名为「预分配」的调用把用户数据删掉，是这份 API
// 最阴的一个坑。所以这里遇到「要求的分配量还不到现有 EOF」时直接返回 nil：
// 语义上「至少预留这么多」本来就已经满足了。
func (s *winSparse) Preallocate(ref oscap.Ref, off, length int64) error {
	if err := checkWindow(off, length); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	h, cleanup, err := winHandle(ref, windows.GENERIC_WRITE)
	if err != nil {
		return err
	}
	defer cleanup()

	size, isDir, err := winFileSize(h)
	if err != nil {
		return err
	}
	if isDir {
		return oscap.ErrInvalidArg
	}
	want := off + length
	if want <= size {
		return nil
	}
	in := fileAllocationInfo{AllocationSize: want}
	return mapWinErr(windows.SetFileInformationByHandle(h, windows.FileAllocationInfo,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in))))
}

// AllocatedRanges 用 FSCTL_QUERY_ALLOCATED_RANGES 取已分配区间。
func (s *winSparse) AllocatedRanges(ref oscap.Ref, off, length int64) ([]oscap.Range, error) {
	if err := checkWindow(off, length); err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, nil
	}
	h, cleanup, err := winHandle(ref, windows.GENERIC_READ)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	size, isDir, err := winFileSize(h)
	if err != nil {
		return nil, err
	}
	if isDir {
		// 目录没有「已分配区间」这回事，如实拒绝而不是报一个假区间。
		return nil, oscap.ErrInvalidArg
	}
	if off >= size {
		return nil, nil
	}
	end := off + length
	if end > size {
		end = size
	}

	out := make([]oscap.Range, 0, 8)
	cur := off
	for cur < end {
		in := fileAllocatedRangeBuffer{FileOffset: cur, Length: end - cur}
		buf := make([]fileAllocatedRangeBuffer, queryRangesBatch)

		var ret uint32
		err := windows.DeviceIoControl(h, fsctlQueryAllocatedRanges,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			(*byte)(unsafe.Pointer(&buf[0])),
			uint32(uintptr(len(buf))*unsafe.Sizeof(buf[0])),
			&ret, nil)

		// ERROR_MORE_DATA 不是失败：缓冲装不下，已返回的部分有效，
		// 带着新的起点再问一次即可。
		more := errors.Is(err, windows.ERROR_MORE_DATA)
		if err != nil && !more {
			if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
				errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
				// 不支持稀疏的卷（FAT32）：降级成「整段已分配」。
				// 多报是安全的，少报会让客户端以为数据丢了（ports.go）。
				return wholeWindow(off, end), nil
			}
			return nil, mapWinErr(err)
		}

		n := int(uintptr(ret) / unsafe.Sizeof(buf[0]))
		if n <= 0 {
			// 窗口内全是空洞 —— 正常终止。
			break
		}

		next := cur
		for _, r := range buf[:n] {
			a, b := r.FileOffset, r.FileOffset+r.Length
			if a < off {
				a = off
			}
			if b > end {
				b = end
			}
			if b > a {
				out = append(out, oscap.Range{Offset: a, Length: b - a})
			}
			if r.FileOffset+r.Length > next {
				next = r.FileOffset + r.Length
			}
		}
		if !more || next <= cur {
			break
		}
		cur = next
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SetSparse 在 NTFS 上**两个方向都真实生效**（对比 POSIX 的刻意不对称，
// 见 sparse_linux.go 与 ports.go）。
func (s *winSparse) SetSparse(ref oscap.Ref, v bool) error {
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	h, cleanup, err := winHandle(ref, windows.GENERIC_WRITE)
	if err != nil {
		return err
	}
	defer cleanup()
	return setSparseFlag(h, v)
}

// setSparseFlag 下发 FSCTL_SET_SPARSE。
func setSparseFlag(h windows.Handle, v bool) error {
	in := fileSetSparseBuffer{}
	if v {
		in.SetSparse = 1
	}
	var ret uint32
	return mapWinErr(windows.DeviceIoControl(h, fsctlSetSparse,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &ret, nil))
}
