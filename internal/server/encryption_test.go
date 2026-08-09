package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// newEncTestConn 构造一条只改了加密策略的完整连接。
func newEncTestConn(t *testing.T, required bool) *Connection {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := auth.NewStaticStore(config.Auth{
		Users: []config.User{{Name: "alice", Password: "secret"}},
	}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造账户库失败: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: t.TempDir(), VolumeLabel: "data"})
	if err != nil {
		t.Fatalf("构造 VFS 失败: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	settings := &command.Settings{
		ServerName:         "TESTSRV",
		Domain:             "WORKGROUP",
		StartTime:          time.Now(),
		MinDialect:         dialect.SMB202,
		MaxDialect:         dialect.SMB311,
		EncryptionEnabled:  true,
		EncryptionRequired: required,
		Auth: auth.NewNTLMProvider(auth.Options{
			Store: store, ServerName: "TESTSRV", DomainName: "WORKGROUP",
		}),
		Shares: []*command.Share{
			{Name: "data", Type: wire.ShareTypeDisk, FS: fs},
			{Name: command.IPCShareName, Type: wire.ShareTypePipe},
		},
		Logger: log,
	}

	srv, err := New(Options{Settings: settings, Logger: log})
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	return &Connection{
		srv:     srv,
		log:     log,
		state:   command.NewConn(settings, "test", "test"),
		credits: NewCredits(0),
	}
}

// negotiateFrame 拼一条 NEGOTIATE 请求：dialects 是客户端声称支持的方言，
// caps 是 Capabilities（3.0/3.0.2 靠 CAP_ENCRYPTION 位表达加密能力），
// withEncCtx 控制是否携带 SMB2_ENCRYPTION_CAPABILITIES negotiate context。
func negotiateFrame(t *testing.T, dialects []wire.Dialect, caps uint32, withEncCtx bool) []byte {
	t.Helper()

	req := &wire.NegotiateRequest{
		Dialects:     dialects,
		SecurityMode: wire.NegotiateSigningEnabled,
		Capabilities: wire.Capabilities(caps),
		ClientGUID:   [16]byte{1, 2, 3, 4},
	}
	if withEncCtx {
		// 3.1.1 的 preauth context 是必填项（MS-SMB2 §3.3.5.4），
		// 缺了它服务端会直接拒绝协商，测不到加密这一层。
		pre, err := (&wire.PreauthIntegrityCapabilities{
			HashAlgorithms: []uint16{wire.HashAlgorithmSHA512},
			Salt:           make([]byte, 32),
		}).Encode()
		if err != nil {
			t.Fatalf("编码 PREAUTH context 失败: %v", err)
		}
		enc, err := (&wire.EncryptionCapabilities{
			Ciphers: []uint16{wire.CipherAES128GCM, wire.CipherAES128CCM},
		}).Encode()
		if err != nil {
			t.Fatalf("编码 ENCRYPTION context 失败: %v", err)
		}
		req.Contexts = []wire.NegotiateContext{
			{Type: wire.ContextPreauthIntegrityCapabilities, Data: pre},
			{Type: wire.ContextEncryptionCapabilities, Data: enc},
		}
	}

	hdr := wire.Header{Command: wire.CommandNegotiate, Credits: 1}
	out := hdr.Append(nil)
	out, err := req.Append(out)
	if err != nil {
		t.Fatalf("编码 NEGOTIATE 请求失败: %v", err)
	}
	return out
}

