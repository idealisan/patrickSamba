package wire

import (
	"bytes"
	"testing"
)

// TestLockRequestGolden 用固定字节向量锁死 MS-SMB2 §2.2.26 的布局。
//
// 最容易写错的两点：
//  1. StructureSize 恒为 48（"含单个 SMB2_LOCK_ELEMENT 的大小"），
//     **不随 LockCount 变化**；
//  2. LockSequenceNumber 是**低 4 位**，LockSequenceIndex 是高 28 位
//     （§2.2.26 原文 "The 4 least significant bits of this field"，
//     smbj 的 SMB2LockRequest 也是 (index << 4) + number）。
func TestLockRequestGolden(t *testing.T) {
	body := []byte{
		0x30, 0x00, // StructureSize = 48
		0x02, 0x00, // LockCount = 2
		0x35, 0x00, 0x00, 0x00, // LockSequence: number=5, index=3 → 3<<4|5 = 0x35
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // FileId.Persistent
		0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, // FileId.Volatile
		// SMB2_LOCK_ELEMENT[0]：排他锁 [0, 4096)，冲突立即失败
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Offset = 0
		0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Length = 4096
		0x12, 0x00, 0x00, 0x00, // Flags = EXCLUSIVE | FAIL_IMMEDIATELY
		0x00, 0x00, 0x00, 0x00, // Reserved
		// SMB2_LOCK_ELEMENT[1]：解锁 [4 GiB, +512)
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // Offset = 1<<32
		0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Length = 512
		0x04, 0x00, 0x00, 0x00, // Flags = UNLOCK
		0x00, 0x00, 0x00, 0x00, // Reserved
	}
	if len(body) != lockRequestFixed+2*LockElementSize {
		t.Fatalf("字节向量长度 = %d", len(body))
	}

	want := &LockRequest{
		FileID:       FileID{Persistent: 0x1122334455667788, Volatile: 0x99AABBCCDDEEFF00},
		LockSequence: MakeLockSequence(5, 3),
		Locks: []LockElement{
			{Offset: 0, Length: 4096, Flags: LockFlagExclusiveLock | LockFlagFailImmediately},
			{Offset: 1 << 32, Length: 512, Flags: LockFlagUnlock},
		},
	}

	h := Header{Command: CommandLock, MessageID: 7, TreeID: 1, SessionID: 2}
	msg, err := want.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !bytes.Equal(msg[HeaderSize:], body) {
		t.Errorf("编码不符\n got=% X\nwant=% X", msg[HeaderSize:], body)
	}

	got, err := ParseLockRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FileID != want.FileID || got.LockSequence != want.LockSequence || len(got.Locks) != 2 {
		t.Fatalf("固定字段不一致: %+v", got)
	}
	for i := range want.Locks {
		if got.Locks[i] != want.Locks[i] {
			t.Errorf("Locks[%d] = %+v, 期望 %+v", i, got.Locks[i], want.Locks[i])
		}
	}
	if got.LockSequenceNumber() != 5 {
		t.Errorf("LockSequenceNumber = %d, 期望 5（低 4 位）", got.LockSequenceNumber())
	}
	if got.LockSequenceIndex() != 3 {
		t.Errorf("LockSequenceIndex = %d, 期望 3（高 28 位）", got.LockSequenceIndex())
	}
	if !got.Locks[0].Flags.IsExclusive() || !got.Locks[0].Flags.FailImmediately() ||
		got.Locks[0].Flags.IsUnlock() {
		t.Error("Locks[0] 标志判定错误")
	}
	if !got.Locks[1].Flags.IsUnlock() {
		t.Error("Locks[1] 应为解锁")
	}

	// LockSequence 的两个分量必须能无损往返（15 是 4 位上限，
	// 64 是 §2.2.26 允许的 index 上限）。
	if n, i := uint8(15), uint32(64); MakeLockSequence(n, i) != 64<<4|15 {
		t.Errorf("MakeLockSequence(%d,%d) = %#x", n, i, MakeLockSequence(n, i))
	}
}

