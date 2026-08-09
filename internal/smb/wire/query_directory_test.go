package wire

import (
	"bytes"
	"testing"
)

func TestQueryDirectoryRequestRoundTrip(t *testing.T) {
	h := Header{Command: CommandQueryDirectory, MessageID: 11, TreeID: 1, SessionID: 2}
	req := &QueryDirectoryRequest{
		FileInformationClass: FileIdBothDirectoryInformation,
		Flags:                RestartScans,
		FileID:               FileID{Persistent: 0x1122334455667788, Volatile: 0x99AABBCCDDEEFF00},
		FileName:             "*",
		OutputBufferLength:   65536,
	}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// FileNameOffset 必须相对 SMB2 头起点。
	if got := le.Uint16(msg[HeaderSize+24:]); got != HeaderSize+queryDirectoryRequestFixed {
		t.Errorf("FileNameOffset = %d, 期望 %d", got, HeaderSize+queryDirectoryRequestFixed)
	}

	got, err := ParseQueryDirectoryRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *got != *req {
		t.Errorf("round-trip 不一致\n got=%+v\nwant=%+v", got, req)
	}
	if !got.Restart() || got.SingleEntry() {
		t.Error("Flags 判定错误")
	}

	for n := 0; n < HeaderSize+queryDirectoryRequestFixed; n++ {
		if _, err := ParseQueryDirectoryRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

func TestQueryDirectoryRequestEmptyPattern(t *testing.T) {
	h := Header{Command: CommandQueryDirectory}
	req := &QueryDirectoryRequest{FileInformationClass: FileDirectoryInformation}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 空模式时仍要有 1 字节可变部分占位。
	if len(msg) != HeaderSize+queryDirectoryRequestFixed+1 {
		t.Errorf("长度 = %d, 期望 %d", len(msg), HeaderSize+queryDirectoryRequestFixed+1)
	}
	got, err := ParseQueryDirectoryRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FileName != "" {
		t.Errorf("FileName = %q, 期望空", got.FileName)
	}
}

func TestQueryDirectoryResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandQueryDirectory, Flags: FlagServerToRedir}
	resp := &QueryDirectoryResponse{Buffer: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := le.Uint16(msg[HeaderSize+2:]); got != HeaderSize+queryDirectoryResponseFixed {
		t.Errorf("OutputBufferOffset = %d, 期望 %d", got, HeaderSize+queryDirectoryResponseFixed)
	}
	got, err := ParseQueryDirectoryResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(got.Buffer) != string(resp.Buffer) {
		t.Errorf("Buffer = % X, 期望 % X", got.Buffer, resp.Buffer)
	}

	// 空缓冲：StructureSize 仍是 9，尾部补 1 字节占位。
	empty := &QueryDirectoryResponse{}
	msg, err = empty.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append 空: %v", err)
	}
	if len(msg) != HeaderSize+queryDirectoryResponseFixed+1 {
		t.Errorf("空响应长度 = %d, 期望 %d", len(msg), HeaderSize+queryDirectoryResponseFixed+1)
	}
	if _, err := ParseQueryDirectoryResponse(msg); err != nil {
		t.Fatalf("Parse 空: %v", err)
	}
}

// testDirEntries 是一组长度参差的条目，用来触发对齐填充。
func testDirEntries() []DirEntry {
	return []DirEntry{
		{
			FileIndex: 1, CreationTime: 0x01D7A1B2C3D4E5F6, LastAccessTime: 2,
			LastWriteTime: 3, ChangeTime: 4, EndOfFile: 1234, AllocationSize: 4096,
			FileAttributes: FileAttributeArchive, EaSize: 0, FileID: 0xAABB,
			ShortName: "A~1.TXT", Name: "a.txt",
		},
		{
			FileIndex: 2, EndOfFile: 0, AllocationSize: 0,
			FileAttributes: FileAttributeDirectory, FileID: 0xCCDD,
			Name: "子目录",
		},
		{
			FileIndex: 3, EndOfFile: 7, AllocationSize: 8,
			FileAttributes: FileAttributeNormal, FileID: 0xEEFF,
			Name: "very-long-file-name-to-shift-alignment.bin",
		},
	}
}

