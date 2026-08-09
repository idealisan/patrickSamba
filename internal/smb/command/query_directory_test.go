package command

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// query_directory_test.go —— AAPL readdir_attr 的字节布局 golden test。
//
// 期望值不是从我们自己的实现反推的，而是照着 Samba
// `source3/smbd/smb2_trans2.c` `smbd_marshall_dir_entry()` 的
// `SMB_FIND_ID_BOTH_DIRECTORY_INFO` 分支逐行算出来的（AGENTS.md §3/§9）：
//
//	q = p; p += 4;                              // FileNameLength @60
//	ea_size = ...aapl.max_access; SIVAL(p,0,ea_size); p += 4;   // @64
//	SSVAL(p, 0, 24);                            // @68 ShortNameLength=24, Reserved=0
//	SBVAL(p, 2, ...aapl.rfork_size);            // @70 小端 uint64
//	memcpy(p + 10, ...aapl.finder_info, 16);    // @78 16 字节
//	p += 26;
//	SSVAL(p, 0, aapl_mode); p += 2;             // @94 Reserved2（我们恒 0）
//	SBVAL(p, 0, file_id); p += 8;               // @96

// ---------------------------------------------------------------------------
// 压缩 FinderInfo
// ---------------------------------------------------------------------------

// sampleFinderInfo 是一份典型的 32 字节 FinderInfo。
//
// 前 16 字节是 FInfo（类型码 'TEXT'、创建者码 'ttxt'、flags、位置、文件夹号），
// 后 16 字节是 FXInfo；偏移 24 的 2 字节是扩展 Finder flags
// （vfs_fruit.c readdir_attr_meta_finderi 取的正是这两字节）。
var sampleFinderInfo = func() [vfs.FinderInfoSize]byte {
	var fi [vfs.FinderInfoSize]byte
	copy(fi[0:4], "TEXT")       // 类型码
	copy(fi[4:8], "ttxt")       // 创建者码
	fi[8], fi[9] = 0x40, 0x00   // Finder flags = kHasBeenInited
	fi[10], fi[11] = 0x11, 0x22 // 图标位置 v —— **不得**出现在压缩结果里
	fi[24], fi[25] = 0x80, 0x01 // 扩展 Finder flags
	fi[26], fi[27] = 0x33, 0x44 // FXInfo 其余部分 —— 同样不得出现
	return fi
}()

func TestAAPLCompressFinderInfo(t *testing.T) {
	// 2024-01-02T03:04:05Z；AppleDouble 纪元是 2000-01-01，差值 AD_DATE_DELTA。
	btime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	wantDate := uint32(btime.Unix() - aaplDateDelta)

	t.Run("普通文件", func(t *testing.T) {
		got := aaplCompressFinderInfo(sampleFinderInfo, true, false, btime)

		var want [16]byte
		copy(want[0:4], "TEXT")
		copy(want[4:8], "ttxt")
		want[8], want[9] = 0x40, 0x00
		want[10], want[11] = 0x80, 0x01
		// **大端**（Samba 用 RSIVAL），与条目里其它字段的小端相反。
		binary.BigEndian.PutUint32(want[12:16], wantDate)

		if got != want {
			t.Errorf("压缩 FinderInfo =\n  %x\n期望\n  %x", got, want)
		}
	})

	t.Run("目录不带类型码与创建者码", func(t *testing.T) {
		got := aaplCompressFinderInfo(sampleFinderInfo, true, true, btime)
		if !bytes.Equal(got[0:8], make([]byte, 8)) {
			t.Errorf("目录的 [0:8] = %x, 期望全零（vfs_fruit 只对 S_ISREG 填）", got[0:8])
		}
		// flags 与日期照填。
		if got[8] != 0x40 || got[10] != 0x80 || got[11] != 0x01 {
			t.Errorf("目录的 flags = %x, 期望照常填充", got[8:12])
		}
		if v := binary.BigEndian.Uint32(got[12:16]); v != wantDate {
			t.Errorf("date added = %#x, 期望 %#x", v, wantDate)
		}
	})

	t.Run("没有 Apple 元数据", func(t *testing.T) {
		var zero [vfs.FinderInfoSize]byte
		got := aaplCompressFinderInfo(zero, false, false, btime)

		var want [16]byte
		binary.BigEndian.PutUint32(want[12:16], aaplDateStart)
		if got != want {
			t.Errorf("无元数据时 = %x, 期望只有 AD_DATE_START 兜底 %x", got, want)
		}
	})

	t.Run("零值创建时间回落到 AD_DATE_START", func(t *testing.T) {
		got := aaplCompressFinderInfo(sampleFinderInfo, true, false, time.Time{})
		if v := binary.BigEndian.Uint32(got[12:16]); v != aaplDateStart {
			t.Errorf("date added = %#x, 期望 AD_DATE_START %#x", v, aaplDateStart)
		}
	})
}

