package wire

import "testing"

// FLUSH 的两个保留字段要能被解析出来，但**不参与任何语义判定**：
// 客户端往里填什么都必须照常处理（保留字段，服务端不使用）。
//
// 反向对照：哪天有人拿 Reserved1 当「F_FULLFSYNC 开关」去分支刷盘强度，
// 这条用例不会红 —— 真正拦住「降级成普通 fsync」的是 command 层的
// TestFlushReallyCallsFullSync。这里钉的是「解析不丢字段、且不因非零值报错」。
func TestFlushRequestReservedFields(t *testing.T) {
	r := &FlushRequest{FileID: FileID{Persistent: 0x11, Volatile: 0x22}}
	msg := r.Append(dummyHeader(CommandFlush))

	// Append 必须把保留字段写成 0（客户端义务）。
	body := msg[HeaderSize:]
	if got := le.Uint16(body[2:]); got != 0 {
		t.Errorf("Reserved1 = %d, want 0", got)
	}
	if got := le.Uint32(body[4:]); got != 0 {
		t.Errorf("Reserved2 = %d, want 0", got)
	}

	// 解析回来也要是 0，且 FileID 不跑偏。
	got, err := ParseFlushRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Reserved1 != 0 || got.Reserved2 != 0 {
		t.Errorf("保留字段应为 0，得到 Reserved1=%d Reserved2=%d", got.Reserved1, got.Reserved2)
	}
	if got.FileID != r.FileID {
		t.Errorf("FileID = %+v, want %+v", got.FileID, r.FileID)
	}

	// 对端填了垃圾值：照样解析成功，不报错、不拒绝。
	msg2 := r.Append(dummyHeader(CommandFlush))
	body2 := msg2[HeaderSize:]
	le.PutUint16(body2[2:], 0xBEEF)
	le.PutUint32(body2[4:], 0xDEADBEEF)
	got2, err := ParseFlushRequest(msg2)
	if err != nil {
		t.Fatalf("保留字段非零时解析失败: %v", err)
	}
	if got2.Reserved1 != 0xBEEF || got2.Reserved2 != 0xDEADBEEF {
		t.Errorf("保留字段没被解析出来: %+v", got2)
	}
}
