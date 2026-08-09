//go:build linux || darwin

package native

// native_posix_test.go —— POSIX 侧的**真往返**用例：写进去、再读回来。
//
// # 为什么不用 skip 兜底
//
// 每一项能力都可能因为宿主文件系统不支持而做不到（overlayfs 打不了洞、
// FAT32 没有 xattr）。最省事的写法是 `if !supported { t.Skip() }` ——
// 但那会让整个文件在某台 CI 机器上一行都不执行，还照样报绿，
// 正是 AGENTS.md 反复点名的「空转通过」。
//
// 所以这里一律用**双分支断言**：探测说支持就断言完整往返，
// 探测说不支持就断言实现如实返回 ErrNotSupported。两条路都有判据、
// 都会因为代码坏掉而变红，没有任何一条是「什么都不检查」。
// 覆盖面本身由 TestNativeWiringIsNotVacuous（linux 侧）另行钉死。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// newTestSet 造一套指向临时目录的 native 能力集。
func newTestSet(t *testing.T, readOnly bool) (oscap.Set, string) {
	t.Helper()
	root := t.TempDir()
	set, err := New(oscap.Options{Root: root, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("New(root=%s, readOnly=%v): %v", root, readOnly, err)
	}
	return set, root
}

// hostSupports 问探测层「本机文件系统到底支不支持这一项」。
//
// 用 oscap.ProbeNative 而不是自己再写一遍探测：两处判据分家的话，
// 测试会开始验证一个与产品代码无关的东西。
func hostSupports(t *testing.T, c oscap.Capability, root string) bool {
	t.Helper()
	return oscap.ProbeNative(c, oscap.Options{Root: root})
}

// mkFile 在 root 下建一个带内容的文件并返回路径。
func mkFile(t *testing.T, root, name string, size int) string {
	t.Helper()
	p := filepath.Join(root, name)
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("建文件 %s: %v", p, err)
	}
	return p
}

// --- 扩展属性 ---------------------------------------------------------------

