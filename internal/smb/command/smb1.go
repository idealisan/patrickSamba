package command

import (
	"errors"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// SMB1 多协议协商入口（MS-SMB2 §3.3.5.3.1，protocol-notes §4）。
//
// 老客户端（以及 Linux 内核 cifs 的默认行为、Windows 的向后兼容路径）会先发一个
// SMB1 `SMB_COM_NEGOTIATE`，方言列表里带 `"SMB 2.???"`。服务端此时直接用
// **SMB2 NEGOTIATE Response** 回应，DialectRevision 填通配值 0x02FF，
// 客户端随后再发一个真正的 SMB2 NEGOTIATE。
//
// 本项目只实现这一个 SMB1 入口，**不实现任何 SMB1 文件操作**。
// 因此这里只需要认得 SMB1 NEGOTIATE 的报文体（WordCount=0 + ByteCount + 方言串），
// 不值得为它在 wire 包里铺一整套 SMB1 编解码。
const (
	// smb1HeaderSize 是 SMB1 报文头长度（MS-CIFS §2.2.3.1）。
	smb1HeaderSize = 32
	// smb1CommandNegotiate 是 SMB_COM_NEGOTIATE（MS-CIFS §2.2.4.52）。
	smb1CommandNegotiate = 0x72
	// smb1DialectBufferFormat 是方言串前缀的 BufferFormat 字节（MS-CIFS §2.2.4.52.1）。
	smb1DialectBufferFormat = 0x02
)

// SMB1 协商相关错误。
var (
	// ErrNotSMB1Negotiate 表示这不是一个 SMB1 SMB_COM_NEGOTIATE 报文。
	ErrNotSMB1Negotiate = errors.New("command: 不是 SMB1 NEGOTIATE")
	// ErrNoSMB2Dialect 表示 SMB1 方言列表里没有任何 SMB2 方言，
	// 此时应当关闭连接（我们不实现 SMB1 文件共享）。
	ErrNoSMB2Dialect = errors.New("command: SMB1 方言列表中没有 SMB2")
	// ErrSMB1EncryptionRequired 表示配置要求加密，但客户端在 SMB1 多协议协商里
	// 只报了 "SMB 2.002" —— 2.0.2 没有任何加密能力，此路无救，直接关连接。
	ErrSMB1EncryptionRequired = errors.New("command: 配置要求加密但客户端只协商 SMB 2.002")
)

// SMB1 方言串（MS-SMB2 §3.3.5.3.1）。
const (
	smb1DialectSMB2Wildcard = "SMB 2.???"
	smb1DialectSMB2002      = "SMB 2.002"
)

// ParseSMB1NegotiateDialects 解析 SMB1 SMB_COM_NEGOTIATE 的方言列表。
//
// 报文体布局：`WordCount(1)=0` + `ByteCount(2, 小端)` +
// 若干个 `0x02 + NUL 结尾的 ASCII 方言串`。
func ParseSMB1NegotiateDialects(frame []byte) ([]string, error) {
	if len(frame) < smb1HeaderSize+3 {
		return nil, ErrNotSMB1Negotiate
	}
	if !wire.IsSMB1(frame) || frame[4] != smb1CommandNegotiate {
		return nil, ErrNotSMB1Negotiate
	}
	if frame[smb1HeaderSize] != 0 { // WordCount 必须为 0
		return nil, ErrNotSMB1Negotiate
	}

	byteCount := int(frame[smb1HeaderSize+1]) | int(frame[smb1HeaderSize+2])<<8
	start := smb1HeaderSize + 3
	if byteCount < 0 || start+byteCount > len(frame) {
		return nil, ErrNotSMB1Negotiate
	}
	buf := frame[start : start+byteCount]

	var out []string
	for i := 0; i < len(buf); {
		if buf[i] != smb1DialectBufferFormat {
			return nil, ErrNotSMB1Negotiate
		}
		i++
		j := i
		for j < len(buf) && buf[j] != 0x00 {
			j++
		}
		if j >= len(buf) {
			// 没有结尾 NUL，报文撒谎了。
			return nil, ErrNotSMB1Negotiate
		}
		out = append(out, string(buf[i:j]))
		i = j + 1
	}
	return out, nil
}

// AppendSMB1NegotiateReply 针对 SMB1 多协议协商生成 **SMB2** 应答，追加到 out。
//
// 规则（protocol-notes §4）：
//   - 方言列表含 "SMB 2.???" → 回 DialectRevision=0x02FF，等客户端再发真正的
//     SMB2 NEGOTIATE（**不**把连接标记为已协商）；
//   - 只含 "SMB 2.002" → 直接按 2.0.2 完成协商；
//   - 都没有 → 返回 ErrNoSMB2Dialect，调用方关闭连接。
//
// 加密强制（EncryptionRequired）在这里也必须 fail closed，理由见下方分支注释：
// **本函数是独立于 handleNegotiate 的第二个协商出口**，
// handleNegotiate 里的拒绝逻辑覆盖不到只含 "SMB 2.002" 的那条路径。
func AppendSMB1NegotiateReply(conn *Conn, dialects []string, out []byte) ([]byte, error) {
	wildcard := false
	smb2002 := false
	for _, d := range dialects {
		switch d {
		case smb1DialectSMB2Wildcard:
			wildcard = true
		case smb1DialectSMB2002:
			smb2002 = true
		}
	}
	if !wildcard && !smb2002 {
		return nil, ErrNoSMB2Dialect
	}

	// 加密强制：只含 "SMB 2.002" 时在这里就拒绝。
	//
	// 这条分支下面会直接 `conn.Dialect = dialect.SMB202; NegotiateDone = true`
	// 把协商就地做完，**永远不会再进 handleNegotiate**，因此那里的
	// `EncryptionRequired && Cipher == 0` 一次都不会执行 —— 曾经的缺口就是
	// 客户端走 SMB1 多协议协商只报 "SMB 2.002" 即可拿到 2.0.2 明文会话。
	// 2.0.2 永远不可能满足加密要求，没有任何补救余地，在此拒绝最干净。
	//
	// 含 "SMB 2.???" 通配的那条分支**刻意放过**：它只回一个 0x02FF 占位应答，
	// 不定型方言、不置 NegotiateDone，客户端随后必然再发一个真正的
	// SMB2 NEGOTIATE，那一发会走 handleNegotiate 并被那里的 fail-closed
	// 拦住。所以这里不重复检查，不是漏了。
	if !wildcard && conn.Settings.EncryptionRequired {
		return nil, ErrSMB1EncryptionRequired
	}

	// 通配优先：能升级到更高方言就不要锁死在 2.0.2。
	chosen := dialect.SMB202
	revision := wire.Dialect(dialect.SMB202)
	if wildcard {
		revision = wire.Dialect(dialect.SMB2Wildcard)
		// 通配应答里的能力字段按本端最高可用方言宣告即可，
		// 真正的协商在下一个 SMB2 NEGOTIATE 里做。
		chosen = maxNegotiableDialect(conn)
	}

	hdr := wire.Header{
		Command:   wire.CommandNegotiate,
		Flags:     wire.FlagServerToRedir,
		Credits:   1, // 必须给至少 1，否则客户端发不出下一条请求
		MessageID: 0,
	}
	out = hdr.Append(out)

	resp := &wire.NegotiateResponse{
		SecurityMode:    wire.NegotiateSigningEnabled,
		DialectRevision: revision,
		ServerGUID:      conn.Settings.ServerGUID,
		// 不宣告加密：SMB1 入口只是把客户端升级到 SMB2，真正的加密能力
		// 由随后的 SMB2 NEGOTIATE 协商（那里才看得到 cipher 列表）。
		// 同样不宣告 LEASING：本入口选出的方言是 2.0.2，租约要 2.1 起。
		Capabilities:    wire.Capabilities(chosen.ServerCapabilities(false, false)),
		MaxTransactSize: chosen.MaxTransactSize(),
		MaxReadSize:     chosen.MaxTransactSize(),
		MaxWriteSize:    chosen.MaxTransactSize(),
		SystemTime:      vfs.TimeToFiletime(time.Now()),
		ServerStartTime: vfs.TimeToFiletime(conn.Settings.StartTime),
		SecurityBuffer:  conn.Settings.Auth.InitialToken(),
	}
	if conn.Settings.SigningRequired {
		resp.SecurityMode |= wire.NegotiateSigningRequired
	}

	body, err := resp.Append(out)
	if err != nil {
		return nil, err
	}

	if !wildcard {
		// 只支持 SMB 2.002 的客户端：协商到此结束。
		conn.Dialect = dialect.SMB202
		conn.NegotiateDone = true
		conn.ServerCapabilities = resp.Capabilities
		conn.ServerSecurityMode = resp.SecurityMode
		conn.MaxTransactSize = resp.MaxTransactSize
		conn.MaxReadSize = resp.MaxReadSize
		conn.MaxWriteSize = resp.MaxWriteSize
		conn.SigningRequired = conn.Settings.SigningRequired
	}
	return body, nil
}

// maxNegotiableDialect 返回配置区间内本端可用的最高方言。
func maxNegotiableDialect(conn *Conn) dialect.Dialect {
	r := dialect.Range(conn.Settings.MinDialect, conn.Settings.MaxDialect)
	if len(r) == 0 {
		return dialect.SMB202
	}
	return r[len(r)-1]
}
