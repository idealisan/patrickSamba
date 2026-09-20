package builtin

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

func TestPortableXattrRoundTrip(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", []byte("hello"))

	if _, err := e.set.Xattr.GetXattr(ref, "com.apple.FinderInfo"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("读不存在的属性应得 ErrNotFound，实得 %v", err)
	}
	if names, err := e.set.Xattr.ListXattr(ref); err != nil || names != nil {
		t.Fatalf("一个属性都没有时应得 (nil, nil)，实得 (%v, %v)", names, err)
	}

	want := []byte{0x00, 0x01, 0xFF}
	if err := e.set.Xattr.SetXattr(ref, "com.apple.FinderInfo", want); err != nil {
		t.Fatalf("SetXattr 失败: %v", err)
	}
	got, err := e.set.Xattr.GetXattr(ref, "com.apple.FinderInfo")
	if err != nil {
		t.Fatalf("GetXattr 失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("值不一致: 得 %v 期望 %v", got, want)
	}

	// 覆盖写。
	if err := e.set.Xattr.SetXattr(ref, "com.apple.FinderInfo", []byte("x")); err != nil {
		t.Fatalf("覆盖 SetXattr 失败: %v", err)
	}
	if got, _ := e.set.Xattr.GetXattr(ref, "com.apple.FinderInfo"); !bytes.Equal(got, []byte("x")) {
		t.Fatalf("覆盖后值不对: %v", got)
	}

	// 「存在但为空」必须与「不存在」区分开（ports.go 明文要求）。
	if err := e.set.Xattr.SetXattr(ref, "empty", []byte{}); err != nil {
		t.Fatalf("写空值失败: %v", err)
	}
	v, err := e.set.Xattr.GetXattr(ref, "empty")
	if err != nil {
		t.Fatalf("读空值属性失败: %v", err)
	}
	if v == nil {
		t.Fatal("空值属性应返回非 nil 的零长切片，实得 nil —— 这样调用方无法与「不存在」区分")
	}
	if len(v) != 0 {
		t.Fatalf("空值属性长度应为 0，实得 %d", len(v))
	}

	// ListXattr 报出来的名字必须能拿去 GetXattr 取到（ports.go 强调的对称性）。
	names, err := e.set.Xattr.ListXattr(ref)
	if err != nil {
		t.Fatalf("ListXattr 失败: %v", err)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "com.apple.FinderInfo" || names[1] != "empty" {
		t.Fatalf("属性名列表不对: %v", names)
	}
	for _, n := range names {
		if _, err := e.set.Xattr.GetXattr(ref, n); err != nil {
			t.Fatalf("ListXattr 报了 %q 但 GetXattr 取不到: %v", n, err)
		}
	}

	// 别的对象不该串味。
	other := e.file("b.txt", nil)
	if names, err := e.set.Xattr.ListXattr(other); err != nil || names != nil {
		t.Fatalf("另一个文件不该看到属性，实得 (%v, %v)", names, err)
	}

	if err := e.set.Xattr.RemoveXattr(ref, "empty"); err != nil {
		t.Fatalf("RemoveXattr 失败: %v", err)
	}
	if err := e.set.Xattr.RemoveXattr(ref, "empty"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("重复删除应得 ErrNotFound，实得 %v", err)
	}
	if _, err := e.set.Xattr.GetXattr(ref, "empty"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("删除后读应得 ErrNotFound，实得 %v", err)
	}
}

// TestPortableXattrPrefixIsolation 盯的是 key 拼接：前缀相同的两个路径不能互相看到属性。
//
// 这不是假想问题 —— key 是 pathKey+分隔符+name 拼出来的，
// 分隔符选错（比如用 '/'）时 "a" 的前缀扫描会扫到 "a/b" 上去。
func TestPortableXattrPrefixIsolation(t *testing.T) {
	e := newEnv(t)
	dir := e.file("dir", nil)
	nested := e.file("dir2/x.txt", nil)

	if err := e.set.Xattr.SetXattr(nested, "n", []byte("1")); err != nil {
		t.Fatalf("SetXattr 失败: %v", err)
	}
	if names, err := e.set.Xattr.ListXattr(dir); err != nil || names != nil {
		t.Fatalf("前缀相邻的对象串味了: (%v, %v)", names, err)
	}
}

func TestPortableXattrEmptyName(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)
	if err := e.set.Xattr.SetXattr(ref, "", []byte("x")); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空属性名应得 ErrInvalidArg，实得 %v", err)
	}
	if _, err := e.set.Xattr.GetXattr(ref, ""); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空属性名应得 ErrInvalidArg，实得 %v", err)
	}
	if err := e.set.Xattr.RemoveXattr(ref, ""); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空属性名应得 ErrInvalidArg，实得 %v", err)
	}
}
