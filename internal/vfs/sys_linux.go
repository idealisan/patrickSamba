//go:build linux

package vfs

// sys_linux.go —— Linux 平台相关的系统调用封装（statfs / 强制刷盘 / 稀疏文件）。
//
// 全部走 golang.org/x/sys/unix 的**纯 Go** 系统调用封装，不引入 CGO
// （AGENTS.md C1）。

import (
	"os"

	"golang.org/x/sys/unix"
)

// platformStatFS 取卷级容量信息。
func platformStatFS(path string, info *FSInfo) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return mapError(err)
	}
	// f_bsize 是「文件系统 IO 的最优块大小」，SMB 的 SectorsPerAllocationUnit
	// 就按它算。f_blocks/f_bfree/f_bavail 都以 f_bsize 为单位。
	info.BlockSize = uint32(st.Bsize)
	info.TotalBlocks = st.Blocks
	info.FreeBlocks = st.Bfree
	// f_bavail 是**非特权用户**可用的块数（扣掉 root 保留），
	// 报给客户端应当用它，否则客户端会以为还能写却写到 ENOSPC。
	info.AvailBlocks = st.Bavail
	if st.Namelen > 0 {
		info.MaxComponentLen = uint32(st.Namelen)
	}
	return nil
}

// platformFullSync 在 Linux 上等价于 fsync(2)：
// Linux 的 fsync 本来就要求把数据刷到持久介质（不像 macOS 那样只到磁盘缓存），
// 因此没有 F_FULLFSYNC 这种东西。
func platformFullSync(f *os.File) error {
	return f.Sync()
}

// platformPunchHole 打洞：把 [off, off+length) 变成稀疏空洞。
// Time Machine 的 .sparsebundle 会大量删除 band 文件内容，
// 没有打洞能力会让备份卷持续膨胀。
func platformPunchHole(f *os.File, off, length int64) error {
	err := unix.Fallocate(int(f.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// platformPreallocate 预分配空间（对应 SMB 的 AllocationSize / AlSi context）。
// FALLOC_FL_KEEP_SIZE：只占块不改 EOF，与 Windows 的 AllocationSize 语义一致。
func platformPreallocate(f *os.File, off, length int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, off, length)
	if err != nil {
		return mapError(err)
	}
	return nil
}
