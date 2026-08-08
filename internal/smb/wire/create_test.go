package wire

import (
	"bytes"
	"testing"
)

func TestCreateRequestRoundTrip(t *testing.T) {
	r := &CreateRequest{
		RequestedOplockLevel: OplockLevelNone,
		ImpersonationLevel:   ImpersonationImpersonation,
		DesiredAccess:        GenericRead | FileReadAttributes,
		FileAttributes:       0,
		ShareAccess:          ShareRead | ShareWrite | ShareDelete,
		CreateDisposition:    FileOpen,
		CreateOptions:        FileNonDirectoryFile,
		Name:                 `dir\subdir\file.txt`,
	}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 57 {
		t.Errorf("StructureSize = %d, want 57", le.Uint16(body))
	}
	// NameOffset 相对头起点 = 64 + 56 = 120。
	if got := le.Uint16(body[44:]); got != 120 {
		t.Errorf("NameOffset = %d, want 120", got)
	}
	got, err := ParseCreateRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != r.Name || got.DesiredAccess != r.DesiredAccess ||
		got.CreateDisposition != r.CreateDisposition || got.CreateOptions != r.CreateOptions ||
		got.ShareAccess != r.ShareAccess || got.ImpersonationLevel != r.ImpersonationLevel {
		t.Errorf("round-trip 不一致: %+v", got)
	}
}

func TestCreateRequestEmptyName(t *testing.T) {
	// 共享根目录：Name 为空串，NameLength = 0。
	r := &CreateRequest{
		DesiredAccess:     GenericRead,
		CreateDisposition: FileOpen,
		CreateOptions:     FileDirectoryFile,
	}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCreateRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want 空串", got.Name)
	}
}

func TestCreateRequestWithContexts(t *testing.T) {
	r := &CreateRequest{
		DesiredAccess:     GenericRead,
		CreateDisposition: FileOpen,
		Name:              "a.txt",
		Contexts: []CreateContext{
			{Name: CreateContextDHnQ, Data: make([]byte, 16)},
			{Name: CreateContextMxAc},                           // 无 Data
			{Name: CreateContextQFid},                           // 无 Data
			{Name: CreateContextRqLs, Data: make([]byte, 32)},   // V1 lease
			{Name: CreateContextAlSi, Data: make([]byte, 8)},    //
			{Name: CreateContextAAPL, Data: make([]byte, 24)},   //
			{Name: CreateContextExtA, Data: []byte{1, 2, 3, 4}}, // 长度非 8 倍数
		},
	}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	ctxOff := le.Uint32(body[48:])
	if ctxOff%8 != 0 {
		t.Errorf("CreateContextsOffset = %d，必须 8 字节对齐", ctxOff)
	}

	got, err := ParseCreateRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Contexts) != len(r.Contexts) {
		t.Fatalf("context 数 = %d, want %d", len(got.Contexts), len(r.Contexts))
	}
	for i := range r.Contexts {
		if got.Contexts[i].Name != r.Contexts[i].Name {
			t.Errorf("Contexts[%d].Name = %q, want %q", i, got.Contexts[i].Name, r.Contexts[i].Name)
		}
		if !bytes.Equal(got.Contexts[i].Data, r.Contexts[i].Data) {
			t.Errorf("Contexts[%d].Data = %x, want %x", i, got.Contexts[i].Data, r.Contexts[i].Data)
		}
	}
	if _, ok := FindCreateContext(got.Contexts, CreateContextMxAc); !ok {
		t.Error("应能找到 MxAc context")
	}
	if _, ok := FindCreateContext(got.Contexts, "ZZZZ"); ok {
		t.Error("不应找到不存在的 context")
	}
}

// TestCreateContextInternalOffsets 校验 create context 内部偏移的基准是
// **context 自身起点**（MS-SMB2 §2.2.13.2），而不是消息起点。
func TestCreateContextInternalOffsets(t *testing.T) {
	ctxs := []CreateContext{
		{Name: "MxAc", Data: []byte{1, 2, 3}},
		{Name: "QFid", Data: bytes.Repeat([]byte{9}, 32)},
	}
	blob, err := AppendCreateContexts(nil, ctxs)
	if err != nil {
		t.Fatal(err)
	}
	// 第一个 context：Name 在偏移 16，Data 在 8 字节对齐处（16+4 → 24）。
	if got := le.Uint16(blob[4:]); got != 16 {
		t.Errorf("NameOffset = %d, want 16", got)
	}
	if got := le.Uint16(blob[6:]); got != 4 {
		t.Errorf("NameLength = %d, want 4", got)
	}
	if got := le.Uint16(blob[10:]); got != 24 {
		t.Errorf("DataOffset = %d, want 24（相对本 context 起点且 8 字节对齐）", got)
	}
	if got := le.Uint32(blob[12:]); got != 3 {
		t.Errorf("DataLength = %d, want 3", got)
	}
	next := le.Uint32(blob[0:])
	if next == 0 || next%8 != 0 {
		t.Fatalf("Next = %d，应非 0 且 8 字节对齐", next)
	}
	// 最后一个 context 的 Next 必须是 0。
	if got := le.Uint32(blob[next:]); got != 0 {
		t.Errorf("最后一个 context 的 Next = %d, want 0", got)
	}

	got, err := ParseCreateContexts(blob)
	if err != nil {
		t.Fatalf("ParseCreateContexts: %v", err)
	}
	if len(got) != 2 || got[0].Name != "MxAc" || got[1].Name != "QFid" ||
		!bytes.Equal(got[1].Data, ctxs[1].Data) {
		t.Errorf("round-trip 失败: %+v", got)
	}
}