func TestDirEntryWriterAllClasses(t *testing.T) {
	classes := []FileInfoClass{
		FileDirectoryInformation,
		FileFullDirectoryInformation,
		FileIdFullDirectoryInformation,
		FileBothDirectoryInformation,
		FileIdBothDirectoryInformation,
		FileNamesInformation,
	}
	entries := testDirEntries()

	for _, class := range classes {
		w := NewDirEntryWriter(class, 64*1024)
		for i, e := range entries {
			ok, err := w.Add(e)
			if err != nil {
				t.Fatalf("class %d Add[%d]: %v", class, i, err)
			}
			if !ok {
				t.Fatalf("class %d Add[%d]: 缓冲区应当够用", class, i)
			}
		}
		if w.Count() != len(entries) {
			t.Errorf("class %d Count = %d, 期望 %d", class, w.Count(), len(entries))
		}
		buf := w.Bytes()

		// 每条 NextEntryOffset 必须 8 字节对齐，最后一条为 0。
		pos := 0
		for i := 0; ; i++ {
			next := int(le.Uint32(buf[pos:]))
			if next == 0 {
				if i != len(entries)-1 {
					t.Fatalf("class %d: 链在第 %d 条提前结束", class, i)
				}
				break
			}
			if next%8 != 0 {
				t.Errorf("class %d 条目[%d] NextEntryOffset = %d, 未 8 字节对齐", class, i, next)
			}
			pos += next
		}

		got, err := ParseDirEntries(buf, class)
		if err != nil {
			t.Fatalf("class %d ParseDirEntries: %v", class, err)
		}
		if len(got) != len(entries) {
			t.Fatalf("class %d 解析出 %d 条, 期望 %d", class, len(got), len(entries))
		}
		for i := range got {
			if got[i].Name != entries[i].Name {
				t.Errorf("class %d 条目[%d] Name = %q, 期望 %q", class, i, got[i].Name, entries[i].Name)
			}
			if got[i].FileIndex != entries[i].FileIndex {
				t.Errorf("class %d 条目[%d] FileIndex = %d", class, i, got[i].FileIndex)
			}
			if class == FileNamesInformation {
				continue
			}
			if got[i].EndOfFile != entries[i].EndOfFile ||
				got[i].AllocationSize != entries[i].AllocationSize ||
				got[i].FileAttributes != entries[i].FileAttributes ||
				got[i].CreationTime != entries[i].CreationTime {
				t.Errorf("class %d 条目[%d] 公共字段不一致: %+v", class, i, got[i])
			}
			switch class {
			case FileIdFullDirectoryInformation, FileIdBothDirectoryInformation:
				if got[i].FileID != entries[i].FileID {
					t.Errorf("class %d 条目[%d] FileID = %#x, 期望 %#x", class, i, got[i].FileID, entries[i].FileID)
				}
			}
			switch class {
			case FileBothDirectoryInformation, FileIdBothDirectoryInformation:
				if got[i].ShortName != entries[i].ShortName {
					t.Errorf("class %d 条目[%d] ShortName = %q, 期望 %q", class, i, got[i].ShortName, entries[i].ShortName)
				}
			}
		}
	}
}

func TestDirEntryWriterBufferFull(t *testing.T) {
	const class = FileIdBothDirectoryInformation
	e := DirEntry{Name: "abcdefgh"} // 104 + 16 = 120 字节

	size, err := DirEntrySize(class, e)
	if err != nil {
		t.Fatalf("DirEntrySize: %v", err)
	}
	if size != dirInfoFixedIDBothDir+16 {
		t.Fatalf("DirEntrySize = %d, 期望 %d", size, dirInfoFixedIDBothDir+16)
	}

	// 只放得下一条。
	w := NewDirEntryWriter(class, size+8)
	if ok, err := w.Add(e); err != nil || !ok {
		t.Fatalf("第一条应写入成功: ok=%v err=%v", ok, err)
	}
	before := w.Len()
	ok, err := w.Add(e)
	if err != nil {
		t.Fatalf("第二条 Add: %v", err)
	}
	if ok {
		t.Error("第二条应因空间不足被拒绝")
	}
	if w.Len() != before || w.Count() != 1 {
		t.Errorf("被拒绝的条目不应改动缓冲区: len %d→%d count=%d", before, w.Len(), w.Count())
	}
	// 唯一一条的 NextEntryOffset 必须是 0。
	if next := le.Uint32(w.Bytes()); next != 0 {
		t.Errorf("最后一条 NextEntryOffset = %d, 期望 0", next)
	}

	// 一条都放不下。
	w = NewDirEntryWriter(class, 8)
	if ok, _ := w.Add(e); ok || w.Len() != 0 {
		t.Error("空间不足时不应写入任何字节")
	}
}

func TestDirEntryUnsupportedClass(t *testing.T) {
	if _, ok := DirInfoFixedSize(FileBasicInformation); ok {
		t.Error("FileBasicInformation 不是目录信息类")
	}
	if _, err := AppendDirEntry(nil, FileBasicInformation, DirEntry{}); err == nil {
		t.Error("不支持的类应报错")
	}
	if _, err := ParseDirEntries([]byte{0, 0, 0, 0}, FileBasicInformation); err == nil {
		t.Error("不支持的类应报错")
	}
}

