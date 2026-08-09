package wire

import (
	"bytes"
	"testing"
)

func TestIoctlRequestRoundTrip(t *testing.T) {
	h := Header{Command: CommandIoctl, MessageID: 5, TreeID: 1, SessionID: 2}
	in, err := (&ValidateNegotiateInfoRequest{
		Capabilities: CapDFS | CapLargeMTU,
		ClientGUID:   [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SecurityMode: NegotiateSigningEnabled,
		Dialects:     []Dialect{SMB202, SMB210, SMB300, SMB302},
	}).Encode()
	if err != nil {
		t.Fatalf("Encode input: %v", err)
	}

	req := &IoctlRequest{
		CtlCode:           FSCTLValidateNegotiateInfo,
		FileID:            CompoundFileID,
		MaxOutputResponse: 24,
		Flags:             IoctlIsFSCTL,
		Input:             in,
	}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// InputOffset 相对 SMB2 头起点。
	if got := le.Uint32(msg[HeaderSize+24:]); got != HeaderSize+ioctlRequestFixed {
		t.Errorf("InputOffset = %d, 期望 %d", got, HeaderSize+ioctlRequestFixed)
	}
	if got := le.Uint32(msg[HeaderSize+28:]); int(got) != len(in) {
		t.Errorf("InputCount = %d, 期望 %d", got, len(in))
	}

	got, err := ParseIoctlRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.CtlCode != req.CtlCode || got.FileID != req.FileID ||
		got.Flags != req.Flags || got.MaxOutputResponse != req.MaxOutputResponse {
		t.Errorf("固定字段不一致: %+v", got)
	}
	if !bytes.Equal(got.Input, in) {
		t.Errorf("Input = % X, 期望 % X", got.Input, in)
	}
	if !got.IsFSCTL() {
		t.Error("IsFSCTL 应为 true")
	}
	if !got.FileID.IsCompound() {
		t.Error("全 0xFF 的 FileId 应识别为 compound")
	}

	for n := 0; n < HeaderSize+ioctlRequestFixed; n++ {
		if _, err := ParseIoctlRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 应报错", n)
		}
	}
}

func TestIoctlResponseRoundTrip(t *testing.T) {
	h := Header{Command: CommandIoctl, Flags: FlagServerToRedir}
	out := (&ValidateNegotiateInfoResponse{
		Capabilities: CapLargeMTU,
		ServerGUID:   [16]byte{0xAA, 0xBB},
		SecurityMode: NegotiateSigningEnabled | NegotiateSigningRequired,
		Dialect:      SMB300,
	}).Encode()
	if len(out) != ValidateNegotiateInfoResponseSize {
		t.Fatalf("输出长度 = %d, 期望 24", len(out))
	}

	resp := &IoctlResponse{
		CtlCode: FSCTLValidateNegotiateInfo,
		FileID:  CompoundFileID,
		Flags:   0,
		Output:  out,
	}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 只要有一个缓冲区非空，两个 offset 都指向可变部分起点，空的那个 count 填 0。
	// 这是跟随真实 Samba 的行为，见 appendIoctlBuffers 的注释与
	// testdata/capture/negotiate-smb202/010-s2c-IOCTL.bin。
	if got := le.Uint32(msg[HeaderSize+24:]); got != HeaderSize+ioctlResponseFixed {
		t.Errorf("InputOffset = %d, 期望 %d", got, HeaderSize+ioctlResponseFixed)
	}
	if got := le.Uint32(msg[HeaderSize+28:]); got != 0 {
		t.Errorf("InputCount = %d, 期望 0", got)
	}
	if got := le.Uint32(msg[HeaderSize+32:]); got != HeaderSize+ioctlResponseFixed {
		t.Errorf("OutputOffset = %d, 期望 %d", got, HeaderSize+ioctlResponseFixed)
	}

	got, err := ParseIoctlResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.CtlCode != resp.CtlCode || !bytes.Equal(got.Output, out) || len(got.Input) != 0 {
		t.Errorf("round-trip 不一致: %+v", got)
	}

	vni, err := ParseValidateNegotiateInfoResponse(got.Output)
	if err != nil {
		t.Fatalf("ParseValidateNegotiateInfoResponse: %v", err)
	}
	if vni.Dialect != SMB300 || vni.Capabilities != CapLargeMTU ||
		vni.SecurityMode != NegotiateSigningEnabled|NegotiateSigningRequired ||
		vni.ServerGUID != [16]byte{0xAA, 0xBB} {
		t.Errorf("VALIDATE_NEGOTIATE_INFO 输出不一致: %+v", vni)
	}
	if _, err := ParseValidateNegotiateInfoResponse(out[:23]); err == nil {
		t.Error("不足 24 字节应报错")
	}
}