func TestCreateContextMalformed(t *testing.T) {
	blob, err := AppendCreateContexts(nil, []CreateContext{
		{Name: "MxAc", Data: []byte{1, 2, 3}},
		{Name: "QFid", Data: []byte{4}},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("next 指向自身造成死循环", func(t *testing.T) {
		bad := bytes.Clone(blob)
		le.PutUint32(bad[0:], 0) // 先断链
		le.PutUint32(bad[0:], 8) // Next=8 < 头长 16 → 非法
		if _, err := ParseCreateContexts(bad); err == nil {
			t.Error("Next 小于头长应报错")
		}
	})

	t.Run("next 越界", func(t *testing.T) {
		bad := bytes.Clone(blob)
		le.PutUint32(bad[0:], 0xFFFFFF)
		if _, err := ParseCreateContexts(bad); err == nil {
			t.Error("越界 Next 应报错")
		}
	})

	t.Run("dataLength 越界", func(t *testing.T) {
		bad := bytes.Clone(blob)
		le.PutUint32(bad[12:], 0xFFFF)
		if _, err := ParseCreateContexts(bad); err == nil {
			t.Error("越界 DataLength 应报错")
		}
	})

	t.Run("截断", func(t *testing.T) {
		for n := 1; n < len(blob); n++ {
			// 不能 panic；能否报错取决于截断位置，这里只要求不崩。
			_, _ = ParseCreateContexts(blob[:n])
		}
	})
}

func TestCreateResponseRoundTrip(t *testing.T) {
	r := &CreateResponse{
		OplockLevel:    OplockLevelNone,
		CreateAction:   FileOpened,
		CreationTime:   132000000000000000,
		LastAccessTime: 132000000000000001,
		LastWriteTime:  132000000000000002,
		ChangeTime:     132000000000000003,
		AllocationSize: 4096,
		EndOfFile:      1234,
		FileAttributes: FileAttributeArchive,
		FileID:         FileID{Persistent: 0x11, Volatile: 0x22},
		Contexts: []CreateContext{
			{Name: CreateContextMxAc, Data: MaximalAccessContext{MaximalAccess: MaximalAccessReadWrite}.Encode()},
			{Name: CreateContextQFid, Data: DiskIDContext{DiskFileID: 7, VolumeID: 8}.Encode()},
		},
	}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if le.Uint16(msg[HeaderSize:]) != 89 {
		t.Errorf("StructureSize = %d, want 89", le.Uint16(msg[HeaderSize:]))
	}
	got, err := ParseCreateResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FileID != r.FileID || got.EndOfFile != r.EndOfFile ||
		got.CreateAction != r.CreateAction || got.FileAttributes != r.FileAttributes ||
		got.CreationTime != r.CreationTime {
		t.Errorf("固定字段 round-trip 失败: %+v", got)
	}
	mx, ok := FindCreateContext(got.Contexts, CreateContextMxAc)
	if !ok {
		t.Fatal("缺少 MxAc context")
	}
	m, err := ParseMaximalAccessContext(mx)
	if err != nil {
		t.Fatal(err)
	}
	if m.MaximalAccess != MaximalAccessReadWrite || m.QueryStatus != 0 {
		t.Errorf("MxAc = %+v", m)
	}
	qf, ok := FindCreateContext(got.Contexts, CreateContextQFid)
	if !ok || len(qf) != 32 {
		t.Fatalf("QFid 载荷长度 = %d, want 32", len(qf))
	}
}

func TestCreateResponseNoContextPadding(t *testing.T) {
	// 无 context 时必须补 1 字节占位（StructureSize 89 = 88 + 1）。
	r := &CreateResponse{CreateAction: FileCreated}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(msg) - HeaderSize; n != 89 {
		t.Errorf("体长度 = %d, want 89", n)
	}
	if _, err := ParseCreateResponse(msg); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestFileIDCompound(t *testing.T) {
	if !CompoundFileID.IsCompound() {
		t.Error("全 0xFF 应判定为复合占位 FileId")
	}
	if (FileID{Persistent: 1, Volatile: 2}).IsCompound() {
		t.Error("普通 FileId 不应判定为复合占位")
	}
	var buf [16]byte
	CompoundFileID.put(buf[:])
	for i, v := range buf {
		if v != 0xFF {
			t.Fatalf("buf[%d] = %#x, want 0xFF", i, v)
		}
	}
	if parseFileID(buf[:]) != CompoundFileID {
		t.Error("FileId 编解码不一致")
	}
}

func TestCreateParseTruncated(t *testing.T) {
	r := &CreateRequest{Name: "x", Contexts: []CreateContext{{Name: "MxAc"}}}
	msg, err := r.Append(dummyHeader(CommandCreate))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(msg); n++ {
		if _, err := ParseCreateRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 字节应报错", n)
		}
	}
}