func TestParseDirEntriesMalformed(t *testing.T) {
	if got, err := ParseDirEntries(nil, FileDirectoryInformation); err != nil || got != nil {
		t.Errorf("空缓冲应返回 (nil, nil), 得到 (%v, %v)", got, err)
	}
	// NextEntryOffset 指向自身 → 死循环风险，必须拒绝。
	buf := make([]byte, 2*dirInfoFixedDirectory)
	le.PutUint32(buf[0:], 0)
	le.PutUint32(buf[0:], uint32(dirInfoFixedDirectory-1)) // 小于固定部分
	if _, err := ParseDirEntries(buf, FileDirectoryInformation); err == nil {
		t.Error("NextEntryOffset 小于固定部分应报错")
	}
	// NextEntryOffset 越界。
	le.PutUint32(buf[0:], uint32(len(buf)+8))
	if _, err := ParseDirEntries(buf, FileDirectoryInformation); err == nil {
		t.Error("NextEntryOffset 越界应报错")
	}
	// FileNameLength 越界。
	le.PutUint32(buf[0:], 0)
	le.PutUint32(buf[60:], 0xFFFF)
	if _, err := ParseDirEntries(buf, FileDirectoryInformation); err == nil {
		t.Error("FileNameLength 越界应报错")
	}
}

