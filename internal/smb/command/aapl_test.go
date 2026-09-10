package command

import (
	"bytes"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件的字节向量**不是编造的**（AGENTS.md §3/§9）。
//
// 来源：Apple《Time Machine over SMB Specification》里的 AAPL create context
// 请求/响应完整示例（该文档同时给出了 Table 1-3 / Table 1-4 两张字段表）：
//
//	--- 请求 ---
//	DataLength:         0x00000018   (24)
//	CommandCode:        0x00000001   kAAPL_SERVER_QUERY
//	Reserved:           0x00000000
//	RequestBitmap:      0x0000000000000007
//	ClientCapabilities: 0x000000000000000f
//
//	--- 响应 ---
//	DataLength:         0x00000020   (32)
//	CommandCode:        0x00000001
//	Reserved:           0x00000000
//	ReplyBitmap:        0x0000000000000003
//	ServerCapabilities: 0x0000000000000000
//	VolumeCapabilities: 0x0000000000000004   kAAPL_SUPPORTS_FULL_SYNC

// appleSpecRequest 是上面那份请求示例的 24 字节 Data（小端）。
var appleSpecRequest = mustHex(
	"01000000" + // CommandCode = kAAPL_SERVER_QUERY
		"00000000" + // Reserved
		"0700000000000000" + // RequestBitmap = SERVER_CAPS|VOLUME_CAPS|MODEL_INFO
		"0f00000000000000") // ClientCapabilities

// appleSpecResponse 是上面那份响应示例的 32 字节 Data（小端）。
var appleSpecResponse = mustHex(
	"01000000" + // CommandCode
		"00000000" + // Reserved
		"0300000000000000" + // ReplyBitmap = SERVER_CAPS|VOLUME_CAPS
		"0000000000000000" + // ServerCapabilities
		"0400000000000000") // VolumeCapabilities = kAAPL_SUPPORTS_FULL_SYNC

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseAAPLRequestAppleSpec(t *testing.T) {
	r, err := parseAAPLRequest(appleSpecRequest)
	if err != nil {
		t.Fatalf("parseAAPLRequest: %v", err)
	}
	if r.Command != aaplServerQuery {
		t.Errorf("Command = %d, 期望 %d", r.Command, aaplServerQuery)
	}
	if want := aaplServerCaps | aaplVolumeCaps | aaplModelInfo; r.RequestBitmap != want {
		t.Errorf("RequestBitmap = %#x, 期望 %#x", r.RequestBitmap, want)
	}
	if r.ClientCaps != 0x0f {
		t.Errorf("ClientCapabilities = %#x, 期望 0xf", r.ClientCaps)
	}
	// 0xf 的低位含 kAAPL_SUPPORTS_READ_DIR_ATTR —— readdir_attr 的前提。
	if r.ClientCaps&aaplSupportsReadDirAttr == 0 {
		t.Error("示例客户端能力里应当含 kAAPL_SUPPORTS_READ_DIR_ATTR")
	}
}

// TestParseAAPLRequestBadLength：长度不是 24 一律拒绝（同 Samba check_aapl）。
func TestParseAAPLRequestBadLength(t *testing.T) {
	for _, n := range []int{0, 16, 23, 25, 64} {
		if _, err := parseAAPLRequest(make([]byte, n)); err == nil {
			t.Errorf("长度 %d 的 AAPL 请求应当被拒绝", n)
		}
	}
	// 不能因为越界切片 panic。
	if _, err := parseAAPLRequest(nil); err == nil {
		t.Error("nil AAPL 请求应当被拒绝")
	}
}

// TestBuildAAPLResponseAppleSpec 用 Apple 文档里那份响应示例做 golden test。
func TestBuildAAPLResponseAppleSpec(t *testing.T) {
	got := buildAAPLResponse(aaplServerCaps|aaplVolumeCaps, 0, aaplSupportsFullSync, "")
	if !bytes.Equal(got, appleSpecResponse) {
		t.Errorf("响应 =\n  %x\n期望\n  %x", got, appleSpecResponse)
	}
	if len(got) != 32 {
		t.Errorf("长度 = %d, 期望 32（Apple 示例的 DataLength=0x20）", len(got))
	}
}

// TestBuildAAPLResponseModelInfo 验证带 ModelString 的完整响应布局。
//
// ModelString 段：4 字节保留 0 + 4 字节**字节**长度 + UTF-16LE 字符串
// （Samba check_aapl 的写法：SIVAL(p,0,0); SIVAL(p+4,0,modellen)）。
func TestBuildAAPLResponseModelInfo(t *testing.T) {
	const model = "MacSamba"
	bitmap := aaplServerCaps | aaplVolumeCaps | aaplModelInfo
	serverCaps := aaplUnixBased | aaplSupportsReadDirAttr

	got := buildAAPLResponse(bitmap, serverCaps, aaplSupportsFullSync, model)

	wantLen := 16 + 8 + 8 + 8 + 2*len(model)
	if len(got) != wantLen {
		t.Fatalf("长度 = %d, 期望 %d", len(got), wantLen)
	}
	if v := aaplLE.Uint32(got[0:4]); v != aaplServerQuery {
		t.Errorf("CommandCode = %d, 期望 %d", v, aaplServerQuery)
	}
	if v := aaplLE.Uint32(got[4:8]); v != 0 {
		t.Errorf("Reserved = %#x, 期望 0", v)
	}
	if v := aaplLE.Uint64(got[8:16]); v != bitmap {
		t.Errorf("ReplyBitmap = %#x, 期望 %#x", v, bitmap)
	}
	if v := aaplLE.Uint64(got[16:24]); v != serverCaps {
		t.Errorf("ServerCapabilities = %#x, 期望 %#x", v, serverCaps)
	}
	if v := aaplLE.Uint64(got[24:32]); v != aaplSupportsFullSync {
		t.Errorf("VolumeCapabilities = %#x, 期望 %#x", v, aaplSupportsFullSync)
	}
	if v := aaplLE.Uint32(got[32:36]); v != 0 {
		t.Errorf("ModelString 段保留字段 = %#x, 期望 0", v)
	}
	if v := aaplLE.Uint32(got[36:40]); int(v) != 2*len(model) {
		t.Errorf("ModelString 长度 = %d, 期望 %d（UTF-16LE 字节数）", v, 2*len(model))
	}
	if want := wire.EncodeUTF16LE(model); !bytes.Equal(got[40:], want) {
		t.Errorf("ModelString = %x, 期望 %x", got[40:], want)
	}
}

// TestAAPLModelStringFollowsSettings：AAPL 响应里的 ModelString 必须跟着
// Settings.AppleModel（即配置的 mdns.apple.model）走，不能是硬编码常量。
//
// 反向对照：把 negotiateAAPL 里的 ctx.Conn.Settings.appleModel() 换回
// DefaultAppleModel，「自定义机型」子用例会立刻红 —— 那正是修复前的状态
// （改配置只有 mDNS 侧跟着变，协议回的还是 MacSamba）。
func TestAAPLModelStringFollowsSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		appleModel string
		want       string
	}{
		{"未配置时回落默认值", "", DefaultAppleModel},
		{"配置透传", "TimeCapsule8,119", "TimeCapsule8,119"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAAPLTestContext(t, true, false, true)
			ctx.Conn.Settings.AppleModel = tc.appleModel

			out, err := negotiateAAPL(ctx, aaplCreateRequest(aaplServerQuery,
				aaplServerCaps|aaplVolumeCaps|aaplModelInfo, 0x0f))
			if err != nil {
				t.Fatalf("negotiateAAPL: %v", err)
			}
			if len(out) < 40 {
				t.Fatalf("响应长度 = %d，没有 ModelString 段", len(out))
			}
			if v := aaplLE.Uint32(out[36:40]); int(v) != 2*len(tc.want) {
				t.Fatalf("ModelString 长度 = %d, 期望 %d", v, 2*len(tc.want))
			}
			got, derr := wire.DecodeUTF16LE(out[40:])
			if derr != nil {
				t.Fatalf("解码 ModelString: %v", derr)
			}
			if got != tc.want {
				t.Errorf("ModelString = %q, 期望 %q（Settings.AppleModel=%q）",
					got, tc.want, tc.appleModel)
			}
		})
	}
}

