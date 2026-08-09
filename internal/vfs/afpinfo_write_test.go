package vfs

// afpinfo_write_test.go —— AFP_AfpInfo 写入时的校验。
//
// 回归背景：校验原本只在 flush（Sync/Close）时做，而 SMB2 CLOSE 响应
// 没有地方承载错误，于是一次非法写入表现为「WRITE 成功 → CLOSE 成功 →
// xattr 根本没建 → 后续读回 OBJECT_NAME_NOT_FOUND」，数据无声蒸发。
// info agent 用 smbclient 在真实链路上复现到了这个。
//
// 现在整块写当场校验，让 SMB2 WRITE 回 STATUS_INVALID_PARAMETER。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// badAfpInfo 造一个 60 字节但版本号错误的 blob。
//
// 0x00010000 这个值不是随便挑的：Apple 的 MacExtensions.h 注释里写的是
// 它，而真正的宏是 0x00000100（Samba adouble.c 的 afpinfo_unpack 以宏为准）。
// info agent 第一次手写测试数据时就踩了这个，正好拿来当反例。
func badAfpInfo() []byte {
	buf := make([]byte, AfpInfoSize)
	binary.BigEndian.PutUint32(buf[0:4], afpSignature)
	binary.BigEndian.PutUint32(buf[4:8], 0x00010000) // 错误的版本号
	return buf
}

func TestAfpInfoWriteRejectsMalformed(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "t.txt", "base")

	h, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)

	// 整块写非法内容：必须**当场**失败，而不是等到 Close。
	n, err := h.WriteAt(badAfpInfo(), 0)
	if !errors.Is(err, ErrBadAfpInfo) {
		t.Fatalf("写入非法 AfpInfo = (%d, %v)；期望 ErrBadAfpInfo", n, err)
	}
	if n != 0 {
		t.Errorf("失败的写入不应报告写入字节数，得到 %d", n)
	}

	// 错误的签名同样要拒。
	badSig := NewAfpInfo().Marshal()
	binary.BigEndian.PutUint32(badSig[0:4], 0xDEADBEEF)
	if _, err := h.WriteAt(badSig, 0); !errors.Is(err, ErrBadAfpInfo) {
		t.Errorf("错误签名 = %v；期望 ErrBadAfpInfo", err)
	}

	// 关键：失败之后 Close 必须成功，且**不能**留下半个流。
	if err := h.Close(); err != nil {
		t.Errorf("非法写入被拒后 Close 应成功，得到 %v", err)
	}
	if _, _, err := fs.AppleInfo("t.txt"); err != nil {
		t.Errorf("基础文件不该受影响: %v", err)
	}
}

func TestAfpInfoWriteDoesNotClobberOnReject(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "t.txt", "base")

	// 先写入一份合法的 FinderInfo。
	good := NewAfpInfo()
	copy(good.FinderInfo[:], finderInfoPattern())
	h1, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := h1.WriteAt(good.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := h1.Close(); err != nil {
		t.Fatal(err)
	}

	// 再用非法内容去写：被拒之后磁盘上的旧值必须**原封不动**。
	h2, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := h2.WriteAt(badAfpInfo(), 0); !errors.Is(err, ErrBadAfpInfo) {
		t.Fatalf("期望 ErrBadAfpInfo，得到 %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	fi, _, err := fs.AppleInfo("t.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("被拒的写入破坏了原有 FinderInfo: % x", fi)
	}
}

func TestAfpInfoWriteAcceptsValid(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "t.txt", "base")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	h, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	n, err := h.WriteAt(ai.Marshal(), 0)
	if err != nil || n != AfpInfoSize {
		t.Fatalf("合法整块写 = (%d, %v)", n, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	fi, _, err := fs.AppleInfo("t.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("落盘的 FinderInfo 不对: % x", fi)
	}
}

// TestAfpInfoPartialWriteStillBuffers 确认放宽的部分：分段写仍然走缓冲。
//
// 客户端把 60 字节拆成几次写是合法的，中间态必然不是一个完整合法的
// AfpInfo，此时不能当场拒 —— 只有覆盖完整 [0,60) 的那种写才校验。
func TestAfpInfoPartialWriteStillBuffers(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "t.txt", "base")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	blob := ai.Marshal()

	h, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	// 拆成两段写，每段单独看都不是合法的 AfpInfo。
	if _, err := h.WriteAt(blob[:16], 0); err != nil {
		t.Fatalf("分段写前半 = %v；不应被拒", err)
	}
	if _, err := h.WriteAt(blob[16:], 16); err != nil {
		t.Fatalf("分段写后半 = %v；不应被拒", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	fi, _, err := fs.AppleInfo("t.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("分段写的结果不对: % x", fi)
	}
}

// TestAfpInfoSyncReportsBadBuffer 覆盖分段写留下非法内容的情况。
//
// 这条路径没法在 WRITE 时发现，但 SMB2 FLUSH 是能承载错误的，
// 客户端至少有机会知道。
func TestAfpInfoSyncReportsBadBuffer(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "t.txt", "base")

	h, _ := openStreamH(t, fs, "t.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	// 只改版本号那 4 个字节，缓冲整体变得非法。
	bad := make([]byte, 4)
	binary.BigEndian.PutUint32(bad, 0x00010000)
	if _, err := h.WriteAt(bad, 4); err != nil {
		t.Fatalf("分段写 = %v；不应被拒", err)
	}
	if err := h.Sync(false); !errors.Is(err, ErrBadAfpInfo) {
		t.Errorf("Sync 应报告 ErrBadAfpInfo，得到 %v", err)
	}
}
