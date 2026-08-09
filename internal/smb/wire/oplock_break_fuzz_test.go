package wire

import (
	"bytes"
	"testing"
)

// 本文件补 lock_notify_test.go 遗漏的一块：**lease 族两个解析器的截断防御**。
//
// 现有的 TestOplockBreakGolden 对 ParseOplockBreak 做了逐字节截断扫描，
// 但 ParseLeaseBreakNotification / ParseLeaseBreakAck / PeekOplockBreakKind
// 一个都没有。这三个函数的入参**完全来自网络**（OPLOCK_BREAK 是客户端可以
// 主动发的命令，命令码 0x0012 无需任何前置状态），AGENTS.md §8 要求
// 「先校验长度再切片，不允许 panic 打崩服务」——没有测试就等于没有这条防线。

// TestLeaseBreakParsersRejectTruncation 对 lease 族的两个解析器做逐字节截断扫描。
//
// 判据有两条，缺一不可：
//   - 不 panic（t.Fatal 之前先靠 defer 兜住，panic 会让整个测试进程挂掉，
//     所以这里用子测试隔离）；
//   - 短于固定长度时必须**报错**，不能返回一个字段被静默补零的结构体。
func TestLeaseBreakParsersRejectTruncation(t *testing.T) {
	h := Header{Command: CommandOplockBreak, MessageID: ^uint64(0)}

	notify := (&LeaseBreakNotification{
		NewEpoch:          1,
		Flags:             LeaseBreakAckRequired,
		LeaseKey:          leaseKeyA,
		CurrentLeaseState: LeaseReadCaching | LeaseHandleCaching | LeaseWriteCaching,
		NewLeaseState:     LeaseReadCaching,
	}).Append(h.Append(nil))

	ack := (&LeaseBreakAck{
		LeaseKey:   leaseKeyA,
		LeaseState: LeaseReadCaching,
	}).Append(h.Append(nil))

	cases := []struct {
		name  string
		full  []byte
		parse func([]byte) error
	}{
		{"LeaseBreakNotification", notify, func(b []byte) error {
			_, err := ParseLeaseBreakNotification(b)
			return err
		}},
		{"LeaseBreakAck", ack, func(b []byte) error {
			_, err := ParseLeaseBreakAck(b)
			return err
		}},
		{"OplockBreak", (&OplockBreak{OplockLevel: OplockLevelII}).Append(h.Append(nil)),
			func(b []byte) error {
				_, err := ParseOplockBreak(b)
				return err
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for n := 0; n < len(tc.full); n++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("截断到 %d 字节时 panic：%v", n, r)
						}
					}()
					if err := tc.parse(tc.full[:n]); err == nil {
						t.Fatalf("截断到 %d 字节应报错", n)
					}
				}()
			}
			// 完整长度必须成功，否则上面的扫描是"恒报错"的假阳性。
			if err := tc.parse(tc.full); err != nil {
				t.Fatalf("完整报文应解析成功：%v", err)
			}
		})
	}
}

// TestPeekOplockBreakKindShortInput 覆盖判别函数在畸形/超短输入下的行为。
//
// PeekOplockBreakKind 是收到 OPLOCK_BREAK 后**第一个**被调用的函数，
// 它读的是 b[HeaderSize:HeaderSize+2]。客户端完全可以发一个只有头、
// 甚至连头都不完整的帧过来。
func TestPeekOplockBreakKindShortInput(t *testing.T) {
	h := Header{Command: CommandOplockBreak}
	hdr := h.Append(nil)

	// 长度不足以读出 StructureSize 的一律 Unknown，且不能 panic。
	for n := 0; n < HeaderSize+2; n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%d 字节输入时 panic：%v", n, r)
				}
			}()
			buf := make([]byte, n)
			copy(buf, hdr)
			if k := PeekOplockBreakKind(buf); k != OplockBreakKindUnknown {
				t.Errorf("%d 字节输入应判 Unknown，实际 %v", n, k)
			}
		}()
	}
	if PeekOplockBreakKind(nil) != OplockBreakKindUnknown {
		t.Error("nil 输入应判 Unknown")
	}

	// 恰好 HeaderSize+2 字节就足以判别 —— 判别只看 StructureSize，
	// 不要求报文体完整（真正的长度校验在各自的 Parse 里）。
	for _, tc := range []struct {
		size uint16
		want OplockBreakKind
	}{
		{24, OplockBreakKindOplock},
		{36, OplockBreakKindLease},
		{44, OplockBreakKindUnknown}, // 44 是**服务端发出**的通知，客户端不会发
		{0, OplockBreakKindUnknown},
		{0xFFFF, OplockBreakKindUnknown},
	} {
		buf := make([]byte, HeaderSize+2)
		copy(buf, hdr)
		le.PutUint16(buf[HeaderSize:], tc.size)
		if k := PeekOplockBreakKind(buf); k != tc.want {
			t.Errorf("StructureSize=%d 判为 %v，期望 %v", tc.size, k, tc.want)
		}
	}
}