// TestBuildAAPLResponseBitmapSegments 验证「只返回被请求的段」。
func TestBuildAAPLResponseBitmapSegments(t *testing.T) {
	tests := []struct {
		name    string
		bitmap  uint64
		wantLen int
	}{
		{"什么都不要", 0, 16},
		{"只要 ServerCaps", aaplServerCaps, 24},
		{"只要 VolumeCaps", aaplVolumeCaps, 24},
		{"只要 ModelInfo", aaplModelInfo, 16 + 8 + 2*len("MacSamba")},
		{"全都要", aaplServerCaps | aaplVolumeCaps | aaplModelInfo, 40 + 2*len("MacSamba")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAAPLResponse(tc.bitmap, aaplUnixBased, aaplSupportsFullSync, "MacSamba")
			if len(got) != tc.wantLen {
				t.Errorf("长度 = %d, 期望 %d", len(got), tc.wantLen)
			}
			if v := aaplLE.Uint64(got[8:16]); v != tc.bitmap {
				t.Errorf("ReplyBitmap = %#x, 期望 %#x", v, tc.bitmap)
			}
			// VolumeCaps 单独被请求时必须紧跟在头后面，不能留 ServerCaps 的空洞。
			if tc.bitmap == aaplVolumeCaps {
				if v := aaplLE.Uint64(got[16:24]); v != aaplSupportsFullSync {
					t.Errorf("VolumeCapabilities = %#x, 期望 %#x", v, aaplSupportsFullSync)
				}
			}
		})
	}
}

