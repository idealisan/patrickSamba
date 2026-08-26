package command

import (
	"bytes"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件是 server.signing_algorithm 配置 × 客户端 SIGNING_CAPABILITIES
// offer 集合的**协商矩阵**（MS-SMB2 §2.2.3.1.7 / §3.3.5.4）。
// 每一行都钉死一个策略结论，防止将来有人把 auto 悄悄改成"跟随客户端选 GMAC"。

// signingNegotiateReq 构造一条 3.1.1 NEGOTIATE Request（含必需的
// PREAUTH_INTEGRITY context），signing 为 nil 表示不带 SIGNING_CAPABILITIES。
func signingNegotiateReq(t *testing.T, dialects []uint16, signing []uint16) []byte {
	t.Helper()

	ctxs := []wire.NegotiateContext{{
		Type: wire.ContextPreauthIntegrityCapabilities,
		Data: mustPreauthPayload(t),
	}}
	if signing != nil {
		payload, err := (&wire.SigningCapabilities{SigningAlgorithms: signing}).Encode()
		if err != nil {
			t.Fatalf("编码 SIGNING_CAPABILITIES 失败: %v", err)
		}
		ctxs = append(ctxs, wire.NegotiateContext{
			Type: wire.ContextSigningCapabilities, Data: payload,
		})
	}

	req := &wire.NegotiateRequest{
		Dialects:     make([]wire.Dialect, len(dialects)),
		SecurityMode: wire.NegotiateSigningEnabled,
		Contexts:     ctxs,
	}
	for i, d := range dialects {
		req.Dialects[i] = wire.Dialect(d)
	}
	body, err := req.Append(nil)
	if err != nil {
		t.Fatalf("编码 NEGOTIATE 失败: %v", err)
	}

	hdr := wire.Header{
		Command:   wire.CommandNegotiate,
		Credits:   1,
		MessageID: 0,
	}
	msg := hdr.Append(nil)
	return append(msg, body...)
}

// mustPreauthPayload 编码一个客户端侧 PREAUTH_INTEGRITY 载荷（3.1.1 必需）。
func mustPreauthPayload(t *testing.T) []byte {
	t.Helper()
	payload, err := (&wire.PreauthIntegrityCapabilities{
		HashAlgorithms: []uint16{wire.HashAlgorithmSHA512},
		Salt:           bytes.Repeat([]byte{0xA5}, 32),
	}).Encode()
	if err != nil {
		t.Fatalf("编码 PREAUTH 失败: %v", err)
	}
	return payload
}

// runNegotiate 用给定配置跑一次 handleNegotiate，返回 (status, 响应字节, 连接)。
func runNegotiate(t *testing.T, pref SigningPreference, msg []byte) (status.Status, []byte, *Conn) {
	t.Helper()

	conn := NewConn(&Settings{
		ServerName:        "TEST",
		Domain:            "WORKGROUP",
		MinDialect:        dialect.SMB202,
		MaxDialect:        dialect.SMB311,
		SigningPreference: pref,
		Auth: auth.NewNTLMProvider(auth.Options{ServerName: "TEST", DomainName: "WORKGROUP"}),
	}, "client", "server")

	hdr := wire.Header{Command: wire.CommandNegotiate, Credits: 1}
	ctx := NewContext(conn, &Chain{}, hdr, msg, nil)
	err := handleNegotiate(ctx)
	if err == nil {
		return status.Success, ctx.Out, conn
	}
	if st, ok := err.(status.Status); ok {
		return st, ctx.Out, conn
	}
	t.Fatalf("handleNegotiate 返回非 Status 错误: %v", err)
	return 0, nil, nil
}

// respSigningAlg 从 NEGOTIATE Response 里取服务端选择的签名算法；
// 不存在该 context 时第二个返回值为 false。
func respSigningAlg(t *testing.T, resp []byte) (uint16, bool) {
	t.Helper()
	r, err := wire.ParseNegotiateResponse(resp)
	if err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for _, c := range r.Contexts {
		if c.Type == wire.ContextSigningCapabilities {
			caps, err := wire.ParseSigningCapabilities(c.Data)
			if err != nil || len(caps.SigningAlgorithms) != 1 {
				t.Fatalf("响应 SIGNING_CAPABILITIES 非法: %v %+v", err, caps)
			}
			return caps.SigningAlgorithms[0], true
		}
	}
	return 0, false
}

// TestSigningNegotiateMatrix：配置 × 客户端 offer → 选择结果。
//
// 关键行：
//   - auto 下客户端 offer 里带 GMAC 也**必须仍选 CMAC**（默认行为零变化）；
//   - aes-gmac 下 offer 缺 GMAC / 未提供 context / 方言低于 3.1.1 一律显式失败
//     （STATUS_NOT_SUPPORTED，绝不静默降级）。
func TestSigningNegotiateMatrix(t *testing.T) {
	const (
		cmac = wire.SigningAlgorithmAESCMAC
		gmac = wire.SigningAlgorithmAESGMAC
		hmac = wire.SigningAlgorithmHMACSHA256
	)
	d311 := []uint16{0x0311, 0x0300, 0x0210} // 含 3.1.1 的常规方言列表
	d300 := []uint16{0x0300, 0x0210}         // 压到 3.0.2 及以下

	cases := []struct {
		name string
		pref SigningPreference

		dialects    []uint16 // 客户端方言列表
		signOffer   []uint16 // 客户端 SIGNING_CAPABILITIES；nil=未提供
		wantStatus  status.Status
		wantRespAlg uint16                  // wantHasCtx=false 时忽略
		wantHasCtx  bool                    // 响应是否带 SIGNING_CAPABILITIES
		wantConnAlg crypto.SigningAlgorithm // 成功时连接绑定的算法
	}{
		// ---- auto：与历史行为完全一致 ----
		{
			name: "auto/offer[cmac,hmac]/不回应context仍选cmac",
			pref: SigningAuto, dialects: d311, signOffer: []uint16{cmac, hmac},
			wantStatus: status.Success, wantHasCtx: false,
			wantConnAlg: crypto.SigningAESCMAC,
		},
		{
			name: "auto/offer[hmac,gmac]/带gmac也必须仍选cmac",
			pref: SigningAuto, dialects: d311, signOffer: []uint16{hmac, gmac},
			wantStatus: status.Success, wantHasCtx: false,
			wantConnAlg: crypto.SigningAESCMAC,
		},
		{
			name: "auto/无context/默认cmac",
			pref: SigningAuto, dialects: d311, signOffer: nil,
			wantStatus: status.Success, wantHasCtx: false,
			wantConnAlg: crypto.SigningAESCMAC,
		},
		// ---- aes-cmac ----
		{
			name: "aes-cmac/offer含cmac/回应并钉死cmac",
			pref: SigningPreferAESCMAC, dialects: d311, signOffer: []uint16{gmac, cmac},
			wantStatus: status.Success, wantHasCtx: true, wantRespAlg: cmac,
			wantConnAlg: crypto.SigningAESCMAC,
		},
		{
			name: "aes-cmac/offer缺cmac/显式失败",
			pref: SigningPreferAESCMAC, dialects: d311, signOffer: []uint16{gmac, hmac},
			wantStatus: status.NotSupported, wantHasCtx: false,
		},
		{
			name: "aes-cmac/无context/按方言默认放行且不回应",
			pref: SigningPreferAESCMAC, dialects: d311, signOffer: nil,
			wantStatus: status.Success, wantHasCtx: false,
			wantConnAlg: crypto.SigningAESCMAC,
		},
		// ---- aes-gmac ----
		{
			name: "aes-gmac/offer含gmac/回应并钉死gmac",
			pref: SigningPreferAESGMAC, dialects: d311, signOffer: []uint16{gmac, cmac},
			wantStatus: status.Success, wantHasCtx: true, wantRespAlg: gmac,
			wantConnAlg: crypto.SigningAESGMAC,
		},
		{
			name: "aes-gmac/offer仅cmac+hmac/显式失败不降级",
			pref: SigningPreferAESGMAC, dialects: d311, signOffer: []uint16{cmac, hmac},
			wantStatus: status.NotSupported, wantHasCtx: false,
		},
		{
			name: "aes-gmac/无context/显式失败",
			pref: SigningPreferAESGMAC, dialects: d311, signOffer: nil,
			wantStatus: status.NotSupported, wantHasCtx: false,
		},
		{
			name: "aes-gmac/方言压到3.0.2/显式失败",
			pref: SigningPreferAESGMAC, dialects: d300, signOffer: []uint16{gmac, cmac},
			wantStatus: status.NotSupported, wantHasCtx: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := signingNegotiateReq(t, tc.dialects, tc.signOffer)
			st, resp, conn := runNegotiate(t, tc.pref, msg)

			if st != tc.wantStatus {
				t.Fatalf("协商结果 = %s，期望 %s", st, tc.wantStatus)
			}
			// 失败路径：分发器会丢弃半成品响应改发 ERROR Response，
			// 这里只断言连接回到未协商状态。
			if tc.wantStatus != status.Success {
				if conn.NegotiateDone || conn.Dialect != 0 {
					t.Fatalf("失败的协商必须把连接打回未协商状态，实际 dialect=%v done=%v",
						conn.Dialect, conn.NegotiateDone)
				}
				return
			}
			gotAlg, hasCtx := respSigningAlg(t, resp)
			if hasCtx != tc.wantHasCtx {
				t.Fatalf("响应含 SIGNING_CAPABILITIES = %v，期望 %v", hasCtx, tc.wantHasCtx)
			}
			if tc.wantHasCtx && gotAlg != tc.wantRespAlg {
				t.Fatalf("响应选择算法 = %#x，期望 %#x", gotAlg, tc.wantRespAlg)
			}
			if got := conn.SigningAlg(); got != tc.wantConnAlg {
				t.Fatalf("连接绑定算法 = %v，期望 %v", got, tc.wantConnAlg)
			}
			if !conn.NegotiateDone || conn.Dialect != dialect.SMB311 {
				t.Fatalf("连接应处于 3.1.1 已协商状态，实际 dialect=%v done=%v",
					conn.Dialect, conn.NegotiateDone)
			}
		})
	}
}

