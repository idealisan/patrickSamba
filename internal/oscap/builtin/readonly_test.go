package builtin

// readonly_test.go —— 只读共享上**每一个写方法**都必须返回 ErrReadOnly。
//
// 为什么要逐个列出来而不是抽样：本项目吃过「只测了允许这条路径，拒绝那条
// 压根没接线」的亏（AGENTS.md 记录的 encryption_required 假阳性）。
// 写方法漏掉一个，那个方法就会在只读共享上**真的写下去**，
// 而且因为返回 nil，上层永远不会察觉。

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

func TestPortableReadOnlyRejectsEveryWrite(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.bin", bytesRepeat(0xAA, 2*zeroBlockSize))

	// 先在可写状态下铺一份数据，这样只读态下的读路径有东西可读 ——
	// 否则「读得到、写不了」这条性质根本走不到。
	if err := e.set.Xattr.SetXattr(ref, "attr", []byte("v")); err != nil {
		t.Fatalf("准备属性失败: %v", err)
	}
	if err := e.set.Times.SetCreationTime(ref, time.Unix(1, 0)); err != nil {
		t.Fatalf("准备创建时间失败: %v", err)
	}
	if err := e.set.DOS.SetDOSAttributes(ref, 0x2); err != nil {
		t.Fatalf("准备 DOS 属性失败: %v", err)
	}
	h, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("准备命名流失败: %v", err)
	}
	if _, err := h.WriteAt([]byte("payload"), 0); err != nil {
		t.Fatalf("准备命名流内容失败: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("关闭流失败: %v", err)
	}
	if err := e.set.Sparse.PunchHole(ref, 0, zeroBlockSize); err != nil {
		t.Fatalf("准备空洞失败: %v", err)
	}

	e.reopen(true)

	writes := []struct {
		name string
		fn   func() error
	}{
		{"SetXattr", func() error { return e.set.Xattr.SetXattr(ref, "x", []byte("1")) }},
		{"RemoveXattr", func() error { return e.set.Xattr.RemoveXattr(ref, "attr") }},
		{"PunchHole", func() error { return e.set.Sparse.PunchHole(ref, 0, 16) }},
		{"Preallocate", func() error { return e.set.Sparse.Preallocate(ref, 0, 16) }},
		{"SetSparse(true)", func() error { return e.set.Sparse.SetSparse(ref, true) }},
		{"SetSparse(false)", func() error { return e.set.Sparse.SetSparse(ref, false) }},
		{"OpenStream(create)", func() error {
			_, err := e.set.Streams.OpenStream(ref, "new", oscap.StreamCreate|oscap.StreamWrite)
			return err
		}},
		{"OpenStream(write)", func() error {
			_, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamWrite)
			return err
		}},
		{"OpenStream(truncate)", func() error {
			_, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamTruncate)
			return err
		}},
		{"RemoveStream", func() error { return e.set.Streams.RemoveStream(ref, "s") }},
		{"SetCreationTime", func() error { return e.set.Times.SetCreationTime(ref, time.Now()) }},
		{"SetDOSAttributes", func() error { return e.set.DOS.SetDOSAttributes(ref, 0x20) }},
	}
	for _, w := range writes {
		if err := w.fn(); !errors.Is(err, oscap.ErrReadOnly) {
			t.Errorf("%s 在只读共享上应得 ErrReadOnly，实得 %v", w.name, err)
		}
	}

	// 反向对照：读路径必须照常工作，否则上面全绿也可能只是「整个实现都坏了」。
	if v, err := e.set.Xattr.GetXattr(ref, "attr"); err != nil || string(v) != "v" {
		t.Errorf("只读共享上读属性失败: (%q, %v)", v, err)
	}
	if names, err := e.set.Xattr.ListXattr(ref); err != nil || len(names) != 1 {
		t.Errorf("只读共享上列属性失败: (%v, %v)", names, err)
	}
	if ct, err := e.set.Times.CreationTime(ref); err != nil || !ct.Equal(time.Unix(1, 0)) {
		t.Errorf("只读共享上读创建时间失败: (%v, %v)", ct, err)
	}
	if attrs, err := e.set.DOS.DOSAttributes(ref); err != nil || attrs != 0x2 {
		t.Errorf("只读共享上读 DOS 属性失败: (%#x, %v)", attrs, err)
	}
	// FileID 在只读共享上分两种情况：宿主给得出 inode 就照常返回，
	// 给不出就只能如实说 ErrNotSupported（不许发一个会变的号）。
	fi, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	_, hasInode := inodeOf(fi)
	switch _, idErr := e.set.IDs.FileID(ref); {
	case hasInode && idErr != nil:
		t.Errorf("宿主提供 inode 时只读共享也该给出 FileID，实得 %v", idErr)
	case !hasInode && !errors.Is(idErr, oscap.ErrNotSupported):
		t.Errorf("拿不到 inode 且只读时应得 ErrNotSupported，实得 %v", idErr)
	}
	ro, err := e.set.Streams.OpenStream(ref, "s", oscap.StreamRead)
	if err != nil {
		t.Fatalf("只读共享上打开命名流失败: %v", err)
	}
	if size, err := ro.Size(); err != nil || size != int64(len("payload")) {
		t.Errorf("只读共享上流长度不对: %d (err=%v)", size, err)
	}
	if err := ro.Close(); err != nil {
		t.Errorf("关闭失败: %v", err)
	}
	if got, err := e.set.Sparse.AllocatedRanges(ref, 0, 2*zeroBlockSize); err != nil ||
		len(got) != 1 || got[0].Offset != zeroBlockSize {
		t.Errorf("只读共享上查已分配区间不对: (%v, %v)", got, err)
	}

	// 被拒的写不许留下任何痕迹。
	if _, err := e.set.Xattr.GetXattr(ref, "x"); !errors.Is(err, oscap.ErrNotFound) {
		t.Errorf("被拒的 SetXattr 竟然写进去了")
	}
}

// TestPortableReadOnlyWithoutExistingStore 覆盖「只读共享 + 库文件根本不存在」。
//
// 这是真实场景（只读导出的共享从来没被写过），实现必须**不创建任何文件**，
// 读一律「查不到」、写一律 ErrReadOnly，而不是启动失败。
func TestPortableReadOnlyWithoutExistingStore(t *testing.T) {
	e := &env{t: t, root: t.TempDir(), meta: t.TempDir() + "/never-created.db"}
	e.open(true)
	ref := e.file("a.txt", []byte("x"))

	if _, err := e.set.Xattr.GetXattr(ref, "any"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("空库读属性应得 ErrNotFound，实得 %v", err)
	}
	if err := e.set.Xattr.SetXattr(ref, "any", nil); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("空库写属性应得 ErrReadOnly，实得 %v", err)
	}
	if _, err := e.set.Times.CreationTime(ref); !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("空库读创建时间应得 ErrNotSupported，实得 %v", err)
	}
	if _, err := os.Stat(e.meta); err == nil {
		t.Fatalf("只读共享不得创建库文件，但 %s 出现了", e.meta)
	}
}
