package wire

import (
	"bytes"
	"testing"
)

func TestCloseRoundTrip(t *testing.T) {
	req := &CloseRequest{Flags: CloseFlagPostQueryAttrib, FileID: FileID{Persistent: 3, Volatile: 4}}
	msg := req.Append(dummyHeader(CommandClose))
	if n := len(msg) - HeaderSize; n != 24 {
		t.Fatalf("请求体长度 = %d, want 24", n)
	}
	gotReq, err := ParseCloseRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *gotReq != *req || !gotReq.PostQueryAttrib() {
		t.Errorf("请求 round-trip 失败: %+v", gotReq)
	}

	resp := &CloseResponse{
		Flags:          CloseFlagPostQueryAttrib,
		CreationTime:   1,
		LastAccessTime: 2,
		LastWriteTime:  3,
		ChangeTime:     4,
		AllocationSize: 8192,
		EndOfFile:      5000,
		FileAttributes: FileAttributeNormal,
	}
	msg = resp.Append(dummyHeader(CommandClose))
	if n := len(msg) - HeaderSize; n != 60 {
		t.Fatalf("响应体长度 = %d, want 60", n)
	}
	gotResp, err := ParseCloseResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *gotResp != *resp {
		t.Errorf("响应 round-trip 失败: %+v", gotResp)
	}
}

func TestFlushRoundTrip(t *testing.T) {
	req := &FlushRequest{FileID: FileID{Persistent: 9, Volatile: 10}}
	msg := req.Append(dummyHeader(CommandFlush))
	if n := len(msg) - HeaderSize; n != 24 {
		t.Fatalf("请求体长度 = %d, want 24", n)
	}
	got, err := ParseFlushRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FileID != req.FileID {
		t.Errorf("FileID = %v", got.FileID)
	}
	msg = (&FlushResponse{}).Append(dummyHeader(CommandFlush))
	if _, err := ParseFlushResponse(msg); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
}

func TestReadRequestRoundTrip(t *testing.T) {
	r := &ReadRequest{
		Length:       65536,
		Offset:       1 << 32,
		FileID:       FileID{Persistent: 1, Volatile: 2},
		MinimumCount: 1,
	}
	msg, err := r.Append(dummyHeader(CommandRead))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if n := len(msg) - HeaderSize; n != 49 {
		t.Fatalf("请求体长度 = %d, want 49（48 固定 + 1 占位）", n)
	}
	if le.Uint16(msg[HeaderSize:]) != 49 {
		t.Errorf("StructureSize = %d, want 49", le.Uint16(msg[HeaderSize:]))
	}
	got, err := ParseReadRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Length != r.Length || got.Offset != r.Offset || got.FileID != r.FileID ||
		got.MinimumCount != r.MinimumCount {
		t.Errorf("round-trip 失败: %+v", got)
	}
	// 固定部分不完整必须报错；最后 1 字节可变部分占位缺失时保持宽容
	// （真实客户端都会带，但缺了也不影响解析出全部字段）。
	for n := 0; n < HeaderSize+48; n++ {
		if _, err := ParseReadRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

func TestReadResponseRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("stupidSamba"), 100)
	r := &ReadResponse{Data: data}
	msg, err := r.Append(dummyHeader(CommandRead))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 17 {
		t.Errorf("StructureSize = %d, want 17", le.Uint16(body))
	}
	// DataOffset 是 1 字节字段，必须是 0x50（64 + 16）。
	if body[2] != 0x50 {
		t.Errorf("DataOffset = %#x, want 0x50", body[2])
	}
	if int(le.Uint32(body[4:])) != len(data) {
		t.Errorf("DataLength = %d, want %d", le.Uint32(body[4:]), len(data))
	}
	got, err := ParseReadResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(got.Data, data) {
		t.Error("数据不一致")
	}

	// 零长度读：仍需 1 字节占位。
	empty, err := (&ReadResponse{}).Append(dummyHeader(CommandRead))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(empty) - HeaderSize; n != 17 {
		t.Errorf("空 READ 响应体长度 = %d, want 17", n)
	}
}

func TestWriteRoundTrip(t *testing.T) {
	data := []byte("hello smb2 write path")
	r := &WriteRequest{
		Offset: 4096,
		FileID: FileID{Persistent: 5, Volatile: 6},
		Flags:  WriteFlagWriteThrough,
		Data:   data,
	}
	msg, err := r.Append(dummyHeader(CommandWrite))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 49 {
		t.Errorf("StructureSize = %d, want 49", le.Uint16(body))
	}
	// DataOffset 相对头起点 = 64 + 48 = 112。
	if got := le.Uint16(body[2:]); got != 112 {
		t.Errorf("DataOffset = %d, want 112", got)
	}
	got, err := ParseWriteRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Offset != r.Offset || got.FileID != r.FileID || got.Flags != r.Flags ||
		!bytes.Equal(got.Data, data) {
		t.Errorf("round-trip 失败: %+v", got)
	}

	// DataLength 撒谎必须报错。
	bad := bytes.Clone(msg)
	le.PutUint32(bad[HeaderSize+4:], 0xFFFFFFF0)
	if _, err := ParseWriteRequest(bad); err == nil {
		t.Error("越界 Length 应报错")
	}

	resp := &WriteResponse{Count: uint32(len(data))}
	rmsg := resp.Append(dummyHeader(CommandWrite))
	if n := len(rmsg) - HeaderSize; n != 17 {
		t.Fatalf("响应体长度 = %d, want 17（16 固定 + 1 占位）", n)
	}
	gotResp, err := ParseWriteResponse(rmsg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gotResp.Count != resp.Count {
		t.Errorf("Count = %d, want %d", gotResp.Count, resp.Count)
	}
}
