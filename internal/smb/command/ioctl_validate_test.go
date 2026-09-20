package command

import (
	"io"
	"log/slog"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/dialect"
	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// TestValidateNegotiateInfoSMB21 是回归测试，对应 ioctl.go 里对
// FSCTL_VALIDATE_NEGOTIATE_INFO 的处理放宽。
//
// 背景：smbclient 4.22 在 SMB 2.1 上也发这个 IOCTL，以前服务端对 < 3.0 的
// 方言回 STATUS_FILE_CLOSED，smbclient 收到后放弃整条连接（表现为
// "tree connect failed"），导致 §2 要求的 SMB 2.0.2 / 2.1 文件共享对 smbclient
// 不可用。据 AGENTS.md §9「真实客户端行为优先于规范」，服务端现在对所有已协商
// 方言正常复核并回显（3.1.1 仍回 FILE_CLOSED，因为 preauth hash 已覆盖该威胁）。
func TestValidateNegotiateInfoSMB21(t *testing.T) {
	c := newValidateTestConn(t, dialect.SMB210)

	// 客户端重放它当初 NEGOTIATE 时发出的方言列表（忠实重放）。
	replayed := make([]wire.Dialect, len(c.ClientDialects))
	for i, d := range c.ClientDialects {
		replayed[i] = wire.Dialect(uint16(d))
	}
	in := &wire.ValidateNegotiateInfoRequest{
		Capabilities: c.ClientCapabilities,
		ClientGUID:   c.ClientGUID,
		SecurityMode: c.ClientSecurityMode,
		Dialects:     replayed,
	}
	input, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLValidateNegotiateInfo,
		Flags:             wire.IoctlIsFSCTL,
		MaxOutputResponse: 4096,
		Input:             input,
	}

	ctx := &Context{Conn: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ioctlValidateNegotiate(ctx, req); err != nil {
		t.Fatalf("ioctlValidateNegotiate 应成功, 实际 err=%v", err)
	}

	// ctx.Out 是 IOCTL Response 报文体（不含 64 字节 SMB2 头），且 Output 位于
	// 末尾（无 Input），直接取尾部固定长度解析。
	if len(ctx.Out) < wire.ValidateNegotiateInfoResponseSize {
		t.Fatalf("响应长度 = %d, 太短", len(ctx.Out))
	}
	out := ctx.Out[len(ctx.Out)-wire.ValidateNegotiateInfoResponseSize:]
	vni, err := wire.ParseValidateNegotiateInfoResponse(out)
	if err != nil {
		t.Fatalf("ParseValidateNegotiateInfoResponse: %v", err)
	}
	if vni.Dialect != wire.Dialect(dialect.SMB210) {
		t.Errorf("回显 Dialect = %#x, 期望 %#x", vni.Dialect, dialect.SMB210)
	}
	if vni.Capabilities != c.ServerCapabilities {
		t.Errorf("回显 Capabilities = %#x, 期望 %#x", vni.Capabilities, c.ServerCapabilities)
	}
	if vni.SecurityMode != c.ServerSecurityMode {
		t.Errorf("回显 SecurityMode = %#x, 期望 %#x", vni.SecurityMode, c.ServerSecurityMode)
	}
}

// TestValidateNegotiateInfoSMB311Rejected：3.1.1 上收到必须回 STATUS_FILE_CLOSED
// （preauth integrity hash 已覆盖降级威胁，客户端本就不该发）。
func TestValidateNegotiateInfoSMB311Rejected(t *testing.T) {
	c := newValidateTestConn(t, dialect.SMB311)
	in := &wire.ValidateNegotiateInfoRequest{
		Capabilities: c.ClientCapabilities,
		ClientGUID:   c.ClientGUID,
		SecurityMode: c.ClientSecurityMode,
		Dialects:     []wire.Dialect{0x0311},
	}
	input, _ := in.Encode()
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLValidateNegotiateInfo,
		Flags:             wire.IoctlIsFSCTL,
		MaxOutputResponse: 4096,
		Input:             input,
	}
	ctx := &Context{Conn: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ioctlValidateNegotiate(ctx, req); err != status.FileClosed {
		t.Errorf("err = %v, 期望 %v", err, status.FileClosed)
	}
}

// TestValidateNegotiateInfoMismatch：客户端重放的方言与服务端记录不一致
// （疑似降级攻击）→ STATUS_ACCESS_DENIED。
func TestValidateNegotiateInfoMismatch(t *testing.T) {
	c := newValidateTestConn(t, dialect.SMB300)
	in := &wire.ValidateNegotiateInfoRequest{
		Capabilities: c.ClientCapabilities,
		ClientGUID:   c.ClientGUID,
		SecurityMode: c.ClientSecurityMode,
		Dialects:     []wire.Dialect{0x0202}, // 与记录的 [0x0202,0x0210,0x0300] 不符
	}
	input, _ := in.Encode()
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLValidateNegotiateInfo,
		Flags:             wire.IoctlIsFSCTL,
		MaxOutputResponse: 4096,
		Input:             input,
	}
	ctx := &Context{Conn: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ioctlValidateNegotiate(ctx, req); err != status.AccessDenied {
		t.Errorf("err = %v, 期望 %v", err, status.AccessDenied)
	}
}

// TestValidateNegotiateInfoDialectSubsetReordered（bh5-F5）：规范允许客户端
// 重放**裁剪或重排**后的方言列表，只要最大公共方言不变（MS-SMB2 §3.3.5.15.12；
// Samba smbd_smb2_protocol_dialect_match，smb2_ioctl_network_fs.c:533-620）。
// 整表顺序相等的旧算法把这类合法客户端误判成降级攻击回 ACCESS_DENIED。
func TestValidateNegotiateInfoDialectSubsetReordered(t *testing.T) {
	c := newValidateTestConn(t, dialect.SMB210)

	in := &wire.ValidateNegotiateInfoRequest{
		Capabilities: c.ClientCapabilities,
		ClientGUID:   c.ClientGUID,
		SecurityMode: c.ClientSecurityMode,
		Dialects:     []wire.Dialect{0x0210, 0x0202}, // 与记录的顺序相反
	}
	input, _ := in.Encode()
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLValidateNegotiateInfo,
		Flags:             wire.IoctlIsFSCTL,
		MaxOutputResponse: 4096,
		Input:             input,
	}
	ctx := &Context{Conn: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ioctlValidateNegotiate(ctx, req); err != nil {
		t.Fatalf("最大公共方言未变的重放应通过, 实际 err=%v", err)
	}
}

// TestValidateNegotiateInfoMaxCommonMismatch（bh5-F5）：最大公共方言变了才是
// 降级/升级篡改，必须拒绝 —— 无论列表怎么裁剪。
func TestValidateNegotiateInfoMaxCommonMismatch(t *testing.T) {
	cases := []struct {
		name     string
		dialects []wire.Dialect
	}{
		{"只剩更低的方言", []wire.Dialect{0x0202}},         // best=2.0.2 ≠ 2.1
		{"混入更高的方言", []wire.Dialect{0x0202, 0x0300}}, // best=3.0 ≠ 2.1
		{"只发更高方言", []wire.Dialect{0x0311}},          // best=3.1.1 ≠ 2.1
		{"空列表", nil}, // 无公共方言
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newValidateTestConn(t, dialect.SMB210)
			in := &wire.ValidateNegotiateInfoRequest{
				Capabilities: c.ClientCapabilities,
				ClientGUID:   c.ClientGUID,
				SecurityMode: c.ClientSecurityMode,
				Dialects:     tc.dialects,
			}
			input, _ := in.Encode()
			req := &wire.IoctlRequest{
				CtlCode:           wire.FSCTLValidateNegotiateInfo,
				Flags:             wire.IoctlIsFSCTL,
				MaxOutputResponse: 4096,
				Input:             input,
			}
			ctx := &Context{Conn: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := ioctlValidateNegotiate(ctx, req); err != status.AccessDenied {
				t.Errorf("err = %v, 期望 %v", err, status.AccessDenied)
			}
		})
	}
}

