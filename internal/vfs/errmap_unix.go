//go:build unix

package vfs

import "syscall"

// mapErrno 把 POSIX errno 映射为 vfs sentinel。未识别的返回 nil，
// 交给调用方继续用 io/fs 的通用判定。
func mapErrno(errno syscall.Errno) error {
	switch errno {
	case syscall.ENOENT:
		return ErrNotFound
	case syscall.EEXIST:
		return ErrExist
	case syscall.ENOTDIR:
		return ErrNotDir
	case syscall.EISDIR:
		return ErrIsDir
	case syscall.ENOTEMPTY:
		return ErrNotEmpty
	case syscall.EACCES, syscall.EPERM:
		return ErrPermission
	case syscall.EROFS:
		return ErrReadOnly
	case syscall.ENOSPC, syscall.EDQUOT:
		return ErrNoSpace
	case syscall.EFBIG:
		return ErrTooLarge
	case syscall.EBADF:
		return ErrClosed
	case syscall.ENAMETOOLONG, syscall.EILSEQ, syscall.ELOOP:
		// ELOOP：软链环。对客户端而言这条路径就是不可用的。
		return ErrInvalidPath
	case syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.EXDEV:
		// EXDEV：跨设备的 rename/link。共享内部一般不跨设备，但
		// bind mount 与子卷挂载点会让它真实发生。SMB 层应映射成
		// STATUS_NOT_SAME_DEVICE，而不是含义模糊的「权限不足」。
		return ErrNotSupported
	case syscall.EINVAL, syscall.EOVERFLOW:
		// 参数非法（负 offset、越界 length 等），不是路径问题。
		return ErrInvalidArg
	}
	return nil
}
