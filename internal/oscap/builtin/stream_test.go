package builtin

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

func TestPortableNamedStreamRoundTrip(t *testing.T) {
	e := newEnv(t)
	ref := e.file("doc.txt", []byte("main data"))

	if s, err := e.set.Streams.ListStreams(ref); err != nil || s != nil {
		t.Fatalf("没有命名流时应得 (nil, nil)，实得 (%v, %v)", s, err)
	}
	if _, err := e.set.Streams.OpenStream(ref, "AFP_Resource", oscap.StreamRead); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("不带 StreamCreate 打开不存在的流应得 ErrNotFound，实得 %v", err)
	}
	if _, err := e.set.Streams.OpenStream(ref, "", oscap.StreamRead|oscap.StreamCreate); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空流名应得 ErrInvalidArg，实得 %v", err)
	}

	h, err := e.set.Streams.OpenStream(ref, "AFP_Resource",
		oscap.StreamRead|oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("创建流失败: %v", err)
	}

	// 刚创建、尚未写入就应该能被列出来 —— 客户端已经拿到成功的 create 应答了。
	streams, err := e.set.Streams.ListStreams(ref)
	if err != nil || len(streams) != 1 || streams[0].Name != "AFP_Resource" || streams[0].Size != 0 {
		t.Fatalf("新建的空流未被列出: (%v, %v)", streams, err)
	}

	payload := []byte("resource fork bytes")
	n, err := h.WriteAt(payload, 0)
	if err != nil || n != len(payload) {
		t.Fatalf("WriteAt 失败: n=%d err=%v", n, err)
	}
	if size, err := h.Size(); err != nil || size != int64(len(payload)) {
		t.Fatalf("Size 不对: %d (err=%v)", size, err)
	}

	buf := make([]byte, len(payload))
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt 失败: %v", err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("读回来不一致: %q", buf)
	}

	// 稀疏写：跳到偏移之后写，中间补零。
	if _, err := h.WriteAt([]byte("Z"), 100); err != nil {
		t.Fatalf("越位 WriteAt 失败: %v", err)
	}
	if size, _ := h.Size(); size != 101 {
		t.Fatalf("越位写后长度应为 101，实得 %d", size)
	}
	gap := make([]byte, 10)
	if _, err := h.ReadAt(gap, 50); err != nil {
		t.Fatalf("读空隙失败: %v", err)
	}
	if !bytes.Equal(gap, make([]byte, 10)) {
		t.Fatalf("空隙应补零，实得 %v", gap)
	}

	if err := h.Truncate(5); err != nil {
		t.Fatalf("Truncate 失败: %v", err)
	}
	if size, _ := h.Size(); size != 5 {
		t.Fatalf("截断后长度应为 5，实得 %d", size)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 关闭后一律 ErrClosed，不许静默成功。
	if err := h.Close(); !errors.Is(err, oscap.ErrClosed) {
		t.Fatalf("重复 Close 应得 ErrClosed，实得 %v", err)
	}
	if _, err := h.Size(); !errors.Is(err, oscap.ErrClosed) {
		t.Fatalf("关闭后 Size 应得 ErrClosed，实得 %v", err)
	}
	if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, oscap.ErrClosed) {
		t.Fatalf("关闭后 WriteAt 应得 ErrClosed，实得 %v", err)
	}

	// 落盘校验：重开库之后内容还在。
	e.reopen(false)
	h2, err := e.set.Streams.OpenStream(ref, "AFP_Resource", oscap.StreamRead)
	if err != nil {
		t.Fatalf("重开后打开流失败: %v", err)
	}
	got := make([]byte, 5)
	if _, err := h2.ReadAt(got, 0); err != nil {
		t.Fatalf("重开后读失败: %v", err)
	}
	if !bytes.Equal(got, payload[:5]) {
		t.Fatalf("重开后内容不对: %q", got)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	if err := e.set.Streams.RemoveStream(ref, "AFP_Resource"); err != nil {
		t.Fatalf("RemoveStream 失败: %v", err)
	}
	if err := e.set.Streams.RemoveStream(ref, "AFP_Resource"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("重复删除应得 ErrNotFound，实得 %v", err)
	}
	if s, err := e.set.Streams.ListStreams(ref); err != nil || s != nil {
		t.Fatalf("删除后应得 (nil, nil)，实得 (%v, %v)", s, err)
	}
}

func TestPortableNamedStreamReadAtEOFContract(t *testing.T) {
	e := newEnv(t)
	ref := e.file("doc.txt", nil)
	h, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamRead|oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("创建流失败: %v", err)
	}
	defer func() { _ = h.Close() }()

	if _, err := h.WriteAt([]byte("abc"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}

	// 读到末尾且未填满：必须是 (n, io.EOF)。SMB READ 靠这个判流尾。
	buf := make([]byte, 10)
	n, err := h.ReadAt(buf, 1)
	if n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("短读契约不对: n=%d err=%v，期望 (2, io.EOF)", n, err)
	}
	// 完全越过末尾：(0, io.EOF)。
	if n, err := h.ReadAt(buf, 3); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("越尾读契约不对: n=%d err=%v", n, err)
	}
	// 恰好读满且到末尾：err 必须是 nil（多读一个字节才是 EOF）。
	if n, err := h.ReadAt(buf[:3], 0); n != 3 || err != nil {
		t.Fatalf("读满不应报 EOF: n=%d err=%v", n, err)
	}
	if _, err := h.ReadAt(buf, -1); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("负偏移应得 ErrInvalidArg，实得 %v", err)
	}
}

func TestPortableNamedStreamTruncateFlag(t *testing.T) {
	e := newEnv(t)
	ref := e.file("doc.txt", nil)

	h, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("创建流失败: %v", err)
	}
	if _, err := h.WriteAt([]byte("0123456789"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	h2, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamWrite|oscap.StreamTruncate)
	if err != nil {
		t.Fatalf("带截断打开失败: %v", err)
	}
	if size, _ := h2.Size(); size != 0 {
		t.Fatalf("StreamTruncate 后长度应为 0，实得 %d", size)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	streams, err := e.set.Streams.ListStreams(ref)
	if err != nil || len(streams) != 1 || streams[0].Size != 0 {
		t.Fatalf("截断未落盘: (%v, %v)", streams, err)
	}
}

// TestPortableNamedStreamReadOnlyHandleRejectsWrite 验证「只读打开的句柄不能写」。
//
// 这条与共享级只读是两回事：共享可写，但这个句柄只申请了读。
func TestPortableNamedStreamReadOnlyHandleRejectsWrite(t *testing.T) {
	e := newEnv(t)
	ref := e.file("doc.txt", nil)

	h, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("创建流失败: %v", err)
	}
	if _, err := h.WriteAt([]byte("data"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	ro, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamRead)
	if err != nil {
		t.Fatalf("只读打开失败: %v", err)
	}
	defer func() { _ = ro.Close() }()
	if _, err := ro.WriteAt([]byte("x"), 0); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读句柄写入应被拒，实得 %v", err)
	}
	if err := ro.Truncate(0); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读句柄截断应被拒，实得 %v", err)
	}
	// 被拒之后内容不许有任何变化。
	if size, _ := ro.Size(); size != 4 {
		t.Fatalf("被拒的写不该改变内容，长度实得 %d", size)
	}
}