// ---------------------------------------------------------------------------
// 目录项就地改写
// ---------------------------------------------------------------------------

// TestPatchIDBothDirEntryLayout 对单条目录项做字节级 golden 比对。
func TestPatchIDBothDirEntryLayout(t *testing.T) {
	const name = "band-00001"
	base := wire.DirEntry{
		CreationTime:   0x01d0000000000001,
		LastAccessTime: 0x01d0000000000002,
		LastWriteTime:  0x01d0000000000003,
		ChangeTime:     0x01d0000000000004,
		EndOfFile:      8 << 20,
		AllocationSize: 8 << 20,
		FileAttributes: wire.FileAttributeNormal,
		FileID:         0x1122334455667788,
		Name:           name,
	}

	buf, err := wire.AppendDirEntry(nil, wire.FileIdBothDirectoryInformation, base)
	if err != nil {
		t.Fatalf("AppendDirEntry: %v", err)
	}
	// 未改写前：EaSize/ShortName/Reserved2 全零。
	if !bytes.Equal(buf[64:96], make([]byte, 32)) {
		t.Fatalf("基线目录项 [64:96] = %x, 期望全零", buf[64:96])
	}

	attr := aaplDirAttr{
		MaxAccess:  wire.MaximalAccessReadWrite,
		RsrcSize:   0x0000_0000_0001_2345,
		FinderInfo: aaplCompressFinderInfo(sampleFinderInfo, true, false, time.Unix(aaplDateDelta+1, 0).UTC()),
	}
	if !attr.patchIDBothDirEntry(buf, 0) {
		t.Fatal("patchIDBothDirEntry 返回 false")
	}

	// --- @64 EaSize ← max_access（小端） ---
	if v := binary.LittleEndian.Uint32(buf[64:68]); v != wire.MaximalAccessReadWrite {
		t.Errorf("EaSize(@64) = %#x, 期望 max_access %#x", v, wire.MaximalAccessReadWrite)
	}
	// --- @68 ShortNameLength=24, @69 Reserved=0 ---
	if buf[68] != 24 {
		t.Errorf("ShortNameLength(@68) = %d, 期望 24（Samba SSVAL(p,0,24)）", buf[68])
	}
	if buf[69] != 0 {
		t.Errorf("Reserved(@69) = %d, 期望 0", buf[69])
	}
	// --- @70 rfork_size（小端 uint64） ---
	if v := binary.LittleEndian.Uint64(buf[70:78]); v != attr.RsrcSize {
		t.Errorf("rfork_size(@70) = %#x, 期望 %#x", v, attr.RsrcSize)
	}
	// --- @78 压缩 FinderInfo（16 字节） ---
	if !bytes.Equal(buf[78:94], attr.FinderInfo[:]) {
		t.Errorf("FinderInfo(@78) = %x, 期望 %x", buf[78:94], attr.FinderInfo[:])
	}
	// --- @94 Reserved2 恒 0（不实现 NFS ACE 就不能填 unix_mode） ---
	if v := binary.LittleEndian.Uint16(buf[94:96]); v != 0 {
		t.Errorf("Reserved2(@94) = %#x, 期望 0", v)
	}
	// --- 改写不得越界污染 FileId 与文件名 ---
	if v := binary.LittleEndian.Uint64(buf[96:104]); v != base.FileID {
		t.Errorf("FileId(@96) = %#x, 期望 %#x", v, base.FileID)
	}
	if got, _ := wire.DecodeUTF16LE(buf[104:]); got != name {
		t.Errorf("FileName = %q, 期望 %q", got, name)
	}
	// --- 公共前缀（0..64）必须原封不动 ---
	if v := binary.LittleEndian.Uint64(buf[8:16]); v != base.CreationTime {
		t.Errorf("CreationTime 被改写: %#x", v)
	}
	if v := binary.LittleEndian.Uint32(buf[60:64]); int(v) != wire.UTF16LELen(name) {
		t.Errorf("FileNameLength 被改写: %d", v)
	}
}