func TestLockRequestErrors(t *testing.T) {
	h := Header{Command: CommandLock}
	req := &LockRequest{Locks: []LockElement{{Length: 1, Flags: LockFlagSharedLock}}}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// 任意截断都不能 panic，必须报错。
	for n := 0; n < len(msg); n++ {
		if _, err := ParseLockRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
	// LockCount = 0 违反 §2.2.26。
	zero := bytes.Clone(msg)
	le.PutUint16(zero[HeaderSize+2:], 0)
	if _, err := ParseLockRequest(zero); err == nil {
		t.Error("LockCount=0 应报错")
	}
	// LockCount 超过实际字节数不能越界读。
	big := bytes.Clone(msg)
	le.PutUint16(big[HeaderSize+2:], 1000)
	if _, err := ParseLockRequest(big); err == nil {
		t.Error("LockCount 越界应报错")
	}
	// 编码侧也要拒绝空数组。
	if _, err := (&LockRequest{}).Append(nil); err == nil {
		t.Error("空 Locks 编码应报错")
	}
	// StructureSize 必须恒为 48，哪怕只有 1 个元素。
	if got := le.Uint16(msg[HeaderSize:]); got != 48 {
		t.Errorf("StructureSize = %d, 期望 48", got)
	}
}

func TestLockResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandLock, Flags: FlagServerToRedir}
	msg := (&LockResponse{}).Append(h.Append(nil))
	if len(msg) != HeaderSize+4 {
		t.Fatalf("长度 = %d, 期望 %d", len(msg), HeaderSize+4)
	}
	if got := le.Uint16(msg[HeaderSize:]); got != 4 {
		t.Errorf("StructureSize = %d, 期望 4", got)
	}
	if _, err := ParseLockResponse(msg); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// TestChangeNotifyRequestGolden 锁死 MS-SMB2 §2.2.35 的布局。