// TestAAPLContextWireRoundTrip 验证 AAPL context 能按 MS-SMB2 §2.2.13.2
// 的 create context 链格式编码并原样解回来（8 字节对齐是历史上的老坑）。
func TestAAPLContextWireRoundTrip(t *testing.T) {
	in := []wire.CreateContext{{Name: wire.CreateContextAAPL, Data: appleSpecRequest}}
	raw, err := wire.AppendCreateContexts(nil, in)
	if err != nil {
		t.Fatalf("AppendCreateContexts: %v", err)
	}
	out, err := wire.ParseCreateContexts(raw)
	if err != nil {
		t.Fatalf("ParseCreateContexts: %v", err)
	}
	data, ok := wire.FindCreateContext(out, wire.CreateContextAAPL)
	if !ok {
		t.Fatal("解析结果里找不到 AAPL context")
	}
	if !bytes.Equal(data, appleSpecRequest) {
		t.Errorf("往返后 Data = %x, 期望 %x", data, appleSpecRequest)
	}
}

// ---------------------------------------------------------------------------
// negotiateAAPL 端到端
// ---------------------------------------------------------------------------

func TestNegotiateAAPL(t *testing.T) {
	tests := []struct {
		name            string
		timeMachine     bool
		caseSensitive   bool
		appleMeta       bool
		clientCaps      uint64
		wantServerCaps  uint64
		wantVolumeCaps  uint64
		wantReaddirAttr bool
	}{
		{
			name:            "Time Machine 共享 + 客户端支持 readdir_attr",
			timeMachine:     true,
			appleMeta:       true,
			clientCaps:      0x0f,
			wantServerCaps:  aaplUnixBased | aaplSupportsReadDirAttr,
			wantVolumeCaps:  aaplSupportsFullSync,
			wantReaddirAttr: true,
		},
		{
			// 普通共享不宣告 FULL_SYNC —— 与 Samba 的 fruit:time machine 一致。
			name:            "普通共享",
			appleMeta:       true,
			clientCaps:      0x0f,
			wantServerCaps:  aaplUnixBased | aaplSupportsReadDirAttr,
			wantVolumeCaps:  0,
			wantReaddirAttr: true,
		},
		{
			// 客户端没声明支持就绝不能单方面开启：会把目录项写成客户端读不懂的布局。
			name:            "客户端不支持 readdir_attr",
			appleMeta:       true,
			clientCaps:      0,
			wantServerCaps:  aaplUnixBased,
			wantVolumeCaps:  0,
			wantReaddirAttr: false,
		},
		{
			name:            "后端拿不出 Apple 元数据",
			appleMeta:       false,
			clientCaps:      0x0f,
			wantServerCaps:  aaplUnixBased,
			wantVolumeCaps:  0,
			wantReaddirAttr: false,
		},
		{
			name:            "大小写敏感卷",
			caseSensitive:   true,
			appleMeta:       true,
			clientCaps:      0x0f,
			wantServerCaps:  aaplUnixBased | aaplSupportsReadDirAttr,
			wantVolumeCaps:  aaplCaseSensitive,
			wantReaddirAttr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAAPLTestContext(t, tc.timeMachine, tc.caseSensitive, tc.appleMeta)
			req := aaplCreateRequest(aaplServerQuery,
				aaplServerCaps|aaplVolumeCaps|aaplModelInfo, tc.clientCaps)

			out, err := negotiateAAPL(ctx, req)
			if err != nil {
				t.Fatalf("negotiateAAPL: %v", err)
			}
			if len(out) < 32 {
				t.Fatalf("响应长度 = %d, 太短", len(out))
			}
			if v := aaplLE.Uint64(got64(out, 16)); v != tc.wantServerCaps {
				t.Errorf("ServerCapabilities = %#x, 期望 %#x", v, tc.wantServerCaps)
			}
			if v := aaplLE.Uint64(got64(out, 24)); v != tc.wantVolumeCaps {
				t.Errorf("VolumeCapabilities = %#x, 期望 %#x", v, tc.wantVolumeCaps)
			}
			if got := ctx.Conn.AAPLReaddirAttr(); got != tc.wantReaddirAttr {
				t.Errorf("readdir_attr 协商结果 = %v, 期望 %v", got, tc.wantReaddirAttr)
			}
		})
	}
}

