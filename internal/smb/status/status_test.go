package status

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 表中的值取自 MS-ERREF §2.3.1，与 docs/protocol-notes.md §13 一致。
func TestKnownValues(t *testing.T) {
	cases := []struct {
		s    Status
		v    uint32
		name string
	}{
		{Success, 0x00000000, "STATUS_SUCCESS"},
		{Pending, 0x00000103, "STATUS_PENDING"},
		{BufferOverflow, 0x80000005, "STATUS_BUFFER_OVERFLOW"},
		{NoMoreFiles, 0x80000006, "STATUS_NO_MORE_FILES"},
		{Unsuccessful, 0xC0000001, "STATUS_UNSUCCESSFUL"},
		{NotImplemented, 0xC0000002, "STATUS_NOT_IMPLEMENTED"},
		{InvalidParameter, 0xC000000D, "STATUS_INVALID_PARAMETER"},
		{NoSuchFile, 0xC000000F, "STATUS_NO_SUCH_FILE"},
		{InvalidDeviceRequest, 0xC0000010, "STATUS_INVALID_DEVICE_REQUEST"},
		{EndOfFile, 0xC0000011, "STATUS_END_OF_FILE"},
		{MoreProcessingRequired, 0xC0000016, "STATUS_MORE_PROCESSING_REQUIRED"},
		{AccessDenied, 0xC0000022, "STATUS_ACCESS_DENIED"},
		{BufferTooSmall, 0xC0000023, "STATUS_BUFFER_TOO_SMALL"},
		{ObjectNameNotFound, 0xC0000034, "STATUS_OBJECT_NAME_NOT_FOUND"},
		{ObjectNameCollision, 0xC0000035, "STATUS_OBJECT_NAME_COLLISION"},
		{ObjectPathNotFound, 0xC000003A, "STATUS_OBJECT_PATH_NOT_FOUND"},
		{SharingViolation, 0xC0000043, "STATUS_SHARING_VIOLATION"},
		{LogonFailure, 0xC000006D, "STATUS_LOGON_FAILURE"},
		{FileIsADirectory, 0xC00000BA, "STATUS_FILE_IS_A_DIRECTORY"},
		{NotSupported, 0xC00000BB, "STATUS_NOT_SUPPORTED"},
		{NetworkNameDeleted, 0xC00000C9, "STATUS_NETWORK_NAME_DELETED"},
		{BadNetworkName, 0xC00000CC, "STATUS_BAD_NETWORK_NAME"},
		{DirectoryNotEmpty, 0xC0000101, "STATUS_DIRECTORY_NOT_EMPTY"},
		{NotADirectory, 0xC0000103, "STATUS_NOT_A_DIRECTORY"},
		{FileClosed, 0xC000010E, "STATUS_FILE_CLOSED"},
		{Cancelled, 0xC0000120, "STATUS_CANCELLED"},
		{FileInvalid, 0xC0000128, "STATUS_FILE_INVALID"},
		{FSDriverRequired, 0xC000019C, "STATUS_FS_DRIVER_REQUIRED"},
		{UserSessionDeleted, 0xC0000203, "STATUS_USER_SESSION_DELETED"},
		{NotFound, 0xC0000225, "STATUS_NOT_FOUND"},
		{PathNotCovered, 0xC0000257, "STATUS_PATH_NOT_COVERED"},
		{InsuffServerResources, 0xC000027E, "STATUS_INSUFF_SERVER_RESOURCES"},
		{SMBNoPreauthIntegrityHashOverlap, 0xC05D0000, "STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP"},
	}
	for _, c := range cases {
		if uint32(c.s) != c.v {
			t.Errorf("%s = 0x%08X, 期望 0x%08X", c.name, uint32(c.s), c.v)
		}
		if got := c.s.String(); got != c.name {
			t.Errorf("Status(0x%08X).String() = %q, 期望 %q", c.v, got, c.name)
		}
	}
}

func TestUnknownString(t *testing.T) {
	s := Status(0xDEADBEEF)
	want := "STATUS_UNKNOWN(0xDEADBEEF)"
	if got := s.String(); got != want {
		t.Errorf("String() = %q, 期望 %q", got, want)
	}
}

func TestSeverity(t *testing.T) {
	if !Success.IsSuccess() || Success.IsError() {
		t.Error("STATUS_SUCCESS 的 severity 判定错误")
	}
	if !Pending.IsSuccess() {
		t.Error("STATUS_PENDING 应属于成功类（Sev=0）")
	}
	if !BufferOverflow.IsWarning() || BufferOverflow.IsError() {
		t.Error("STATUS_BUFFER_OVERFLOW 应属于警告类（Sev=2）")
	}
	if !AccessDenied.IsError() {
		t.Error("STATUS_ACCESS_DENIED 应属于错误类（Sev=3）")
	}
}

func TestFromVFSError(t *testing.T) {
	cases := []struct {
		err  error
		want Status
	}{
		{nil, Success},
		{vfs.ErrNotFound, ObjectNameNotFound},
		{vfs.ErrExist, ObjectNameCollision},
		{vfs.ErrNotDir, NotADirectory},
		{vfs.ErrIsDir, FileIsADirectory},
		{vfs.ErrNotEmpty, DirectoryNotEmpty},
		{vfs.ErrPermission, AccessDenied},
		{vfs.ErrReadOnly, MediaWriteProtected},
		{vfs.ErrInvalidPath, ObjectPathInvalid},
		{vfs.ErrNotSupported, NotSupported},
		{vfs.ErrNoSpace, DiskFull},
		{vfs.ErrTooLarge, FileSystemLimitation},
		{vfs.ErrClosed, FileClosed},
		{io.EOF, EndOfFile},
		{fs.ErrNotExist, ObjectNameNotFound},
		{fs.ErrPermission, AccessDenied},
		{errors.New("某个未知错误"), Unsuccessful},
		// Status 本身作为 error 时应原样透传。
		{SharingViolation, SharingViolation},
		// 包装过的错误也要能识别。
		{fmt.Errorf("open foo: %w", vfs.ErrNotFound), ObjectNameNotFound},
	}
	for _, c := range cases {
		if got := FromVFSError(c.err); got != c.want {
			t.Errorf("FromVFSError(%v) = %v, 期望 %v", c.err, got, c.want)
		}
	}
}