// newValidateTestConn 造一个「协商已完成」的连接桩，只填充
// ioctlValidateNegotiate 需要的字段（其余字段真实协商时由 handleNegotiate 填）。
//
// ClientDialects 取「本端支持且 ≤ 协商结果」的完整列表，与真实 NEGOTIATE
// 的自洽性一致：真实客户端重放自己发过的列表时，最大公共方言必然等于
// 协商出的那个（bh5-F5 之前这里存的是与协商结果矛盾的桩数据）。
func newValidateTestConn(t *testing.T, d dialect.Dialect) *Conn {
	t.Helper()
	var guid [16]byte
	copy(guid[:], []byte("0123456789abcdef"))
	c := NewConn(&Settings{ServerGUID: guid}, "test", "test")
	c.Dialect = d
	c.ClientGUID = guid
	c.ClientCapabilities = wire.Capabilities(0x00000001)
	c.ClientSecurityMode = wire.NegotiateSigningEnabled
	for _, x := range dialect.All {
		if x <= d {
			c.ClientDialects = append(c.ClientDialects, x)
		}
	}
	// 这些值由 NEGOTIATE handler 在真实协商时填充。
	c.ServerCapabilities = wire.Capabilities(0x00000001)
	c.ServerSecurityMode = wire.NegotiateSigningEnabled
	return c
}
