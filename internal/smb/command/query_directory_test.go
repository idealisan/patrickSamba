package command

import (
	"bytes"
	"encoding/binary"
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
	copy(fi[0:4], "TEXT")   // 类型码
	copy(fi[4:8], "ttxt")   // 创建者码
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
	// 资源派生走 AppleDouble 旁路文件 `._band-00000`，与 vfs 的 ADS 实现一致。
	rsrc := []byte("RESOURCEFORK")
	writeAppleDouble(t, filepath.Join(root, "._"+fileName), sampleFinderInfo, rsrc)

	for _, aaplOn := range []bool{false, true} {
		name := "readdir_attr 关"
		if aaplOn {
			name = "readdir_attr 开"
		}
		t.Run(name, func(t *testing.T) {
			ctx, open := newQueryDirTestContext(t, root, aaplOn)
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
	writeAppleDouble(t, filepath.Join(root, "._f"), sampleFinderInfo, []byte("RSRC"))

	ctx, open := newQueryDirTestContext(t, root, true)
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

// TestAAPLChildPath 覆盖 "." / ".." 与共享根的拼接。
func TestAAPLChildPath(t *testing.T) {
	tests := []struct {
		dir, name, want string
		ok              bool
	}{
		{"", "f", "f", true},
		{"", ".", "", true},
		{"", "..", "", false},
		{"a/b", "f", "a/b/f", true},
		{"a/b", ".", "a/b", true},
		{"a/b", "..", "", false},
	}
	for _, tc := range tests {
		s := &aaplDirAttrSource{dir: tc.dir}
		got, ok := s.childPath(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("childPath(dir=%q, %q) = (%q, %v), 期望 (%q, %v)",
				tc.dir, tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// newQueryDirTestContext 建一个指向 root 的真实 LocalFS 共享，并打开根目录句柄。
func newQueryDirTestContext(t *testing.T, root string, aaplOn bool) (*Context, *Open) {
	t.Helper()

	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

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

// writeAppleDouble 造一个 AppleDouble 旁路文件（`._name`），
// 里面带 FinderInfo 与资源派生 —— 与 vfs 的 AFP_Resource 实现使用同一格式。
func writeAppleDouble(t *testing.T, path string, fi [vfs.FinderInfoSize]byte, rsrc []byte) {
	t.Helper()

	ad := vfs.NewAppleDouble()
	ad.SetFinderInfo(fi)
	ad.SetResource(rsrc)
	if err := os.WriteFile(path, ad.Encode(), 0o644); err != nil {
		t.Fatalf("写 AppleDouble %s: %v", path, err)
	}
}
