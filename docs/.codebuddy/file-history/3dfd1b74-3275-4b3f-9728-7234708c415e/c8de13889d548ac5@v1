package command

import (
	"sync"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
)

// Session 是一个已认证（或认证中）的 SMB2 会话（MS-SMB2 §3.3.1.8 Session）。
//
// 生命周期：SESSION_SETUP 第一轮创建 → 多轮 NTLM 握手 → Established
// → LOGOFF / 连接断开时销毁。
type Session struct {
	// ID 是 SessionId，连接内唯一且非 0。
	ID uint64

	// Conn 是所属连接。
	Conn *Conn

	mu sync.RWMutex

	// authCtx 是进行中的认证上下文（NTLM 多轮握手）。认证完成后置 nil。
	authCtx auth.Context

	// established 表示认证已完成，可以处理除 SESSION_SETUP 外的命令。
	established bool

	// identity 是认证成功后的身份。
	identity *auth.Identity

	// sessionKey 是 ExportedSessionKey（NTLM 产出，16 字节）。
	sessionKey []byte
	// signingKey / applicationKey / encryptKey / decryptKey 是派生密钥。
	// 2.x 下 signingKey 直接等于 sessionKey。
	signingKey     []byte
	applicationKey []byte
	// encryptKey 用于加密**服务端发出**的消息（S2C）。
	encryptKey []byte
	// decryptKey 用于解密**客户端发来**的消息（C2S）。
	decryptKey []byte

	// signingRequired 表示本会话的请求必须带有效签名。
	signingRequired bool
	// encryptData 表示本会话必须加密（SMB2_SESSION_FLAG_ENCRYPT_DATA）。
	encryptData bool

	// preauthHash 是**会话级** preauth integrity hash（仅 3.1.1）。
	// 建连时从 Conn.PreauthHash 复制而来，随 SESSION_SETUP 报文继续滚动。
	preauthHash [64]byte

	// trees 是本会话的树连接表。
	trees      map[uint32]*Tree
	nextTreeID uint32

	// opens 是本会话持有的打开句柄表，按 Volatile ID 索引
	// （MS-SMB2 §3.3.1.8 Session.OpenTable）。
	opens        map[uint64]*Open
	nextVolatile uint64

	closed bool
}

func newSession(c *Conn, id uint64) *Session {
	s := &Session{
		ID:     id,
		Conn:   c,
		trees:  make(map[uint32]*Tree),
		opens:  make(map[uint64]*Open),
		closed: false,
	}
	// 3.1.1：会话级 preauth hash 从连接级定型值分叉。
	s.preauthHash = c.PreauthHash
	return s
}

// ---- 认证状态 ----

// AuthContext 返回进行中的认证上下文，尚未开始时返回 nil。
func (s *Session) AuthContext() auth.Context {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authCtx
}

// SetAuthContext 设置认证上下文（第一轮 SESSION_SETUP 时调用）。
func (s *Session) SetAuthContext(ctx auth.Context) {
	s.mu.Lock()
	s.authCtx = ctx
	s.mu.Unlock()
}

// Established 报告认证是否已完成。
func (s *Session) Established() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.established
}

// Identity 返回认证身份，未认证时返回 nil。
func (s *Session) Identity() *auth.Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.identity
}

// Establish 标记认证完成并记录身份。
func (s *Session) Establish(id *auth.Identity) {
	s.mu.Lock()
	s.established = true
	s.identity = id
	s.authCtx = nil
	s.mu.Unlock()
}

// ---- 密钥 ----

// Keys 是一次性设置的会话密钥集合。
type Keys struct {
	SessionKey     []byte
	SigningKey     []byte
	ApplicationKey []byte
	EncryptKey     []byte
	DecryptKey     []byte
}

// SetKeys 安装派生出的会话密钥。
func (s *Session) SetKeys(k Keys) {
	s.mu.Lock()
	s.sessionKey = k.SessionKey
	s.signingKey = k.SigningKey
	s.applicationKey = k.ApplicationKey
	s.encryptKey = k.EncryptKey
	s.decryptKey = k.DecryptKey
	s.mu.Unlock()
}

// SessionKey 返回 ExportedSessionKey。
func (s *Session) SessionKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionKey
}

// SigningKey 返回用于签名/验签的密钥。
func (s *Session) SigningKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.signingKey
}

// EncryptKey 返回服务端发出方向（S2C）的加密密钥。
func (s *Session) EncryptKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.encryptKey
}

