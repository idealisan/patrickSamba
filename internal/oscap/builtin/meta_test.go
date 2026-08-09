package builtin

// meta_test.go —— 创建时间、DOS 属性位、稳定 FileID 三项的往返用例。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

func TestCreationTimeRoundTrip(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)

	// 没记过就如实说没有 —— 绝不拿 mtime 冒充（ports.go）。
	if _, err := e.set.Times.CreationTime(ref); !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("未记录时应得 ErrNotSupported，实得 %v", err)
	}

	want := time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.UTC)
	if err := e.set.Times.SetCreationTime(ref, want); err != nil {
		t.Fatalf("SetCreationTime 失败: %v", err)
	}
	got, err := e.set.Times.CreationTime(ref)
	if err != nil {
		t.Fatalf("CreationTime 失败: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("创建时间不一致: 得 %v 期望 %v", got, want)
	}

	// 纳秒必须活下来：SMB 的 FILETIME 是 100ns 精度，
	// 存成秒会让客户端看到时间在每次往返里跳动。
	if got.Nanosecond() != want.Nanosecond() {
		t.Fatalf("纳秒丢了: 得 %d 期望 %d", got.Nanosecond(), want.Nanosecond())
	}

	// 落盘后重开仍在（证明真的进了库，不是内存里的假象）。
	e.reopen(false)
	if got, err := e.set.Times.CreationTime(ref); err != nil || !got.Equal(want) {
		t.Fatalf("重开后创建时间丢失: (%v, %v)", got, err)
	}
}

func TestCreationTimeEpochAndFuture(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)
	for _, want := range []time.Time{
		time.Unix(0, 0).UTC(),
		time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), // FILETIME 纪元，早于 Unix 纪元
		time.Date(2300, 6, 7, 8, 9, 10, 11, time.UTC),
	} {
		if err := e.set.Times.SetCreationTime(ref, want); err != nil {
			t.Fatalf("SetCreationTime(%v) 失败: %v", want, err)
		}
		got, err := e.set.Times.CreationTime(ref)
		if err != nil || !got.Equal(want) {
			t.Fatalf("时间 %v 往返失败: (%v, %v)", want, got, err)
		}
	}
}

func TestDOSAttributesRoundTrip(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)

	if _, err := e.set.DOS.DOSAttributes(ref); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("从未设置过应得 ErrNotFound，实得 %v", err)
	}

	const hiddenSystem = 0x2 | 0x4
	if err := e.set.DOS.SetDOSAttributes(ref, hiddenSystem); err != nil {
		t.Fatalf("SetDOSAttributes 失败: %v", err)
	}
	got, err := e.set.DOS.DOSAttributes(ref)
	if err != nil || got != hiddenSystem {
		t.Fatalf("DOS 属性往返失败: (%#x, %v)", got, err)
	}

	// 显式清零与「没人设置过」必须是两种答案 —— 否则 vfs 无法区分
	//「用户要求清空」和「你自己去合成」。
	if err := e.set.DOS.SetDOSAttributes(ref, 0); err != nil {
		t.Fatalf("清零失败: %v", err)
	}
	got, err = e.set.DOS.DOSAttributes(ref)
	if err != nil {
		t.Fatalf("显式清零后应返回 (0, nil)，实得 err=%v", err)
	}
	if got != 0 {
		t.Fatalf("显式清零后应为 0，实得 %#x", got)
	}
}

func TestFileIDStableUniqueAndSurvivesRename(t *testing.T) {
	e := newEnv(t)
	a := e.file("a.txt", []byte("a"))
	b := e.file("b.txt", []byte("b"))

	ida, err := e.set.IDs.FileID(a)
	if err != nil {
		t.Fatalf("FileID(a) 失败: %v", err)
	}
	idb, err := e.set.IDs.FileID(b)
	if err != nil {
		t.Fatalf("FileID(b) 失败: %v", err)
	}
	if ida == idb {
		t.Fatalf("不同对象拿到同一个 FileID %d —— 违反 ports.go 的唯一性契约", ida)
	}
	if again, _ := e.set.IDs.FileID(a); again != ida {
		t.Fatalf("同一对象两次查询不一致: %d vs %d", ida, again)
	}

	// 跨重命名保持不变 —— 只在宿主能给出 inode 时成立。
	// 拿不到 inode 时走库分配号，那条路按路径记账，本来就做不到，
	// 所以这里**条件断言**而不是无脑要求（假绿与假红都要避免）。
	fi, err := os.Stat(a.Path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	_, hasInode := inodeOf(fi)

	renamed := oscap.Ref{Path: filepath.Join(e.root, "a-renamed.txt")}
	if err := os.Rename(a.Path, renamed.Path); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	after, err := e.set.IDs.FileID(renamed)
	if err != nil {
		t.Fatalf("重命名后 FileID 失败: %v", err)
	}
	if hasInode && after != ida {
		t.Fatalf("宿主提供 inode 时 FileID 必须跨重命名不变: %d -> %d", ida, after)
	}
	if !hasInode {
		t.Logf("宿主不提供 inode，FileID 走库分配号（不跨重命名），本次 %d -> %d", ida, after)
	}
}

func TestFileIDMissingObject(t *testing.T) {
	e := newEnv(t)
	ref := oscap.Ref{Path: filepath.Join(e.root, "nope")}
	if _, err := e.set.IDs.FileID(ref); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("对不存在的对象取 FileID 应得 ErrNotFound，实得 %v", err)
	}
}

// TestFileIDFallbackAllocation 直接验证「拿不到 inode」那条路：
// 库分配的号必须唯一、稳定、且与真实 inode 不撞号段。
//
// 不能靠「找一个没有 inode 的文件系统」来触发（构建机上不存在），
// 所以绕开 inodeOf 直接测底层分配器 —— 这条路径在 Windows 上是常态，
// 而 CI 跑在 Linux 上，不这么测它就永远不会被执行到。
func TestFileIDFallbackAllocation(t *testing.T) {
	e := newEnv(t)
	a := e.file("a.txt", nil)
	b := e.file("b.txt", nil)
	ad := e.set.Xattr.(*adapter)

	ida, err := ad.st.assignFileID(ad.st.objKey(a.Path))
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	idb, err := ad.st.assignFileID(ad.st.objKey(b.Path))
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if ida == idb {
		t.Fatalf("两个对象分到同一个号 %d", ida)
	}
	if ida < idFallbackBase || idb < idFallbackBase {
		t.Fatalf("分配号必须落在 [2^63, ...) 号段以避开真实 inode: %d %d", ida, idb)
	}
	again, err := ad.st.assignFileID(ad.st.objKey(a.Path))
	if err != nil || again != ida {
		t.Fatalf("重复分配必须返回同一个号: %d vs %d (err=%v)", ida, again, err)
	}
}

// TestDefaultMetadataPathOutsideRoot 盯的是一条硬要求：
// 默认库文件**不能落在共享根里面**，否则客户端会在共享里看见它。
func TestDefaultMetadataPathOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	p, err := defaultMetadataPath(root)
	if err != nil {
		t.Fatalf("defaultMetadataPath 失败: %v", err)
	}
	rel, relErr := filepath.Rel(root, p)
	inside := relErr == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if inside {
		t.Fatalf("默认库文件落在共享根里了: %s（相对 %s 是 %s）", p, root, rel)
	}

	// 真开一次，确认落点可用且共享目录里没多出东西。
	set, err := New(oscap.Options{Root: root})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer func() { _ = set.Close() }()

	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("共享根被污染了: %v", ents)
	}
}
