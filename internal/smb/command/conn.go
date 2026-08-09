package command

import (
	"crypto/sha512"
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// Conn 是一条 TCP 连接上的 **SMB2 协议状态**（MS-SMB2 §3.3.1.5 Connection）。
//
// 它不持有 socket，也不做 IO —— 传输由 internal/server 负责。
// 本结构由连接的读循环单 goroutine 驱动，但 session/tree/open 表仍加锁保护，
// 因为将来异步命令（SMB2_FLAGS_ASYNC_COMMAND）会从别的 goroutine 访问。
type Conn struct {
	// Settings 是服务端级设置，只读。
	Settings *Settings

	// RemoteAddr / LocalAddr 仅用于日志。
	RemoteAddr string
	LocalAddr  string

	// ---- 协商结果（NEGOTIATE 后定型）----

	// Dialect 是协商出的方言。0 表示尚未协商。
	Dialect dialect.Dialect
	// NegotiateDone 表示已经完成一次成功的 SMB2 NEGOTIATE。
	// MS-SMB2 §3.3.5.4：同一连接上重复 NEGOTIATE 必须回
	// STATUS_INVALID_PARAMETER。
	NegotiateDone bool

	// ClientGUID / ClientCapabilities / ClientSecurityMode 来自客户端的
	// NEGOTIATE Request，FSCTL_VALIDATE_NEGOTIATE_INFO 要原样回显。
	ClientGUID         [16]byte
	ClientCapabilities wire.Capabilities
	ClientSecurityMode wire.SecurityMode
	// ClientDialects 是客户端提供的方言列表，
	// FSCTL_VALIDATE_NEGOTIATE_INFO 校验要用（MS-SMB2 §3.3.5.15.12）。
	ClientDialects []dialect.Dialect

	// ServerCapabilities 是本端在 NEGOTIATE Response 里宣告的能力位，
	// FSCTL_VALIDATE_NEGOTIATE_INFO 要原样回显。
	ServerCapabilities wire.Capabilities
	// ServerSecurityMode 同上。
	ServerSecurityMode wire.SecurityMode

	// MaxTransactSize / MaxReadSize / MaxWriteSize 是本端宣告的上限，
	// 收到超限的 READ/WRITE 时据此拒绝。
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32

	// SigningRequired 是协商后的最终结论：本连接是否强制要求签名。
	SigningRequired bool

	// Cipher 是 3.1.1 协商出的加密算法 ID（0 表示不加密）。
	Cipher uint16
	// SigningAlgorithm 是 3.1.1 协商出的签名算法 ID。
	SigningAlgorithm uint16

	// PreauthHash 是 **连接级** preauth integrity hash（仅 3.1.1）。
	//
	// MS-SMB2 §3.3.5.4 / protocol-notes §6：
	// NEGOTIATE Request/Response 之后该值定型；建立会话时**复制**到
	// Session 级继续更新，不得污染连接级（同一连接可有多个会话）。
	PreauthHash [64]byte

	// ---- 会话表 ----

	mu            sync.RWMutex
	sessions      map[uint64]*Session
	nextSessionID uint64

	// closed 表示连接已开始拆除，不再接受新会话。
	closed bool

	// aapl 是 Apple SMB2 扩展的协商结果（见 aapl.go）。
	// 由 CREATE 上的 AAPL create context 置位，QUERY_DIRECTORY 读取。
	aapl aaplState
}

// NewConn 创建连接级协议状态。
func NewConn(s *Settings, remote, local string) *Conn {
	return &Conn{
		Settings:   s,
		RemoteAddr: remote,
		LocalAddr:  local,
		sessions:   make(map[uint64]*Session),
	}
}

// SupportsMultiCredit 报告当前方言是否支持多信用（LARGE_MTU）。
func (c *Conn) SupportsMultiCredit() bool {
	return c.Dialect != 0 && c.Dialect.SupportsMultiCredit()
}

// SigningAlg 返回本连接实际使用的签名算法。
//
// 3.1.1 可以通过 SIGNING_CAPABILITIES negotiate context 协商；
// 其余方言由方言本身决定（2.x → HMAC-SHA256，3.0/3.0.2 → AES-CMAC）。
func (c *Conn) SigningAlg() crypto.SigningAlgorithm {
	if c.Dialect == dialect.SMB311 && c.SigningAlgorithm != 0 {
		return crypto.SigningAlgorithm(c.SigningAlgorithm)
	}
	if c.Dialect == dialect.SMB311 {
		// 3.1.1 未协商 SIGNING_CAPABILITIES 时默认 AES-CMAC。
		return crypto.SigningAESCMAC
	}
	return crypto.SigningAlgorithmForDialect(uint16(c.Dialect))
}

// UpdatePreauthHash 把一条完整消息滚进连接级 preauth hash。
//
// H = SHA512(H_prev || msg)，msg **不含** Direct TCP 的 4 字节长度前缀。
// 只有 3.1.1 需要这个链（MS-SMB2 §3.3.5.4）。
func (c *Conn) UpdatePreauthHash(msg []byte) {
	if c.Dialect != dialect.SMB311 {
		return
	}
	c.PreauthHash = rollPreauthHash(c.PreauthHash, msg)
}

// rollPreauthHash 计算 SHA512(prev || msg)。
func rollPreauthHash(prev [64]byte, msg []byte) [64]byte {
	h := sha512.New()
	h.Write(prev[:])
	h.Write(msg)
	var out [64]byte
	copy(out[:], h.Sum(nil))
	return out
}

// NewSession 分配一个新会话并登记到会话表。
//
// SessionId 从 1 开始单调递增（0 是保留值，表示"无会话"）。
// 超过上限时返回 STATUS_TOO_MANY_SESSIONS。
func (c *Conn) NewSession() (*Session, status.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, status.ConnectionDisconnected
	}
	if len(c.sessions) >= c.Settings.maxSessions() {
		return nil, status.TooManySessions
	}

	c.nextSessionID++
	s := newSession(c, c.nextSessionID)
	c.sessions[s.ID] = s
	return s, status.Success
}

// Session 按 ID 查找会话。找不到返回 nil。
func (c *Conn) Session(id uint64) *Session {
	if id == 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[id]
}

// RemoveSession 从会话表摘除并释放该会话持有的全部资源。
func (c *Conn) RemoveSession(id uint64) {
	c.mu.Lock()
	s := c.sessions[id]
	delete(c.sessions, id)
	c.mu.Unlock()

	if s != nil {
		s.Close()
	}
}

// Close 拆除连接上的全部会话（含其树与句柄）。可重复调用。
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	sessions := make([]*Session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.sessions = make(map[uint64]*Session)
	c.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}

// SessionCount 返回当前会话数，用于日志与测试。
func (c *Conn) SessionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.sessions)
}
