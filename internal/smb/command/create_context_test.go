package command

import (
	"encoding/binary"
	"encoding/hex"

	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// create_context_test.go —— create context 注册表的 golden test。
//
// 这一组用例存在的唯一理由：**证明把散在 createFile 里的 if 链改成注册表
// 之后，回给客户端的 context 链一个字节都没变**。
//
// 因此 golden 的十六进制串是在**重构之前**的实现上跑出来的
// （createResponseContexts + negotiateAAPL + parseMxAcRequest 的老路径），
// 重构之后原样保留。任何一次 diff 都意味着线上行为变了。

// goldenCreateAttr 是 golden 用例里固定的文件属性。
// 时间用固定值，golden 才可复现。
func goldenCreateAttr() *vfs.Attr {
	return &vfs.Attr{
		FileID:    0x0011_2233_4455_6677,
		WriteTime: vfs.FiletimeToTime(0x01d0_1234_5678_9abc),
	}
}

// goldenAAPLRequest 造一条 AAPL server query 请求（24 字节，小端）。
// RequestBitmap 三段全要，ClientCapabilities 声明支持 readdir_attr。
func goldenAAPLRequest() wire.CreateContext {
	data := make([]byte, 24)
	binary.LittleEndian.PutUint32(data[0:4], 1) // kAAPL_SERVER_QUERY
	binary.LittleEndian.PutUint64(data[8:16], 0x7)
	binary.LittleEndian.PutUint64(data[16:24], 0x1)
	return wire.CreateContext{Name: wire.CreateContextAAPL, Data: data}
}

// encodeCreateContexts 把 context 链编成线字节，便于与 golden 比对。
func encodeCreateContexts(t *testing.T, ctxs []wire.CreateContext) string {
	t.Helper()
	if len(ctxs) == 0 {
		return ""
	}
	b, err := wire.AppendCreateContexts(nil, ctxs)
	if err != nil {
		t.Fatalf("AppendCreateContexts: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestCreateResponseContextsGolden 固定住响应 context 链的字节。
func TestCreateResponseContextsGolden(t *testing.T) {
	tests := []struct {
		name string
		reqs []wire.CreateContext
		want string
	}{
		{
			name: "没有任何 context",
			reqs: nil,
			want: "",
		},
		{
			name: "只有 QFid",
			reqs: []wire.CreateContext{{Name: wire.CreateContextQFid}},
			want: goldenQFidOnly,
		},
		{
			name: "QFid + MxAc + AAPL（macOS 的典型形态）",
			reqs: []wire.CreateContext{
				{Name: wire.CreateContextQFid},
				{Name: wire.CreateContextMxAc},
				goldenAAPLRequest(),
			},
			want: goldenQFidMxAcAAPL,
		},
		{
			// 客户端把顺序打乱：响应链的次序**不跟随请求**，
			// 固定为 QFid → MxAc → AAPL，字节应与上一条完全一致。
			name: "请求顺序打乱后响应链次序不变",
			reqs: []wire.CreateContext{
				goldenAAPLRequest(),
				{Name: wire.CreateContextMxAc},
				{Name: wire.CreateContextQFid},
			},
			want: goldenQFidMxAcAAPL,
		},
		{
			// 不认识的 context 必须**静默忽略**：既不报错，也不出现在响应里。
			name: "夹杂未知 context 时静默忽略",
			reqs: []wire.CreateContext{
				{Name: "ZzZz", Data: []byte{1, 2, 3, 4, 5}},
				{Name: wire.CreateContextQFid},
				{Name: "TWrp", Data: make([]byte, 8)},
			},
			want: goldenQFidOnly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAAPLTestContext(t, true, false, true)
			req := &wire.CreateRequest{Contexts: tc.reqs}
			attr := goldenCreateAttr()

			got := encodeCreateContexts(t, goldenRespond(t, ctx, req, attr))
			if got != tc.want {
				t.Errorf("context 链字节不符\n got=%s\nwant=%s", got, tc.want)
			}
		})
	}
}

// TestCreateContextMalformedRejected 覆盖会让整个 CREATE 失败的畸形载荷。
//
// 反向对照：合法的空 MxAc 与合法的 24 字节 AAPL 必须放行，
// 否则"畸形被拒"这条结论可能只是因为整条路径都在拒。
func TestCreateContextMalformedRejected(t *testing.T) {
	tests := []struct {
		name string
		reqs []wire.CreateContext
		want error
	}{
		{
			name: "MxAc 长度非法",
			reqs: []wire.CreateContext{{Name: wire.CreateContextMxAc, Data: make([]byte, 7)}},
			want: status.InvalidParameter,
		},
		{
			name: "AAPL 长度非法",
			reqs: []wire.CreateContext{{Name: wire.CreateContextAAPL, Data: make([]byte, 23)}},
			want: status.InvalidParameter,
		},
		{
			name: "AAPL 命令码未知",
			reqs: []wire.CreateContext{func() wire.CreateContext {
				c := goldenAAPLRequest()
				binary.LittleEndian.PutUint32(c.Data[0:4], 99)
				return c
			}()},
			want: status.InvalidParameter,
		},
		{
			// 反向对照 1：空 Data 的 MxAc 是**合法且常见**的形态。
			name: "MxAc 空 Data 合法",
			reqs: []wire.CreateContext{{Name: wire.CreateContextMxAc}},
			want: nil,
		},
		{
			// 反向对照 2：合法 AAPL 必须放行。
			name: "AAPL 合法请求放行",
			reqs: []wire.CreateContext{goldenAAPLRequest()},
			want: nil,
		},
		{
			// 反向对照 3：未知 context 载荷再怎么畸形也不能让 CREATE 失败。
			name: "未知 context 的畸形载荷不影响 CREATE",
			reqs: []wire.CreateContext{{Name: "ZzZz", Data: make([]byte, 3)}},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAAPLTestContext(t, true, false, true)
			err := goldenParse(t, ctx, &wire.CreateRequest{Contexts: tc.reqs})
			if !errEquiv(err, tc.want) {
				t.Errorf("err = %v, 期望 %v", err, tc.want)
			}
		})
	}
}

func errEquiv(got, want error) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got == want
}

// ---------------------------------------------------------------------------
// golden 值
//
// 生成方式：在**重构前**的 HEAD 上跑本文件，把实际输出抄进来。
// 布局按 MS-SMB2 §2.2.13.2（Next / NameOffset / NameLength / DataOffset /
// DataLength 均为小端，各段 8 字节对齐）。
// ---------------------------------------------------------------------------

const (
	// goldenQFidOnly：单条 QFid 响应（§2.2.14.2.9，32 字节载荷）。
	goldenQFidOnly = "" +
		"00000000" + // Next = 0（最后一条）
		"1000" + "0400" + "0000" + "1800" + "20000000" + // NameOff/NameLen/Rsvd/DataOff/DataLen
		"51466964" + "00000000" + // "QFid" + 对齐填充
		"7766554433221100" + // DiskFileId = 0x0011223344556677，小端
		"0000000000000000" + // VolumeId = 0
		"00000000000000000000000000000000" // Reserved(16)

	// goldenQFidMxAcAAPL：QFid → MxAc → AAPL 三条。
	goldenQFidMxAcAAPL = "" +
		// --- QFid ---
		"38000000" + // Next = 0x38（头 16 + 名 4 + 填充 4 + 载荷 32）
		"1000" + "0400" + "0000" + "1800" + "20000000" +
		"5146696400000000" +
		"7766554433221100" +
		"0000000000000000" +
		"00000000000000000000000000000000" +
		// --- MxAc ---
		"20000000" + // Next = 0x20（头 16 + 名 4 + 填充 4 + 载荷 8）
		"1000" + "0400" + "0000" + "1800" + "08000000" +
		"4d78416300000000" +
		"00000000" + // QueryStatus = STATUS_SUCCESS
		"ff011f00" + // MaximalAccess = 0x001f01ff
		// --- AAPL ---
		"00000000" + // Next = 0（最后一条）
		"1000" + "0400" + "0000" + "1800" + "38000000" +
		"4141504c00000000" +
		"01000000" + "00000000" + // CommandCode=1 / Reserved
		"0700000000000000" + // ReplyBitmap = 0x7
		"0500000000000000" + // ServerCapabilities = READ_DIR_ATTR|UNIX_BASED
		"0400000000000000" + // VolumeCapabilities = FULL_SYNC
		"00000000" + "10000000" + // Reserved / ModelString 字节数 = 16
		"4d0061006300530061006d00620061" + "00" // "MacSamba" UTF-16LE
)

// ---------------------------------------------------------------------------
// 被测路径的适配层
//
// 重构前后只有这两个函数换实现，golden 常量与用例表原样不动 ——
// 这正是"字节一致"这个结论的检验点。
// ---------------------------------------------------------------------------

// goldenParse 走一遍请求侧解析（注册表的 Parse 阶段），返回会让 CREATE
// 失败的错误。重构前后只有这一层换实现，golden 常量与用例表原样不动。
func goldenParse(t *testing.T, ctx *Context, req *wire.CreateRequest) error {
	t.Helper()
	if _, err := newCreateContexts(ctx, req); err != nil {
		return err
	}
	return nil
}

// goldenRespond 走一遍完整流程（注册表的 Parse + Respond 阶段）并返回响应
// context 链。Respond 需要一个完整的 *Open —— 但 QFid/MxAc/AAPL 这三个
// handler 只用 attr 与 ctx.Tree，不碰 open 的具体字段，故传最小实例即可。
func goldenRespond(t *testing.T, ctx *Context, req *wire.CreateRequest, attr *vfs.Attr) []wire.CreateContext {
	t.Helper()
	cc, err := newCreateContexts(ctx, req)
	if err != nil {
		t.Fatalf("newCreateContexts: %v", err)
	}
	resp := &wire.CreateResponse{}
	if err := cc.respond(ctx, &Open{}, resp, attr); err != nil {
		t.Fatalf("respond: %v", err)
	}
	return resp.Contexts
}