// TestOplockBreakAppendIsCleanOnDirtyBuffer 防一类很隐蔽的 bug：
// Append 依赖 grow() 把新增区间清零来实现「Reserved 字段恒为 0」。
// 如果哪天 grow 改成复用未清零的底层数组，Reserved/BreakReason/
// AccessMaskHint/ShareMaskHint 就会漏出内存里的残留字节。
//
// 这不只是洁癖：LeaseBreakNotification 的 BreakReason / AccessMaskHint /
// ShareMaskHint 三个字段规范要求必须为 0，填了非 0 值的话 Windows 客户端
// 会按"服务端给了提示"来解释，行为不可预测。
func TestOplockBreakAppendIsCleanOnDirtyBuffer(t *testing.T) {
	// 造一个带脏数据、且容量远大于长度的缓冲区。
	dirty := make([]byte, 0, 512)
	for i := 0; i < 512; i++ {
		dirty = append(dirty, 0xA5)
	}
	dirty = dirty[:0]

	notify := &LeaseBreakNotification{
		LeaseKey:          leaseKeyA,
		CurrentLeaseState: LeaseReadCaching,
		NewLeaseState:     LeaseNone,
	}
	out := notify.Append(dirty)
	if len(out) != leaseBreakNotifyFixed {
		t.Fatalf("通知体长度 %d，期望 %d", len(out), leaseBreakNotifyFixed)
	}
	assertZero(t, out[32:44], "BreakReason/AccessMaskHint/ShareMaskHint")

	ackOut := (&LeaseBreakAck{LeaseKey: leaseKeyA}).Append(dirty)
	if len(ackOut) != leaseBreakAckFixed {
		t.Fatalf("确认体长度 %d，期望 %d", len(ackOut), leaseBreakAckFixed)
	}
	assertZero(t, ackOut[2:4], "Reserved")
	assertZero(t, ackOut[28:36], "LeaseDuration")

	obOut := (&OplockBreak{OplockLevel: OplockLevelBatch}).Append(dirty)
	if len(obOut) != oplockBreakFixed {
		t.Fatalf("oplock 体长度 %d，期望 %d", len(obOut), oplockBreakFixed)
	}
	assertZero(t, obOut[3:8], "Reserved/Reserved2")
}

// TestOplockLevelValues 锁死 OplockLevel 的四个规范取值。
//
// 这些值不是连续的（NONE=0, II=1, EXCLUSIVE=8, BATCH=9, LEASE=0xFF），
// 极容易被"顺手改成枚举"的重构写成 0/1/2/3，而那样编译照过、
// 单测（如果只测 round-trip）也照过，只有真实客户端会莫名其妙。
func TestOplockLevelValues(t *testing.T) {
	cases := []struct {
		l    OplockLevel
		want uint8
		name string
	}{
		{OplockLevelNone, 0x00, "SMB2_OPLOCK_LEVEL_NONE"},
		{OplockLevelII, 0x01, "SMB2_OPLOCK_LEVEL_II"},
		{OplockLevelExclusive, 0x08, "SMB2_OPLOCK_LEVEL_EXCLUSIVE"},
		{OplockLevelBatch, 0x09, "SMB2_OPLOCK_LEVEL_BATCH"},
		{OplockLevelLease, 0xFF, "SMB2_OPLOCK_LEVEL_LEASE"},
	}
	for _, tc := range cases {
		if uint8(tc.l) != tc.want {
			t.Errorf("%s = %#x, 规范值 %#x", tc.name, uint8(tc.l), tc.want)
		}
	}

	// OplockLevel 在 OPLOCK_BREAK 体里占 1 字节（偏移 2），
	// 0xFF 不能被截断成别的值。
	msg := (&OplockBreak{OplockLevel: OplockLevelLease}).
		Append(Header{Command: CommandOplockBreak}.Append(nil))
	if got := msg[HeaderSize+2]; got != 0xFF {
		t.Errorf("编码后的 OplockLevel = %#x, 期望 0xFF", got)
	}
	back, err := ParseOplockBreak(msg)
	if err != nil {
		t.Fatalf("ParseOplockBreak: %v", err)
	}
	if back.OplockLevel != OplockLevelLease {
		t.Errorf("round-trip 后 OplockLevel = %#x", uint8(back.OplockLevel))
	}
}

// TestOplockBreakFileIDByteOrder 单独钉死 FileId 的字节序。
//
// FileId 是 Persistent(8) + Volatile(8)，各自**小端**。写反了不会有任何
// 编译期或 round-trip 期症状（编码解码用的是同一套代码，错得一致就看不出来），
// 只有和真实客户端对接时才会表现为"服务端认不出这个句柄"。
// 所以必须拿一个手写的字节向量来比。
func TestOplockBreakFileIDByteOrder(t *testing.T) {
	ob := &OplockBreak{
		OplockLevel: OplockLevelNone,
		FileID:      FileID{Persistent: 0x0102030405060708, Volatile: 0x1112131415161718},
	}
	body := ob.Append(nil)
	wantPersistent := []byte{0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01}
	wantVolatile := []byte{0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11}
	if !bytes.Equal(body[8:16], wantPersistent) {
		t.Errorf("Persistent 字节序不对\n got=% X\nwant=% X", body[8:16], wantPersistent)
	}
	if !bytes.Equal(body[16:24], wantVolatile) {
		t.Errorf("Volatile 字节序不对\n got=% X\nwant=% X", body[16:24], wantVolatile)
	}
}