// TestEncryptionRequiredRejectsDowngradedDialect 覆盖一个真实的**降级绕过**：
//
// `encryption_required: true` 曾经只在协商到 3.1.1 时才真的生效，因为加密算法
// 只在 negotiate context 里协商，而 negotiate context 只有 3.1.1 才有。客户端
// 一句 `smbclient -m SMB2_10` 把方言压低，服务端就静默降级成明文放行 ——
// 既不拒绝也不告警，配置形同虚设。
//
// 现在的语义是 fail closed：协商不出加密算法就拒绝协商。
func TestEncryptionRequiredRejectsDowngradedDialect(t *testing.T) {
	cases := []struct {
		name     string
		dialects []wire.Dialect
		caps     uint32
		encCtx   bool
	}{
		{"2.0.2 无加密能力", []wire.Dialect{wire.Dialect(dialect.SMB202)}, 0, false},
		{"2.1 无加密能力", []wire.Dialect{wire.Dialect(dialect.SMB210)}, 0, false},
		{"3.0 但未宣告 CAP_ENCRYPTION", []wire.Dialect{wire.Dialect(dialect.SMB300)}, 0, false},
		{"3.0.2 但未宣告 CAP_ENCRYPTION", []wire.Dialect{wire.Dialect(dialect.SMB302)}, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newEncTestConn(t, true)
			resp, err := c.handleSMB2Chain(negotiateFrame(t, tc.dialects, tc.caps, tc.encCtx))
			if err != nil {
				t.Fatalf("处理 NEGOTIATE 出错: %v", err)
			}
			if got := respStatus(t, resp); got != status.AccessDenied {
				t.Fatalf("要求加密时低方言协商应回 STATUS_ACCESS_DENIED，实际 %s", got)
			}
			if c.state.Cipher != 0 {
				t.Fatalf("被拒的协商不应留下 Cipher=%#x", c.state.Cipher)
			}
		})
	}
}

// TestEncryptionRequiredAcceptsSMB30WithCapEncryption：3.0/3.0.2 是**有**加密
// 能力的，只是走 CAP_ENCRYPTION 位而不是 negotiate context。之前这条路没接上，
// 导致这类客户端明文畅通；接上之后它们应当正常协商出 AES-128-CCM
// （MS-SMB2 §3.3.5.4：3.1.1 之前的方言算法固定为 AES-128-CCM）。
func TestEncryptionRequiredAcceptsSMB30WithCapEncryption(t *testing.T) {
	for _, d := range []dialect.Dialect{dialect.SMB300, dialect.SMB302} {
		t.Run(d.String(), func(t *testing.T) {
			c := newEncTestConn(t, true)
			frame := negotiateFrame(t, []wire.Dialect{wire.Dialect(d)}, dialect.CapEncryption, false)
			resp, err := c.handleSMB2Chain(frame)
			if err != nil {
				t.Fatalf("处理 NEGOTIATE 出错: %v", err)
			}
			if got := respStatus(t, resp); got != status.Success {
				t.Fatalf("3.0 宣告 CAP_ENCRYPTION 时应协商成功，实际 %s", got)
			}
			if c.state.Cipher != wire.CipherAES128CCM {
				t.Fatalf("3.0 的加密算法应为 AES-128-CCM，实际 %#x", c.state.Cipher)
			}
			if uint32(c.state.ServerCapabilities)&dialect.CapEncryption == 0 {
				t.Fatal("协商出加密后服务端应回宣 CAP_ENCRYPTION")
			}
		})
	}
}

// TestEncryptionNotRequiredStillAllowsLowDialect：不要求加密时低方言照常放行，
// 免得 fail closed 的修法误伤默认配置（默认 encryption_required=false）。
func TestEncryptionNotRequiredStillAllowsLowDialect(t *testing.T) {
	c := newEncTestConn(t, false)
	resp, err := c.handleSMB2Chain(negotiateFrame(t,
		[]wire.Dialect{wire.Dialect(dialect.SMB210)}, 0, false))
	if err != nil {
		t.Fatalf("处理 NEGOTIATE 出错: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("不要求加密时 2.1 应协商成功，实际 %s", got)
	}
	if c.state.Cipher != 0 {
		t.Fatalf("2.1 没有加密能力，Cipher 应为 0，实际 %#x", c.state.Cipher)
	}
}
