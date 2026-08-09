package auth

// NTLMSSP 服务端 Provider —— 把 SPNEGO 外壳、NTLM 报文层与 NTLMv2 校验
// 拼成 auth.Provider / auth.Context。
//
// 多轮握手（MS-SMB2 §3.3.5.5）：
//
//	客户端 NEGOTIATE(Type1) → 服务端 CHALLENGE(Type2)  [done=false]
//	客户端 AUTHENTICATE(Type3) → 成功                   [done=true]
//
// done=false 由 server 层翻译成 STATUS_MORE_PROCESSING_REQUIRED。

import (
	"crypto/rand"
	"crypto/subtle"
	"io"
	"time"
)

// Options 配置 NTLMSSP Provider。
type Options struct {
	// Store 是账户后端。为 nil 时只能进行匿名认证。
	Store AccountStore

	// ServerName 是 NetBIOS 计算机名，进 CHALLENGE 的 TargetName
	// 与 MsvAvNbComputerName。
	ServerName string
	// DomainName 是 NetBIOS 工作组/域名，进 MsvAvNbDomainName。
	DomainName string
	// DNSComputerName / DNSDomainName 进对应的 AV_PAIR。
	// 留空则退化为 ServerName / DomainName。
	DNSComputerName string
	DNSDomainName   string

	// AllowAnonymous 允许匿名（null）会话。默认关闭。
	AllowAnonymous bool

	// Rand 是随机源，nil 表示 crypto/rand.Reader。仅测试需要注入。
	Rand io.Reader
	// Now 返回当前时间，nil 表示 time.Now。仅测试需要注入。
	Now func() time.Time
}

// NTLMProvider 实现 Provider。构造后只读，并发安全。
type NTLMProvider struct {
	opts    Options
	initial []byte
}

var _ Provider = (*NTLMProvider)(nil)

// NewNTLMProvider 创建 NTLMSSP(+SPNEGO) 认证提供者。
func NewNTLMProvider(opts Options) *NTLMProvider {
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DNSComputerName == "" {
		opts.DNSComputerName = opts.ServerName
	}
	if opts.DNSDomainName == "" {
		opts.DNSDomainName = opts.DomainName
	}
	return &NTLMProvider{
		opts: opts,
		// 只宣告 NTLMSSP：本项目不做 Kerberos（依赖系统 krb5 配置会违反 C8）。
		initial: NegTokenInit2([][]byte{oidNTLMSSP}),
	}
}

// Name 返回机制名，用于日志。
func (p *NTLMProvider) Name() string { return "ntlmssp" }

// InitialToken 返回 SMB2 NEGOTIATE Response 里的 SPNEGO negTokenInit2。
//
// 返回的切片是内部缓冲的副本，调用方可自由持有。
func (p *NTLMProvider) InitialToken() []byte {
	out := make([]byte, len(p.initial))
	copy(out, p.initial)
	return out
}

// NewContext 创建一次新的认证会话。
func (p *NTLMProvider) NewContext() Context { return &ntlmContext{p: p} }

// ntlmContext 是一次会话的握手状态。非并发安全（由单个会话串行使用）。
type ntlmContext struct {
	p *NTLMProvider

	// spnego 表示客户端使用了 SPNEGO 外壳（否则是裸 NTLMSSP）。
	spnego bool
	// mechSent 表示已经在 negTokenResp 里回过 supportedMech。
	mechSent bool

	// negotiateMsg / challengeMsg 是 MIC 计算需要的原始 NTLM 报文字节。
	negotiateMsg    []byte
	challengeMsg    []byte
	serverChallenge [8]byte

	// clientMechListDER 是客户端 negTokenInit 里 MechTypeList 的完整 DER，
	// 计算 / 校验 SPNEGO mechListMIC 的输入就是它（RFC 4178 §5）。
	clientMechListDER []byte
	// clientMechListMIC 是客户端在最终 negTokenResp 里带的 mechListMIC（可能为空）。
	clientMechListMIC []byte

	identity   *Identity
	sessionKey []byte
	done       bool
}

var _ Context = (*ntlmContext)(nil)

// Identity 返回认证成功后的身份，未完成时为 nil。
func (c *ntlmContext) Identity() *Identity { return c.identity }

// SessionKey 返回 16 字节 ExportedSessionKey；匿名/guest 会话为全零。
func (c *ntlmContext) SessionKey() []byte {
	if c.sessionKey == nil {
		return make([]byte, SessionKeyLen)
	}
	return c.sessionKey
}

