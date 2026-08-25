//go:build windows

package vfs

// 这些用例只在 Windows 上跑（旁路存储只在 Windows 编译进来）。
// 开发期在 Linux 上验证过：把 metadata_windows.go 的 build tag 临时改成 linux
// 即可复现全部断言。

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) MetadataStore {
	t.Helper()
	s, err := openMetadataStore(t.TempDir(), filepath.Join(t.TempDir(), "md.db"), "")
	if err != nil {
		t.Fatalf("openMetadataStore: %v", err)
	}
	if s == nil {
		t.Fatal("Windows 上不应返回 nil store")
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMetadataPutGet(t *testing.T) {
	s := newTestStore(t)

	if _, ok := s.Get("nope"); ok {
		t.Error("不存在的 key 应返回 ok=false")
	}

	want := Metadata{UID: 501, GID: 20, Mode: 0o644}
	if err := s.Put("a/b.txt", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get("a/b.txt")
	if !ok || got != want {
		t.Errorf("Get = %+v, %v; want %+v, true", got, ok, want)
	}

	// 覆盖写
	want2 := Metadata{UID: 1000, GID: 1000, Mode: 0o600}
	if err := s.Put("a/b.txt", want2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("a/b.txt"); got != want2 {
		t.Errorf("覆盖写后 Get = %+v, want %+v", got, want2)
	}

	// 空 key 是调用方 bug，不能静默写进去
	if err := s.Put("", want); err == nil {
		t.Error("空 key 应报错")
	}
}

// TestMetadataDeleteSubtree 覆盖「删目录必须连子树一起清」——
// 否则在同名路径下重建对象会读到上一个对象的属主。
func TestMetadataDeleteSubtree(t *testing.T) {
	s := newTestStore(t)

	md := Metadata{UID: 1, GID: 2, Mode: 0o755}
	for _, k := range []string{"d", "d/f1", "d/sub/f2", "dd", "dd/f3"} {
		if err := s.Put(k, md); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete("d"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, k := range []string{"d", "d/f1", "d/sub/f2"} {
		if _, ok := s.Get(k); ok {
			t.Errorf("%q 应已被删除", k)
		}
	}
	// 前缀相近但不是子树的键不能被误删
	for _, k := range []string{"dd", "dd/f3"} {
		if _, ok := s.Get(k); !ok {
			t.Errorf("%q 不是 d 的子树，不应被删除", k)
		}
	}
}

func TestMetadataRenameSubtree(t *testing.T) {
	s := newTestStore(t)

	f1 := Metadata{UID: 11, GID: 12, Mode: 0o640}
	f2 := Metadata{UID: 21, GID: 22, Mode: 0o600}
	dir := Metadata{UID: 31, GID: 32, Mode: 0o750}
	if err := s.Put("old", dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("old/f1", f1); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("old/sub/f2", f2); err != nil {
		t.Fatal(err)
	}
	// 相近前缀，不该被搬走
	if err := s.Put("older", dir); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	for _, k := range []string{"old", "old/f1", "old/sub/f2"} {
		if _, ok := s.Get(k); ok {
			t.Errorf("源键 %q 应已被清除", k)
		}
	}
	cases := map[string]Metadata{"new": dir, "new/f1": f1, "new/sub/f2": f2}
	for k, want := range cases {
		got, ok := s.Get(k)
		if !ok || got != want {
			t.Errorf("Get(%q) = %+v, %v; want %+v, true", k, got, ok, want)
		}
	}
	if _, ok := s.Get("older"); !ok {
		t.Error("older 只是前缀相近，不应被搬走")
	}
}

// TestMetadataRenameOverwrite 覆盖「改名覆盖已有对象」：
// 目标的旧记录（含子树）必须被清掉，不能残留。
func TestMetadataRenameOverwrite(t *testing.T) {
	s := newTestStore(t)

	src := Metadata{UID: 1, GID: 1, Mode: 0o644}
	stale := Metadata{UID: 999, GID: 999, Mode: 0o777}
	if err := s.Put("src", src); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("dst", stale); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("dst/leftover", stale); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename("src", "dst"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("dst"); !ok || got != src {
		t.Errorf("dst = %+v, %v; want %+v, true", got, ok, src)
	}
	if _, ok := s.Get("dst/leftover"); ok {
		t.Error("被覆盖目标的子树记录必须清掉，否则会读到陈旧属主")
	}
}

// TestMetadataRenameMissing：源没有任何记录时不能把目标清掉。
func TestMetadataRenameMissing(t *testing.T) {
	s := newTestStore(t)

	keep := Metadata{UID: 7, GID: 7, Mode: 0o600}
	if err := s.Put("dst", keep); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("ghost", "dst"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("dst"); !ok || got != keep {
		t.Errorf("源无记录时不应动目标，dst = %+v, %v", got, ok)
	}
	// 退化输入
	if err := s.Rename("", "x"); err != nil {
		t.Errorf("空 key 应静默忽略: %v", err)
	}
	if err := s.Rename("a", "a"); err != nil {
		t.Errorf("同名改名应静默忽略: %v", err)
	}
}

func TestMetadataPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "md.db")

	s, err := openMetadataStore(dir, p, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Metadata{UID: 501, GID: 20, Mode: 0o664}
	if err := s.Put("f.txt", want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := openMetadataStore(dir, p, "")
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if got, ok := s2.Get("f.txt"); !ok || got != want {
		t.Errorf("重启后记录丢失: %+v, %v", got, ok)
	}
}

func TestMetadataCodec(t *testing.T) {
	md := Metadata{UID: 0xDEADBEEF, GID: 0x01020304, Mode: 0o7777}
	buf := encodeMetadata(md)
	if len(buf) != metadataRecordLen {
		t.Fatalf("编码长度 = %d, want %d", len(buf), metadataRecordLen)
	}
	// 小端
	if buf[0] != 0xEF || buf[1] != 0xBE || buf[2] != 0xAD || buf[3] != 0xDE {
		t.Errorf("UID 未按小端编码: % x", buf[:4])
	}
	got, ok := decodeMetadata(buf)
	if !ok || got != md {
		t.Errorf("往返 = %+v, %v; want %+v", got, ok, md)
	}
	// 截断的记录必须当作「没有」而不是 panic
	for n := 0; n < metadataRecordLen; n++ {
		if _, ok := decodeMetadata(buf[:n]); ok {
			t.Errorf("长度 %d 的残缺记录不应被接受", n)
		}
	}
	if _, ok := decodeMetadata(nil); ok {
		t.Error("nil 记录不应被接受")
	}
}

// TestDefaultMetadataPathOutsideShare：默认落点绝不能落在共享目录里，
// 否则客户端会看见并可能删掉这个库。
func TestDefaultMetadataPathOutsideShare(t *testing.T) {
	root := t.TempDir()
	p, err := defaultMetadataPath(root, "")
	if err != nil {
		t.Skipf("环境没有用户配置目录: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("默认落点应是绝对路径: %q", p)
	}
	if rel, err := filepath.Rel(root, p); err == nil && !filepath.IsAbs(rel) && rel != ".." && len(rel) > 2 && rel[:3] != ".."+string(filepath.Separator) {
		t.Errorf("默认落点 %q 落在共享目录 %q 内部", p, root)
	}
	// 同一个 root 必须稳定，否则重启后旧记录就找不到了
	p2, _ := defaultMetadataPath(root, "")
	if p != p2 {
		t.Errorf("默认落点不稳定: %q vs %q", p, p2)
	}
}

// TestDefaultMetadataPathDistinguishesInstances：非空 InstanceID 必须改变
// 默认落点，且同一 InstanceID 两次推导结果一致 —— 这是「共享同一目录的多个
// 服务进程各用各的 bbolt 库（bbolt 按 path 拿 flock）」的前提。
func TestDefaultMetadataPathDistinguishesInstances(t *testing.T) {
	root := t.TempDir()
	a1, err := defaultMetadataPath(root, "127.0.0.1:4451")
	if err != nil {
		t.Skipf("环境没有用户配置目录: %v", err)
	}
	a2, _ := defaultMetadataPath(root, "127.0.0.1:4451")
	b, err := defaultMetadataPath(root, "127.0.0.1:4452")
	if err != nil {
		t.Fatal(err)
	}
	hist, _ := defaultMetadataPath(root, "")
	if a1 != a2 {
		t.Errorf("同一实例两次推导不一致: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("不同实例得到了同一个默认落点: %q", a1)
	}
	if a1 == hist {
		t.Errorf("实例片段没有编进文件名: %q", a1)
	}
}
