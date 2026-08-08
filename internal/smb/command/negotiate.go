package command

import (
	"crypto/rand"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	// NEGOTIATE 是唯一在没有会话时也必须处理的命令。
	register(wire.CommandNegotiate, false, false, handleNegotiate)
}

// preauthSaltSize 是 3.1.1 PREAUTH_INTEGRITY_CAPABILITIES 里服务端 Salt 的长度。
//
// MS-SMB2 §2.2.3.1.1 没有规定长度，Windows 与 Samba 都用 32 字节。
const preauthSaltSize = 32

// serverSupportedCiphers 是本端实现的加密算法，用于与客户端列表求交集。
//
// AES-CCM 由 internal/smb/crypto 自实现（标准库没有），
// AES-GCM 来自标准库。四种都支持。
var serverSupportedCiphers = []uint16{
	wire.CipherAES128GCM,
	wire.CipherAES128CCM,
	wire.CipherAES256GCM,
	wire.CipherAES256CCM,
}

// serverSupportedSigningAlgorithms 是本端实现的签名算法。
var serverSupportedSigningAlgorithms = []uint16{
	wire.SigningAlgorithmAESCMAC,
	wire.SigningAlgorithmAESGMAC,
	wire.SigningAlgorithmHMACSHA256,
}

// handleNegotiate 处理 SMB2 NEGOTIATE（MS-SMB2 §3.3.5.4）。
func handleNegotiate(ctx *Context) error {
	conn := ctx.Conn

	// MS-SMB2 §3.3.5.4：一条连接上只允许协商一次。
	if conn.NegotiateDone {
		return status.InvalidParameter
	}

	req, err := wire.ParseNegotiateRequest(ctx.Msg)
	if err != nil {
		ctx.Log.Debug("NEGOTIATE 请求解析失败", "remote", conn.RemoteAddr, "err", err)
		return status.InvalidParameter
	}

	client := make([]dialect.Dialect, 0, len(req.Dialects))
	for _, d := range req.Dialects {
		client = append(client, dialect.Dialect(d))
	}

	chosen, ok := dialect.Negotiate(client, conn.Settings.MinDialect, conn.Settings.MaxDialect)
	if !ok {
		ctx.Log.Warn("没有共同支持的 SMB 方言",
			"remote", conn.RemoteAddr, "client_dialects", client)
		return status.NotSupported
	}

	conn.Dialect = chosen
	conn.ClientGUID = req.ClientGUID
	conn.ClientCapabilities = req.Capabilities
	conn.ClientSecurityMode = req.SecurityMode
	conn.ClientDialects = client

	// 3.1.1 的 preauth integrity hash 从 NEGOTIATE Request 本身开始滚动。
	// 必须在这里做（方言此时才确定），响应部分由 internal/server 在
	// 复合链拼装完成后追加（见 Context.HashResponseConn）。
	conn.UpdatePreauthHash(ctx.Msg)

	resp := &wire.NegotiateResponse{
		DialectRevision: wire.Dialect(chosen),
		ServerGUID:      conn.Settings.ServerGUID,
		MaxTransactSize: chosen.MaxTransactSize(),
		MaxReadSize:     chosen.MaxTransactSize(),
		MaxWriteSize:    chosen.MaxTransactSize(),
		SystemTime:      fileTime(time.Now()),
		ServerStartTime: fileTime(conn.Settings.StartTime),
		SecurityBuffer:  conn.Settings.Auth.InitialToken(),
	}

	// SecurityMode：我们**始终**声明支持签名；是否强制由配置决定
	// （MS-SMB2 §2.2.4 SecurityMode）。
	resp.SecurityMode = wire.NegotiateSigningEnabled
	if conn.Settings.SigningRequired {
		resp.SecurityMode |= wire.NegotiateSigningRequired
	}

	// 3.1.1 用 negotiate context 协商加密；3.0/3.0.2 用 Capabilities 位。
	encryptionOK := conn.Settings.EncryptionEnabled && chosen.SupportsEncryption()
	resp.Capabilities = wire.Capabilities(chosen.ServerCapabilities(encryptionOK))

	if chosen.SupportsNegotiateContexts() {
		if err := negotiateContexts(ctx, req, resp, encryptionOK); err != nil {
			return err
		}
	}

	conn.ServerCapabilities = resp.Capabilities
	conn.ServerSecurityMode = resp.SecurityMode
	conn.MaxTransactSize = resp.MaxTransactSize
	conn.MaxReadSize = resp.MaxReadSize
	conn.MaxWriteSize = resp.MaxWriteSize
	// 只要任何一方要求签名，本连接就强制签名（MS-SMB2 §3.3.5.4）。
	conn.SigningRequired = conn.Settings.SigningRequired ||
		req.SecurityMode&wire.NegotiateSigningRequired != 0
	conn.NegotiateDone = true

	out, err := resp.Append(ctx.Out)
	if err != nil {
		ctx.Log.Error("NEGOTIATE 响应编码失败", "remote", conn.RemoteAddr, "err", err)
		return status.InsuffServerResources
	}
	ctx.Out = out

	// 响应字节也要滚进连接级 preauth hash（在头回填之后由 server 执行）。
	ctx.HashResponseConn = chosen.SupportsPreauthIntegrity()

	ctx.Log.Info("SMB 方言协商完成",
		"remote", conn.RemoteAddr,
		"dialect", chosen.String(),
		"signing_required", conn.SigningRequired,
		"cipher", conn.Cipher,
	)
	return nil
}

