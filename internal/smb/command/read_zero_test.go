package command

import (
	"bytes"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// 本文件钉的是 bh4-A#3：零长度 READ 应成功返回 0 字节，而不是
// STATUS_END_OF_FILE；且目录/权限检查必须排在一切按内容判定的状态之前。
// Samba smb2_read.c:404-407 仅在 nread==0 && in_length!=0 时回 END_OF_FILE；
// torture source4/torture/smb2/read.c:92-97（Windows 归纳）：
// length=0,min_count=0 → OK 且 0 字节；length=0,min_count=1 → END_OF_FILE。

// TestZeroLengthReadSucceeds：length=0 的读必须成功回空数据，
// 不论文件大小与 offset 在哪里。
func TestZeroLengthReadSucceeds(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("abcdef"), 16))
	for _, off := range []uint64{0, 32, 1 << 40} {
		resp, err := p.runRead(t, p.b, off, 0)
		if err != nil {
			t.Fatalf("offset=%d length=0 的读应成功，实际 %v", off, err)
		}
		if len(resp.Data) != 0 {
			t.Fatalf("offset=%d length=0 应回 0 字节，实际 %d", off, len(resp.Data))
		}
	}
}

// TestZeroLengthReadChecksAccessFirst：零长度读也必须先过权限检查 ——
// 无读权限的句柄应回 ACCESS_DENIED 而不是 END_OF_FILE。
// （旧行为「先看长度后鉴权」会向无权客户端泄露文件存在性之外的语义。）
func TestZeroLengthReadChecksAccessFirst(t *testing.T) {
	p := newLockIOPair(t, []byte("secret"))
	noRead := &Open{
		Path:          "f",
		Tree:          p.tree,
		Handle:        p.a.Handle,
		GrantedAccess: wire.FileWriteData, // 有写无读
	}
	wantStatus(t, "无读权限句柄的零长读",
		func() error {
			_, err := p.runRead(t, noRead, 0, 0)
			return err
		}(), status.AccessDenied)

	// 非零长度同样要拒（顺带钉住顺序对两种长度一致）。
	wantStatus(t, "无读权限句柄的非零长读",
		func() error {
			_, err := p.runRead(t, noRead, 0, 4)
			return err
		}(), status.AccessDenied)
}

// TestZeroLengthReadOnDirRejected：目录句柄的读（含零长）一律
// INVALID_DEVICE_REQUEST，不能因长度为 0 就提前放行为 END_OF_FILE。
func TestZeroLengthReadOnDirRejected(t *testing.T) {
	p := newLockIOPair(t, []byte("x"))
	dirOpen := &Open{
		Path:          "",
		Tree:          p.tree,
		IsDir:         true,
		GrantedAccess: wire.FileReadData | wire.FileExecute,
	}
	for _, l := range []uint64{0, 16} {
		wantStatus(t, "目录句柄 READ",
			func() error {
				_, err := p.runRead(t, dirOpen, 0, l)
				return err
			}(), status.InvalidDeviceRequest)
	}
}

// TestReadPastEOFStillEndOfFile：回归钉子 —— offset ≥ EOF 且请求了至少
// 1 字节时仍然必须回 END_OF_FILE（findings-bh4 B#1 查证双方一致的行为）。
func TestReadPastEOFStillEndOfFile(t *testing.T) {
	p := newLockIOPair(t, []byte("tiny"))
	wantStatus(t, "offset 越界读", func() error {
		_, err := p.runRead(t, p.b, 100, 4)
		return err
	}(), status.EndOfFile)
	// 部分越界：能读到多少回多少（4 字节文件从 2 读 → 2 字节）。
	if resp, err := p.runRead(t, p.b, 2, 10); err != nil {
		t.Fatalf("部分越界读不应报错: %v", err)
	} else if string(resp.Data) != "ny" {
		t.Fatalf("部分越界读应回 \"ny\"，实际 %q", resp.Data)
	}
}