// TestNegotiateAAPLReaddirAttrNeedsServerCapsBit：RequestBitmap 里没有
// kAAPL_SERVER_CAPS 时，即使客户端声明了 READ_DIR_ATTR 也不得启用 readdir_attr。
//
// Samba check_aapl() 把 readdir_attr_enabled 的赋值放在
// `if (req_bitmap & SMB2_CRTCTX_AAPL_SERVER_CAPS)` 分支内部：客户端没请求
// 这一段就拿不到 ServerCapabilities，无从知道服务端会改写目录项布局，
// 会把 rfork_size + 压缩 FinderInfo 误读成 8.3 短名。
func TestNegotiateAAPLReaddirAttrNeedsServerCapsBit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bitmap uint64
		want   bool
	}{
		{"只请求 ModelInfo", aaplModelInfo, false},
		{"只请求 VolumeCaps", aaplVolumeCaps, false},
		{"bitmap 为 0", 0, false},
		{"请求 ServerCaps", aaplServerCaps, true},
		{"macOS 的全 bitmap", aaplServerCaps | aaplVolumeCaps | aaplModelInfo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAAPLTestContext(t, true, false, true)
			req := aaplCreateRequest(aaplServerQuery, tc.bitmap, aaplSupportsReadDirAttr)
			if _, err := negotiateAAPL(ctx, req); err != nil {
				t.Fatalf("negotiateAAPL: %v", err)
			}
			if got := ctx.Conn.AAPLReaddirAttr(); got != tc.want {
				t.Errorf("bitmap=%#x 时 readdir_attr = %v, 期望 %v", tc.bitmap, got, tc.want)
			}
		})
	}
}

// TestNegotiateAAPLAbsent：请求里没有 AAPL context 时不产生响应，也不置状态。
func TestNegotiateAAPLAbsent(t *testing.T) {
	ctx := newAAPLTestContext(t, true, false, true)
	out, err := negotiateAAPL(ctx, &wire.CreateRequest{})
	if err != nil {
		t.Fatalf("negotiateAAPL: %v", err)
	}
	if out != nil {
		t.Errorf("响应 = %x, 期望 nil", out)
	}
	if ctx.Conn.AAPLReaddirAttr() {
		t.Error("没协商就不该开启 readdir_attr")
	}
}

// TestNegotiateAAPLUnsupportedCommand：kAAPL_RESOLVE_ID 与未知命令都拒绝。
//
// 我们从不在 VolumeCapabilities 里宣告 kAAPL_SUPPORT_RESOLVE_ID，
// 因此客户端不该发这个命令；真发了就按 Samba check_aapl 回 INVALID_PARAMETER。
func TestNegotiateAAPLUnsupportedCommand(t *testing.T) {
	for _, cmd := range []uint32{aaplResolveID, 3, 0xffffffff} {
		ctx := newAAPLTestContext(t, true, false, true)
		req := aaplCreateRequest(cmd, aaplServerCaps, 0x0f)
		if _, err := negotiateAAPL(ctx, req); err != status.InvalidParameter {
			t.Errorf("cmd=%d 时 err = %v, 期望 %v", cmd, err, status.InvalidParameter)
		}
		if ctx.Conn.AAPLReaddirAttr() {
			t.Errorf("cmd=%d 被拒绝后不该开启 readdir_attr", cmd)
		}
	}
}

// TestNegotiateAAPLMalformed：畸形长度不得 panic，必须回 INVALID_PARAMETER。
func TestNegotiateAAPLMalformed(t *testing.T) {
	ctx := newAAPLTestContext(t, true, false, true)
	req := &wire.CreateRequest{Contexts: []wire.CreateContext{
		{Name: wire.CreateContextAAPL, Data: []byte{1, 0, 0}},
	}}
	if _, err := negotiateAAPL(ctx, req); err != status.InvalidParameter {
		t.Errorf("err = %v, 期望 %v", err, status.InvalidParameter)
	}
}

