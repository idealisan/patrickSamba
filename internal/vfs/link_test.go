package vfs

// link_test.go —— 硬链接（FileLinkInformation）测试。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func linkerOf(t *testing.T, fs *LocalFS) HardLinker {
	t.Helper()
	hl, ok := any(fs).(HardLinker)
	if !ok {
		t.Fatal("LocalFS 未实现 HardLinker")
	}
	return hl
}

func TestLinkBasic(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "dir/src.txt", "payload")
	hl := linkerOf(t, fs)

	if err := hl.Link("dir/src.txt", "dir/hard.txt", false); err != nil {
		t.Fatalf("Link: %v", err)
	}

	// 两个名字必须指向同一个对象：内容一致、FileID（inode）一致、
	// 链接数为 2。客户端就是靠 FileID 判断「这是同一个文件」的。
	src, err := fs.Stat("dir/src.txt")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := fs.Stat("dir/hard.txt")
	if err != nil {
		t.Fatalf("链接目标不可见: %v", err)
	}
	if src.FileID != dst.FileID {
		t.Errorf("FileID 不一致: src=%d dst=%d", src.FileID, dst.FileID)
	}
	if dst.Size != int64(len("payload")) {
		t.Errorf("链接目标大小 = %d；期望 %d", dst.Size, len("payload"))
	}
	if dst.NLink < 2 {
		t.Errorf("NLink = %d；硬链接后应 >= 2", dst.NLink)
	}

	// 通过任一名字写入，另一个名字必须看得到。
	if err := os.WriteFile(filepath.Join(fs.Root(), "dir", "hard.txt"), []byte("changed!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, err := fs.Stat("dir/src.txt"); err != nil || a.Size != 8 {
		t.Errorf("写 hard.txt 后 src.txt 大小 = %v, %v；期望 8", a, err)
	}
}

func TestLinkReplace(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "src", "aaa")
	writeFile(t, fs, "dst", "bbbbbb")
	hl := linkerOf(t, fs)

	// ReplaceIfExists=false：目标已存在 → 冲突。
	if err := hl.Link("src", "dst", false); !errors.Is(err, ErrExist) {
		t.Errorf("replace=false 时 err = %v；期望 ErrExist", err)
	}
	// 原目标不能被动过。
	if a, _ := fs.Stat("dst"); a == nil || a.Size != 6 {
		t.Errorf("失败的 Link 不应改动目标: %+v", a)
	}

	// ReplaceIfExists=true：覆盖。
	if err := hl.Link("src", "dst", true); err != nil {
		t.Fatalf("replace=true 时 Link: %v", err)
	}
	a, err := fs.Stat("dst")
	if err != nil || a.Size != 3 {
		t.Errorf("覆盖后 dst = %+v, %v；期望大小 3", a, err)
	}
}

func TestLinkSelf(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "same", "keepme")
	hl := linkerOf(t, fs)

	// 链到自己：必须是 no-op 而不是「先删再建」——后者会丢数据。
	if err := hl.Link("same", "same", true); err != nil {
		t.Fatalf("Link 到自身: %v", err)
	}
	a, err := fs.Stat("same")
	if err != nil || a.Size != int64(len("keepme")) {
		t.Fatalf("自链接后文件被破坏: %+v, %v", a, err)
	}
}

func TestLinkErrors(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")
	if err := os.Mkdir(filepath.Join(fs.Root(), "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	hl := linkerOf(t, fs)

	cases := []struct {
		name         string
		src, dst     string
		want         error
		wantAnyError bool
	}{
		{name: "源不存在", src: "nope", dst: "x", want: ErrNotFound},
		{name: "源是目录", src: "d", dst: "dlink", want: ErrIsDir},
		{name: "源为流路径", src: "f:AFP_Resource:$DATA", dst: "x", want: ErrInvalidPath},
		{name: "目标为流路径", src: "f", dst: "x:AFP_Resource:$DATA", want: ErrInvalidPath},
		{name: "目标穿越到共享外", src: "f", dst: "../escape", wantAnyError: true},
		{name: "源穿越到共享外", src: "../../etc/passwd", dst: "x", wantAnyError: true},
		{name: "目标绝对路径", src: "f", dst: "/etc/passwd", wantAnyError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := hl.Link(c.src, c.dst, false)
			if c.wantAnyError {
				if err == nil {
					t.Fatalf("Link(%q,%q) 竟然成功了", c.src, c.dst)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Link(%q,%q) err = %v；期望 %v", c.src, c.dst, err, c.want)
			}
		})
	}

	// 穿越用例必须真的没在共享外留下东西。
	if _, err := os.Lstat(filepath.Join(filepath.Dir(fs.Root()), "escape")); !os.IsNotExist(err) {
		t.Errorf("路径穿越在共享外创建了链接")
	}
}

func TestLinkReadOnlyShare(t *testing.T) {
	fs := newTestFS(t, true)
	writeFile(t, fs, "f", "x")
	if err := linkerOf(t, fs).Link("f", "g", false); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读共享上的 Link = %v；期望 ErrReadOnly", err)
	}
	if _, err := fs.Stat("g"); !errors.Is(err, ErrNotFound) {
		t.Errorf("只读共享上不应真的建出链接")
	}
}