// TestXattrRoundTrip 钉住 Xattr 的写入-读回往返，以及 ports.go 里那几条
// 容易被实现方忽略的边角契约。
func TestXattrRoundTrip(t *testing.T) {
	set, root := newTestSet(t, false)
	if set.Xattr == nil {
		t.Fatal("POSIX 上 Xattr 必须接线（native_{linux,darwin}.go 都填了 posixXattr）")
	}
	ref := oscap.Ref{Path: mkFile(t, root, "x.bin", 16)}

	err := set.Xattr.SetXattr(ref, "com.apple.FinderInfo", []byte("finder"))
	if !hostSupports(t, oscap.CapXattr, root) {
		// 反向断言：做不到就必须如实说，不能静默成功。
		if !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("探测说本机不支持 xattr，SetXattr 应当返回 ErrNotSupported，实际 %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("SetXattr: %v", err)
	}

	got, err := set.Xattr.GetXattr(ref, "com.apple.FinderInfo")
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if string(got) != "finder" {
		t.Fatalf("GetXattr 读回 %q，期望 %q", got, "finder")
	}

	// 覆盖写：不是追加，也不该报 ErrExist。
	if err := set.Xattr.SetXattr(ref, "com.apple.FinderInfo", []byte("v2")); err != nil {
		t.Fatalf("覆盖 SetXattr: %v", err)
	}
	if got, _ := set.Xattr.GetXattr(ref, "com.apple.FinderInfo"); string(got) != "v2" {
		t.Fatalf("覆盖后读回 %q，期望 %q", got, "v2")
	}

	// 「存在但为空」≠「不存在」（ports.go 明文规定）。
	if err := set.Xattr.SetXattr(ref, "empty", nil); err != nil {
		t.Fatalf("写空值: %v", err)
	}
	switch v, err := set.Xattr.GetXattr(ref, "empty"); {
	case err != nil:
		t.Fatalf("读空值属性应当成功，实际 %v", err)
	case v == nil:
		t.Fatal("「存在但为空」必须返回非 nil 的空切片，实际返回 nil —— 调用方无法与「不存在」区分")
	case len(v) != 0:
		t.Fatalf("空值属性读回 %d 字节，期望 0", len(v))
	}

	names, err := set.Xattr.ListXattr(ref)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	if !contains(names, "com.apple.FinderInfo") || !contains(names, "empty") {
		t.Fatalf("ListXattr = %v，两个已写入的名字都该在里面", names)
	}

	if err := set.Xattr.RemoveXattr(ref, "com.apple.FinderInfo"); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
	if _, err := set.Xattr.GetXattr(ref, "com.apple.FinderInfo"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("删除后 GetXattr 应当返回 ErrNotFound，实际 %v", err)
	}
	if err := set.Xattr.RemoveXattr(ref, "com.apple.FinderInfo"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("重复删除应当返回 ErrNotFound，实际 %v", err)
	}
}

// TestXattrNameSymmetry 钉住 posix.go 里那条**双向对称**契约：
// ListXattr 报出来的名字，客户端拿去 GetXattr 必须能取到。
//
// 专挑 `user.` 开头的名字下手 —— 这正是 posix.go 注释里说的、
// 「已带已知命名空间就原样透传」那种规则会破对称的输入：
// 透传规则下磁盘上的 `user.user.x` 会被解码成 `user.x`，
// 再编码回去却指向另一个属性，于是列得出来、读不到。
func TestXattrNameSymmetry(t *testing.T) {
	set, root := newTestSet(t, false)
	if !hostSupports(t, oscap.CapXattr, root) {
		if err := set.Xattr.SetXattr(oscap.Ref{Path: mkFile(t, root, "s.bin", 1)}, "user.x", nil); !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("不支持 xattr 时应当返回 ErrNotSupported，实际 %v", err)
		}
		return
	}
	ref := oscap.Ref{Path: mkFile(t, root, "s.bin", 1)}

	for _, name := range []string{"user.x", "plain", "com.apple.metadata\uf022kMDLabel"} {
		if err := set.Xattr.SetXattr(ref, name, []byte(name)); err != nil {
			t.Fatalf("SetXattr(%q): %v", name, err)
		}
	}
	names, err := set.Xattr.ListXattr(ref)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	for _, name := range names {
		v, err := set.Xattr.GetXattr(ref, name)
		if err != nil {
			t.Errorf("ListXattr 报出了 %q 却读不到它（对称性被破坏）: %v", name, err)
			continue
		}
		if string(v) != name {
			t.Errorf("名字 %q 读回的值是 %q —— 编解码没有落到同一个属性上", name, v)
		}
	}
}

// TestXattrRejectsReservedPrefix 确认命名流的落盘名字空间没有被 Xattr 能力
// 顶穿。漏出去的话客户端能把资源叉当普通属性覆盖掉，直接损坏数据。
func TestXattrRejectsReservedPrefix(t *testing.T) {
	set, root := newTestSet(t, false)
	ref := oscap.Ref{Path: mkFile(t, root, "r.bin", 1)}

	if err := set.Xattr.SetXattr(ref, reservedStreamPrefix+"AFP_Resource", []byte("x")); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("写保留前缀属性应当返回 ErrInvalidArg，实际 %v", err)
	}
	if _, err := set.Xattr.GetXattr(ref, reservedStreamPrefix+"AFP_Resource"); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("读保留前缀属性应当返回 ErrInvalidArg，实际 %v", err)
	}
}