func TestChangeNotifyRequestGolden(t *testing.T) {
	body := []byte{
		0x20, 0x00, // StructureSize = 32
		0x01, 0x00, // Flags = SMB2_WATCH_TREE
		0x00, 0x00, 0x01, 0x00, // OutputBufferLength = 65536
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // FileId.Persistent
		0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, // FileId.Volatile
		0x17, 0x00, 0x00, 0x00, // CompletionFilter = NAME|DIR_NAME|ATTR|LAST_WRITE
		0x00, 0x00, 0x00, 0x00, // Reserved
	}
	want := &ChangeNotifyRequest{
		Flags:              WatchTree,
		OutputBufferLength: 65536,
		FileID:             FileID{Persistent: 0x1122334455667788, Volatile: 0x99AABBCCDDEEFF00},
		CompletionFilter: NotifyChangeFileName | NotifyChangeDirName |
			NotifyChangeAttributes | NotifyChangeLastWrite,
	}

	h := Header{Command: CommandChangeNotify, MessageID: 9, TreeID: 1, SessionID: 2}
	msg := want.Append(h.Append(nil))
	if !bytes.Equal(msg[HeaderSize:], body) {
		t.Errorf("编码不符\n got=% X\nwant=% X", msg[HeaderSize:], body)
	}
	// StructureSize(32) == 固定部分长度，没有可变部分，不补占位字节。
	if len(msg) != HeaderSize+32 {
		t.Errorf("长度 = %d, 期望 %d", len(msg), HeaderSize+32)
	}

	got, err := ParseChangeNotifyRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *got != *want {
		t.Errorf("round-trip 不一致\n got=%+v\nwant=%+v", got, want)
	}
	if !got.WatchTree() {
		t.Error("WatchTree 应为 true")
	}
	if want.CompletionFilter&^CompletionFilterAll != 0 {
		t.Error("CompletionFilterAll 未覆盖用到的位")
	}
	for n := 0; n < len(msg); n++ {
		if _, err := ParseChangeNotifyRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

// TestNotifyEntriesGolden 锁死 MS-FSCC §2.7.1 FILE_NOTIFY_INFORMATION 的
// 布局与 **4 字节对齐**（注意不是目录枚举的 8 字节）。
func TestNotifyEntriesGolden(t *testing.T) {
	// "a.txt" 是 5 个字符 = 10 字节，条目总长 12+10 = 22，
	// 下一条必须落在 align4(22) = 24 处。
	golden := []byte{
		0x18, 0x00, 0x00, 0x00, // NextEntryOffset = 24
		0x01, 0x00, 0x00, 0x00, // Action = FILE_ACTION_ADDED
		0x0A, 0x00, 0x00, 0x00, // FileNameLength = 10 字节
		'a', 0x00, '.', 0x00, 't', 0x00, 'x', 0x00, 't', 0x00,
		0x00, 0x00, // 4 字节对齐填充
		0x00, 0x00, 0x00, 0x00, // NextEntryOffset = 0（最后一条）
		0x03, 0x00, 0x00, 0x00, // Action = FILE_ACTION_MODIFIED
		0x04, 0x00, 0x00, 0x00, // FileNameLength = 4 字节
		'b', 0x00, 'c', 0x00,
	}
	entries := []NotifyEntry{
		{Action: FileActionAdded, Name: "a.txt"},
		{Action: FileActionModified, Name: "bc"},
	}

	w := NewNotifyWriter(1 << 16)
	for i, e := range entries {
		ok, err := w.Add(e)
		if err != nil || !ok {
			t.Fatalf("Add[%d] = %v, %v", i, ok, err)
		}
	}
	if !bytes.Equal(w.Bytes(), golden) {
		t.Errorf("编码不符\n got=% X\nwant=% X", w.Bytes(), golden)
	}
	if w.Count() != 2 {
		t.Errorf("Count = %d", w.Count())
	}

	got, err := ParseNotifyEntries(golden)
	if err != nil {
		t.Fatalf("ParseNotifyEntries: %v", err)
	}
	if len(got) != 2 || got[0] != entries[0] || got[1] != entries[1] {
		t.Errorf("解析 = %+v", got)
	}

	// 空输入是合法的（STATUS_NOTIFY_ENUM_DIR 时就不带任何条目）。
	if e, err := ParseNotifyEntries(nil); err != nil || e != nil {
		t.Errorf("空输入 = %v, %v", e, err)
	}
	// 截断的链必须报错而不是 panic。
	for n := 1; n < len(golden); n++ {
		if _, err := ParseNotifyEntries(golden[:n]); err == nil && n < 22 {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
	// NextEntryOffset 指向缓冲区外必须报错。
	bad := bytes.Clone(golden)
	le.PutUint32(bad[0:], 0xFFFFFF00)
	if _, err := ParseNotifyEntries(bad); err == nil {
		t.Error("越界 NextEntryOffset 应报错")
	}
}

func TestNotifyWriterRespectsMax(t *testing.T) {
	// 单条 "a.txt" 需要 22 字节，给 21 应该放不下。
	w := NewNotifyWriter(21)
	ok, err := w.Add(NotifyEntry{Action: FileActionAdded, Name: "a.txt"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ok {
		t.Error("21 字节应放不下 22 字节的条目")
	}
	if w.Len() != 0 || w.Count() != 0 {
		t.Error("放不下时缓冲区必须保持不变")
	}

	// 给 22 刚好放得下一条，第二条放不下且不破坏第一条。
	w = NewNotifyWriter(22)
	if ok, _ := w.Add(NotifyEntry{Action: FileActionAdded, Name: "a.txt"}); !ok {
		t.Fatal("22 字节应放得下")
	}
	if ok, _ := w.Add(NotifyEntry{Action: FileActionModified, Name: "b"}); ok {
		t.Error("第二条应放不下")
	}
	if w.Len() != 22 || w.Count() != 1 {
		t.Errorf("Len=%d Count=%d, 期望 22/1", w.Len(), w.Count())
	}
	// 最后一条的 NextEntryOffset 必须是 0。
	if le.Uint32(w.Bytes()) != 0 {
		t.Error("单条时 NextEntryOffset 应为 0")
	}
}

func TestChangeNotifyResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandChangeNotify, Flags: FlagServerToRedir}
	w := NewNotifyWriter(1 << 16)
	if _, err := w.Add(NotifyEntry{Action: FileActionRemoved, Name: "gone"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	resp := &ChangeNotifyResponse{Buffer: w.Bytes()}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := le.Uint16(msg[HeaderSize+2:]); got != HeaderSize+changeNotifyResponseFixed {
		t.Errorf("OutputBufferOffset = %d, 期望 %d", got, HeaderSize+changeNotifyResponseFixed)
	}
	got, err := ParseChangeNotifyResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(got.Buffer, resp.Buffer) {
		t.Errorf("Buffer = % X", got.Buffer)
	}

	// 空缓冲（STATUS_NOTIFY_ENUM_DIR 的情形）：补 1 字节占位。
	empty, err := (&ChangeNotifyResponse{}).Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append 空: %v", err)
	}
	if len(empty) != HeaderSize+changeNotifyResponseFixed+1 {
		t.Errorf("空响应长度 = %d, 期望 %d", len(empty), HeaderSize+changeNotifyResponseFixed+1)
	}
	if e, err := ParseChangeNotifyResponse(empty); err != nil || len(e.Buffer) != 0 {
		t.Errorf("空响应解析 = %v, %v", e, err)
	}
}

// TestOplockBreakGolden 锁死 MS-SMB2 §2.2.23.1 / §2.2.24.1 / §2.2.25.1
// 三种 oplock 族报文（线上布局完全一致，24 字节）。
func TestOplockBreakGolden(t *testing.T) {
	body := []byte{
		0x18, 0x00, // StructureSize = 24
		0x01,                   // OplockLevel = SMB2_OPLOCK_LEVEL_II
		0x00,                   // Reserved
		0x00, 0x00, 0x00, 0x00, // Reserved2
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // FileId.Persistent
		0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, // FileId.Volatile
	}
	want := &OplockBreak{
		OplockLevel: OplockLevelII,
		FileID:      FileID{Persistent: 0x1122334455667788, Volatile: 0x99AABBCCDDEEFF00},
	}

	h := Header{Command: CommandOplockBreak, MessageID: ^uint64(0)}
	msg := want.Append(h.Append(nil))
	if !bytes.Equal(msg[HeaderSize:], body) {
		t.Errorf("编码不符\n got=% X\nwant=% X", msg[HeaderSize:], body)
	}
	got, err := ParseOplockBreak(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *got != *want {
		t.Errorf("round-trip 不一致: %+v", got)
	}
	if PeekOplockBreakKind(msg) != OplockBreakKindOplock {
		t.Error("应判别为 oplock 族")
	}
	for n := 0; n < len(msg); n++ {
		if _, err := ParseOplockBreak(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

// TestLeaseBreakGolden 锁死 §2.2.23.2（44 字节通知）与
// §2.2.24.2 / §2.2.25.2（36 字节确认/响应）两种 lease 族布局。
func TestLeaseBreakGolden(t *testing.T) {
	key := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	notifyBody := []byte{
		0x2C, 0x00, // StructureSize = 44
		0x02, 0x00, // NewEpoch = 2
		0x01, 0x00, 0x00, 0x00, // Flags = ACK_REQUIRED
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, // LeaseKey
		0x07, 0x00, 0x00, 0x00, // CurrentLeaseState = R|H|W
		0x01, 0x00, 0x00, 0x00, // NewLeaseState = R
		0x00, 0x00, 0x00, 0x00, // BreakReason，必须 0
		0x00, 0x00, 0x00, 0x00, // AccessMaskHint，必须 0
		0x00, 0x00, 0x00, 0x00, // ShareMaskHint，必须 0
	}
	notify := &LeaseBreakNotification{
		NewEpoch:          2,
		Flags:             LeaseBreakAckRequired,
		LeaseKey:          key,
		CurrentLeaseState: LeaseReadCaching | LeaseHandleCaching | LeaseWriteCaching,
		NewLeaseState:     LeaseReadCaching,
	}
	h := Header{Command: CommandOplockBreak, MessageID: ^uint64(0)}
	msg := notify.Append(h.Append(nil))
	if !bytes.Equal(msg[HeaderSize:], notifyBody) {
		t.Errorf("通知编码不符\n got=% X\nwant=% X", msg[HeaderSize:], notifyBody)
	}
	gotN, err := ParseLeaseBreakNotification(msg)
	if err != nil {
		t.Fatalf("ParseLeaseBreakNotification: %v", err)
	}
	if *gotN != *notify {
		t.Errorf("通知 round-trip 不一致: %+v", gotN)
	}

	ackBody := []byte{
		0x24, 0x00, // StructureSize = 36
		0x00, 0x00, // Reserved
		0x00, 0x00, 0x00, 0x00, // Flags，必须 0
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, // LeaseKey
		0x01, 0x00, 0x00, 0x00, // LeaseState = R
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // LeaseDuration，必须 0
	}
	ack := &LeaseBreakAck{LeaseKey: key, LeaseState: LeaseReadCaching}
	amsg := ack.Append(Header{Command: CommandOplockBreak}.Append(nil))
	if !bytes.Equal(amsg[HeaderSize:], ackBody) {
		t.Errorf("确认编码不符\n got=% X\nwant=% X", amsg[HeaderSize:], ackBody)
	}
	gotA, err := ParseLeaseBreakAck(amsg)
	if err != nil {
		t.Fatalf("ParseLeaseBreakAck: %v", err)
	}
	if *gotA != *ack {
		t.Errorf("确认 round-trip 不一致: %+v", gotA)
	}

	// 两族必须能靠 StructureSize 判别，不能混淆。
	if PeekOplockBreakKind(amsg) != OplockBreakKindLease {
		t.Error("36 字节应判别为 lease 族")
	}
	if _, err := ParseOplockBreak(amsg); err == nil {
		t.Error("把 lease 族当 oplock 族解析应报错")
	}
	bad := bytes.Clone(amsg)
	le.PutUint16(bad[HeaderSize:], 99)
	if PeekOplockBreakKind(bad) != OplockBreakKindUnknown {
		t.Error("未知 StructureSize 应判别为 unknown")
	}
	if PeekOplockBreakKind(nil) != OplockBreakKindUnknown {
		t.Error("空输入应判别为 unknown，且不能 panic")
	}
	for n := 0; n < len(amsg); n++ {
		if _, err := ParseLeaseBreakAck(amsg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}
