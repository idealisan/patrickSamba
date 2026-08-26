// Package status 定义 SMB2 使用的 NTSTATUS 错误码及其与内部错误的映射。
//
// 依据 MS-ERREF §2.3.1 "NTSTATUS Values"。
// 按 AGENTS.md §5 P5，所有内部错误都必须能映射到本包定义的 Status，
// 禁止在 handler 里裸写魔数。
package status

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// Status 是一个 32 位 NTSTATUS 值。
//
// 线格式：SMB2 Header 偏移 0x08 处的 4 字节，**小端**（MS-SMB2 §2.2.1.2）。
// 本类型只表示数值本身，字节序由 wire 包负责。
type Status uint32

// NTSTATUS 的严重级别位域（MS-ERREF §2.3）。Sev 占最高 2 位。
const (
	sevSuccess       = 0x0
	sevInformational = 0x1
	sevWarning       = 0x2
	sevError         = 0x3
)

// 常用 NTSTATUS 值（MS-ERREF §2.3.1）。
const (
	// 成功类。
	Success  Status = 0x00000000 // STATUS_SUCCESS
	Pending  Status = 0x00000103 // STATUS_PENDING
	Notify   Status = 0x0000010B // STATUS_NOTIFY_CLEANUP
	NotifyEn Status = 0x0000010C // STATUS_NOTIFY_ENUM_DIR

	// 警告 / 信息类。
	BufferOverflow   Status = 0x80000005 // STATUS_BUFFER_OVERFLOW
	NoMoreFiles      Status = 0x80000006 // STATUS_NO_MORE_FILES
	StoppedOnSymlink Status = 0x8000002D // STATUS_STOPPED_ON_SYMLINK

	// 错误类。
	Unsuccessful                     Status = 0xC0000001 // STATUS_UNSUCCESSFUL
	NotImplemented                   Status = 0xC0000002 // STATUS_NOT_IMPLEMENTED
	InvalidInfoClass                 Status = 0xC0000003 // STATUS_INVALID_INFO_CLASS
	InfoLengthMismatch               Status = 0xC0000004 // STATUS_INFO_LENGTH_MISMATCH
	AccessViolation                  Status = 0xC0000005 // STATUS_ACCESS_VIOLATION
	InvalidHandle                    Status = 0xC0000008 // STATUS_INVALID_HANDLE
	InvalidParameter                 Status = 0xC000000D // STATUS_INVALID_PARAMETER
	NoSuchDevice                     Status = 0xC000000E // STATUS_NO_SUCH_DEVICE
	NoSuchFile                       Status = 0xC000000F // STATUS_NO_SUCH_FILE
	InvalidDeviceRequest             Status = 0xC0000010 // STATUS_INVALID_DEVICE_REQUEST
	EndOfFile                        Status = 0xC0000011 // STATUS_END_OF_FILE
	MoreProcessingRequired           Status = 0xC0000016 // STATUS_MORE_PROCESSING_REQUIRED
	NoMemory                         Status = 0xC0000017 // STATUS_NO_MEMORY
	AccessDenied                     Status = 0xC0000022 // STATUS_ACCESS_DENIED
	BufferTooSmall                   Status = 0xC0000023 // STATUS_BUFFER_TOO_SMALL
	ObjectTypeMismatch               Status = 0xC0000024 // STATUS_OBJECT_TYPE_MISMATCH
	ObjectNameInvalid                Status = 0xC0000033 // STATUS_OBJECT_NAME_INVALID
	ObjectNameNotFound               Status = 0xC0000034 // STATUS_OBJECT_NAME_NOT_FOUND
	ObjectNameCollision              Status = 0xC0000035 // STATUS_OBJECT_NAME_COLLISION
	ObjectPathInvalid                Status = 0xC0000039 // STATUS_OBJECT_PATH_INVALID
	ObjectPathNotFound               Status = 0xC000003A // STATUS_OBJECT_PATH_NOT_FOUND
	ObjectPathSyntaxBad              Status = 0xC000003B // STATUS_OBJECT_PATH_SYNTAX_BAD
	DataError                        Status = 0xC000003E // STATUS_DATA_ERROR
	SharingViolation                 Status = 0xC0000043 // STATUS_SHARING_VIOLATION
	LockConflict                     Status = 0xC0000054 // STATUS_FILE_LOCK_CONFLICT
	LockNotGranted                   Status = 0xC0000055 // STATUS_LOCK_NOT_GRANTED
	DeletePending                    Status = 0xC0000056 // STATUS_DELETE_PENDING
	RangeNotLocked                   Status = 0xC000007E // STATUS_RANGE_NOT_LOCKED
	InvalidLockRange                 Status = 0xC00001A1 // STATUS_INVALID_LOCK_RANGE
	DiskFull                         Status = 0xC000007F // STATUS_DISK_FULL
	InsufficientResources            Status = 0xC000009A // STATUS_INSUFFICIENT_RESOURCES
	MediaWriteProtected              Status = 0xC00000A2 // STATUS_MEDIA_WRITE_PROTECTED
	NotSameDevice                    Status = 0xC00000D4 // STATUS_NOT_SAME_DEVICE
	LogonFailure                     Status = 0xC000006D // STATUS_LOGON_FAILURE
	AccountRestriction               Status = 0xC000006E // STATUS_ACCOUNT_RESTRICTION
	AccountDisabled                  Status = 0xC0000072 // STATUS_ACCOUNT_DISABLED
	FileIsADirectory                 Status = 0xC00000BA // STATUS_FILE_IS_A_DIRECTORY
	NotSupported                     Status = 0xC00000BB // STATUS_NOT_SUPPORTED
	BadNetworkPath                   Status = 0xC00000BE // STATUS_BAD_NETWORK_PATH
	NetworkAccessDenied              Status = 0xC00000CA // STATUS_NETWORK_ACCESS_DENIED
	NetworkNameDeleted               Status = 0xC00000C9 // STATUS_NETWORK_NAME_DELETED
	BadDeviceType                    Status = 0xC00000CB // STATUS_BAD_DEVICE_TYPE
	BadNetworkName                   Status = 0xC00000CC // STATUS_BAD_NETWORK_NAME
	TooManySessions                  Status = 0xC00000CE // STATUS_TOO_MANY_SESSIONS
	RequestNotAccepted               Status = 0xC00000D0 // STATUS_REQUEST_NOT_ACCEPTED
	DirectoryNotEmpty                Status = 0xC0000101 // STATUS_DIRECTORY_NOT_EMPTY
	NotADirectory                    Status = 0xC0000103 // STATUS_NOT_A_DIRECTORY
	FileClosed                       Status = 0xC000010E // STATUS_FILE_CLOSED
	Cancelled                        Status = 0xC0000120 // STATUS_CANCELLED
	CannotDelete                     Status = 0xC0000121 // STATUS_CANNOT_DELETE
	FileInvalid                      Status = 0xC0000128 // STATUS_FILE_INVALID
	FileDeleted                      Status = 0xC0000123 // STATUS_FILE_DELETED
	InvalidLevel                     Status = 0xC0000148 // STATUS_INVALID_LEVEL
	FSDriverRequired                 Status = 0xC000019C // STATUS_FS_DRIVER_REQUIRED
	UserSessionDeleted               Status = 0xC0000203 // STATUS_USER_SESSION_DELETED
	ConnectionDisconnected           Status = 0xC000020C // STATUS_CONNECTION_DISCONNECTED
	NotFound                         Status = 0xC0000225 // STATUS_NOT_FOUND
	PathNotCovered                   Status = 0xC0000257 // STATUS_PATH_NOT_COVERED
	NotAReparsePoint                 Status = 0xC0000275 // STATUS_NOT_A_REPARSE_POINT
	InsuffServerResources            Status = 0xC000027E // STATUS_INSUFF_SERVER_RESOURCES
	InvalidDeviceState               Status = 0xC0000184 // STATUS_INVALID_DEVICE_STATE
	FileSystemLimitation             Status = 0xC0000427 // STATUS_FILE_SYSTEM_LIMITATION
	InvalidBufferSize                Status = 0xC0000206 // STATUS_INVALID_BUFFER_SIZE
	SMBNoPreauthIntegrityHashOverlap Status = 0xC05D0000 // STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP
	SMBBadClusterDialect             Status = 0xC05D0001 // STATUS_SMB_BAD_CLUSTER_DIALECT

	// oplock / lease 相关（MS-ERREF §2.3.1）。单独成组，避免打乱上面各组的对齐。
	//
	// OplockBreakInProgress 属于**成功类**（高两位为 0），语义是
	// 「open/create 完成时该文件上正有一个 oplock break 在进行中」，
	// 用于 CREATE 响应（MS-SMB2 §3.3.5.9.7）；客户端应按成功处理。
	// OplockNotGranted 表示 oplock 请求被拒绝。
	// InvalidOplockProtocol 表示收到了非法的 oplock break 确认
	// （MS-SMB2 §3.3.5.22.1 / §3.3.5.22.2：Ack 与当前 break 状态不匹配）。
	OplockBreakInProgress Status = 0x00000108 // STATUS_OPLOCK_BREAK_IN_PROGRESS
	OplockNotGranted      Status = 0xC00000E2 // STATUS_OPLOCK_NOT_GRANTED
	InvalidOplockProtocol Status = 0xC00000E3 // STATUS_INVALID_OPLOCK_PROTOCOL
)