func TestIoctlEmptyBuffers(t *testing.T) {
	h := Header{Command: CommandIoctl, Flags: FlagServerToRedir}
	resp := &IoctlResponse{CtlCode: FSCTLSetSparse}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// StructureSize 是 49 = 48+1，空缓冲要补 1 字节占位。
	if len(msg) != HeaderSize+ioctlResponseFixed+1 {
		t.Errorf("长度 = %d, 期望 %d", len(msg), HeaderSize+ioctlResponseFixed+1)
	}
	got, err := ParseIoctlResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Input) != 0 || len(got.Output) != 0 {
		t.Error("空缓冲区解析应为空")
	}
}

func TestIoctlBothBuffersAligned(t *testing.T) {
	h := Header{Command: CommandIoctl, Flags: FlagServerToRedir}
	resp := &IoctlResponse{
		CtlCode: FSCTLPipeTransceive,
		Input:   []byte{1, 2, 3}, // 3 字节，之后要补到 8 字节对齐
		Output:  []byte{4, 5, 6, 7},
	}
	msg, err := resp.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	inOff := le.Uint32(msg[HeaderSize+24:])
	outOff := le.Uint32(msg[HeaderSize+32:])
	if inOff != HeaderSize+ioctlResponseFixed {
		t.Errorf("InputOffset = %d", inOff)
	}
	if (outOff-HeaderSize)%8 != 0 {
		t.Errorf("OutputOffset = %d, 体内偏移未 8 字节对齐", outOff)
	}
	got, err := ParseIoctlResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(got.Input, resp.Input) || !bytes.Equal(got.Output, resp.Output) {
		t.Errorf("round-trip 不一致: in=% X out=% X", got.Input, got.Output)
	}
}

func TestIoctlBadOffset(t *testing.T) {
	h := Header{Command: CommandIoctl}
	req := &IoctlRequest{CtlCode: FSCTLDFSGetReferrals, Input: []byte{1, 2, 3, 4}}
	msg, err := req.Append(h.Append(nil))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// InputCount 越界。
	bad := bytes.Clone(msg)
	le.PutUint32(bad[HeaderSize+28:], 0xFFFFFF)
	if _, err := ParseIoctlRequest(bad); err == nil {
		t.Error("InputCount 越界应报错")
	}
	// InputOffset 越界。
	bad = bytes.Clone(msg)
	le.PutUint32(bad[HeaderSize+24:], 0xFFFFFF)
	if _, err := ParseIoctlRequest(bad); err == nil {
		t.Error("InputOffset 越界应报错")
	}
}