// aaplCreateRequest 造一个只带 AAPL context 的 CREATE 请求。
func aaplCreateRequest(cmd uint32, bitmap, clientCaps uint64) *wire.CreateRequest {
	data := make([]byte, aaplRequestSize)
	aaplLE.PutUint32(data[0:4], cmd)
	aaplLE.PutUint64(data[8:16], bitmap)
	aaplLE.PutUint64(data[16:24], clientCaps)
	return &wire.CreateRequest{Contexts: []wire.CreateContext{
		{Name: wire.CreateContextAAPL, Data: data},
	}}
}

func got64(b []byte, off int) []byte { return b[off : off+8] }

func newAAPLTestContext(t *testing.T, timeMachine, caseSensitive, appleMeta bool) *Context {
	t.Helper()

	var fs vfs.FileSystem = &aaplFakeFS{caseSensitive: caseSensitive}
	if appleMeta {
		fs = &aaplFakeAppleFS{aaplFakeFS{caseSensitive: caseSensitive}}
	}
	share := &Share{Name: "backup", Type: wire.ShareTypeDisk, FS: fs, TimeMachine: timeMachine}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	return &Context{
		Conn: conn,
		Tree: &Tree{ID: 1, Share: share},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// aaplFakeFS 是最小的 vfs.FileSystem 桩，只有 StatFS 有实质行为。
type aaplFakeFS struct{ caseSensitive bool }

func (f *aaplFakeFS) Open(*vfs.OpenRequest) (vfs.Handle, vfs.Action, error) {
	return nil, 0, vfs.ErrNotSupported
}
func (f *aaplFakeFS) Stat(string) (*vfs.Attr, error)    { return nil, vfs.ErrNotFound }
func (f *aaplFakeFS) Remove(string) error               { return vfs.ErrNotSupported }
func (f *aaplFakeFS) Mkdir(string, uint32) error        { return vfs.ErrNotSupported }
func (f *aaplFakeFS) Rename(string, string, bool) error { return vfs.ErrNotSupported }
func (f *aaplFakeFS) Streams(string) ([]vfs.StreamInfo, error) {
	return nil, vfs.ErrNotSupported
}
func (f *aaplFakeFS) ReadOnly() bool { return false }
func (f *aaplFakeFS) Close() error   { return nil }
func (f *aaplFakeFS) StatFS() (*vfs.FSInfo, error) {
	return &vfs.FSInfo{BlockSize: 4096, CaseSensitive: f.caseSensitive}, nil
}

// aaplFakeAppleFS 额外实现 vfs.AppleMetadata，代表「后端能提供 Apple 元数据」。
type aaplFakeAppleFS struct{ aaplFakeFS }

func (f *aaplFakeAppleFS) AppleInfo(string) ([vfs.FinderInfoSize]byte, int64, error) {
	var fi [vfs.FinderInfoSize]byte
	return fi, 0, nil
}

var (
	_ vfs.FileSystem    = (*aaplFakeFS)(nil)
	_ vfs.AppleMetadata = (*aaplFakeAppleFS)(nil)
)

// TestBuildAAPLResponseMasksUnknownBits：未知的请求位不得出现在回复里。
//
// 回复的语义是"我提供了哪些段"，不是"你要了哪些"。原样回显请求位的话，
// 客户端请求了 0x8 就会拿到一个"置了位却没有字节"的回复，按位去解析时
// 后面的 ServerCaps / VolumeCaps / ModelString 全部错位 ——
// 这种故障不会报错，只会表现成随机的怪值，极难定位。
//
// 变异自检：把 buildAAPLResponse 里的掩码去掉 → 本例与长度断言一起变红。
func TestBuildAAPLResponseMasksUnknownBits(t *testing.T) {
	const model = "MacSamba"
	known := aaplServerCaps | aaplVolumeCaps | aaplModelInfo

	// 未知位：0x8、0x10、以及高位随便撒几个。
	for _, unknown := range []uint64{0x8, 0x10, 0x8000000000000000} {
		req := unknown | known
		got := buildAAPLResponse(req, aaplUnixBased, aaplSupportsFullSync, model)

		if v := aaplLE.Uint64(got[8:16]); v != known {
			t.Errorf("请求位 %#x 时 ReplyBitmap = %#x，期望掩码后的 %#x", unknown, v, known)
		}
		// 长度必须只按已知位计算：多算一个段就会多出 8 字节的空洞。
		wantLen := 16 + 8 + 8 + 8 + 2*len(model)
		if len(got) != wantLen {
			t.Errorf("请求位 %#x 时长度 = %d，期望 %d", unknown, len(got), wantLen)
		}
	}
}
