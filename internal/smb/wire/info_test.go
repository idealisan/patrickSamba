package wire

import (
	"bytes"
	"testing"
)

func TestQueryInfoRequestRoundTrip(t *testing.T) {
	h := Header{Command: CommandQueryInfo, MessageID: 3, TreeID: 1, SessionID: 2}
	req := &QueryInfoRequest{
		InfoType:           InfoTypeFile,
		FileInfoClass:      uint8(FileAllInformation),
		OutputBufferLength: 4096,
		FileID:             FileID{Persistent: 1, Volatile: 2},
	}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 无 InputBuffer 时补 1 字节占位。
	if len(msg) != HeaderSize+queryInfoRequestFixed+1 {
		t.Errorf("长度 = %d, 期望 %d", len(msg), HeaderSize+queryInfoRequestFixed+1)
	}
	got, err := ParseQueryInfoRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.InfoType != req.InfoType || got.FileClass() != FileAllInformation ||
		got.OutputBufferLength != req.OutputBufferLength || got.FileID != req.FileID {
		t.Errorf("round-trip 不一致: %+v", got)
	}

	// 带 InputBuffer（QUERY_INFO(SECURITY) / FullEa 会用到）。
	req.Input = []byte{1, 2, 3, 4, 5}
	req.InfoType = InfoTypeSecurity
	req.AdditionalInformation = OwnerSecurityInformation | DACLSecurityInformation
	msg, err = req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append(input): %v", err)
	}
	if off := le.Uint16(msg[HeaderSize+8:]); off != HeaderSize+queryInfoRequestFixed {
		t.Errorf("InputBufferOffset = %d", off)
	}
	got, err = ParseQueryInfoRequest(msg)
	if err != nil {
		t.Fatalf("Parse(input): %v", err)
	}
	if !bytes.Equal(got.Input, req.Input) || got.AdditionalInformation != req.AdditionalInformation {
		t.Errorf("Input 往返不一致: %+v", got)
	}

	for n := 0; n < HeaderSize+queryInfoRequestFixed; n++ {
		if _, err := ParseQueryInfoRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

func TestQueryInfoResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandQueryInfo, Flags: FlagServerToRedir}
	info := FileStandardInfo{AllocationSize: 4096, EndOfFile: 1234, NumberOfLinks: 1}
	resp := &QueryInfoResponse{Buffer: info.Encode()}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off := le.Uint16(msg[HeaderSize+2:]); off != HeaderSize+queryInfoResponseFixed {
		t.Errorf("OutputBufferOffset = %d", off)
	}
	got, err := ParseQueryInfoResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	back, err := ParseFileStandardInfo(got.Buffer)
	if err != nil {
		t.Fatalf("ParseFileStandardInfo: %v", err)
	}
	if back != info {
		t.Errorf("FileStandardInformation 往返不一致: %+v", back)
	}
}