// names 用于 String()。仅收录本包定义的值。
var names = map[Status]string{
	Success:          "STATUS_SUCCESS",
	Pending:          "STATUS_PENDING",
	Notify:           "STATUS_NOTIFY_CLEANUP",
	NotifyEn:         "STATUS_NOTIFY_ENUM_DIR",
	BufferOverflow:   "STATUS_BUFFER_OVERFLOW",
	NoMoreFiles:      "STATUS_NO_MORE_FILES",
	StoppedOnSymlink: "STATUS_STOPPED_ON_SYMLINK",

	Unsuccessful:           "STATUS_UNSUCCESSFUL",
	NotImplemented:         "STATUS_NOT_IMPLEMENTED",
	InvalidInfoClass:       "STATUS_INVALID_INFO_CLASS",
	InfoLengthMismatch:     "STATUS_INFO_LENGTH_MISMATCH",
	AccessViolation:        "STATUS_ACCESS_VIOLATION",
	InvalidHandle:          "STATUS_INVALID_HANDLE",
	InvalidParameter:       "STATUS_INVALID_PARAMETER",
	NoSuchDevice:           "STATUS_NO_SUCH_DEVICE",
	NoSuchFile:             "STATUS_NO_SUCH_FILE",
	InvalidDeviceRequest:   "STATUS_INVALID_DEVICE_REQUEST",
	EndOfFile:              "STATUS_END_OF_FILE",
	MoreProcessingRequired: "STATUS_MORE_PROCESSING_REQUIRED",
	NoMemory:               "STATUS_NO_MEMORY",
	AccessDenied:           "STATUS_ACCESS_DENIED",
	BufferTooSmall:         "STATUS_BUFFER_TOO_SMALL",
	ObjectTypeMismatch:     "STATUS_OBJECT_TYPE_MISMATCH",
	ObjectNameInvalid:      "STATUS_OBJECT_NAME_INVALID",
	ObjectNameNotFound:     "STATUS_OBJECT_NAME_NOT_FOUND",
	ObjectNameCollision:    "STATUS_OBJECT_NAME_COLLISION",
	ObjectPathInvalid:      "STATUS_OBJECT_PATH_INVALID",
	ObjectPathNotFound:     "STATUS_OBJECT_PATH_NOT_FOUND",
	ObjectPathSyntaxBad:    "STATUS_OBJECT_PATH_SYNTAX_BAD",
	DataError:              "STATUS_DATA_ERROR",
	SharingViolation:       "STATUS_SHARING_VIOLATION",
	LockConflict:           "STATUS_FILE_LOCK_CONFLICT",
	LockNotGranted:         "STATUS_LOCK_NOT_GRANTED",
	DeletePending:          "STATUS_DELETE_PENDING",
	RangeNotLocked:         "STATUS_RANGE_NOT_LOCKED",
	InvalidLockRange:       "STATUS_INVALID_LOCK_RANGE",
	DiskFull:               "STATUS_DISK_FULL",
	InsufficientResources:  "STATUS_INSUFFICIENT_RESOURCES",
	MediaWriteProtected:    "STATUS_MEDIA_WRITE_PROTECTED",
	NotSameDevice:          "STATUS_NOT_SAME_DEVICE",
	LogonFailure:           "STATUS_LOGON_FAILURE",
	AccountRestriction:     "STATUS_ACCOUNT_RESTRICTION",
	AccountDisabled:        "STATUS_ACCOUNT_DISABLED",
	FileIsADirectory:       "STATUS_FILE_IS_A_DIRECTORY",
	NotSupported:           "STATUS_NOT_SUPPORTED",
	BadNetworkPath:         "STATUS_BAD_NETWORK_PATH",
	NetworkAccessDenied:    "STATUS_NETWORK_ACCESS_DENIED",
	NetworkNameDeleted:     "STATUS_NETWORK_NAME_DELETED",
	BadDeviceType:          "STATUS_BAD_DEVICE_TYPE",
	BadNetworkName:         "STATUS_BAD_NETWORK_NAME",
	TooManySessions:        "STATUS_TOO_MANY_SESSIONS",
	RequestNotAccepted:     "STATUS_REQUEST_NOT_ACCEPTED",
	DirectoryNotEmpty:      "STATUS_DIRECTORY_NOT_EMPTY",
	NotADirectory:          "STATUS_NOT_A_DIRECTORY",
	FileClosed:             "STATUS_FILE_CLOSED",
	Cancelled:              "STATUS_CANCELLED",
	CannotDelete:           "STATUS_CANNOT_DELETE",
	FileInvalid:            "STATUS_FILE_INVALID",
	FileDeleted:            "STATUS_FILE_DELETED",
	InvalidLevel:           "STATUS_INVALID_LEVEL",
	FSDriverRequired:       "STATUS_FS_DRIVER_REQUIRED",
	UserSessionDeleted:     "STATUS_USER_SESSION_DELETED",
	ConnectionDisconnected: "STATUS_CONNECTION_DISCONNECTED",
	NotFound:               "STATUS_NOT_FOUND",
	PathNotCovered:         "STATUS_PATH_NOT_COVERED",
	NotAReparsePoint:       "STATUS_NOT_A_REPARSE_POINT",
	InsuffServerResources:  "STATUS_INSUFF_SERVER_RESOURCES",
	InvalidDeviceState:     "STATUS_INVALID_DEVICE_STATE",
	FileSystemLimitation:   "STATUS_FILE_SYSTEM_LIMITATION",
	InvalidBufferSize:      "STATUS_INVALID_BUFFER_SIZE",

	SMBNoPreauthIntegrityHashOverlap: "STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP",
	SMBBadClusterDialect:             "STATUS_SMB_BAD_CLUSTER_DIALECT",

	OplockBreakInProgress: "STATUS_OPLOCK_BREAK_IN_PROGRESS",
	OplockNotGranted:      "STATUS_OPLOCK_NOT_GRANTED",
	InvalidOplockProtocol: "STATUS_INVALID_OPLOCK_PROTOCOL",
}

