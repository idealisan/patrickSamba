//go:build windows

package vfs

// sparse_windows.go —— NTFS 的稀疏文件探测与标记。
//
// 走 golang.org/x/sys/windows 的 DeviceIoControl，是纯 Go 的 syscall
// 封装，不需要 CGO（AGENTS.md C1）。

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fsctlQueryAllocatedRanges 的控制码（winioctl.h）：
//
//	FSCTL_QUERY_ALLOCATED_RANGES =
//	    CTL_CODE(FILE_DEVICE_FILE_SYSTEM, 51, METHOD_NEITHER, FILE_READ_DATA)
const fsctlQueryAllocatedRanges uint32 = 0x000940CF

// fileAllocatedRangeBuffer 对应 winioctl.h 的 FILE_ALLOCATED_RANGE_BUFFER
// （MS-FSCC §2.3.21.1）。输入是一个，输出是数组。
type fileAllocatedRangeBuffer struct {
	FileOffset int64
	Length     int64
}

// fileSetSparseBuffer 对应 winioctl.h 的 FILE_SET_SPARSE_BUFFER。
// SetSparse 是 BOOLEAN（1 字节）。
type fileSetSparseBuffer struct {
	SetSparse byte
}

// queryRangesBatch 是一次 DeviceIoControl 最多取回的区间数。
// 128 个 = 2 KiB 输出缓冲，对 .sparsebundle 的 band（8 MiB 上下）足够，
// 不够时下面的循环会带着新的起点继续问。
const queryRangesBatch = 128

// platformAllocatedRanges 返回 [off, end) 内已分配的子区间。
// 调用方保证 0 <= off < end <= EOF。
func platformAllocatedRanges(f *os.File, off, end int64) ([]Range, error) {
	h := windows.Handle(f.Fd())
	out := make([]Range, 0, 8)
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

		// ERROR_MORE_DATA 不是失败：缓冲装不下，已返回的部分是有效的，
		// 带着新的起点再问一次即可。
		more := errors.Is(err, windows.ERROR_MORE_DATA)
		if err != nil && !more {
			if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
				errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
				// FAT32 等不支持稀疏的卷：降级为「整段已分配」。
				return wholeRange(off, end), nil
			}
			return nil, mapError(err)
		}

		n := int(uintptr(ret) / unsafe.Sizeof(buf[0]))
		if n <= 0 {
			// 没有已分配区间 —— 窗口内全是空洞，正常终止。
			break
		}

		next := cur
		for _, r := range buf[:n] {
			s, e := r.FileOffset, r.FileOffset+r.Length
			if s < off {
				s = off
			}
			if e > end {
				e = end
			}
			if e > s {
				out = append(out, Range{Offset: s, Length: e - s})
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

// platformSetSparse 用 FSCTL_SET_SPARSE 标记/取消稀疏。
//
// 卷不支持稀疏（FAT32）时返回 ERROR_INVALID_FUNCTION，
// 按「不支持」如实上报，让上层决定退化策略。
func platformSetSparse(f *os.File, v bool) error {
	in := fileSetSparseBuffer{}
	if v {
		in.SetSparse = 1
	}
	var ret uint32
	err := windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &ret, nil)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return ErrNotSupported
		}
		return mapError(err)
	}
	return nil
}
