//go:build windows

package vfs

import "syscall"

// Windows 系统错误码（winerror.h）。这里用本地常量而不是 syscall 包里
// 零散导出的那几个，避免不同 Go 版本导出集合差异导致编译失败。
const (
	errFileNotFound     syscall.Errno = 2   // ERROR_FILE_NOT_FOUND
	errPathNotFound     syscall.Errno = 3   // ERROR_PATH_NOT_FOUND
	errAccessDenied     syscall.Errno = 5   // ERROR_ACCESS_DENIED
	errInvalidHandle    syscall.Errno = 6   // ERROR_INVALID_HANDLE
	errWriteProtect     syscall.Errno = 19  // ERROR_WRITE_PROTECT
	errSharingViolation syscall.Errno = 32  // ERROR_SHARING_VIOLATION
	errFileExists       syscall.Errno = 80  // ERROR_FILE_EXISTS
	errDiskFull         syscall.Errno = 112 // ERROR_DISK_FULL
	errInvalidName      syscall.Errno = 123 // ERROR_INVALID_NAME
	errDirNotEmpty      syscall.Errno = 145 // ERROR_DIR_NOT_EMPTY
	errAlreadyExists    syscall.Errno = 183 // ERROR_ALREADY_EXISTS
	errFilenameTooLong  syscall.Errno = 206 // ERROR_FILENAME_EXCED_RANGE
	errDirectory        syscall.Errno = 267 // ERROR_DIRECTORY
	errNotSupported     syscall.Errno = 50  // ERROR_NOT_SUPPORTED
	errInvalidParam     syscall.Errno = 87  // ERROR_INVALID_PARAMETER
	errNegativeSeek     syscall.Errno = 131 // ERROR_NEGATIVE_SEEK
)

func mapErrno(errno syscall.Errno) error {
	switch errno {
	case errFileNotFound, errPathNotFound:
		return ErrNotFound
	case errFileExists, errAlreadyExists:
		return ErrExist
	case errAccessDenied, errSharingViolation:
		return ErrPermission
	case errWriteProtect:
		return ErrReadOnly
	case errDirNotEmpty:
		return ErrNotEmpty
	case errDiskFull:
		return ErrNoSpace
	case errInvalidHandle:
		return ErrClosed
	case errInvalidName, errFilenameTooLong:
		return ErrInvalidPath
	case errDirectory:
		return ErrNotDir
	case errNotSupported:
		return ErrNotSupported
	case errInvalidParam, errNegativeSeek:
		return ErrInvalidArg
	}
	return nil
}