// TestSigningNegotiateMalformedContext：MS-SMB2 §3.3.5.4 —— 显式策略下
// SigningAlgorithmCount==0 的 SIGNING_CAPABILITIES 必须 STATUS_INVALID_PARAMETER；
// auto 维持历史宽容行为（整个 context 忽略，协商照常成功）。
func TestSigningNegotiateMalformedContext(t *testing.T) {
	bad := []byte{0x00, 0x00} // Count=0

	buildMsg := func(t *testing.T) []byte {
		t.Helper()
		ctxs := []wire.NegotiateContext{
			{Type: wire.ContextPreauthIntegrityCapabilities, Data: mustPreauthPayload(t)},
			{Type: wire.ContextSigningCapabilities, Data: bad},
		}
		req := &wire.NegotiateRequest{
			Dialects:     []wire.Dialect{0x0311, 0x0300},
			SecurityMode: wire.NegotiateSigningEnabled,
			Contexts:     ctxs,
		}
		body, err := req.Append(nil)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		return append(wire.Header{Command: wire.CommandNegotiate}.Append(nil), body...)
	}

	msg := buildMsg(t)
	if st, _, _ := runNegotiate(t, SigningAuto, msg); st != status.Success {
		t.Fatalf("auto 应忽略畸形 context 并成功，实际 %s", st)
	}
	msg = buildMsg(t)
	if st, _, _ := runNegotiate(t, SigningPreferAESGMAC, msg); st != status.InvalidParameter {
		t.Fatalf("aes-gmac 对畸形 context 应回 INVALID_PARAMETER，实际 %s", st)
	}
	msg = buildMsg(t)
	if st, _, _ := runNegotiate(t, SigningPreferAESCMAC, msg); st != status.InvalidParameter {
		t.Fatalf("aes-cmac 对畸形 context 应回 INVALID_PARAMETER，实际 %s", st)
	}
}
