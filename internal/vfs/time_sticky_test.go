package vfs

// time_sticky_test.go —— 显式设置 write time 后的 sticky/pending 语义
// （bh3-F6）。
//
// 缺陷背景：SET_INFO(FileBasicInformation) 设完 LastWriteTime 后，任何一次
// 写入都让内核把 mtime 刷回当前时间，客户端再查询发现自己设的值没了。
//
// Samba 对照：setting_write_time 时调 set_sticky_write_time_fsp
// （source3/smbd/dosmode.c:1274–1285，置 fsp.write_time_forced）；后续写/close
// 跳过自然更新（source3/smbd/fileio.c:135/174/198，bug #2045）。Windows 服务端
// 同为 sticky 行为。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStickyWriteTimeSurvivesWriteAndClose：句柄上显式设过 LastWriteTime 后，
// 同一句柄的写入与关闭都不得冲掉它。
func TestStickyWriteTimeSurvivesWriteAndClose(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "0123456789")

	h, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Flags: OpenWrite | OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}

	forced := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if err := h.SetAttr(&Attr{WriteTime: forced}, AttrWriteTime); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.WriteTime.Sub(forced).Abs() > 2*time.Second {
		t.Fatalf("设置后立刻回读就错了：got %v want %v", a.WriteTime, forced)
	}

	// 关键断言：显式设值之后的写入不得更新 mtime。
	if _, err := h.WriteAt([]byte("abcde"), 0); err != nil {
		t.Fatal(err)
	}
	a, err = h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.WriteTime.Sub(forced).Abs() > 2*time.Second {
		t.Errorf("写入后 mtime 被冲掉：got %v want ≈%v（sticky 语义缺失，bh3-F6）", a.WriteTime, forced)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// Close 是最后关口：即使中间有截断等绕过 WriteAt 的元数据变更，
	// 关闭时也必须把所设值补偿回去。
	fi, err := os.Stat(filepath.Join(fs.Root(), "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.ModTime(); got.Sub(forced).Abs() > 2*time.Second {
		t.Errorf("Close 后 mtime = %v，want ≈%v", got, forced)
	}
}

// TestNaturalMtimeUpdatesWithoutForce：对照组 —— 没有显式设过时间的句柄，
// 写入必须照常刷新 mtime。防止「一刀切不更新」的假修复混过来。
func TestNaturalMtimeUpdatesWithoutForce(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "g.txt", "old")

	old := time.Now().Add(-48 * time.Hour)
	p := filepath.Join(fs.Root(), "g.txt")
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	h, _, err := fs.Open(&OpenRequest{
		Path: "g.txt", Flags: OpenWrite, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.ModTime(); got.Sub(old) < 24*time.Hour {
		t.Errorf("无显式设值时写入应自然刷新 mtime：got %v, 仍停留在 %v", got, old)
	}
}
