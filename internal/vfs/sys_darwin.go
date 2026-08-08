//go:build darwin

package vfs

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// platformStatFS 取卷级容量信息。
func platformStatFS(path string, info *FSInfo) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return mapError(err)
	}
	info.BlockSize = st.Bsize
	info.TotalBlocks = st.Blocks
	info.FreeBlocks = st.Bfree
	info.AvailBlocks = uint64(st.Bavail)
	// macOS 的 statfs 不带 f_namelen，用通用上限。
	info.MaxComponentLen = MaxComponentLen
	return nil
}

// platformFullSync 实现 macOS 的 **F_FULLFSYNC**。
//
// 这是 Time Machine 能否正确工作的关键（AGENTS.md §2 阶段二）：
// macOS 上的 fsync(2) 只保证把数据交给磁盘控制器，**不保证**磁盘把它
// 从易失缓存写进盘片；F_FULLFSYNC 才会下发 barrier/flush cache 命令。
// Apple 的客户端在 SMB2 FLUSH 之后就认为数据已经持久化了。
//
// unix.FcntlInt 是纯 Go 的 fcntl(2) 封装，不需要 CGO（AGENTS.md C1）。
func platformFullSync(f *os.File) error {
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		// 某些文件系统（例如网络挂载、部分虚拟机的 virtiofs）不支持
		// F_FULLFSYNC，会返回 ENOTTY/EINVAL/ENOTSUP。
		// 此时退化为普通 fsync 总比直接失败好。
		return f.Sync()
	}
	return nil
}

// platformPunchHole：macOS 有 F_PUNCHHOLE（0x63，10.11+），但
// x/sys/unix 没有导出对应的 fpunchhole_t 结构体与封装，硬拼要用 unsafe。
//
// 本项目的服务端主力平台是 Linux，macOS 主要作为**客户端**做 Time Machine
// 验收（AGENTS.md §3），所以这里先如实返回不支持，
// 由上层退化为「写零」而不是悄悄假装成功。
//
// TODO: 需要在 macOS 上跑服务端时补齐：
//
//	type fpunchhole struct{ flags uint32; reserved uint32; offset, length int64 }
//	unix.Syscall(unix.SYS_FCNTL, fd, unix.F_PUNCHHOLE, uintptr(unsafe.Pointer(&arg)))
func platformPunchHole(*os.File, int64, int64) error {
	return ErrNotSupported
}

// platformPreallocate 用 F_PREALLOCATE 预分配连续空间。
//
// F_PEOFPOSMODE：offset 相对**当前 EOF**计算，这与 SMB 的 AllocationSize
// 「在现有内容之外再预留多少」语义一致。
// 先试 F_ALLOCATECONTIG（要求连续），失败再退回 F_ALLOCATEALL（允许碎片），
// 这是 Apple 文档建议的用法。
func platformPreallocate(f *os.File, off, length int64) error {
	if length <= 0 {
		return nil
	}
	store := unix.Fstore_t{
		Flags:   unix.F_ALLOCATECONTIG,
		Posmode: unix.F_PEOFPOSMODE,
		Offset:  off,
		Length:  length,
	}
	if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &store); err != nil {
		store.Flags = unix.F_ALLOCATEALL
		if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &store); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// openNoFollow 是 open(2) 的 O_NOFOLLOW 标志。
const openNoFollow = unix.O_NOFOLLOW

// platformSetCreateTime：macOS 可以用 setattrlist(ATTR_CMN_CRTIME) 设置
// 创建时间，但 x/sys/unix 没有导出对应封装，硬拼要用 unsafe。
// 服务端主力平台是 Linux，这里先如实返回不支持。
// TODO: 需要时用 unix.Setattrlist 补齐。
func platformSetCreateTime(*os.File, string, time.Time) error { return ErrNotSupported }

// platformSetDOSAttributes：macOS 有 UF_HIDDEN 等 BSD 文件标志可以对应
// FILE_ATTRIBUTE_HIDDEN，但语义并不完全一致，先由调用方退化成 chmod。
func platformSetDOSAttributes(string, uint32) error { return ErrNotSupported }

// errnoNoAttr 是「扩展属性不存在」的 errno。macOS 用 ENOATTR。
const errnoNoAttr = unix.ENOATTR