func TestValidateNegotiateInfoRequestRoundTrip(t *testing.T) {
	want := &ValidateNegotiateInfoRequest{
		Capabilities: CapLargeMTU,
		ClientGUID:   [16]byte{0xDE, 0xAD, 0xBE, 0xEF},
		SecurityMode: NegotiateSigningEnabled,
		Dialects:     []Dialect{SMB202, SMB210, SMB300, SMB302, SMB311},
	}
	b, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(b) != validateNegotiateInfoRequestFixed+5*2 {
		t.Fatalf("长度 = %d", len(b))
	}
	if got := le.Uint16(b[22:]); got != 5 {
		t.Errorf("DialectCount = %d, 期望 5", got)
	}
	got, err := ParseValidateNegotiateInfoRequest(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Capabilities != want.Capabilities || got.ClientGUID != want.ClientGUID ||
		got.SecurityMode != want.SecurityMode || len(got.Dialects) != len(want.Dialects) {
		t.Errorf("round-trip 不一致: %+v", got)
	}
	for i := range got.Dialects {
		if got.Dialects[i] != want.Dialects[i] {
			t.Errorf("Dialects[%d] = %#x", i, got.Dialects[i])
		}
	}
	// DialectCount 与实际长度不符必须报错，不能越界读。
	le.PutUint16(b[22:], 100)
	if _, err := ParseValidateNegotiateInfoRequest(b); err == nil {
		t.Error("DialectCount 越界应报错")
	}
	if _, err := ParseValidateNegotiateInfoRequest(b[:20]); err == nil {
		t.Error("截断应报错")
	}
}

func TestAllocatedRangesAndZeroData(t *testing.T) {
	ranges := []FileAllocatedRangeBuffer{{FileOffset: 0, Length: 4096}, {FileOffset: 1 << 20, Length: 8192}}
	b := AppendAllocatedRanges(nil, ranges)
	if len(b) != 32 {
		t.Fatalf("长度 = %d, 期望 32", len(b))
	}
	if le.Uint64(b[16:]) != 1<<20 || le.Uint64(b[24:]) != 8192 {
		t.Error("区间编码错误")
	}

	in, err := ParseAllocatedRangesInput(b)
	if err != nil {
		t.Fatalf("ParseAllocatedRangesInput: %v", err)
	}
	if in != ranges[0] {
		t.Errorf("输入解析 = %+v", in)
	}
	if _, err := ParseAllocatedRangesInput(b[:15]); err == nil {
		t.Error("截断应报错")
	}

	zb := make([]byte, 16)
	le.PutUint64(zb[0:], 100)
	le.PutUint64(zb[8:], 200)
	z, err := ParseZeroDataInput(zb)
	if err != nil {
		t.Fatalf("ParseZeroDataInput: %v", err)
	}
	if z.FileOffset != 100 || z.BeyondFinalZero != 200 {
		t.Errorf("SET_ZERO_DATA = %+v", z)
	}
	le.PutUint64(zb[8:], 50) // 结束 < 起始
	if _, err := ParseZeroDataInput(zb); err == nil {
		t.Error("非法区间应报错")
	}
}

func TestCtlCodeString(t *testing.T) {
	if got := FSCTLValidateNegotiateInfo.String(); got != "FSCTL_VALIDATE_NEGOTIATE_INFO" {
		t.Errorf("String = %q", got)
	}
	if got := CtlCode(0x12345678).String(); got != "FSCTL(0x12345678)" {
		t.Errorf("未知码 String = %q", got)
	}
}

// TestSetSparseInput 覆盖 MS-FSCC §2.3.69 FILE_SET_SPARSE_BUFFER。
//
// 最关键的一条：**空输入等价于 TRUE**。macOS/Windows 都会发不带输入缓冲区的
// FSCTL_SET_SPARSE，若默认按 false 处理，Time Machine 的 band 文件不会稀疏化。
func TestSetSparseInput(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"空输入视为 TRUE", nil, true},
		{"零长切片视为 TRUE", []byte{}, true},
		{"0x01 → TRUE", []byte{0x01}, true},
		{"0xFF → TRUE（非 0 即真）", []byte{0xFF}, true},
		{"0x00 → FALSE", []byte{0x00}, false},
		{"多余字节忽略", []byte{0x01, 0xAA, 0xBB}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSetSparseInput(c.in)
			if err != nil {
				t.Fatalf("ParseSetSparseInput: %v", err)
			}
			if got != c.want {
				t.Errorf("= %v, 期望 %v", got, c.want)
			}
		})
	}

	// 编码侧固定 1 字节。
	if b := EncodeSetSparseInput(true); len(b) != FileSetSparseBufferSize || b[0] != 1 {
		t.Errorf("EncodeSetSparseInput(true) = % X", b)
	}
	if b := EncodeSetSparseInput(false); len(b) != FileSetSparseBufferSize || b[0] != 0 {
		t.Errorf("EncodeSetSparseInput(false) = % X", b)
	}
	// 往返。
	for _, want := range []bool{true, false} {
		got, err := ParseSetSparseInput(EncodeSetSparseInput(want))
		if err != nil || got != want {
			t.Errorf("round-trip %v = %v, %v", want, got, err)
		}
	}
	if FSCTLSetSparse != 0x000900C4 {
		t.Errorf("FSCTL_SET_SPARSE = %#08x, 期望 0x000900C4", uint32(FSCTLSetSparse))
	}
}

