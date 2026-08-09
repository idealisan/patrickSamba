//go:build linux

package oscap

import (
	"os"

	"golang.org/x/sys/unix"
)

// probeXattrName 是探测用的扩展属性名。
//
// Linux 的非特权进程只能写 `user.` 命名空间，其余会 EPERM ——
// 少了这个前缀，探测在任何文件系统上都会失败，等于永远判「不支持」。
const probeXattrName = "user.stupidsamba.capprobe"

func probeNative(c Capability, o Options) bool {
	switch c {
	case CapXattr:
		return probeXattr(o.Root)

	case CapNamedStream:
		// POSIX 上 native 命名流承载在扩展属性里（Samba vfs_fruit 的做法），
		// 所以它的可用性**完全取决于** xattr。见 ports.go 的 NamedStream 注释。
		return probeXattr(o.Root)

	case CapSparseFile:
		return probeSparseLinux(o.Root)

	case CapStableFileID:
		return probeInode(o.Root)

	case CapCreationTime:
		return probeBirthTimeLinux(o.Root)

	case CapDOSAttributes:
		// POSIX 没有存放 DOS 属性位的地方。
		//
		// 注意别把「用 user.DOSATTRIB 扩展属性存一份」当成 native：那是我们
		// 自己发明的存储格式，只不过借了 xattr 当载体，本质是旁路存储 ——
		// 按本项目的划分它属于 builtin。native 的定义是「借助 OS **既有**的
		// 能力表达同一份语义」，而 Linux 内核对 DOSATTRIB 一无所知。
		return false
	}
	return false
}

// probeSparseLinux 探测打洞能力。
//
// 判据用 fallocate(FALLOC_FL_PUNCH_HOLE) 而不是 SEEK_HOLE：
// 能查空洞不等于能打洞（tmpfs 就是典型），而对 Time Machine 而言
// **打洞才是那个不可替代的能力**——查不到空洞最多是多读一遍零，
// 打不了洞会让备份卷永远只涨不落。判据必须对着真正要用的那件事。
func probeSparseLinux(root string) bool {
	return withProbeFile(root, func(f *os.File) bool {
		const probeSize = 1 << 20 // 1 MiB：比任何文件系统的块都大，足够打出一个洞
		if err := f.Truncate(probeSize); err != nil {
			return false
		}
		err := unix.Fallocate(int(f.Fd()),
			unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, 4096)
		return err == nil
	})
}

// probeBirthTimeLinux 探测能否拿到真实创建时间。
//
// Linux 只有 statx(2) 能给出 btime，而且**文件系统得真的存了它**：
// ext4（inode 256 字节）、xfs v5、btrfs 有；ext3、老 ext4、tmpfs、
// 多数网络文件系统没有 —— 此时 statx 照样成功返回，只是结果里的
// STATX_BTIME 位不置位。所以必须查 stx.Mask，光看 err == nil 会假阳性。
func probeBirthTimeLinux(root string) bool {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, root, 0, unix.STATX_BTIME, &stx)
	if err != nil {
		// 内核 < 4.11 没有 statx，返回 ENOSYS。
		return false
	}
	return stx.Mask&unix.STATX_BTIME != 0
}