// negotiateContexts 处理 3.1.1 的 negotiate context 协商（MS-SMB2 §3.3.5.4）。
func negotiateContexts(ctx *Context, req *wire.NegotiateRequest,
	resp *wire.NegotiateResponse, encryptionOK bool) error {

	conn := ctx.Conn
	sawPreauth := false

	for _, c := range req.Contexts {
		switch c.Type {
		case wire.ContextPreauthIntegrityCapabilities:
			p, err := wire.ParsePreauthIntegrityCapabilities(c.Data)
			if err != nil {
				return status.InvalidParameter
			}
			if !containsU16(p.HashAlgorithms, wire.HashAlgorithmSHA512) {
				// 无交集：MS-SMB2 §3.3.5.4 要求回专用错误码。
				return status.SMBNoPreauthIntegrityHashOverlap
			}
			sawPreauth = true

			salt := make([]byte, preauthSaltSize)
			if _, err := rand.Read(salt); err != nil {
				return status.InsuffServerResources
			}
			data, err := (&wire.PreauthIntegrityCapabilities{
				HashAlgorithms: []uint16{wire.HashAlgorithmSHA512},
				Salt:           salt,
			}).Encode()
			if err != nil {
				return status.InsuffServerResources
			}
			resp.Contexts = append(resp.Contexts, wire.NegotiateContext{
				Type: wire.ContextPreauthIntegrityCapabilities,
				Data: data,
			})

		case wire.ContextEncryptionCapabilities:
			e, err := wire.ParseEncryptionCapabilities(c.Data)
			if err != nil {
				return status.InvalidParameter
			}
			// MS-SMB2 §3.3.5.4：收到 ENCRYPTION_CAPABILITIES 就**必须**回应；
			// 无交集时回 Ciphers[0] = 0x0000 表示不加密。
			chosenCipher := uint16(0)
			if encryptionOK {
				chosenCipher = firstSupported(e.Ciphers, serverSupportedCiphers)
			}
			conn.Cipher = chosenCipher
			data, err := (&wire.EncryptionCapabilities{
				Ciphers: []uint16{chosenCipher},
			}).Encode()
			if err != nil {
				return status.InsuffServerResources
			}
			resp.Contexts = append(resp.Contexts, wire.NegotiateContext{
				Type: wire.ContextEncryptionCapabilities,
				Data: data,
			})

		case wire.ContextSigningCapabilities:
			s, err := wire.ParseSigningCapabilities(c.Data)
			if err != nil {
				return status.InvalidParameter
			}
			// MS-SMB2 §3.3.5.4：选客户端列表中**第一个**本端支持的算法。
			alg := firstSupported(s.SigningAlgorithms, serverSupportedSigningAlgorithms)
			conn.SigningAlgorithm = alg
			data, err := (&wire.SigningCapabilities{
				SigningAlgorithms: []uint16{alg},
			}).Encode()
			if err != nil {
				return status.InsuffServerResources
			}
			resp.Contexts = append(resp.Contexts, wire.NegotiateContext{
				Type: wire.ContextSigningCapabilities,
				Data: data,
			})

		case wire.ContextCompressionCapabilities,
			wire.ContextNetnameNegotiateContextID,
			wire.ContextTransportCapabilities,
			wire.ContextRDMATransformCapabilities:
			// 不支持压缩 / RDMA；NETNAME 与 TRANSPORT 规范要求忽略。
			// 对压缩**不回应**即表示不启用（protocol-notes §4）。

		default:
			// 未知 context 一律忽略（MS-SMB2 §3.3.5.4 要求向前兼容）。
		}
	}

	// MS-SMB2 §3.3.5.4：3.1.1 客户端**必须**恰好发一个
	// PREAUTH_INTEGRITY_CAPABILITIES，没有就是协议错误。
	if !sawPreauth {
		return status.InvalidParameter
	}

	// 未协商 SIGNING_CAPABILITIES 时，3.1.1 默认用 AES-CMAC（protocol-notes §6）。
	if conn.SigningAlgorithm == 0 && !hasContext(req.Contexts, wire.ContextSigningCapabilities) {
		conn.SigningAlgorithm = wire.SigningAlgorithmAESCMAC
	}
	return nil
}

func hasContext(list []wire.NegotiateContext, t wire.NegotiateContextType) bool {
	for _, c := range list {
		if c.Type == t {
			return true
		}
	}
	return false
}

func containsU16(list []uint16, v uint16) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// firstSupported 返回 client 列表中第一个出现在 server 列表里的值；
// 无交集返回 0。
func firstSupported(client, server []uint16) uint16 {
	for _, c := range client {
		if containsU16(server, c) {
			return c
		}
	}
	return 0
}

// filetimeEpochOffset 是 1601-01-01 UTC 到 1970-01-01 UTC 的 100ns 数
// （protocol-notes §11）。
const filetimeEpochOffset = 116444736000000000

// fileTime 把 Go 时间转换为 Windows FILETIME（1601 epoch，100ns 单位）。
func fileTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano()/100 + filetimeEpochOffset)
}
