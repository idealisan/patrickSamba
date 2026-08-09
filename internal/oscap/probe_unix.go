//go:build linux || darwin

package oscap

// probe_unix.go —— linux 与 darwin 共用的探测实现。
//
// 平台特有的部分（statx vs st_birthtimespec、打洞方式、xattr 命名空间）
// 在 probe_linux.go / probe_darwin.go 里。

import (
	"errors"

	"golang.org/x/sys/unix"
)

// probeXattr 判断 root 所在的文件系统是否真的支持扩展属性。
//
// 为什么不能只用 listxattr 判定：老内核上的 tmpfs（Linux 6.6 之前）
// **listxattr 成功返回 0**，但 `user.*` 的 setxattr 直接 EOPNOTSUPP ——
// 只看 listxattr 会得到一个假阳性，native 模式于是启动通过、运行时才炸。
// 所以主判据是一次真实的 set + remove。
//
// 只有在写失败的原因是**权限/只读**（不是「不支持」）时，才退回只读判据：
// 只读共享上写不进去是必然的，但属性照样读得出来，这时报 false 会平白丢掉
// 一项本来可用的原生能力。
func probeXattr(root string) bool {
	err := unix.Setxattr(root, probeXattrName, []byte{1}, 0)
	switch {
	case err == nil:
		_ = unix.Removexattr(root, probeXattrName)
		return true
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		_, lerr := unix.Listxattr(root, nil)
		return lerr == nil
	default:
		// ENOTSUP / EOPNOTSUPP / ENOSPC / 名字不合法 …… 一律判不支持。
		return false
	}
}

// probeInode 判断能否拿到稳定的 inode 号。
//
// POSIX 的 st_ino 在同一卷内唯一且跨重命名不变，正好是 StableFileID 要的语义。
// 拿到 0 视为不可用：0 是「未知」的惯用哨兵，用它当 FileID 会让所有对象撞车。
func probeInode(root string) bool {
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return false
	}
	return st.Ino != 0
}
