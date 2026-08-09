//go:build !windows && !metabolt

package meta

// noop_test.go —— 验证非 Windows 平台上的空实现真的是「空」的。
//
// AGENTS.md §5 P7 的原话是「Linux/macOS 原生能力足够，使用 noop 实现，零开销」。
// 「零开销」是可证伪的：不建文件、不占内存、编译期就能被消掉。

import (
	"os"
	"testing"
	"unsafe"
)

func TestNoopIsInert(t *testing.T) {
	if Enabled {
		t.Fatal("非 Windows 平台 Enabled 必须为 false")
	}

	dir := t.TempDir()
	s, err := Open(dir, dir+"/should-not-be-created.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s == nil {
		t.Fatal("必须返回空实现而不是 nil —— nil 接口一旦被漏判就是 panic")
	}

	rec := Record{UID: 501, GID: 20, Mode: 0o644, FileID: 7}
	if err := s.Put("a/b.txt", rec); err != nil {
		t.Errorf("Put 应静默成功: %v", err)
	}
	// 反向对照：Put 报了成功，但**绝不能真的存下来** ——
	// 若这里查得到，说明 Linux 上误链了真实实现，宿主原生属性会被旁路值覆盖。
	if got, ok := s.Get("a/b.txt"); ok {
		t.Errorf("noop 不该存下任何东西，却取到 %+v", got)
	}
	if m := s.GetDir(""); m != nil {
		t.Errorf("GetDir 应返回 nil, got %v", m)
	}
	if err := s.Delete("a/b.txt"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := s.Rename("a", "b"); err != nil {
		t.Errorf("Rename: %v", err)
	}
	// alive 回调一次都不该被调到（库里本来就没有记录）
	called := false
	n, err := s.Reap(func(string, Record) bool { called = true; return false })
	if n != 0 || err != nil || called {
		t.Errorf("Reap = %d, %v, called=%v; want 0, nil, false", n, err, called)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// 「不建文件」：跑完全套操作后目录必须还是空的
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("noop 不该在磁盘上留下任何东西，实际有 %d 项: %v", len(ents), ents)
	}
}

// TestNoopIsZeroSized：空结构体不占内存，接口值里也就没有堆分配。
func TestNoopIsZeroSized(t *testing.T) {
	if sz := unsafe.Sizeof(noopStore{}); sz != 0 {
		t.Errorf("noopStore 大小 = %d, want 0", sz)
	}
}