// String 返回 NTSTATUS 的符号名；未知值返回十六进制形式。
func (s Status) String() string {
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("STATUS_UNKNOWN(0x%08X)", uint32(s))
}

// Error 使 Status 本身可以当作 error 使用，方便在协议层向上传递。
func (s Status) Error() string { return s.String() }

// severity 返回 NTSTATUS 的 Sev 字段（最高 2 位）。
func (s Status) severity() uint32 { return uint32(s) >> 30 }

// IsSuccess 报告该状态是否表示成功（Sev == 0）。
// 注意 STATUS_PENDING(0x00000103) 也属于成功类。
func (s Status) IsSuccess() bool { return s.severity() == sevSuccess }

// IsInformational 报告该状态是否为信息类（Sev == 1）。
func (s Status) IsInformational() bool { return s.severity() == sevInformational }

// IsWarning 报告该状态是否为警告类（Sev == 2），如 STATUS_BUFFER_OVERFLOW。
// 警告类响应仍然携带完整的响应体。
func (s Status) IsWarning() bool { return s.severity() == sevWarning }

// IsError 报告该状态是否为错误类（Sev == 3）。
func (s Status) IsError() bool { return s.severity() == sevError }

// FromVFSError 把 internal/vfs 的 sentinel error 映射为 NTSTATUS。
//
// nil → STATUS_SUCCESS。无法识别的错误保守映射为 STATUS_UNSUCCESSFUL，
// 避免把内部实现细节泄漏给客户端。
func FromVFSError(err error) Status {
	if err == nil {
		return Success
	}

	// Status 本身可以作为 error 传递，优先直接透传。
	var s Status
	if errors.As(err, &s) {
		return s
	}

	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return ObjectNameNotFound
	case errors.Is(err, vfs.ErrExist):
		return ObjectNameCollision
	case errors.Is(err, vfs.ErrNotDir):
		return NotADirectory
	case errors.Is(err, vfs.ErrIsDir):
		return FileIsADirectory
	case errors.Is(err, vfs.ErrNotEmpty):
		return DirectoryNotEmpty
	case errors.Is(err, vfs.ErrPermission):
		return AccessDenied
	case errors.Is(err, vfs.ErrReadOnly):
		return MediaWriteProtected
	case errors.Is(err, vfs.ErrInvalidPath):
		return ObjectPathInvalid
	case errors.Is(err, vfs.ErrNotSupported):
		return NotSupported
	case errors.Is(err, vfs.ErrNoSpace):
		return DiskFull
	case errors.Is(err, vfs.ErrTooLarge):
		return FileSystemLimitation
	case errors.Is(err, vfs.ErrClosed):
		return FileClosed
	}

	// 标准库错误（本地磁盘后端可能直接透出 *os.PathError 等）。
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return EndOfFile
	case errors.Is(err, fs.ErrNotExist):
		return ObjectNameNotFound
	case errors.Is(err, fs.ErrExist):
		return ObjectNameCollision
	case errors.Is(err, fs.ErrPermission):
		return AccessDenied
	case errors.Is(err, fs.ErrInvalid):
		return InvalidParameter
	case errors.Is(err, fs.ErrClosed):
		return FileClosed
	case errors.Is(err, os.ErrDeadlineExceeded):
		return Cancelled
	}

	return Unsuccessful
}