// TestPatchIDBothDirEntryBounds：越界一律拒绝，绝不 panic（AGENTS.md §5）。
func TestPatchIDBothDirEntryBounds(t *testing.T) {
	var a aaplDirAttr
	for _, tc := range []struct {
		name  string
		bufSz int
		start int
	}{
		{"空缓冲", 0, 0},
		{"固定部分差一字节", aaplDirEntryFixed - 1, 0},
		{"起点为负", 256, -1},
		{"起点越界", 256, 256},
		{"尾部放不下", 256, 256 - aaplDirEntryFixed + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a.patchIDBothDirEntry(make([]byte, tc.bufSz), tc.start) {
				t.Error("越界的改写应当被拒绝")
			}
		})
	}
}

// TestAAPLDirEntryStartOffset 验证 query_directory.go 里自算的条目起点
// 与 wire.DirEntryWriter 的对齐规则一致 —— 算错就会把 Apple 字段
// 写进上一条目录项的尾巴里，是这块最危险的地方。
func TestAAPLDirEntryStartOffset(t *testing.T) {
	// 故意用长度不同的名字，制造各种 8 字节对齐填充。
	names := []string{".", "..", "a", "ab", "abc", "band-00042", "sparsebundle"}

	w := wire.NewDirEntryWriter(wire.FileIdBothDirectoryInformation, 1<<20)
	starts := make([]int, 0, len(names))
	for i, n := range names {
		start := w.Len()
		if w.Count() > 0 {
			start = (start + 7) &^ 7
		}
		ok, err := w.Add(wire.DirEntry{Name: n, FileID: uint64(i + 1)})
		if err != nil || !ok {
			t.Fatalf("Add(%q): ok=%v err=%v", n, ok, err)
		}
		starts = append(starts, start)

		attr := aaplDirAttr{MaxAccess: uint32(i + 1), RsrcSize: uint64(i+1) * 100}
		if !attr.patchIDBothDirEntry(w.Bytes(), start) {
			t.Fatalf("patchIDBothDirEntry(%q, %d) 失败", n, start)
		}
	}

	// 用独立的解析器沿 NextEntryOffset 链走一遍，核对起点。
	buf := w.Bytes()
	pos := 0
	for i := range names {
		if pos != starts[i] {
			t.Fatalf("条目[%d] 链上的起点 = %d, 自算 = %d", i, pos, starts[i])
		}
		if v := binary.LittleEndian.Uint32(buf[pos+64 : pos+68]); v != uint32(i+1) {
			t.Errorf("条目[%d] max_access = %d, 期望 %d", i, v, i+1)
		}
		if v := binary.LittleEndian.Uint64(buf[pos+70 : pos+78]); v != uint64(i+1)*100 {
			t.Errorf("条目[%d] rfork_size = %d, 期望 %d", i, v, uint64(i+1)*100)
		}
		next := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		if i == len(names)-1 {
			if next != 0 {
				t.Errorf("最后一条的 NextEntryOffset = %d, 期望 0", next)
			}
			break
		}
		pos += next
	}

	// 顺带确认解析器也能读回来（FileId 没被 Apple 字段冲掉）。
	got, err := wire.ParseDirEntries(buf, wire.FileIdBothDirectoryInformation)
	if err != nil {
		t.Fatalf("ParseDirEntries: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("解析出 %d 条, 期望 %d 条", len(got), len(names))
	}
	for i := range got {
		if got[i].Name != names[i] {
			t.Errorf("条目[%d] 名字 = %q, 期望 %q", i, got[i].Name, names[i])
		}
		if got[i].FileID != uint64(i+1) {
			t.Errorf("条目[%d] FileId = %d, 期望 %d", i, got[i].FileID, i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// handleQueryDirectory 端到端
// ---------------------------------------------------------------------------

// TestQueryDirectoryAAPLEndToEnd 走完整的 handler：真实 LocalFS + 真实句柄，
// 分别在 readdir_attr 开/关两种模式下断言同一个目录的字节布局。
func TestQueryDirectoryAAPLEndToEnd(t *testing.T) {
	root := t.TempDir()
	const fileName = "band-00000"
	if err := os.WriteFile(filepath.Join(root, fileName), bytes.Repeat([]byte{7}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	rsrc := []byte("RESOURCEFORK")
	writeAppleMetadata(t, fs, fileName, sampleFinderInfo, rsrc)

	for _, aaplOn := range []bool{false, true} {
		name := "readdir_attr 关"
		if aaplOn {
			name = "readdir_attr 开"
		}
		t.Run(name, func(t *testing.T) {
			ctx, open := newQueryDirTestContext(t, fs, aaplOn)
			entries := runQueryDirectory(t, ctx, open, "*")

			raw, ok := entries[fileName]
			if !ok {
				t.Fatalf("枚举结果里没有 %q", fileName)
			}

			eaSize := binary.LittleEndian.Uint32(raw[64:68])
			shortLen := raw[68]

			if !aaplOn {
				// 标准布局：没有 EA 就是 0，短名区全零。
				if eaSize != 0 {
					t.Errorf("EaSize = %#x, 未协商时期望 0", eaSize)
				}
				if !bytes.Equal(raw[68:94], make([]byte, 26)) {
					t.Errorf("短名区 = %x, 未协商时期望全零", raw[68:94])
				}
				return
			}

			if eaSize != wire.MaximalAccessReadWrite {
				t.Errorf("max_access = %#x, 期望 %#x", eaSize, wire.MaximalAccessReadWrite)
			}
			if shortLen != 24 {
				t.Errorf("ShortNameLength = %d, 期望 24", shortLen)
			}
			if v := binary.LittleEndian.Uint64(raw[70:78]); v != uint64(len(rsrc)) {
				t.Errorf("rfork_size = %d, 期望 %d", v, len(rsrc))
			}
			if !bytes.Equal(raw[78:82], []byte("TEXT")) {
				t.Errorf("Finder 类型码 = %q, 期望 TEXT", raw[78:82])
			}
			if !bytes.Equal(raw[82:86], []byte("ttxt")) {
				t.Errorf("Finder 创建者码 = %q, 期望 ttxt", raw[82:86])
			}
			if raw[86] != 0x40 {
				t.Errorf("Finder flags = %#x, 期望 0x40", raw[86])
			}
			if raw[88] != 0x80 || raw[89] != 0x01 {
				t.Errorf("扩展 Finder flags = %x, 期望 8001", raw[88:90])
			}
			if v := binary.BigEndian.Uint32(raw[90:94]); v == 0 || v == aaplDateStart {
				t.Errorf("date added = %#x, 期望取自真实创建时间", v)
			}
			if v := binary.LittleEndian.Uint16(raw[94:96]); v != 0 {
				t.Errorf("Reserved2 = %#x, 期望 0（不实现 NFS ACE）", v)
			}

			// 目录项 "." 是目录：不得携带类型码/创建者码与资源派生。
			dot, ok := entries["."]
			if !ok {
				t.Fatal("枚举结果里没有 \".\"")
			}
			if !bytes.Equal(dot[78:86], make([]byte, 8)) {
				t.Errorf("目录的类型/创建者码 = %x, 期望全零", dot[78:86])
			}
			if v := binary.LittleEndian.Uint64(dot[70:78]); v != 0 {
				t.Errorf("目录的 rfork_size = %d, 期望 0", v)
			}
		})
	}
}

// TestQueryDirectoryAAPLOtherInfoClass：readdir_attr 只改
// FileIdBothDirectoryInformation，别的 information class 必须保持标准布局。
func TestQueryDirectoryAAPLOtherInfoClass(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	writeAppleMetadata(t, fs, "f", sampleFinderInfo, []byte("RSRC"))

	ctx, open := newQueryDirTestContext(t, fs, true)
	if newAAPLDirAttrSource(ctx, open, wire.FileBothDirectoryInformation) != nil {
		t.Error("FileBothDirectoryInformation 不该启用 readdir_attr")
	}
	if newAAPLDirAttrSource(ctx, open, wire.FileIdFullDirectoryInformation) != nil {
		t.Error("FileIdFullDirectoryInformation 不该启用 readdir_attr")
	}
	if newAAPLDirAttrSource(ctx, open, wire.FileIdBothDirectoryInformation) == nil {
		t.Error("FileIdBothDirectoryInformation 应当启用 readdir_attr")
	}
}

// TestAAPLChildPath 覆盖相对共享根的路径拼接。
func TestAAPLChildPath(t *testing.T) {
	tests := []struct{ dir, name, want string }{
		{"", "f", "f"},
		{"a/b", "f", "a/b/f"},
	}
	for _, tc := range tests {
		s := &aaplDirAttrSource{dir: tc.dir}
		if got := s.childPath(tc.name); got != tc.want {
			t.Errorf("childPath(dir=%q, %q) = %q, 期望 %q", tc.dir, tc.name, got, tc.want)
		}
	}
}

// TestAAPLAppleInfoDotEntries：".." 一律跳过（可能指到共享根外），
// "." 走按路径查的慢路径 —— DirAppleMetadata.AppleInfoAt 明确拒收这两个名字。
func TestAAPLAppleInfoDotEntries(t *testing.T) {
	h := &recordingDirMeta{}
	f := &recordingFSMeta{}
	s := &aaplDirAttrSource{handle: h, fs: f, dir: "a/b"}

	if _, _, ok := s.appleInfo(".."); ok {
		t.Error("\"..\" 不该取 Apple 元数据")
	}
	if h.calls != 0 || f.calls != 0 {
		t.Errorf("\"..\" 触发了后端调用: handle=%d fs=%d", h.calls, f.calls)
	}

	if _, _, ok := s.appleInfo("."); !ok {
		t.Error("\".\" 应当取到元数据")
	}
	if h.calls != 0 {
		t.Errorf("\".\" 不该走目录句柄快路径（AppleInfoAt 会拒收），调用了 %d 次", h.calls)
	}
	if f.last != "a/b" {
		t.Errorf("\".\" 查询的路径 = %q, 期望 %q", f.last, "a/b")
	}

	// 普通条目走快路径，且传的是**单个分量**而不是完整路径。
	if _, _, ok := s.appleInfo("band-0"); !ok {
		t.Error("普通条目应当取到元数据")
	}
	if h.calls != 1 || h.last != "band-0" {
		t.Errorf("快路径调用 %d 次、name=%q, 期望 1 次 %q", h.calls, h.last, "band-0")
	}
}

// TestAAPLAppleInfoFallback：目录句柄快路径失败时退回按路径查，
// 而不是把这条目录项的 Apple 字段丢空。
func TestAAPLAppleInfoFallback(t *testing.T) {
	h := &recordingDirMeta{err: vfs.ErrNotSupported}
	f := &recordingFSMeta{}
	s := &aaplDirAttrSource{handle: h, fs: f, dir: "bands"}

	if _, _, ok := s.appleInfo("0a"); !ok {
		t.Fatal("快路径失败后应当退回慢路径")
	}
	if f.last != "bands/0a" {
		t.Errorf("慢路径查询的是 %q, 期望 %q", f.last, "bands/0a")
	}

	// 两条路径都没有时，退化成「没有元数据」而不是崩掉。
	s2 := &aaplDirAttrSource{}
	if _, _, ok := s2.appleInfo("x"); ok {
		t.Error("没有任何后端时不该报告取到元数据")
	}
}

type recordingDirMeta struct {
	calls int
	last  string
	err   error
}

func (r *recordingDirMeta) AppleInfoAt(name string) ([vfs.FinderInfoSize]byte, int64, error) {
	r.calls++
	r.last = name
	var fi [vfs.FinderInfoSize]byte
	return fi, 0, r.err
}

// AppleInfoAtBatch 只为满足接口 —— 见 aapl.go 里为什么 readdir_attr 走逐条版本。
func (r *recordingDirMeta) AppleInfoAtBatch(names []string) ([]vfs.AppleInfoResult, error) {
	out := make([]vfs.AppleInfoResult, len(names))
	for i, n := range names {
		fi, rs, err := r.AppleInfoAt(n)
		out[i] = vfs.AppleInfoResult{FinderInfo: fi, RsrcSize: rs, Err: err}
	}
	return out, nil
}

type recordingFSMeta struct {
	calls int
	last  string
	err   error
}

func (r *recordingFSMeta) AppleInfo(p string) ([vfs.FinderInfoSize]byte, int64, error) {
	r.calls++
	r.last = p
	var fi [vfs.FinderInfoSize]byte
	return fi, 0, r.err
}

var (
	_ vfs.DirAppleMetadata = (*recordingDirMeta)(nil)
	_ vfs.AppleMetadata    = (*recordingFSMeta)(nil)
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// newQueryDirTestFS 建一个指向 root 的真实 LocalFS。
func newQueryDirTestFS(t testing.TB, root string) *vfs.LocalFS {
	t.Helper()

	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// newQueryDirTestContext 在 fs 上建共享与会话，并打开根目录句柄。
func newQueryDirTestContext(t testing.TB, fs vfs.FileSystem, aaplOn bool) (*Context, *Open) {
	t.Helper()

	share := &Share{Name: "backup", Type: wire.ShareTypeDisk, FS: fs, TimeMachine: true}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	conn.MaxTransactSize = 1 << 20
	if aaplOn {
		conn.aapl.readdirAttr.Store(true)
	}

	sess, st := conn.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession: %v", st)
	}
	tree := &Tree{ID: 1, Share: share, Session: sess}

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "",
		Flags:       vfs.OpenRead | vfs.OpenDirectory,
		Disposition: vfs.Disposition(wire.FileOpen),
	})
	if err != nil {
		t.Fatalf("打开根目录: %v", err)
	}
	open := &Open{
		Tree:          tree,
		Session:       sess,
		Path:          "",
		Handle:        h,
		IsDir:         true,
		GrantedAccess: wire.Access(wire.MaximalAccessReadWrite),
	}
	if st := sess.AddOpen(open); st != status.Success {
		t.Fatalf("AddOpen: %v", st)
	}
	t.Cleanup(open.close)

	ctx := &Context{
		Conn:    conn,
		Chain:   &Chain{},
		Session: sess,
		Tree:    tree,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return ctx, open
}

// runQueryDirectory 把 handler 跑到枚举结束，返回「名字 → 该条目的原始字节」。
func runQueryDirectory(t *testing.T, ctx *Context, open *Open, pattern string) map[string][]byte {
	t.Helper()

	out := make(map[string][]byte)
	for round := 0; round < 64; round++ {
		req := &wire.QueryDirectoryRequest{
			FileInformationClass: wire.FileIdBothDirectoryInformation,
			FileID:               wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile},
			OutputBufferLength:   64 * 1024,
		}
		if round == 0 {
			req.FileName = pattern
		}
		msg, err := req.Append(make([]byte, wire.HeaderSize))
		if err != nil {
			t.Fatalf("编码请求: %v", err)
		}
		ctx.Msg = msg
		ctx.Out = make([]byte, wire.HeaderSize)

		if err := handleQueryDirectory(ctx); err != nil {
			if err == status.NoMoreFiles {
				return out
			}
			t.Fatalf("handleQueryDirectory: %v", err)
		}

		resp, err := wire.ParseQueryDirectoryResponse(ctx.Out)
		if err != nil {
			t.Fatalf("解析响应: %v", err)
		}
		splitDirEntries(t, resp.Buffer, out)
	}
	t.Fatal("枚举没有在 64 轮内结束")
	return nil
}

// drainQueryDirectory 把目录枚举到底，只数条数，不保留字节 ——
// 给 benchmark 用，避免测量结果被测试辅助代码的 map 分配污染。
func drainQueryDirectory(tb testing.TB, ctx *Context, open *Open) int {
	tb.Helper()

	total := 0
	for {
		req := &wire.QueryDirectoryRequest{
			FileInformationClass: wire.FileIdBothDirectoryInformation,
			FileID:               wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile},
			OutputBufferLength:   64 * 1024,
		}
		if total == 0 {
			req.FileName = "*"
		}
		msg, err := req.Append(make([]byte, wire.HeaderSize))
		if err != nil {
			tb.Fatalf("编码请求: %v", err)
		}
		ctx.Msg = msg
		ctx.Out = make([]byte, wire.HeaderSize)

		if err := handleQueryDirectory(ctx); err != nil {
			if err == status.NoMoreFiles {
				return total
			}
			tb.Fatalf("handleQueryDirectory: %v", err)
		}
		resp, err := wire.ParseQueryDirectoryResponse(ctx.Out)
		if err != nil {
			tb.Fatalf("解析响应: %v", err)
		}
		total += countDirEntries(tb, resp.Buffer)
	}
}

// countDirEntries 沿 NextEntryOffset 链数条数。
func countDirEntries(tb testing.TB, buf []byte) int {
	n, pos := 0, 0
	for {
		if pos+aaplDirEntryFixed > len(buf) {
			tb.Fatalf("目录项在 %d 处被截断", pos)
		}
		n++
		next := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		if next == 0 {
			return n
		}
		pos += next
	}
}

// splitDirEntries 把目录项链切成「名字 → 该条目的固定部分+名字」的原始字节。
func splitDirEntries(t *testing.T, buf []byte, out map[string][]byte) {
	t.Helper()

	pos := 0
	for {
		if pos+aaplDirEntryFixed > len(buf) {
			t.Fatalf("目录项在 %d 处被截断（缓冲 %d 字节）", pos, len(buf))
		}
		next := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		nameLen := int(binary.LittleEndian.Uint32(buf[pos+60 : pos+64]))
		end := pos + aaplDirEntryFixed + nameLen
		if end > len(buf) {
			t.Fatalf("目录项名字越界: %d > %d", end, len(buf))
		}
		name, err := wire.DecodeUTF16LE(buf[pos+aaplDirEntryFixed : end])
		if err != nil {
			t.Fatalf("解码名字: %v", err)
		}
		entry := make([]byte, end-pos)
		copy(entry, buf[pos:end])
		out[name] = entry

		if next == 0 {
			return
		}
		pos += next
	}
}

// writeAppleMetadata 通过**公开的 ADS 路径**给对象写上 FinderInfo 与资源派生
// （AFP_AfpInfo → netatalk xattr，AFP_Resource → `._` 旁路文件）。
//
// 刻意不直接拼磁盘格式：这样这条测试同时验证了 readdir_attr 读到的东西
// 与客户端通过流写进去的东西是同一份。
func writeAppleMetadata(t *testing.T, fs vfs.FileSystem, path string, fi [vfs.FinderInfoSize]byte, rsrc []byte) {
	t.Helper()

	ai := vfs.NewAfpInfo()
	ai.FinderInfo = fi
	writeStream(t, fs, path, vfs.StreamAFPInfo, ai.Marshal())
	if len(rsrc) > 0 {
		writeStream(t, fs, path, vfs.StreamAFPResource, rsrc)
	}
}

func writeStream(t *testing.T, fs vfs.FileSystem, path, stream string, data []byte) {
	t.Helper()

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        path,
		Stream:      stream,
		Flags:       vfs.OpenWrite,
		Disposition: vfs.TruncateAlways,
	})
	if err != nil {
		t.Fatalf("打开流 %s:%s: %v", path, stream, err)
	}
	defer func() { _ = h.Close() }()

	if _, err := h.WriteAt(data, 0); err != nil {
		t.Fatalf("写流 %s:%s: %v", path, stream, err)
	}
}

// ---------------------------------------------------------------------------
// 大目录性能：readdir_attr 开 vs 关
// ---------------------------------------------------------------------------
//
// 动机：commit 9b7185b 在 vfs 层量过纯枚举，结论是「无需优化」。
// 但 readdir_attr 打开后每条目录项要**额外**向后端问一次 Apple 元数据
// （LocalFS 里是 lstat + getxattr + open/stat `._name`），
// 那个结论未必还成立 —— 所以在**命令层**重新量一遍完整路径。
//
// 跑法：
//
//	go test -run XXX -bench 'BenchmarkQueryDirectory' -benchtime 1x -benchmem \
//	    ./internal/smb/command/
//
// # 实测（AMD EPYC 9K65, linux/amd64, 2 核容器，条目为空文件、无 Apple 元数据）
//
// 第一版（走 FileSystem.AppleInfo，每条目录项重新做一次全路径解析）：
//
//	条目数   关        开        倍数   每条增量   AAPL 总 allocs
//	 1 000    4.3 ms    12.5 ms   2.9×    8.2 µs      21 316
//	10 000  125   ms   282   ms   2.3×   15.7 µs     215 144
//	50 000  387   ms  1053   ms   2.7×   13.3 µs   1 082 417
//
// 改用 vfs.DirAppleMetadata.AppleInfoAt 的目录句柄快路径之后
// （省掉逐级 lstat 的路径解析，并靠目录快照免掉注定 ENOENT 的 `._name` 探测）：
//
//	条目数   关        开        倍数   AAPL 总 allocs
//	 1 000    9.9 ms    16.6 ms   1.7×      12 314
//	10 000  154   ms   304   ms   2.0×     125 135
//	50 000  576   ms   554   ms   ≈1×      632 416
//
// 每条目录项的额外分配从约 13.7 降到约 4.7。绝对耗时的**倍数**在这台
// 与其它 agent 抢 CPU 的 2 核容器上噪声很大（50k 那一行甚至出现开比关还快），
// 分配数才是可信的对照指标。
//
// # 结论：不做进一步优化
//
//  1. 增量随条目数**线性**，没有任何超线性行为。
//  2. 客户端感知的是**单页延迟**而不是总时长。64 KiB 输出缓冲一页约装 480 条，
//     即每页 10 ms 量级，离任何客户端超时都很远。
//  3. 参照物：Samba 的 vfs_fruit 在 FRUIT_META_STREAM 模式下是对每个条目
//     **完整 CREATE + PREAD + CLOSE** 一个 AFP_AfpInfo 流，比我们一次
//     getxattr 贵得多。macOS 在真实 Samba 上就是这个体量，说明可接受。
//  4. 这条路径只在 macOS 协商了 AAPL 之后才走，Windows/Linux 客户端零开销。
//
// 不要在命令层加并发 fan-out：那是拿复杂度换一个尚未证明会造成问题的
// 常数因子（AGENTS.md §5 P6）。

// buildBandFiles 在 root 下造 n 个模拟 .sparsebundle band 的文件。
func buildBandFiles(tb testing.TB, root string, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("%x", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

func benchmarkQueryDirectory(b *testing.B, n int, aaplOn bool) {
	root := b.TempDir()
	buildBandFiles(b, root, n)
	fs := newQueryDirTestFS(b, root)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, open := newQueryDirTestContext(b, fs, aaplOn)
		got := drainQueryDirectory(b, ctx, open)
		if got != n+2 { // +2 是 "." 与 ".."
			b.Fatalf("枚举到 %d 条, 期望 %d", got, n+2)
		}
		open.close()
	}
}

func BenchmarkQueryDirectory1k(b *testing.B)      { benchmarkQueryDirectory(b, 1_000, false) }
func BenchmarkQueryDirectory1kAAPL(b *testing.B)  { benchmarkQueryDirectory(b, 1_000, true) }
func BenchmarkQueryDirectory10k(b *testing.B)     { benchmarkQueryDirectory(b, 10_000, false) }
func BenchmarkQueryDirectory10kAAPL(b *testing.B) { benchmarkQueryDirectory(b, 10_000, true) }

func BenchmarkQueryDirectory50k(b *testing.B) {
	if testing.Short() {
		b.Skip("建 5 万个文件较慢，-short 下跳过")
	}
	benchmarkQueryDirectory(b, 50_000, false)
}

func BenchmarkQueryDirectory50kAAPL(b *testing.B) {
	if testing.Short() {
		b.Skip("建 5 万个文件较慢，-short 下跳过")
	}
	benchmarkQueryDirectory(b, 50_000, true)
}
