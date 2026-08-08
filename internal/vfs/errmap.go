package vfs

// errmap.go —— 把宿主机文件系统错误收敛为 fs.go 里定义的跨后端 sentinel。
//
// SMB 层只认识这些 sentinel（再由它映射成 NTSTATUS，AGENTS.md §5 P5），
// 因此本包对外**绝不能**泄漏裸的 *os.PathError / syscall.Errno。

import (
	"errors"
	"io"
	"io/fs"
	"syscall"
)

// mapError 把任意错误映射为 vfs 的 sentinel error。
//
// 已经是 vfs sentinel 的原样返回；io.EOF 原样返回（Handle.ReadAt 与
// Handle.ReadDir 依赖它表达「读完了」）。
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if isVFSError(err) {
		return err
	}

	// errno 优先：fs.ErrPermission 在部分平台会把 EACCES 与 EPERM 混同，
	// 而 SMB 需要区分「无权限」与「目录非空」等更细的语义。
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if mapped := mapErrno(errno); mapped != nil {
			return mapped
		}
	}

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrExist):
		return ErrExist
	case errors.Is(err, fs.ErrPermission):
		return ErrPermission
	case errors.Is(err, fs.ErrClosed):
		return ErrClosed
	case errors.Is(err, fs.ErrInvalid):
		return ErrInvalidPath
	}
	return err
}

func isVFSError(err error) bool {
	for _, s := range []error{
		ErrNotFound, ErrExist, ErrNotDir, ErrIsDir, ErrNotEmpty,
		ErrPermission, ErrReadOnly, ErrInvalidPath, ErrNotSupported,
		ErrNoSpace, ErrTooLarge, ErrClosed, ErrInvalidArg,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
