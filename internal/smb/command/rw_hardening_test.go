package command

import (
	"encoding/binary"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件钉的是 bh4-A#11/A#12/A#13 三条 INFO 级读写边界：
//
//	A#11 WRITE 的 DataOffset 必须精确等于 SMB2 头 + 固定体长（64+48=112），
//	    指到别处回 INVALID_PARAMETER（Samba smb2_write.c:74-76）。
//	    实测抓包 internal/smb/wire/testdata/capture/create-read-write/
//	    017-c2s-WRITE.bin：DataOffset=112、Length=39、StructureSize=49。
//	A#12 CreditCharge 必须覆盖载荷（ceil(len/64K)，MS-SMB2 §3.3.5.2.6；
//	    Samba smbd_smb2_request_verify_creditcharge）。
//	A#13 WRITE_UNBUFFERED 在 ≥3.0.2 上并入 write-through/Sync 语义
//	    （Samba smb2_write.c:289-293）。

// patchDataOffset 把已编码 WRITE 消息里的 DataOffset 字段改写成 v
// （MS-SMB2 §2.2.21：DataOffset 在体内偏移 2，即消息绝对偏移 66，小端）。
func patchDataOffset(msg []byte, v uint16) {
	binary.LittleEndian.PutUint16(msg[66:68], v)
}

// newHardeningCtx 造一个方言可调的读写测试环境（复用 lockIOPair 的文件布局）。
func newHardeningCtx(t *testing.T, d dialect.Dialect) *lockIOPair {
	t.Helper()
	p := newLockIOPair(t, make([]byte, 4096))
	// lockIOPair.ctx 是占位字段（每次请求要重建），方言设在自己建的
	// Context.Conn 上即可；这里只是提前验证 d 的合法性。
	if !d.SupportsMultiCredit() && d != dialect.SMB202 {
		t.Fatalf("测试误用：未知方言 %v", d)
	}
	return p
}

// TestWriteDataOffsetMustBeExact（bh4-A#11）：Length>0 时 DataOffset 偏离
// 64+48 必须回 STATUS_INVALID_PARAMETER —— 否则数据会被从错误位置切片，
// 写进文件的是移位的垃圾字节。
func TestWriteDataOffsetMustBeExact(t *testing.T) {
	p := newHardeningCtx(t, dialect.SMB311)
	data := []byte("0123456789")

	build := func() ([]byte, *wire.WriteRequest) {
		req := &wire.WriteRequest{Offset: 0, FileID: compoundFID, Data: data}
		msg, err := req.Append(make([]byte, wire.HeaderSize))
		if err != nil {
			t.Fatalf("编码 WRITE Request: %v", err)
		}
		return msg, req
	}

	run := func(msg []byte, req *wire.WriteRequest, open *Open) error {
		ctx := &Context{
			Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
			Chain: &Chain{},
			Tree:  p.tree,
			Msg:   msg,
			Out:   make([]byte, wire.HeaderSize),
		}
		ctx.Chain.LastOpen = open
		return handleWrite(ctx)
	}

	msg, req := build()
	patchDataOffset(msg, 112) // 正确值：64 头 + 48 固定体
	if err := run(msg, req, p.a); err != nil {
		t.Fatalf("DataOffset=112 的正常写不应失败: %v", err)
	}

	for _, off := range []uint16{64, 100, 113, 128} {
		msg, req := build()
		patchDataOffset(msg, off)
		wantStatus(t, "DataOffset 指向错误位置",
			run(msg, req, p.b), status.InvalidParameter)
	}

	// 零长度写不做该校验（字段此时无意义，部分客户端发 0）。
	zreq := &wire.WriteRequest{Offset: 0, FileID: compoundFID}
	zmsg, aerr := zreq.Append(make([]byte, wire.HeaderSize))
	if aerr != nil {
		t.Fatalf("编码: %v", aerr)
	}
	patchDataOffset(zmsg, 0)
	if err := run(zmsg, zreq, p.a); err != nil {
		t.Fatalf("零长度写不应做 DataOffset 校验: %v", err)
	}
}

// TestCreditChargeMustCoverPayload（bh4-A#12）：多信用连接上，载荷超过
// 一个 64K quantum 而头部 CreditCharge 少付时必须回 INVALID_PARAMETER
// （MS-SMB2 §3.3.5.2.6）。READ 与 WRITE 双侧同判。
func TestCreditChargeMustCoverPayload(t *testing.T) {
	p := newHardeningCtx(t, dialect.SMB300)

	runWrite := func(charge uint16, payload int) error {
		req := &wire.WriteRequest{Offset: 0, FileID: compoundFID, Data: make([]byte, payload)}
		msg, err := req.Append(make([]byte, wire.HeaderSize))
		if err != nil {
			t.Fatalf("编码: %v", err)
		}
		ctx := &Context{
			Conn:   &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
			Chain:  &Chain{},
			Tree:   p.tree,
			Header: wire.Header{CreditCharge: charge},
			Msg:    msg,
			Out:    make([]byte, wire.HeaderSize),
		}
		ctx.Conn.Dialect = dialect.SMB300
		ctx.Chain.LastOpen = p.b
		return handleWrite(ctx)
	}

	const q = 65536
	wantStatus(t, "131073B 只付 1 credit", runWrite(1, 2*q+1), status.InvalidParameter)
	wantStatus(t, "131073B 零 credit", runWrite(0, 2*q+1), status.InvalidParameter)
	if err := runWrite(3, 2*q+1); err != nil {
		t.Fatalf("131073B 付足 3 credit 不应失败: %v", err)
	}
	// 子 quantum 策略保守放行：≤64K 的载荷不查 charge（见实现的 TODO 注）。
	if err := runWrite(0, q); err != nil {
		t.Fatalf("64K 及以下载荷不应被拒: %v", err)
	}

	// READ 同判。
	runRead := func(charge uint16, length uint32) error {
		req := &wire.ReadRequest{Offset: 0, Length: length, FileID: compoundFID}
		msg, err := req.Append(make([]byte, wire.HeaderSize))
		if err != nil {
			t.Fatalf("编码: %v", err)
		}
		ctx := &Context{
			Conn:   &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
			Chain:  &Chain{},
			Tree:   p.tree,
			Header: wire.Header{CreditCharge: charge},
			Msg:    msg,
			Out:    make([]byte, wire.HeaderSize),
		}
		ctx.Conn.Dialect = dialect.SMB300
		ctx.Chain.LastOpen = p.a
		return handleRead(ctx)
	}
	wantStatus(t, "READ 131073B 只付 2 credit", runRead(2, 2*q+1), status.InvalidParameter)
	if err := runRead(3, 2*q+1); err != nil {
		t.Fatalf("READ 131073B 付足 3 credit 不应失败: %v", err)
	}
	if err := runRead(0, q); err != nil {
		t.Fatalf("READ 64K 及以下不应被拒: %v", err)
	}

	// 非多信用方言（2.0.2）豁免 —— CreditCharge 字段在 2.0.2 恒为 0。
	p202 := newHardeningCtx(t, dialect.SMB202)
	req := &wire.WriteRequest{Offset: 0, FileID: compoundFID, Data: make([]byte, 2*q+1)}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  p202.tree,
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = p202.b
	if err := handleWrite(ctx); err != nil {
		t.Fatalf("2.0.2 无多信用，charge 校验必须豁免: %v", err)
	}
}

// TestWriteUnbufferedImpliesSync（bh4-A#13）：WRITE_UNBUFFERED 标志在
// ≥3.0.2 的方言上等价于 WRITE_THROUGH（Samba smb2_write.c:289-293：
// 置 write_through=true）；2.1/3.0 上仍被忽略。
func TestWriteUnbufferedImpliesSync(t *testing.T) {
	open := &Open{}

	cases := []struct {
		dialect dialect.Dialect
		flags   uint32
		want    bool
	}{
		{dialect.SMB210, wire.WriteFlagWriteUnbuffer, false},
		{dialect.SMB300, wire.WriteFlagWriteUnbuffer, false},
		{dialect.SMB302, wire.WriteFlagWriteUnbuffer, true},
		{dialect.SMB311, wire.WriteFlagWriteUnbuffer, true},
		{dialect.SMB311, wire.WriteFlagWriteThrough, true},
		{dialect.SMB311, 0, false},
		{dialect.SMB302, wire.WriteFlagWriteUnbuffer | wire.WriteFlagWriteThrough, true},
	}
	for _, tc := range cases {
		got := writeShouldSync(tc.flags, open, tc.dialect)
		if got != tc.want {
			t.Errorf("dialect=%v flags=%#x: writeShouldSync=%v, want %v",
				tc.dialect, tc.flags, got, tc.want)
		}
	}

	// CreateOptions 上的 FILE_WRITE_THROUGH 仍然独立生效。
	open.CreateOptions = wire.FileWriteThrough
	if !writeShouldSync(0, open, dialect.SMB202) {
		t.Error("FILE_WRITE_THROUGH 打开选项在任何方言上都应触发 Sync")
	}
}
