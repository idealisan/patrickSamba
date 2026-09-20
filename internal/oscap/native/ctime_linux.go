//go:build linux

package native

// ctime_linux.go —— oscap.CreationTime 的原生实现：statx(2) 的 STATX_BTIME。
//
// 注意这不是 POSIX 的 ctime（inode 变更时间）。用 ctime 冒充创建时间会在
// 「文件被 chmod 过」之后变成一个跳动的值，而 macOS 的 Finder 与 Time Machine
// 会显示并比较创建时间。

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// linuxTimes 实现 oscap.CreationTime。
type linuxTimes struct{}

var _ oscap.CreationTime = linuxTimes{}

func (linuxTimes) CreationTime(ref oscap.Ref) (time.Time, error) {
	// AT_STATX_DONT_SYNC：不为了元数据精确性去唤醒网络/慢速文件系统。
	flags := unix.AT_STATX_DONT_SYNC
	dirfd := unix.AT_FDCWD
	path := ref.Path
	if fd, ok := refFD(ref); ok {
		// AT_EMPTY_PATH + 空路径 = 对这个 fd 本身做 statx，
		// 省掉一次路径解析，也避开 stat 与 open 之间的 TOCTOU。
		dirfd, path = fd, ""
		flags |= unix.AT_EMPTY_PATH
	} else {
		// 与 vfs 全程的 lstat 语义一致。
		flags |= unix.AT_SYMLINK_NOFOLLOW
	}

	var stx unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_BTIME, &stx); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
			// 内核 < 4.11 没有 statx；被 seccomp 拦掉时是 EPERM。
			return time.Time{}, oscap.ErrNotSupported
		}
		return time.Time{}, mapPosixErr(err)
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		// **必须查 Mask**：文件系统没记 btime（ext3、老 ext4、部分 tmpfs、
		// 多数网络文件系统）时 statx 照样成功返回，只是不置这个位。
		// 光看 err == nil 会假阳性，然后把一个零时间当成创建时间报上去。
		//
		// 如实报不支持，让上层去问 builtin 要那个记下来的真值 —— 这正是
		// ports.go 说的「不要拿 mtime 冒充」。
		return time.Time{}, oscap.ErrNotSupported
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), nil
}

// SetCreationTime 在 Linux 上**做不到**。
//
// 内核没有任何设置 btime 的接口：statx 只读，utimensat 只能改 atime/mtime，
// 连 debugfs 那种离线手段都要求文件系统未挂载。这不是「我们还没做」，
// 是「Linux 没有这个能力」。
//
// 于是 CreationTime 这一项在 Linux 上是**读得到、写不进**的半边能力。
// 按 capability.go 的划分原则（一项能力要么整套能用要么整套不能用），
// 它本该整项判为不支持、由 builtin 接管；但 probe_linux.go 现在把
// CapCreationTime 探测为「文件系统记了 btime 就是 true」，也就是说
// **矩阵会把这一项交给 native**，客户端的 SET_INFO(FileBasicInformation)
// 创建时间就会一路吃到 STATUS_NOT_SUPPORTED。
//
// 这里如实返回 ErrNotSupported 而不是假装成功，有两个理由：
//  1. 这与 vfs 今天的行为完全一致（internal/vfs/sys_linux.go 的
//     platformSetCreateTime 同样返回 ErrNotSupported），不是回归；
//  2. 假装成功会让客户端写完立刻读回一个对不上的值 —— 那种「写了但没生效
//     且不报错」正是本项目反复栽过的「成功回显 ≠ 事情真的发生了」。
//
// 真要让 Linux 上的创建时间可写，正确解法是让矩阵把这一项交给 builtin
// （旁路存一份），或者由 vfs 在 native 报 ErrNotSupported 时回落到旁路存储。
// 那是 port 侧的策略决定，已同步给 oscap-port，不在本包擅自决定。
func (linuxTimes) SetCreationTime(oscap.Ref, time.Time) error {
	return oscap.ErrNotSupported
}