func TestFileInfoClassSizes(t *testing.T) {
	basic := FileBasicInfo{
		CreationTime: 1, LastAccessTime: 2, LastWriteTime: 3, ChangeTime: 4,
		FileAttributes: FileAttributeArchive,
	}
	b := basic.Encode()
	if len(b) != FileBasicInfoSize {
		t.Fatalf("FileBasicInformation 长度 = %d, 期望 %d", len(b), FileBasicInfoSize)
	}
	got, err := ParseFileBasicInfo(b)
	if err != nil || got != basic {
		t.Errorf("FileBasicInformation 往返: %+v err=%v", got, err)
	}
	// 36 字节短形式必须接受（部分客户端 SET_INFO 会省略尾部 Reserved）。
	if _, err := ParseFileBasicInfo(b[:36]); err != nil {
		t.Errorf("36 字节短形式应被接受: %v", err)
	}
	if _, err := ParseFileBasicInfo(b[:35]); err == nil {
		t.Error("35 字节应报错")
	}

	std := FileStandardInfo{AllocationSize: 8192, EndOfFile: 100, NumberOfLinks: 2, Directory: true}
	if len(std.Encode()) != FileStandardInfoSize {
		t.Error("FileStandardInformation 长度错误")
	}

	nw := FileNetworkOpenInfo{CreationTime: 9, EndOfFile: 42, FileAttributes: FileAttributeDirectory}
	nb := nw.Encode()
	if len(nb) != FileNetworkOpenInfoSize {
		t.Fatalf("FileNetworkOpenInformation 长度 = %d", len(nb))
	}
	gotNW, err := ParseFileNetworkOpenInfo(nb)
	if err != nil || gotNW != nw {
		t.Errorf("FileNetworkOpenInformation 往返: %+v err=%v", gotNW, err)
	}

	if len(EncodeFileInternalInfo(7)) != FileInternalInfoSize ||
		len(EncodeFileEaInfo(0)) != FileEaInfoSize ||
		len(EncodeFileAccessInfo(FileReadData)) != FileAccessInfoSize ||
		len(EncodeFilePositionInfo(0)) != FilePositionInfoSize ||
		len(EncodeFileModeInfo(0)) != FileModeInfoSize ||
		len(EncodeFileAlignmentInfo(FileByteAlignment)) != FileAlignmentInfoSize ||
		len(EncodeFileAttributeTagInfo(0, 0)) != FileAttributeTagInfoSize ||
		len(EncodeFileIDInfo(1, 2)) != FileIDInfoSize {
		t.Error("某个定长 info class 长度不对")
	}

	pos, err := ParseFilePositionInfo(EncodeFilePositionInfo(1 << 40))
	if err != nil || pos != 1<<40 {
		t.Errorf("FilePositionInformation 往返 = %d err=%v", pos, err)
	}
}

func TestFileNameInfoRoundTrip(t *testing.T) {
	b := EncodeFileNameInfo(`\dir\文件.txt`)
	if le.Uint32(b) != uint32(len(b)-4) {
		t.Error("FileNameLength 不对")
	}
	got, err := ParseFileNameInfo(b)
	if err != nil || got != `\dir\文件.txt` {
		t.Errorf("FileNameInformation 往返 = %q err=%v", got, err)
	}
	// 长度字段越界必须报错。
	le.PutUint32(b, 0xFFFF)
	if _, err := ParseFileNameInfo(b); err == nil {
		t.Error("FileNameLength 越界应报错")
	}
	if _, err := ParseFileNameInfo(b[:3]); err == nil {
		t.Error("截断应报错")
	}
}