// Step 处理一个 GSS-API token。
func (c *ntlmContext) Step(in []byte) ([]byte, bool, error) {
	if c.done {
		return nil, true, ErrInvalidToken
	}

	tok, err := ParseSPNEGO(in)
	if err != nil {
		return nil, false, err
	}
	c.spnego = !tok.Raw

	// mechListMIC 的计算输入固定是客户端**第一个** negTokenInit 里的
	// MechTypeList，后续 negTokenResp 不再重复携带，必须在这里记下来。
	if tok.Init && len(tok.MechTypesDER) > 0 {
		c.clientMechListDER = tok.MechTypesDER
	}
	c.clientMechListMIC = tok.MechListMIC

	// 客户端只列了机制没带 token（或带的是 Kerberos token）：
	// 按 RFC 4178 §4.2.2 回 accept-incomplete + supportedMech，让它改发 NTLM。
	if len(tok.Token) == 0 || !IsNTLMSSP(tok.Token) {
		if tok.Init && len(tok.MechTypes) > 0 && !tok.HasMech(oidNTLMSSP) {
			return nil, false, ErrMechUnsupported
		}
		if !c.spnego {
			return nil, false, ErrInvalidToken
		}
		c.mechSent = true
		return NegTokenResp(NegAcceptIncomplete, oidNTLMSSP, nil, nil), false, nil
	}

	mt, err := MessageType(tok.Token)
	if err != nil {
		return nil, false, err
	}
	switch mt {
	case MsgTypeNegotiate:
		return c.handleNegotiate(tok.Token)
	case MsgTypeAuthenticate:
		return c.handleAuthenticate(tok.Token)
	default:
		return nil, false, ErrInvalidToken
	}
}

// wrap 按当前外壳形态包装服务端要回的 NTLM 报文。
//
// mic 为服务端的 mechListMIC，为空时不写 [3] 字段。
func (c *ntlmContext) wrap(state NegState, ntlm, mic []byte) []byte {
	if !c.spnego {
		return ntlm
	}
	var mech []byte
	if !c.mechSent {
		// supportedMech 只在第一次回应时携带（RFC 4178 §4.2.2）。
		mech = oidNTLMSSP
		c.mechSent = true
	}
	return NegTokenResp(state, mech, ntlm, mic)
}

// handleNegotiate 处理 Type 1，生成 Type 2 CHALLENGE。
func (c *ntlmContext) handleNegotiate(msg []byte) ([]byte, bool, error) {
	neg, err := ParseNegotiateMessage(msg)
	if err != nil {
		return nil, false, err
	}
	c.negotiateMsg = append([]byte(nil), msg...)

	if _, err := io.ReadFull(c.p.opts.Rand, c.serverChallenge[:]); err != nil {
		return nil, false, err
	}

	ch := &ChallengeMessage{
		TargetName:      c.p.opts.ServerName,
		Flags:           c.challengeFlags(neg.Flags),
		ServerChallenge: c.serverChallenge,
		TargetInfo:      c.targetInfo(),
		Version: Version{
			// 宣告一个通用的现代版本号，只影响客户端的展示与兼容分支。
			Major: 6, Minor: 1, Build: 7601, NTLMRevision: NTLMRevisionCurrent,
		},
	}
	c.challengeMsg = ch.Marshal()
	return c.wrap(NegAcceptIncomplete, c.challengeMsg, nil), false, nil
}

// challengeFlags 计算 CHALLENGE 里回给客户端的协商标志。
//
// 原则：只保留双方都支持的位，并强制打开 NTLMv2 必需的
// EXTENDED_SESSIONSECURITY / TARGET_INFO / ALWAYS_SIGN。
func (c *ntlmContext) challengeFlags(client NegotiateFlags) NegotiateFlags {
	f := NegotiateNTLM |
		NegotiateRequestTarget |
		NegotiateTargetTypeServer |
		NegotiateTargetInfo |
		NegotiateAlwaysSign |
		NegotiateExtendedSessionSecurity |
		NegotiateVersion |
		Negotiate128 |
		Negotiate56

	// 字符集：优先 Unicode。
	if client.Has(NegotiateUnicode) || !client.Has(NegotiateOEM) {
		f |= NegotiateUnicode
	} else {
		f |= NegotiateOEM
	}

	// 这些位必须**镜像客户端的请求**，否则密钥协商会不一致。
	for _, bit := range []NegotiateFlags{
		NegotiateSign, NegotiateSeal, NegotiateKeyExch, NegotiateIdentify,
	} {
		if client.Has(bit) {
			f |= bit
		}
	}
	return f
}

