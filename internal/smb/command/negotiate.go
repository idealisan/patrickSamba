package command

import (
	"crypto/rand"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// preauthSaltSize 是 3.1.1 PREAUTH_INTEGRITY_CAPABILITIES 里 Salt 的长度。
//
// MS-SMB2 §2.2.3.1.1 没有规定具体长度，Windows 实现用 32 字节，
// 这里对齐它（协议上只要求双方各自把对方的报文原样滚进 hash）。
const preauthSaltSize = 32

// cipherPreference 是本端对 3.1.1 加密算法的偏好顺序（MS-SMB2 §2.2.3.1.2）。
//
// GCM 优先于 CCM：两者安全性等价，但 GCM 在有 AES-NI 的机器上快一个数量级，
// 且 Windows / macOS 现代客户端默认也偏好 GCM。
var cipherPreference = []uint16{
	wire.CipherAES128GCM,
	wire.CipherAES256GCM,
	wire.CipherAES128CCM,
	wire.CipherAES256CCM,
}

func init() {
	// NEGOTIATE 是连接上的第一条命令，既没有会话也没有树。
	register(wire.CommandNegotiate, false, false, handleNegotiate)
}

// handleNegotiate 处理 SMB2 NEGOTIATE（MS-SMB2 §2.2.3 / §3.3.5.4）。
func handleNegotiate(ctx *Context) error {
	c := ctx.Conn
	set := c.Settings

	// MS-SMB2 §3.3.5.4：同一连接上重复 NEGOTIATE 必须回 STATUS_INVALID_PARAMETER。
	// 不这样做会给降级攻击留口子（协商完 3.1.1 再"重协商"回 2.0.2）。
	if c.NegotiateDone {
		ctx.Log.Warn("同一连接上重复 NEGOTIATE")
		return status.InvalidParameter
	}

	req, err := wire.ParseNegotiateRequest(ctx.Msg)
	if err != nil {
		ctx.Log.Warn("NEGOTIATE 请求解析失败", "err", err)
		return status.InvalidParameter
	}
	if len(req.Dialects) == 0 {
		return status.InvalidParameter
	}

	// ---- 方言协商：取双方交集中的最高方言（MS-SMB2 §3.3.5.4）----
	client := make([]dialect.Dialect, len(req.Dialects))
	for i, d := range req.Dialects {
		client[i] = dialect.Dialect(d)
	}
	d, ok := dialect.Negotiate(client, set.MinDialect, set.MaxDialect)
	if !ok {
		ctx.Log.Warn("没有可用的公共方言", "client", client,
			"server", dialect.Range(set.MinDialect, set.MaxDialect))
		return status.NotSupported
	}

	// 先定型方言：3.1.1 的 preauth hash 更新依赖它。
	c.Dialect = d
	c.ClientGUID = req.ClientGUID
	c.ClientCapabilities = req.Capabilities
	c.ClientSecurityMode = req.SecurityMode
	c.ClientDialects = client

	// MS-SMB2 §3.3.5.4：3.1.1 的 preauth hash 从全零开始，
	// 第一条滚进去的就是 NEGOTIATE Request 本身。
	c.UpdatePreauthHash(ctx.Msg)

	// ---- 签名算法策略 ----
	//
	// aes-gmac 只能通过 3.1.1 的 SIGNING_CAPABILITIES negotiate context
	// 协商出来（MS-SMB2 §2.2.3.1.7：该 context 仅在方言列表含 3.1.1 时有效）。
	// 客户端把方言压到 3.1.1 以下时显式失败、绝不静默降级回 CMAC/HMAC ——
	// 否则 aes-gmac 配置对"会压方言的客户端"形同虚设。
	if set.SigningPreference == SigningPreferAESGMAC && !d.SupportsNegotiateContexts() {
		ctx.Log.Warn("配置要求 AES-GMAC 签名但客户端协商到非 3.1.1 方言，拒绝协商",
			"dialect", d)
		return status.NotSupported
	}

	// ---- 签名策略 ----
	//
	// 只要任意一方要求签名，本连接就强制签名（MS-SMB2 §3.3.5.4）。
	c.SigningRequired = set.SigningRequired ||
		req.SecurityMode&wire.NegotiateSigningRequired != 0

	// 服务端始终宣告"支持签名"。要求与否由 SIGNING_REQUIRED 位表达。
	c.ServerSecurityMode = wire.NegotiateSigningEnabled
	if set.SigningRequired {
		c.ServerSecurityMode |= wire.NegotiateSigningRequired
	}

	// ---- 尺寸上限 ----
	maxSize := d.MaxTransactSize()
	c.MaxTransactSize = maxSize
	c.MaxReadSize = maxSize
	c.MaxWriteSize = maxSize

	// ---- 加密算法协商 ----
	//
	// 两条完全不同的路径，都必须走到，否则 encryption_required 会被降级绕过：
	//   - 3.1.1：走 SMB2_ENCRYPTION_CAPABILITIES negotiate context；
	//   - 3.0/3.0.2：没有 negotiate context，能力用 Capabilities 里的
	//     CAP_ENCRYPTION 位表达，算法固定为 AES-128-CCM（MS-SMB2 §3.3.5.4）。
	var respContexts []wire.NegotiateContext
	switch {
	case d.SupportsNegotiateContexts():
		respContexts, err = negotiateContexts(ctx, req)
		if err != nil {
			c.Dialect = 0 // 协商失败，连接回到未协商状态
			return err
		}
	case d.SupportsEncryption() && set.EncryptionEnabled &&
		uint32(req.Capabilities)&dialect.CapEncryption != 0:
		c.Cipher = wire.CipherAES128CCM
	}

	// 加密强制必须 fail closed。
	//
	// 曾经的缺口：上面的加密协商在 3.1.1 之前的方言里根本不产生 Cipher，
	// 而 session_setup 里 `EncryptionRequired && Cipher != 0` 才置
	// SessionFlagEncryptData —— 于是客户端只要在 NEGOTIATE 里把方言压到
	// 3.1.1 以下（`smbclient -m SMB2_10`），就能让 encryption_required
	// 静默失效，全程明文且服务端不拒绝、不告警。
	//
	// 2.0.2/2.1 属于"方言根本没有加密能力"，此处一律拒绝协商；
	// 3.0/3.0.2 若客户端没宣告 CAP_ENCRYPTION 也一样拒绝。
	if set.EncryptionRequired && c.Cipher == 0 {
		ctx.Log.Warn("配置要求加密但本连接协商不出加密算法，拒绝协商",
			"dialect", d,
			"dialect_supports_encryption", d.SupportsEncryption(),
			"client_caps", req.Capabilities)
		c.Dialect = 0
		return status.AccessDenied
	}

	encryption := c.Cipher != 0
	c.ServerCapabilities = wire.Capabilities(d.ServerCapabilities(encryption))

	now := time.Now()
	start := set.StartTime
	if start.IsZero() {
		start = now
	}

	resp := &wire.NegotiateResponse{
		SecurityMode:    c.ServerSecurityMode,
		DialectRevision: wire.Dialect(d),
		ServerGUID:      set.ServerGUID,
		Capabilities:    c.ServerCapabilities,
		MaxTransactSize: c.MaxTransactSize,
		MaxReadSize:     c.MaxReadSize,
		MaxWriteSize:    c.MaxWriteSize,
		SystemTime:      vfs.TimeToFiletime(now),
		ServerStartTime: vfs.TimeToFiletime(start),
		// SPNEGO negTokenInit2：宣告服务端支持的认证机制。
		SecurityBuffer: set.Auth.InitialToken(),
		Contexts:       respContexts,
	}

	out, err := resp.Append(ctx.Out)
	if err != nil {
		ctx.Log.Error("NEGOTIATE 响应编码失败", "err", err)
		c.Dialect = 0
		return status.InsufficientResources
	}
	ctx.Out = out

	// 响应字节也要滚进连接级 preauth hash —— 但必须等响应头回填之后，
	// 所以只在这里打标记，实际计算在 Dispatch 里做。
	ctx.HashResponseConn = true

	c.NegotiateDone = true
	ctx.Log.Info("SMB 方言协商完成",
		"dialect", d.String(),
		"signing_required", c.SigningRequired,
		"signing_algorithm", c.SigningAlg().String(),
		"cipher", c.Cipher,
		"max_transact", c.MaxTransactSize)
	return nil
}

// negotiateContexts 处理 3.1.1 的 negotiate context 协商，返回要回给客户端的
// context 列表（MS-SMB2 §2.2.3.1 / §3.3.5.4）。
func negotiateContexts(ctx *Context, req *wire.NegotiateRequest) ([]wire.NegotiateContext, error) {
	c := ctx.Conn
	set := c.Settings
	var out []wire.NegotiateContext

	preauthCtx, err := negotiatePreauth(ctx, req)
	if err != nil {
		return nil, err
	}
	out = append(out, preauthCtx)

	if encCtx, ok, err := negotiateCipher(ctx, req); err != nil {
		return nil, err
	} else if ok {
		out = append(out, encCtx)
	}

	// SIGNING_CAPABILITIES 协商（MS-SMB2 §2.2.3.1.7 / §3.3.5.4）。
	//
	// auto：刻意**不回应**该 context。MS-SMB2 §3.2.5.2：响应里没有它时，
	// 客户端把签名算法置为方言默认值（3.x 为 AES-CMAC），这正是
	// crypto.SigningAlgorithmForDialect 的行为，双方天然一致。
	// 客户端即便宣告支持 AES-GMAC 也按现状选 CMAC —— 默认配置对
	// 存量客户端零风险的机器保证（见 signing_negotiate_test.go 矩阵）。
	if set.SigningPreference == SigningAuto {
		if hasContext(req.Contexts, wire.ContextSigningCapabilities) {
			c.SigningAlgorithm = wire.SigningAlgorithmAESCMAC
		}
		return out, nil
	}

	sigCtx, err := negotiateSigning(ctx, req)
	if err != nil {
		return nil, err
	}
	if sigCtx.Type != 0 {
		out = append(out, sigCtx)
	}
	return out, nil
}

// negotiateSigning 在显式指定 aes-cmac / aes-gmac 时选择签名算法并构造
// 要回给客户端的 SIGNING_CAPABILITIES context（MS-SMB2 §3.3.5.4）。
//
// 规范语义与本实现的差异（team-lead 拍板的策略，注释留档）：
//   - §3.3.5.4 允许"无共同算法时服务端仍置 AES-CMAC"，那等于让客户端
//     收到它没宣告过的算法后自行断连 —— 静默降级。本端改为在协商阶段
//     直接回 STATUS_NOT_SUPPORTED（该节列明的 NEGOTIATE 合法状态码），
//     显式失败优于静默降级。
//   - aes-cmac 且客户端未提供该 context 时按方言默认值（就是 CMAC）放行、
//     不回应 context —— 与规范默认路径一致；aes-gmac 同情形则拒绝。
//
// 返回 Type==0 的 context 表示不需要回应（仅 aes-cmac 的缺省分支）。
func negotiateSigning(ctx *Context, req *wire.NegotiateRequest) (wire.NegotiateContext, error) {
	var zero wire.NegotiateContext
	c := ctx.Conn
	set := c.Settings

	data, ok := findContext(req.Contexts, wire.ContextSigningCapabilities)
	if !ok {
		if set.SigningPreference == SigningPreferAESGMAC {
			ctx.Log.Warn("配置要求 AES-GMAC 签名但客户端未提供 SIGNING_CAPABILITIES，拒绝协商")
			return zero, status.NotSupported
		}
		// aes-cmac：客户端没提 context，走方言默认值（3.1.1 即 CMAC），无需回应。
		return zero, nil
	}

	caps, err := wire.ParseSigningCapabilities(data)
	if err != nil || len(caps.SigningAlgorithms) == 0 {
		// MS-SMB2 §3.3.5.4：DataLength 小于结构体尺寸或 Count 为 0 时必须
		// STATUS_INVALID_PARAMETER。（auto 路径不解析、维持历史宽容行为。）
		ctx.Log.Warn("SIGNING_CAPABILITIES 解析失败", "err", err)
		return zero, status.InvalidParameter
	}

	want := wire.SigningAlgorithmAESCMAC
	if set.SigningPreference == SigningPreferAESGMAC {
		want = wire.SigningAlgorithmAESGMAC
	}
	if !containsU16(caps.SigningAlgorithms, want) {
		ctx.Log.Warn("客户端不支持配置要求的签名算法，拒绝协商",
			"required", want, "client_offered", caps.SigningAlgorithms)
		return zero, status.NotSupported
	}
	c.SigningAlgorithm = want

	payload, err := (&wire.SigningCapabilities{SigningAlgorithms: []uint16{want}}).Encode()
	if err != nil {
		return zero, status.InsufficientResources
	}
	return wire.NegotiateContext{
		Type: wire.ContextSigningCapabilities,
		Data: payload,
	}, nil
}

// negotiatePreauth 校验并回应 SMB2_PREAUTH_INTEGRITY_CAPABILITIES。
//
// MS-SMB2 §3.3.5.4：协商到 3.1.1 时该 context **必须**存在，
// 且双方必须至少有一个共同的哈希算法（目前规范只定义了 SHA-512）。
func negotiatePreauth(ctx *Context, req *wire.NegotiateRequest) (wire.NegotiateContext, error) {
	var zero wire.NegotiateContext

	data, ok := findContext(req.Contexts, wire.ContextPreauthIntegrityCapabilities)
	if !ok {
		ctx.Log.Warn("3.1.1 协商缺少 PREAUTH_INTEGRITY_CAPABILITIES")
		return zero, status.InvalidParameter
	}
	caps, err := wire.ParsePreauthIntegrityCapabilities(data)
	if err != nil {
		ctx.Log.Warn("PREAUTH_INTEGRITY_CAPABILITIES 解析失败", "err", err)
		return zero, status.InvalidParameter
	}
	if !containsU16(caps.HashAlgorithms, wire.HashAlgorithmSHA512) {
		ctx.Log.Warn("客户端不支持 SHA-512 preauth 哈希", "algorithms", caps.HashAlgorithms)
		return zero, status.SMBNoPreauthIntegrityHashOverlap
	}

	salt := make([]byte, preauthSaltSize)
	if _, err := rand.Read(salt); err != nil {
		ctx.Log.Error("生成 preauth salt 失败", "err", err)
		return zero, status.InsufficientResources
	}

	payload, err := (&wire.PreauthIntegrityCapabilities{
		HashAlgorithms: []uint16{wire.HashAlgorithmSHA512},
		Salt:           salt,
	}).Encode()
	if err != nil {
		return zero, status.InsufficientResources
	}
	return wire.NegotiateContext{
		Type: wire.ContextPreauthIntegrityCapabilities,
		Data: payload,
	}, nil
}

// negotiateCipher 选择 3.1.1 的加密算法。
//
// 返回 ok=false 表示不需要回 ENCRYPTION_CAPABILITIES（客户端没提，或本端
// 没开加密）。MS-SMB2 §3.3.5.4：客户端提了但没有共同算法时，服务端要回一个
// Ciphers 为空（等价于 SMB2_ENCRYPTION_NONE）的 context，而不是直接失败。
func negotiateCipher(ctx *Context, req *wire.NegotiateRequest) (wire.NegotiateContext, bool, error) {
	var zero wire.NegotiateContext
	c := ctx.Conn
	set := c.Settings

	data, ok := findContext(req.Contexts, wire.ContextEncryptionCapabilities)
	if !ok {
		if set.EncryptionRequired {
			ctx.Log.Warn("配置要求加密但客户端未提供 ENCRYPTION_CAPABILITIES")
			return zero, false, status.AccessDenied
		}
		return zero, false, nil
	}
	if !set.EncryptionEnabled {
		// 本端没开加密：不回该 context，客户端会当作服务端不支持。
		return zero, false, nil
	}

	caps, err := wire.ParseEncryptionCapabilities(data)
	if err != nil {
		ctx.Log.Warn("ENCRYPTION_CAPABILITIES 解析失败", "err", err)
		return zero, false, status.InvalidParameter
	}

	chosen := uint16(0)
	for _, want := range cipherPreference {
		if containsU16(caps.Ciphers, want) {
			chosen = want
			break
		}
	}
	if chosen == 0 {
		if set.EncryptionRequired {
			ctx.Log.Warn("配置要求加密但与客户端无共同算法", "client", caps.Ciphers)
			return zero, false, status.AccessDenied
		}
		ctx.Log.Warn("与客户端无共同加密算法，本会话不加密", "client", caps.Ciphers)
	}
	c.Cipher = chosen

	payload, err := (&wire.EncryptionCapabilities{Ciphers: []uint16{chosen}}).Encode()
	if err != nil {
		return zero, false, status.InsufficientResources
	}
	return wire.NegotiateContext{
		Type: wire.ContextEncryptionCapabilities,
		Data: payload,
	}, true, nil
}

// findContext 返回第一个指定类型的 negotiate context 载荷。
func findContext(ctxs []wire.NegotiateContext, typ wire.NegotiateContextType) ([]byte, bool) {
	for _, c := range ctxs {
		if c.Type == typ {
			return c.Data, true
		}
	}
	return nil, false
}

// hasContext 报告是否存在指定类型的 negotiate context。
func hasContext(ctxs []wire.NegotiateContext, typ wire.NegotiateContextType) bool {
	_, ok := findContext(ctxs, typ)
	return ok
}

// containsU16 报告 list 中是否含有 v。
func containsU16(list []uint16, v uint16) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
