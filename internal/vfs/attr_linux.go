//go:build linux

package vfs

import (
	"io/fs"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// fillSysAttr 从 Linux 的 struct stat 补齐 fs.FileInfo 表达不了的字段。
func fillSysAttr(fi fs.FileInfo, a *Attr) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	a.UID = st.Uid
	a.GID = st.Gid
	a.NLink = uint32(st.Nlink)
	a.FileID = st.Ino
	// st_blocks 的单位固定是 512 字节（POSIX），与文件系统块大小无关。
	// 稀疏文件的 blocks 会显著小于 size，这正是 SPARSE_FILE 位的依据。
	a.Alloc = int64(st.Blocks) * 512
	a.AccessTime = time.Unix(st.Atim.Unix())
	a.WriteTime = time.Unix(st.Mtim.Unix())
	// ChangeTime 忠实反映 ctime（inode 变更时间）。
	a.ChangeTime = time.Unix(st.Ctim.Unix())
	// Linux 的 struct stat 没有创建时间。兜底口径对齐 Samba 的
	// calc_create_time_stat（source3/lib/system.c:131–150）：
	// MIN(ctime, mtime, atime)，atime 异常为零时退 MIN(ctime, mtime)。
	// 真正的 btime 由 statCreateTime 用 statx(2) 取，见下。
	a.CreateTime = calcBtimeFallback(a.ChangeTime, a.WriteTime, a.AccessTime)
}

// fillSysAttrFromFile 在 Linux 上无需额外处理：fstat 的结果与 lstat 同构，
// 上面的 fillSysAttr 已经覆盖。
func fillSysAttrFromFile(*os.File, *Attr) {}

// statxUnavailable 记录 statx(2) 是否不可用（内核 < 4.11，或被 seccomp 拦截）。
// 探测一次即可，避免每次 stat 都白付一次 ENOSYS 的系统调用开销。
var statxUnavailable atomic.Bool

// statCreateTime 用 statx(2) 取真正的创建时间（btime）。
//
// 为什么值得多一次系统调用：macOS 的 Finder 与 Time Machine 会显示并比较
// 创建时间，用 ctime 冒充会在「文件被 chmod 过」之后变成一个跳动的值。
// ext4/xfs/btrfs 都记录了 btime，只是老的 stat(2) 接口取不到。
//
// 取不到时返回 false，调用方保留 fillSysAttr 填的 ctime 兜底。
func statCreateTime(path string) (time.Time, bool) {
	if statxUnavailable.Load() {
		return time.Time{}, false
	}
	var stx unix.Statx_t
	// AT_SYMLINK_NOFOLLOW：与本包其余 lstat 语义保持一致。
	// AT_STATX_DONT_SYNC：不为了元数据精确性去唤醒网络/慢速文件系统。
	err := unix.Statx(unix.AT_FDCWD, path,
		unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC,
		unix.STATX_BTIME, &stx)
	if err != nil {
		if err == unix.ENOSYS || err == unix.EPERM {
			statxUnavailable.Store(true)
		}
		return time.Time{}, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		// 文件系统不记录 btime（例如 tmpfs 的老实现）。
		return time.Time{}, false
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true
}

// fileAccessTime 从 FileInfo 取当前 atime。
//
// 唯一消费方是 sticky write time 的补偿回写（bh3-F6）：os.Chtimes 必须
// 同时给 atime 与 mtime，而 POSIX 拿不到「只改其一」的接口，所以先把
// 当前 atime 读出来原样写回去。取不到时返回 false，调用方放弃补偿
// （尽力而为，不让补偿动作污染数据路径的错误处理）。
func fileAccessTime(fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Atim.Unix()), true
}