// targetInfo 构造 CHALLENGE 的 TargetInfo。
//
// MS-NLMP §2.2.2.1：前四项（NbDomainName / NbComputerName /
// DnsDomainName / DnsComputerName）不全时部分 Windows 客户端会拒绝，
// 因此一律带齐，并附上 Timestamp（NTLMv2 需要它来做重放窗口判断）。
func (c *ntlmContext) targetInfo() AvPairs {
	o := &c.p.opts
	return AvPairs{
		{ID: MsvAvNbDomainName, Value: utf16le(o.DomainName)},
		{ID: MsvAvNbComputerName, Value: utf16le(o.ServerName)},
		{ID: MsvAvDnsDomainName, Value: utf16le(o.DNSDomainName)},
		{ID: MsvAvDnsComputerName, Value: utf16le(o.DNSComputerName)},
		{ID: MsvAvTimestamp, Value: encodeFiletime(filetimeNow(o.Now()))},
	}
}

// handleAuthenticate 处理 Type 3，完成校验。
func (c *ntlmContext) handleAuthenticate(msg []byte) ([]byte, bool, error) {
	m, err := ParseAuthenticateMessage(msg)
	if err != nil {
		return nil, false, err
	}
	if c.challengeMsg == nil {
		// 没发过 CHALLENGE 就收到 AUTHENTICATE，状态机被跳步。
		return nil, false, ErrInvalidToken
	}

	if m.IsAnonymous() {
		if !c.p.opts.AllowAnonymous {
			return nil, false, ErrLogonFailure
		}
		c.identity = &Identity{Workstation: m.Workstation, Anonymous: true}
		c.sessionKey = make([]byte, SessionKeyLen)
		c.done = true
		return c.wrap(NegAcceptCompleted, nil, nil), true, nil
	}

	acct, lookupErr := c.lookup(m.UserName, m.DomainName)

	// 用户不存在时也走一遍等价耗时的校验，避免用户枚举（provider.go 的要求）。
	ntHash := acct.NTHash
	ntowf := NTOWFv2(ntHash, m.UserName, m.DomainName)
	sbk, blob, ok := VerifyNTLMv2(ntowf, c.serverChallenge, m.NTResponse)
	if lookupErr != nil {
		ok = false
	}

	if !ok {
		if c.p.opts.Store != nil && c.p.opts.Store.AllowGuest() {
			// 降级为 guest：没有可用的 session key，会话不能签名。
			c.identity = &Identity{
				User:        m.UserName,
				Domain:      m.DomainName,
				Workstation: m.Workstation,
				Guest:       true,
			}
			c.sessionKey = make([]byte, SessionKeyLen)
			c.done = true
			return c.wrap(NegAcceptCompleted, nil, nil), true, nil
		}
		return nil, false, ErrLogonFailure
	}

	esk, err := ExportedSessionKey(KeyExchangeKey(sbk), m.Flags, m.EncryptedRandomSessionKey)
	if err != nil {
		return nil, false, err
	}

	if err := c.verifyMIC(m, blob, esk); err != nil {
		return nil, false, err
	}

	// mechListMIC 必须在确立 identity 之前校验：它防的是中间人篡改 SPNEGO
	// 机制列表做降级，校验不过等同于认证失败。
	mic, err := c.spnegoMIC(esk, m.Flags)
	if err != nil {
		return nil, false, err
	}

	c.identity = &Identity{
		User:        acct.User,
		Domain:      m.DomainName,
		Workstation: m.Workstation,
		UID:         acct.UID,
		GID:         acct.GID,
	}
	c.sessionKey = append([]byte(nil), esk[:]...)
	c.done = true

	return c.wrap(NegAcceptCompleted, nil, mic), true, nil
}