// TestFileIdBothDirectoryInformationLayout 逐字段锁死 MS-FSCC §2.4.17 的偏移。
//
//	 0 NextEntryOffset(4)   32 ChangeTime(8)       64 EaSize(4)
//	 4 FileIndex(4)         40 EndOfFile(8)        68 ShortNameLength(1)
//	 8 CreationTime(8)      48 AllocationSize(8)   69 Reserved1(1)   ← 易漏
//	16 LastAccessTime(8)    56 FileAttributes(4)   70 ShortName(24)
//	24 LastWriteTime(8)     60 FileNameLength(4)   94 Reserved2(2)   ← 易漏
//	                                               96 FileId(8)
//	                                              104 FileName(变长)
//
// 69 处的 Reserved1 与 94 处的 Reserved2 是最容易少写/多写的两个字段，
// 写错会让 FileId 与 FileName 整体错位，macOS Finder 表现为目录项乱码。
func TestFileIdBothDirectoryInformationLayout(t *testing.T) {
	e := DirEntry{
		FileIndex:      0x11223344,
		CreationTime:   0x0102030405060708,
		LastAccessTime: 0x1112131415161718,
		LastWriteTime:  0x2122232425262728,
		ChangeTime:     0x3132333435363738,
		EndOfFile:      0x4142434445464748,
		AllocationSize: 0x5152535455565758,
		FileAttributes: FileAttributeArchive,
		EaSize:         0x61626364,
		FileID:         0x7172737475767778,
		ShortName:      "AB", // 4 字节 UTF-16LE
		Name:           "ab", // 4 字节 UTF-16LE
	}
	b, err := AppendDirEntry(nil, FileIdBothDirectoryInformation, e)
	if err != nil {
		t.Fatalf("AppendDirEntry: %v", err)
	}
	if len(b) != dirInfoFixedIDBothDir+4 {
		t.Fatalf("长度 = %d, 期望 %d", len(b), dirInfoFixedIDBothDir+4)
	}
	if dirInfoFixedIDBothDir != 104 {
		t.Fatalf("固定部分 = %d, MS-FSCC §2.4.17 要求 104", dirInfoFixedIDBothDir)
	}

	checks := []struct {
		off  int
		size int
		want uint64
		name string
	}{
		{0, 4, 0, "NextEntryOffset"},
		{4, 4, 0x11223344, "FileIndex"},
		{8, 8, 0x0102030405060708, "CreationTime"},
		{16, 8, 0x1112131415161718, "LastAccessTime"},
		{24, 8, 0x2122232425262728, "LastWriteTime"},
		{32, 8, 0x3132333435363738, "ChangeTime"},
		{40, 8, 0x4142434445464748, "EndOfFile"},
		{48, 8, 0x5152535455565758, "AllocationSize"},
		{56, 4, uint64(FileAttributeArchive), "FileAttributes"},
		{60, 4, 4, "FileNameLength"},
		{64, 4, 0x61626364, "EaSize"},
		{68, 1, 4, "ShortNameLength"},
		{69, 1, 0, "Reserved1"},
		{94, 2, 0, "Reserved2"},
		{96, 8, 0x7172737475767778, "FileId"},
	}
	for _, c := range checks {
		var got uint64
		switch c.size {
		case 1:
			got = uint64(b[c.off])
		case 2:
			got = uint64(le.Uint16(b[c.off:]))
		case 4:
			got = uint64(le.Uint32(b[c.off:]))
		case 8:
			got = le.Uint64(b[c.off:])
		}
		if got != c.want {
			t.Errorf("偏移 %d %s = %#x, 期望 %#x", c.off, c.name, got, c.want)
		}
	}
	// ShortName 从 70 开始，"AB" 的 UTF-16LE 是 41 00 42 00，其余 20 字节补零。
	if !bytes.Equal(b[70:94], []byte{'A', 0, 'B', 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("ShortName 字段 = % X", b[70:94])
	}
	// FileName 紧跟固定部分，从 104 开始。
	if !bytes.Equal(b[104:], []byte{'a', 0, 'b', 0}) {
		t.Errorf("FileName = % X", b[104:])
	}
}

// TestDirEntryShortNameRaw 覆盖 AAPL readdir_attr 需要的「整段 24 字节自定义」
// 能力：Samba vfs_fruit 把 ShortName 字段改写成
// 资源派生大小(8B 小端) + 压缩 FinderInfo(16B)，ShortNameLength 填 24。
func TestDirEntryShortNameRaw(t *testing.T) {
	var raw [24]byte
	le.PutUint64(raw[0:8], 0x0000000000123456) // 资源派生大小
	copy(raw[8:24], []byte("FINDERINFO-16BY!"))

	e := DirEntry{
		Name:         "a.txt",
		EaSize:       0x001F01FF, // AAPL 下这里是 max_access
		FileID:       42,
		ShortName:    "IGNORED~1.TXT", // ShortNameRaw 非 nil 时必须被忽略
		ShortNameRaw: raw[:],
	}
	b, err := AppendDirEntry(nil, FileIdBothDirectoryInformation, e)
	if err != nil {
		t.Fatalf("AppendDirEntry: %v", err)
	}
	if b[68] != 24 {
		t.Errorf("ShortNameLength = %d, 期望 24", b[68])
	}
	if b[69] != 0 {
		t.Errorf("Reserved1 = %d, 期望 0", b[69])
	}
	if !bytes.Equal(b[70:94], raw[:]) {
		t.Errorf("ShortName 字段 = % X, 期望 % X", b[70:94], raw[:])
	}
	if le.Uint32(b[64:]) != 0x001F01FF {
		t.Errorf("EaSize(max_access) = %#x", le.Uint32(b[64:]))
	}
	if le.Uint16(b[94:]) != 0 {
		t.Error("Reserved2 必须留 0（不实现 NFS ACE）")
	}
	if le.Uint64(b[96:]) != 42 {
		t.Errorf("FileId = %d", le.Uint64(b[96:]))
	}

	// 解析→再编码必须无损。
	got, err := ParseDirEntries(b, FileIdBothDirectoryInformation)
	if err != nil {
		t.Fatalf("ParseDirEntries: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].ShortNameRaw, raw[:]) {
		t.Fatalf("ShortNameRaw 未还原: %+v", got)
	}
	again, err := AppendDirEntry(nil, FileIdBothDirectoryInformation, got[0])
	if err != nil {
		t.Fatalf("重新编码: %v", err)
	}
	if !bytes.Equal(again, b) {
		t.Errorf("解析→编码不无损\n got=% X\nwant=% X", again, b)
	}

	// 普通短名的解析→编码同样要无损（ShortNameLength 不能被改成 24）。
	plain, err := AppendDirEntry(nil, FileBothDirectoryInformation,
		DirEntry{Name: "longfilename.txt", ShortName: "LONGFI~1.TXT"})
	if err != nil {
		t.Fatalf("AppendDirEntry 普通短名: %v", err)
	}
	pe, err := ParseDirEntries(plain, FileBothDirectoryInformation)
	if err != nil {
		t.Fatalf("ParseDirEntries 普通短名: %v", err)
	}
	if pe[0].ShortName != "LONGFI~1.TXT" {
		t.Errorf("ShortName = %q", pe[0].ShortName)
	}
	pb, err := AppendDirEntry(nil, FileBothDirectoryInformation, pe[0])
	if err != nil {
		t.Fatalf("重新编码普通短名: %v", err)
	}
	if !bytes.Equal(pb, plain) {
		t.Errorf("普通短名解析→编码不无损\n got=% X\nwant=% X", pb, plain)
	}

	// 超过 24 字节必须报错，不能越界写坏后面的 Reserved2/FileId。
	if _, err := AppendDirEntry(nil, FileIdBothDirectoryInformation,
		DirEntry{Name: "x", ShortNameRaw: make([]byte, 25)}); err == nil {
		t.Error("ShortNameRaw 超过 24 字节应报错")
	}
}