// TestXattrReadOnlyShareRejectsWrites 钉住 Options.ReadOnly 的兑现。
//
// 「假装成功」在只读共享上尤其恶劣：客户端以为元数据写进去了，
// 下次读回来却是旧值，而全程没有任何错误（oscap.go Options.ReadOnly 注释）。
func TestXattrReadOnlyShareRejectsWrites(t *testing.T) {
	rw, root := newTestSet(t, false)
	ro, err := New(oscap.Options{Root: root, ReadOnly: true})
	if err != nil {
		t.Fatalf("New(readOnly): %v", err)
	}
	ref := oscap.Ref{Path: mkFile(t, root, "ro.bin", 1)}

	if err := ro.Xattr.SetXattr(ref, "a", []byte("v")); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读共享上 SetXattr 应当返回 ErrReadOnly，实际 %v", err)
	}
	if err := ro.Xattr.RemoveXattr(ref, "a"); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读共享上 RemoveXattr 应当返回 ErrReadOnly，实际 %v", err)
	}
	// 反向对照：拒绝必须来自 ReadOnly 这个策略，而不是"这台机器本来就写不了"。
	// 少了这一条，一个把 SetXattr 整个写坏的实现也能让上面两条断言通过。
	if hostSupports(t, oscap.CapXattr, root) {
		if err := rw.Xattr.SetXattr(ref, "a", []byte("v")); err != nil {
			t.Fatalf("同一个文件在可写共享上应当写得进去，实际 %v —— "+
				"上面的 ErrReadOnly 因此不能证明只读策略生效", err)
		}
	}
}

// --- 命名流 -----------------------------------------------------------------