func TestFileAllInfoRoundTrip(t *testing.T) {
	want := FileAllInfo{
		Basic: FileBasicInfo{CreationTime: 1, LastAccessTime: 2, LastWriteTime: 3,
			ChangeTime: 4, FileAttributes: FileAttributeArchive},
		Standard:             FileStandardInfo{AllocationSize: 4096, EndOfFile: 10, NumberOfLinks: 1},
		IndexNumber:          0xCAFE,
		AccessFlags:          FileReadData | FileWriteData,
		CurrentByteOffset:    0,
		AlignmentRequirement: FileByteAlignment,
		Name:                 `\sub\a.txt`,
	}
	b := want.Encode()
	if len(b) != fileAllInfoFixed+4+UTF16LELen(want.Name) {
		t.Fatalf("FileAllInformation 长度 = %d", len(b))
	}
	got, err := ParseFileAllInfo(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Basic != want.Basic || got.Standard != want.Standard ||
		got.IndexNumber != want.IndexNumber || got.AccessFlags != want.AccessFlags ||
		got.Name != want.Name {
		t.Errorf("round-trip 不一致:\n got=%+v\nwant=%+v", got, want)
	}
	if _, err := ParseFileAllInfo(b[:fileAllInfoFixed]); err == nil {
		t.Error("截断应报错")
	}
}

func TestStreamInfoChain(t *testing.T) {
	streams := []FileStreamInfo{
		{StreamSize: 100, StreamAllocationSize: 4096, Name: `::$DATA`},
		{StreamSize: 7, StreamAllocationSize: 4096, Name: `:AFP_AfpInfo:$DATA`},
		{StreamSize: 0, StreamAllocationSize: 0, Name: `:AFP_Resource:$DATA`},
	}
	b := AppendStreamInfoChain(nil, streams)
	if len(b) == 0 {
		t.Fatal("链不应为空")
	}
	// 每条 NextEntryOffset 必须 8 字节对齐，最后一条为 0。
	pos := 0
	for i := 0; ; i++ {
		next := int(le.Uint32(b[pos:]))
		if next == 0 {
			if i != len(streams)-1 {
				t.Fatalf("链在第 %d 条提前结束", i)
			}
			break
		}
		if next%8 != 0 {
			t.Errorf("条目[%d] NextEntryOffset = %d, 未 8 字节对齐", i, next)
		}
		pos += next
	}

	got, err := ParseStreamInfoChain(b)
	if err != nil {
		t.Fatalf("ParseStreamInfoChain: %v", err)
	}
	if len(got) != len(streams) {
		t.Fatalf("解析出 %d 条, 期望 %d", len(got), len(streams))
	}
	for i := range got {
		if got[i] != streams[i] {
			t.Errorf("条目[%d] = %+v, 期望 %+v", i, got[i], streams[i])
		}
	}

	if got, err := ParseStreamInfoChain(nil); err != nil || got != nil {
		t.Error("空缓冲应返回 (nil, nil)")
	}
	// NextEntryOffset 越界。
	le.PutUint32(b[0:], uint32(len(b)+8))
	if _, err := ParseStreamInfoChain(b); err == nil {
		t.Error("NextEntryOffset 越界应报错")
	}
}

func TestFsInfoClassSizes(t *testing.T) {
	vol := FsVolumeInfo{VolumeCreationTime: 1, VolumeSerialNumber: 0xDEADBEEF, Label: "stupidSamba"}
	vb := vol.Encode()
	if len(vb) != 18+UTF16LELen(vol.Label) {
		t.Errorf("FileFsVolumeInformation 长度 = %d", len(vb))
	}
	if le.Uint32(vb[12:]) != uint32(UTF16LELen(vol.Label)) {
		t.Error("VolumeLabelLength 不对")
	}

	if len((FsSizeInfo{}).Encode()) != FsSizeInfoSize ||
		len((FsFullSizeInfo{}).Encode()) != FsFullSizeInfoSize ||
		len((FsDeviceInfo{}).Encode()) != FsDeviceInfoSize ||
		len((FsSectorSizeInfo{}).Encode()) != FsSectorSizeInfoSize {
		t.Error("某个 fs info class 长度不对")
	}

	full := FsFullSizeInfo{
		TotalAllocationUnits:           1 << 30,
		CallerAvailableAllocationUnits: 1 << 29,
		ActualAvailableAllocationUnits: 1 << 29,
		SectorsPerAllocationUnit:       8,
		BytesPerSector:                 512,
	}
	fb := full.Encode()
	if int64(le.Uint64(fb[0:])) != full.TotalAllocationUnits ||
		le.Uint32(fb[24:]) != 8 || le.Uint32(fb[28:]) != 512 {
		t.Error("FileFsFullSizeInformation 字段位置不对")
	}

	attr := FsAttributeInfo{
		Attributes:                 FileCasePreservedNames | FileUnicodeOnDisk | FileSupportsSparseFiles,
		MaximumComponentNameLength: 255,
		FileSystemName:             "NTFS",
	}
	ab := attr.Encode()
	if len(ab) != 12+UTF16LELen("NTFS") || le.Uint32(ab[4:]) != 255 {
		t.Error("FileFsAttributeInformation 编码错误")
	}
}

func TestSetInfoRequestRoundTrip(t *testing.T) {
	h := Header{Command: CommandSetInfo, MessageID: 4, TreeID: 1, SessionID: 2}
	payload := FileRenameInfo{ReplaceIfExists: true, FileName: `newdir\new.txt`}.Encode()
	req := &SetInfoRequest{
		InfoType:      InfoTypeFile,
		FileInfoClass: uint8(FileRenameInformation),
		FileID:        FileID{Persistent: 9, Volatile: 8},
		Buffer:        payload,
	}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off := le.Uint16(msg[HeaderSize+8:]); off != HeaderSize+setInfoRequestFixed {
		t.Errorf("BufferOffset = %d, 期望 %d", off, HeaderSize+setInfoRequestFixed)
	}
	if n := le.Uint32(msg[HeaderSize+4:]); int(n) != len(payload) {
		t.Errorf("BufferLength = %d, 期望 %d", n, len(payload))
	}
	got, err := ParseSetInfoRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.InfoType != req.InfoType || got.FileClass() != FileRenameInformation ||
		got.FileID != req.FileID || !bytes.Equal(got.Buffer, payload) {
		t.Errorf("round-trip 不一致: %+v", got)
	}

	rn, err := ParseFileRenameInfo(got.Buffer)
	if err != nil {
		t.Fatalf("ParseFileRenameInfo: %v", err)
	}
	if !rn.ReplaceIfExists || rn.FileName != `newdir\new.txt` || rn.RootDirectory != 0 {
		t.Errorf("FileRenameInformation = %+v", rn)
	}

	for n := 0; n < HeaderSize+setInfoRequestFixed; n++ {
		if _, err := ParseSetInfoRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

func TestSetInfoResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandSetInfo, Flags: FlagServerToRedir}
	msg := (&SetInfoResponse{}).Append(h.Append(nil))
	if len(msg) != HeaderSize+2 {
		t.Fatalf("长度 = %d, 期望 %d", len(msg), HeaderSize+2)
	}
	if le.Uint16(msg[HeaderSize:]) != 2 {
		t.Error("StructureSize 应为 2")
	}
	if _, err := ParseSetInfoResponse(msg); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := ParseSetInfoResponse(msg[:HeaderSize+1]); err == nil {
		t.Error("截断应报错")
	}
}

func TestSetInfoPayloads(t *testing.T) {
	// FileDispositionInformation
	if v, err := ParseFileDispositionInfo(EncodeFileDispositionInfo(true)); err != nil || !v {
		t.Errorf("FileDispositionInformation = %v err=%v", v, err)
	}
	if _, err := ParseFileDispositionInfo(nil); err == nil {
		t.Error("空缓冲应报错")
	}

	// FileEndOfFileInformation
	if v, err := ParseFileEndOfFileInfo(EncodeFileEndOfFileInfo(1 << 40)); err != nil || v != 1<<40 {
		t.Errorf("FileEndOfFileInformation = %d err=%v", v, err)
	}
	neg := make([]byte, 8)
	le.PutUint64(neg, ^uint64(0)) // -1
	if _, err := ParseFileEndOfFileInfo(neg); err == nil {
		t.Error("负长度应报错")
	}
	if _, err := ParseFileAllocationInfo(neg); err == nil {
		t.Error("负分配大小应报错")
	}
	if _, err := ParseFileEndOfFileInfo(neg[:7]); err == nil {
		t.Error("截断应报错")
	}

	// FileAllocationInformation
	alloc := make([]byte, 8)
	le.PutUint64(alloc, 8192)
	if v, err := ParseFileAllocationInfo(alloc); err != nil || v != 8192 {
		t.Errorf("FileAllocationInformation = %d err=%v", v, err)
	}

	// FileLinkInformation 与 rename 同布局。
	lk := FileLinkInfo{ReplaceIfExists: false, FileName: `hard.lnk`}
	gotLk, err := ParseFileLinkInfo(lk.Encode())
	if err != nil || gotLk != lk {
		t.Errorf("FileLinkInformation 往返 = %+v err=%v", gotLk, err)
	}

	// FileRenameInformation 的 FileNameLength 越界必须报错。
	bad := lk.Encode()
	le.PutUint32(bad[16:], 0xFFFF)
	if _, err := ParseFileRenameInfo(bad); err == nil {
		t.Error("FileNameLength 越界应报错")
	}
	if _, err := ParseFileRenameInfo(bad[:19]); err == nil {
		t.Error("截断应报错")
	}
}