// DecryptKey 返回客户端发来方向（C2S）的解密密钥。
func (s *Session) DecryptKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.decryptKey
}

// SigningRequired 报告本会话是否强制签名。
func (s *Session) SigningRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.signingRequired
}

// SetSigningRequired 设置本会话是否强制签名。
func (s *Session) SetSigningRequired(v bool) {
	s.mu.Lock()
	s.signingRequired = v
	s.mu.Unlock()
}

// EncryptData 报告本会话是否强制加密。
func (s *Session) EncryptData() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.encryptData
}

// SetEncryptData 设置本会话是否强制加密。
func (s *Session) SetEncryptData(v bool) {
	s.mu.Lock()
	s.encryptData = v
	s.mu.Unlock()
}

// PreauthHash 返回会话级 preauth integrity hash。
func (s *Session) PreauthHash() [64]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.preauthHash
}

// UpdatePreauthHash 把一条完整消息滚进会话级 preauth hash（仅 3.1.1 有意义）。
func (s *Session) UpdatePreauthHash(msg []byte) {
	s.mu.Lock()
	s.preauthHash = rollPreauthHash(s.preauthHash, msg)
	s.mu.Unlock()
}

// ---- 树表 ----

// NewTree 建立一个树连接。TreeId 从 1 开始单调递增（0 保留）。
func (s *Session) NewTree(share *Share) (*Tree, status.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, status.UserSessionDeleted
	}
	if len(s.trees) >= s.Conn.Settings.maxTrees() {
		return nil, status.InsuffServerResources
	}

	s.nextTreeID++
	t := &Tree{
		ID:      s.nextTreeID,
		Share:   share,
		Session: s,
	}
	s.trees[t.ID] = t
	return t, status.Success
}

// Tree 按 TreeId 查找树连接。找不到返回 nil。
func (s *Session) Tree(id uint32) *Tree {
	if id == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.trees[id]
}

// RemoveTree 断开一个树连接，并关闭其上所有句柄。
func (s *Session) RemoveTree(id uint32) status.Status {
	s.mu.Lock()
	t := s.trees[id]
	if t == nil {
		s.mu.Unlock()
		return status.NetworkNameDeleted
	}
	delete(s.trees, id)

	// 摘出属于该树的句柄，锁外关闭（Close 可能阻塞在 IO 上）。
	var doomed []*Open
	for vid, o := range s.opens {
		if o.Tree == t {
			delete(s.opens, vid)
			doomed = append(doomed, o)
		}
	}
	s.mu.Unlock()

	for _, o := range doomed {
		o.close()
	}
	return status.Success
}

// ---- 句柄表 ----

// AddOpen 登记一个新打开的句柄并分配 FileId。
//
// Persistent 与 Volatile 都用同一个单调递增值：我们不支持 durable handle，
// 因此 Persistent 只需在会话内唯一即可（MS-SMB2 §2.2.14.1）。
func (s *Session) AddOpen(o *Open) status.Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return status.UserSessionDeleted
	}
	if len(s.opens) >= s.Conn.Settings.maxOpens() {
		return status.InsuffServerResources
	}

	s.nextVolatile++
	o.Volatile = s.nextVolatile
	o.Persistent = s.nextVolatile
	o.Session = s
	s.opens[o.Volatile] = o
	return status.Success
}

// Open 按 FileId 查找句柄。persistent 不匹配时视为无效句柄。
func (s *Session) Open(persistent, volatile uint64) *Open {
	s.mu.RLock()
	o := s.opens[volatile]
	s.mu.RUnlock()
	if o == nil || o.Persistent != persistent {
		return nil
	}
	return o
}

// RemoveOpen 从句柄表摘除一个句柄（不负责关闭底层资源）。
func (s *Session) RemoveOpen(volatile uint64) *Open {
	s.mu.Lock()
	o := s.opens[volatile]
	delete(s.opens, volatile)
	s.mu.Unlock()
	return o
}

// OpenCount 返回当前持有的句柄数，用于日志与测试。
func (s *Session) OpenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.opens)
}

// Close 拆除会话：关闭全部句柄与树连接。可重复调用。
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	opens := make([]*Open, 0, len(s.opens))
	for _, o := range s.opens {
		opens = append(opens, o)
	}
	s.opens = make(map[uint64]*Open)
	s.trees = make(map[uint32]*Tree)
	s.authCtx = nil
	s.mu.Unlock()

	for _, o := range opens {
		o.close()
	}
}