// TestNamedStreamRoundTrip 钉住命名流的写入-读回往返。
func TestNamedStreamRoundTrip(t *testing.T) {
	set, root := newTestSet(t, false)
	if set.Streams == nil {
		t.Fatal("POSIX 上 NamedStream 必须接线（承载在 xattr 上）")
	}
	ref := oscap.Ref{Path: mkFile(t, root, "doc.txt", 8)}

	h, err := set.Streams.OpenStream(ref, "AFP_Resource",
		oscap.StreamRead|oscap.StreamWrite|oscap.StreamCreate)
	if !hostSupports(t, oscap.CapNamedStream, root) {
		if !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("探测说不支持命名流，OpenStream 应当返回 ErrNotSupported，实际 %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("OpenStream(create): %v", err)
	}

	payload := []byte("resource fork payload")
	if n, err := h.WriteAt(payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt = (%d, %v)，期望 (%d, nil)", n, err, len(payload))
	}
	if size, err := h.Size(); err != nil || size != int64(len(payload)) {
		t.Fatalf("Size = (%d, %v)，期望 (%d, nil)", size, err, len(payload))
	}
	buf := make([]byte, len(payload))
	if n, err := h.ReadAt(buf, 0); err != nil || n != len(payload) {
		t.Fatalf("ReadAt = (%d, %v)，期望 (%d, nil)", n, err, len(payload))
	}
	if string(buf) != string(payload) {
		t.Fatalf("读回 %q，期望 %q", buf, payload)
	}

	// 越过原长度写：空隙必须补零（与普通文件 pwrite 语义一致）。
	if _, err := h.WriteAt([]byte("Z"), int64(len(payload))+3); err != nil {
		t.Fatalf("跳跃写: %v", err)
	}
	grown := make([]byte, len(payload)+4)
	if _, err := h.ReadAt(grown, 0); err != nil {
		t.Fatalf("跳跃写后 ReadAt: %v", err)
	}
	if grown[len(payload)] != 0 || grown[len(payload)+2] != 0 || grown[len(payload)+3] != 'Z' {
		t.Fatalf("跳跃写后内容 %q —— 空隙没有补零", grown)
	}

	if err := h.Truncate(4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if size, err := h.Size(); err != nil || size != 4 {
		t.Fatalf("截断后 Size = (%d, %v)，期望 (4, nil)", size, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 关闭后再用必须报 ErrClosed，而不是继续读写一个已经交还的对象。
	if _, err := h.Size(); !errors.Is(err, oscap.ErrClosed) {
		t.Fatalf("关闭后 Size 应当返回 ErrClosed，实际 %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("重复 Close 应当幂等返回 nil，实际 %v", err)
	}

	infos, err := set.Streams.ListStreams(ref)
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "AFP_Resource" || infos[0].Size != 4 {
		t.Fatalf("ListStreams = %+v，期望单条 {AFP_Resource 4}", infos)
	}

	if err := set.Streams.RemoveStream(ref, "AFP_Resource"); err != nil {
		t.Fatalf("RemoveStream: %v", err)
	}
	if infos, err := set.Streams.ListStreams(ref); err != nil || infos != nil {
		t.Fatalf("删除后 ListStreams = (%+v, %v)，期望 (nil, nil)", infos, err)
	}
	if _, err := set.Streams.OpenStream(ref, "AFP_Resource", oscap.StreamRead); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("打开已删除的流应当返回 ErrNotFound，实际 %v", err)
	}
	if err := set.Streams.RemoveStream(ref, "AFP_Resource"); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("重复删除应当返回 ErrNotFound，实际 %v", err)
	}
}

// TestNamedStreamHiddenFromXattr 钉住两项能力之间的隔离。
//
// POSIX 上命名流就落在 xattr 里，所以 ListXattr 必须把它藏起来 ——
// 漏出去的话客户端会把资源叉当成一个普通扩展属性读走甚至覆盖掉。
func TestNamedStreamHiddenFromXattr(t *testing.T) {
	set, root := newTestSet(t, false)
	if !hostSupports(t, oscap.CapNamedStream, root) {
		return // 另一个用例已对不支持路径下过断言，这里不重复
	}
	ref := oscap.Ref{Path: mkFile(t, root, "hide.txt", 4)}

	h, err := set.Streams.OpenStream(ref, "AFP_AfpInfo", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := h.WriteAt([]byte("info"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	_ = h.Close()

	names, err := set.Xattr.ListXattr(ref)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	for _, n := range names {
		if strings.Contains(n, "DosStream") || strings.Contains(n, "AFP_AfpInfo") {
			t.Fatalf("命名流的落盘属性 %q 从 ListXattr 漏出去了（全部：%v）——"+
				"客户端能据此覆盖掉资源叉", n, names)
		}
	}
	// 反向对照：确认流**真的写进去了**。少了这一条，一个 OpenStream
	// 什么都没落盘的实现也能让上面的循环平安通过。
	if infos, err := set.Streams.ListStreams(ref); err != nil || len(infos) != 1 {
		t.Fatalf("ListStreams = (%+v, %v)，期望恰好 1 条 —— "+
			"上面的「没漏出去」因此不能证明隔离生效", infos, err)
	}
}

// TestNamedStreamRejectsUnsafeNames 确认安全校验挂在 port 暴露的那一面上
// （AGENTS.md §8）。validateStreamName 的单测已在 native_test.go，
// 这里走的是**真实调用路径**，防止实现方忘了接上校验。
func TestNamedStreamRejectsUnsafeNames(t *testing.T) {
	set, root := newTestSet(t, false)
	ref := oscap.Ref{Path: mkFile(t, root, "safe.txt", 1)}

	for _, name := range []string{"", "..", "a/b", `a\b`, "a:b", "a\x00b"} {
		if _, err := set.Streams.OpenStream(ref, name, oscap.StreamRead|oscap.StreamCreate); !errors.Is(err, oscap.ErrInvalidArg) {
			t.Errorf("OpenStream(%q) 应当返回 ErrInvalidArg，实际 %v", name, err)
		}
		if err := set.Streams.RemoveStream(ref, name); !errors.Is(err, oscap.ErrInvalidArg) {
			t.Errorf("RemoveStream(%q) 应当返回 ErrInvalidArg，实际 %v", name, err)
		}
	}
	// 过长的流名如实拒绝，**不许截断**：截断会让两个不同的名字落到同一个
	// xattr，后写的悄悄覆盖先写的。
	long := strings.Repeat("n", maxStreamNameLen+1)
	if _, err := set.Streams.OpenStream(ref, long, oscap.StreamRead|oscap.StreamCreate); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Errorf("超长流名应当返回 ErrInvalidArg，实际 %v", err)
	}
}

// TestNamedStreamSizeLimitIsHonest 钉住「写不下就说写不下」。
//
// xattr 承载上限是本实现的硬约束，超了必须报 ENOSPC（vfs 的 errmap 会把它
// 变成 STATUS_DISK_FULL），绝不能写一半还回报成功。
//
// 用一个小得本机一定装得下的上限做断言（见 streamMaxBytes），这样测试不靠
// 宿主文件系统的真实 xattr 值上限来碰运气 —— overlayfs 的上限可能远低于
// 64 KiB，那样超限写入会被文件系统本身挡掉，反而掩盖「代码自己的上限检查」
// 有没有接线。验收判据必须可证伪：限照代码逻辑走，不照运气走。
func TestNamedStreamSizeLimitIsHonest(t *testing.T) {
	set, root := newTestSet(t, false)
	if !hostSupports(t, oscap.CapNamedStream, root) {
		return
	}
	ref := oscap.Ref{Path: mkFile(t, root, "big.txt", 1)}

	const lim = 1024
	old := streamMaxBytes
	streamMaxBytes = lim
	t.Cleanup(func() { streamMaxBytes = old })

	// 反向对照：恰好等于上限的写入必须成功且读回一致。
	ok, err := set.Streams.OpenStream(ref, "OK", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("OpenStream(OK): %v", err)
	}
	if _, err := ok.WriteAt(make([]byte, lim), 0); err != nil {
		t.Fatalf("写入 %d 字节（恰好等于上限）应成功，实际 %v", lim, err)
	}
	if size, err := ok.Size(); err != nil || size != lim {
		t.Fatalf("流长度 = (%d, %v)，期望 (%d, nil)", size, err, lim)
	}
	_ = ok.Close()
	if re, err := set.Streams.OpenStream(ref, "OK", oscap.StreamRead); err != nil {
		t.Fatalf("重开 OK: %v", err)
	} else {
		buf := make([]byte, lim)
		n, err := re.ReadAt(buf, 0)
		_ = re.Close()
		if err != nil || n != lim {
			t.Fatalf("读回 OK 失败: (%d, %v)", n, err)
		}
	}

	// 越界：上限+1 必须被拒，且是**代码**在动手前拦的，不是文件系统偶然挡的。
	big, err := set.Streams.OpenStream(ref, "Big", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("OpenStream(Big): %v", err)
	}
	defer func() { _ = big.Close() }()
	if _, err := big.WriteAt(make([]byte, lim+1), 0); !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("超过承载上限应当返回 ENOSPC，实际 %v", err)
	}
	// 写失败了流长度就该纹丝不动。
	if size, err := big.Size(); err != nil || size != 0 {
		t.Fatalf("写失败后 Size = (%d, %v)，期望 (0, nil) —— 写了一半是最坏的结果", size, err)
	}
	// 干净拒绝：超限写入绝不能在宿主上留下「半个流」。OpenStream(Create) 会
	// 先建出一个空流条目（Size 0），但**写过的数据一个字节都不能落下** ——
	// 写 lim+1 字节超限被拒后，流若存在也必须还是 0 字节。一个把数据截断到
	// 上限悄悄写下去的实现会让这里变成 lim 字节，本断言正是用来抓它。
	if re, err := set.Streams.OpenStream(ref, "Big", oscap.StreamRead); err != nil {
		t.Fatalf("超限写入后读 Big 失败: %v", err)
	} else {
		if sz, err := re.Size(); err != nil || sz != 0 {
			t.Fatalf("超限写入后 Big 的 Size = (%d, %v)，期望 0 —— 代码把数据写一半了", sz, err)
		}
		_ = re.Close()
	}
}

// --- 稳定 FileID ------------------------------------------------------------

// TestFileIDStableAcrossRename 钉住 ports.go 里 StableFileID 的两条硬契约：
// 跨重命名不变、不同对象不撞。
//
// 这两条不是洁癖：macOS 会据此判断「文件还是不是原来那个」，
// 一个会变的 FileID 比没有 FileID 更糟。
func TestFileIDStableAcrossRename(t *testing.T) {
	set, root := newTestSet(t, false)
	if set.IDs == nil {
		t.Fatal("POSIX 上 StableFileID 必须接线（posixIDs）")
	}
	p := mkFile(t, root, "before.txt", 4)

	id1, err := set.IDs.FileID(oscap.Ref{Path: p})
	if !hostSupports(t, oscap.CapStableFileID, root) {
		if !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("探测说拿不到稳定 ID，FileID 应当返回 ErrNotSupported，实际 %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("FileID: %v", err)
	}
	if id1 == 0 {
		t.Fatal("FileID 返回 0 —— 那是「未知」的哨兵，所有对象会撞成同一个")
	}

	renamed := filepath.Join(root, "after.txt")
	if err := os.Rename(p, renamed); err != nil {
		t.Fatalf("重命名: %v", err)
	}
	id2, err := set.IDs.FileID(oscap.Ref{Path: renamed})
	if err != nil {
		t.Fatalf("重命名后 FileID: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("FileID 跨重命名变了：%d → %d", id1, id2)
	}

	// 不同对象不得撞车。
	other := mkFile(t, root, "other.txt", 4)
	id3, err := set.IDs.FileID(oscap.Ref{Path: other})
	if err != nil {
		t.Fatalf("另一个文件 FileID: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("两个不同文件返回了同一个 FileID %d", id1)
	}

	// 有句柄与没句柄两条路径必须给出同一个答案。
	// Ref.Handle 为 nil 是常态（SMB 的 OpenAttrOnly），两条路分家是
	// 一种只在特定操作序列下才现形的 bug。
	f, err := os.Open(renamed)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	defer func() { _ = f.Close() }()
	id4, err := set.IDs.FileID(oscap.Ref{Path: renamed, Handle: f})
	if err != nil {
		t.Fatalf("带句柄 FileID: %v", err)
	}
	if id4 != id2 {
		t.Fatalf("带句柄(%d)与按路径(%d)的 FileID 不一致", id4, id2)
	}
}

// TestFileIDOnMissingObject 确认错误可被 port 层识别。
func TestFileIDOnMissingObject(t *testing.T) {
	set, root := newTestSet(t, false)
	_, err := set.IDs.FileID(oscap.Ref{Path: filepath.Join(root, "nope")})
	if !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("不存在的对象应当返回 ErrNotFound，实际 %v", err)
	}
}

// --- 创建时间 ---------------------------------------------------------------

// TestCreationTimeRead 钉住创建时间的读取。
//
// 重点是**不许拿 mtime 冒充**：文件系统没记 btime 时必须 ErrNotSupported，
// 否则上层永远不知道该去问 builtin 要那个记下来的真值（ports.go）。
func TestCreationTimeRead(t *testing.T) {
	set, root := newTestSet(t, false)
	if set.Times == nil {
		t.Fatal("POSIX 上 CreationTime 必须接线")
	}
	before := time.Now().Add(-2 * time.Second)
	ref := oscap.Ref{Path: mkFile(t, root, "ct.txt", 4)}

	got, err := set.Times.CreationTime(ref)
	if !hostSupports(t, oscap.CapCreationTime, root) {
		if !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("本机拿不到 btime，CreationTime 应当返回 ErrNotSupported（不许拿 mtime 冒充），实际 (%v, %v)", got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("CreationTime: %v", err)
	}
	if got.IsZero() {
		t.Fatal("CreationTime 返回零时间却没有报错 —— 这正是「Mask 没查」的典型症状")
	}
	after := time.Now().Add(2 * time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("创建时间 %v 落在 [%v, %v] 之外 —— 报出来的多半不是 btime", got, before, after)
	}
}

// TestCreationTimeOnMissingObject 确认错误可被 port 层识别。
func TestCreationTimeOnMissingObject(t *testing.T) {
	set, root := newTestSet(t, false)
	_, err := set.Times.CreationTime(oscap.Ref{Path: filepath.Join(root, "nope")})
	if !errors.Is(err, oscap.ErrNotFound) && !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("不存在的对象应当返回 ErrNotFound（或整项 ErrNotSupported），实际 %v", err)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