// spnegoMIC 校验客户端的 mechListMIC 并生成服务端自己的（RFC 4178 §5）。
//
// 计算输入是**客户端 negTokenInit 里 MechTypeList 的完整 DER**
// （含外层 SEQUENCE 的 tag 与 length），不是里面几个 OID 的裸拼接 ——
// RFC 4178 §5 对此写得含糊，这里以 Windows / Samba 的实际行为为准。
//
// 两个方向各用一个独立的 NTLM 会话安全状态，序号都从 0 开始：
// mechListMIC 是各自方向上第一次、也是唯一一次 GSS_GetMIC
// （认证完成后 SMB2 改用自己派生的签名密钥，不再走 NTLM 的 MAC）。
//
// 返回服务端要放进最终 negTokenResp 的 mechListMIC；不需要时为 nil。
func (c *ntlmContext) spnegoMIC(esk [16]byte, flags NegotiateFlags) ([]byte, error) {
	// 裸 NTLMSSP 没有 SPNEGO 外壳，客户端也没发过 mechTypes，无从计算。
	if !c.spnego || len(c.clientMechListDER) == 0 {
		return nil, nil
	}
	// 没协商 extended session security 就没有 SignKey（MS-NLMP §3.4.5.2），
	// 这类老客户端本来也不会发 mechListMIC。
	if !flags.Has(NegotiateExtendedSessionSecurity) {
		return nil, nil
	}
	// 客户端没带 MIC 时不能强制要求（老客户端不发），
	// 也不主动回一个 —— RFC 4178 §5 是"对端发了才回"的对称语义。
	if len(c.clientMechListMIC) == 0 {
		return nil, nil
	}

	// 校验方向：客户端是发送方，用 client-to-server 的 SigningKey。
	cc, err := NewSigningContext(esk[:], flags, true)
	if err != nil {
		return nil, err
	}
	want := cc.MIC(c.clientMechListDER)
	if subtle.ConstantTimeCompare(want[:], c.clientMechListMIC) != 1 {
		return nil, ErrLogonFailure
	}

	// 生成方向：服务端是发送方，用 server-to-client 的 SigningKey。
	sc, err := NewSigningContext(esk[:], flags, false)
	if err != nil {
		return nil, err
	}
	out := sc.MIC(c.clientMechListDER)
	return out[:], nil
}

// dummyAccount 用于用户不存在时的等时校验，NTHash 恒为零值。
var dummyAccount = &Account{}

func (c *ntlmContext) lookup(user, domain string) (*Account, error) {
	if c.p.opts.Store == nil {
		return dummyAccount, ErrNoSuchUser
	}
	acct, err := c.p.opts.Store.Lookup(user, domain)
	if err != nil || acct == nil {
		return dummyAccount, ErrNoSuchUser
	}
	return acct, nil
}

// verifyMIC 在客户端声明带 MIC 时校验它（MS-NLMP §3.2.5.1.2）。
//
// MsvAvFlags 的 0x00000002 位置位表示 AUTHENTICATE_MESSAGE 里有 MIC，
// 此时服务端**必须**校验，否则中间人可以篡改 NEGOTIATE 里的标志位降级。
func (c *ntlmContext) verifyMIC(m *AuthenticateMessage, blob []byte, esk [16]byte) error {
	// blob 布局（MS-NLMP §2.2.2.7 NTLMv2_CLIENT_CHALLENGE）：
	// RespType(1) HiRespType(1) Z(2) Z(4) Timestamp(8) ClientChallenge(8) Z(4) AvPairs
	const clientChallengeHeader = 28
	if len(blob) < clientChallengeHeader {
		return nil // 不是 NTLMv2 blob（LMv2-only 之类），没有 AV_PAIR 可看
	}
	avs, err := ParseAvPairs(blob[clientChallengeHeader:])
	if err != nil {
		return ErrInvalidToken
	}
	v, ok := avs.Get(MsvAvFlags)
	if !ok || len(v) < 4 {
		return nil
	}
	flags := uint32(v[0]) | uint32(v[1])<<8 | uint32(v[2])<<16 | uint32(v[3])<<24
	if flags&AvFlagMICPresent == 0 {
		return nil
	}
	if !m.MICPresent {
		// 声明有 MIC 却没有字段 —— 拒绝，不能当作没带处理。
		return ErrLogonFailure
	}
	want := ComputeMIC(esk, c.negotiateMsg, c.challengeMsg, m.rawWithZeroMIC())
	if subtle.ConstantTimeCompare(want[:], m.MIC[:]) != 1 {
		return ErrLogonFailure
	}
	return nil
}

// ServerChallenge 返回本次握手使用的 8 字节服务端质询（供日志/测试）。
func (c *ntlmContext) ServerChallenge() [8]byte { return c.serverChallenge }
