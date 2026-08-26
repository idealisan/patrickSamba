//go:build linux

package vfs

// attr_linux_test.go —— Linux 平台的属性映射（bh3 F7）。
//
// F7：btime 兜底口径。statx BTIME 拿不到时（老内核/不支持 btime 的文件系统），
// 创建时间回退值曾直接用 ctime；Samba 的 calc_create_time_stat
// （source3/lib/system.c:131–150）取的是 **MIN(ctime, mtime, atime)**
// （atime 异常为零时退 MIN(ctime, mtime)）。对 rsync/cp -p 保时拷贝的文件，
// 两者给出的创建时间不同。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBtimeFallbackIsMinOfTimes：mtime/atime 早于 ctime 时（os.Chtimes 会把
// inode 的 ctime 刷成当前时间），兜底创建时间必须是三者最小值，而不是 ctime。
func TestBtimeFallbackIsMinOfTimes(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	atime := now.Add(-3 * time.Hour)
	mtime := now.Add(-5 * time.Hour) // 三者中最小
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}
	// Chtimes 之后：ctime ≈ now，atime = now-3h，mtime = now-5h。
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}

	a := &Attr{}
	fillSysAttr(fi, a)

	// ChangeTime 仍必须忠实反映 ctime（≈now），不能被兜底逻辑污染。
	if a.ChangeTime.Before(now.Add(-2 * time.Hour)) {
		t.Errorf("ChangeTime = %v，应保持为 ctime（≈%v）", a.ChangeTime, now)
	}
	// CreateTime 兜底 = MIN(ctime, mtime, atime) = mtime。
	if !a.CreateTime.Equal(mtime.Truncate(time.Second)) &&
		a.CreateTime.Sub(mtime).Abs() > 2*time.Second {
		t.Errorf("CreateTime = %v，期望回退到 mtime %v（MIN 口径），而不是 ctime %v",
			a.CreateTime, mtime, a.ChangeTime)
	}
}

// TestCalcBtimeFallbackAtimeAnomaly：atime 为零值（异常）时退 MIN(ctime, mtime)。
// 对照 Samba system.c 对 null timespec 的跳过逻辑。
func TestCalcBtimeFallbackAtimeAnomaly(t *testing.T) {
	ct := time.Unix(2000000000, 0)
	mt := ct.Add(-2 * time.Hour)

	got := calcBtimeFallback(ct, mt, time.Time{})
	if !got.Equal(mt) {
		t.Errorf("atime 零值时应取 MIN(ctime,mtime)，got %v want %v", got, mt)
	}

	at := ct.Add(-1 * time.Hour)
	got = calcBtimeFallback(ct, mt, at)
	if !got.Equal(mt) {
		t.Errorf("正常三值应取最小，got %v want %v", got, mt)
	}
}