// TestAllocatedRangesArray 覆盖 MS-FSCC §2.3.20/§2.3.21 的数组语义与字节布局。
func TestAllocatedRangesArray(t *testing.T) {
	if FSCTLQueryAllocatedRanges != 0x000940CF {
		t.Errorf("FSCTL_QUERY_ALLOCATED_RANGES = %#08x, 期望 0x000940CF", uint32(FSCTLQueryAllocatedRanges))
	}

	// 固定字节向量：两个区间，全部小端。
	//   [0]  FileOffset = 0x0000000000002000 (8192)
	//   [8]  Length     = 0x0000000000001000 (4096)
	//   [16] FileOffset = 0x0000000100000000 (4 GiB，验证高 32 位不被截断)
	//   [24] Length     = 0x0000000000000200 (512)
	golden := []byte{
		0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	want := []FileAllocatedRangeBuffer{
		{FileOffset: 8192, Length: 4096},
		{FileOffset: 1 << 32, Length: 512},
	}
	if got := AppendAllocatedRanges(nil, want); !bytes.Equal(got, golden) {
		t.Errorf("编码 = % X\n期望   = % X", got, golden)
	}
	got, err := ParseAllocatedRanges(golden)
	if err != nil {
		t.Fatalf("ParseAllocatedRanges: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("区间数 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, 期望 %+v", i, got[i], want[i])
		}
	}

	// 空数组是合法应答（整段都是洞）。
	if b := AppendAllocatedRanges(nil, nil); len(b) != 0 {
		t.Errorf("空区间列表应编码为 0 字节, 实际 %d", len(b))
	}
	if r, err := ParseAllocatedRanges(nil); err != nil || r != nil {
		t.Errorf("空输出应解析为 nil, 得到 %v, %v", r, err)
	}
	// 非 16 整数倍必须报错，不能越界读。
	if _, err := ParseAllocatedRanges(golden[:20]); err == nil {
		t.Error("长度非 16 整数倍应报错")
	}

	// 输入侧：负值与溢出都要拒绝（§2.3.20 处理规则）。
	bad := make([]byte, FileAllocatedRangeBufferSize)
	le.PutUint64(bad[0:], 1<<63) // FileOffset 解释为 INT64 是负数
	if _, err := ParseAllocatedRangesInput(bad); err == nil {
		t.Error("负 FileOffset 应报错")
	}
	le.PutUint64(bad[0:], 0)
	le.PutUint64(bad[8:], 1<<63) // Length 为负
	if _, err := ParseAllocatedRangesInput(bad); err == nil {
		t.Error("负 Length 应报错")
	}
	le.PutUint64(bad[0:], 1<<62)
	le.PutUint64(bad[8:], 1<<62+1) // 相加溢出 INT64
	if _, err := ParseAllocatedRangesInput(bad); err == nil {
		t.Error("offset+length 溢出应报错")
	}
}
