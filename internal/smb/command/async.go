package command

import (
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// ---------------------------------------------------------------------------
// 异步未决请求表（MS-SMB2 §3.3.4.4 / §3.3.5.16）
//
// 这是 CHANGE_NOTIFY 与阻塞锁异步等待的**共用前置设施**。在此之前本服务
// 所有 handler 都在连接的读循环里同步执行，由此产生两处与规范的差距：
//
//  1. 阻塞 LOCK 只能做同步有界等待（等的时候这条连接收不到 CANCEL）；
//  2. CHANGE_NOTIFY 只能恒回 STATUS_NOT_SUPPORTED（没有挂起通路，回
//     PENDING 之后无从补发）。
//
// 本文件建立「把一条请求挂起、稍后由任意 goroutine 补发响应」的能力，
// 并把挂起中的请求登记进一张可按 CANCEL 键定位的表。
// ---------------------------------------------------------------------------

// maxPendingAsync 是单连接同时挂起的异步请求数上限（AGENTS.md §8 资源上限）。
//
// 挂起一条请求意味着一个 goroutine 与一份响应缓冲常驻，必须有上限，
// 否则一个只发不收的客户端就能用 CHANGE_NOTIFY / 阻塞 LOCK 把服务端
// 内存吃干。达到上限时 Defer 返回失败，handler 退回同步语义（立即应答）。
const maxPendingAsync = 256

// AsyncSink 由 internal/server 实现并注入进 Conn，供协议层把一条
// **异步完成**的响应写回客户端。
//
// 分层约束（AGENTS.md §5）：依赖只能自上而下，command **不得** import
// server 与 crypto。所以这里只定义接口，实现放在 internal/server/async_send.go。
//
// 参数为什么把 signKey 与 encrypted 一起透传：二者都来自**当初那条请求**
// 的处理结果（checkSignature 算出的签名密钥、请求是否来自 SMB3 加密信封），
// 补发响应时原始 Context 早已销毁，只有把它们显式带过来才能在补发时
// 复现"该签就签、该加密就加密"的同一套判定。
//
// 实现方必须保证：
//   - 与读循环的响应写出**互斥**（Transport 复用同一个写头缓冲，并发写会
//     同时造成 data race 与报文交错）；
//   - 可从任意 goroutine 调用；
//   - 连接已关闭时返回错误而不是 panic。
type AsyncSink interface {
	// SendAsyncResponse 写出一条异步响应。
	//
	// hdr 是**响应头**（已含 MessageId / AsyncId / SessionId / TreeId /
	// Credits），body 是响应体（可为 nil，此时实现方按该命令的空响应处理）。
	// signKey 非 nil 时必须对整条消息签名；encrypted 为 true 时必须用会话的
	// S2C 密钥封装成 TRANSFORM_HEADER。
	SendAsyncResponse(hdr wire.Header, body []byte, signKey []byte, encrypted bool) error
}

// SetAsyncSink 注入异步响应写出通道。由 internal/server 在连接建立时调用。
//
// 传 nil 等于关闭挂起能力（单元测试里构造裸 Conn 时就是这种情况）：
// 此时 Context.Defer 恒失败，所有 handler 自动退回同步语义。
func (c *Conn) SetAsyncSink(s AsyncSink) {
	c.mu.Lock()
	c.asyncSink = s
	c.mu.Unlock()
}

// asyncSink 返回已注入的异步写出通道；未注入时返回 nil。
//
// 加锁读而不是裸读：注入发生在读循环启动前，但**调用**发生在别的
// goroutine 上，裸读会被 -race 判定为竞争（即便实际时序安全）。
// 挂起是低频动作，这把锁的代价可以忽略。
func (c *Conn) asyncSinkOrNil() AsyncSink {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.asyncSink
}

// asyncKey 定位一条未决请求。
//
// MS-SMB2 §3.3.5.16 给了两套匹配规则，取决于请求是否置了
// SMB2_FLAGS_ASYNC_COMMAND：
//
//	置位   → 按 AsyncId 匹配（AsyncId 由**服务端**在 interim 响应里分配）
//	未置位 → 按 (SessionId, MessageId) 匹配
//
// 两套规则不能混用：一个 AsyncId 与一个 MessageId 数值相等是可能的，
// 把键做成单一 uint64 会张冠李戴。
type asyncKey struct {
	async     bool
	asyncID   uint64
	sessionID uint64
	messageID uint64
}

// AsyncRequest 是一条已挂起的请求。
//
// 生命周期：
//  1. handler 调 Context.Defer 拿到它（此刻已登记进连接的未决表）；
//  2. handler 起一个 goroutine 去做真正的等待，并立即返回 nil；
//  3. 等待结束（或被 CANCEL）时调 Complete —— **至多生效一次**。
//
// 并发约定：Defer 之后、Complete 之前，本对象可被读循环（CANCEL）与
// 等待 goroutine 同时访问，所有字段都经 mu 保护。
type AsyncRequest struct {
	conn *Conn
	key  asyncKey

	// hdr 是原始**请求头**的副本，用于派生最终响应头。
	// 必须拷成值：原始 Context 在 handler 返回后就被回收了。
	hdr wire.Header

	// signKey 非 nil 时最终响应必须签名（来自 checkSignature 的判定）。
	signKey []byte
	// encrypted 表示原请求来自 SMB3 加密信封，最终响应也必须加密。
	encrypted bool
	// interim 表示已经就本请求回过 interim STATUS_PENDING。
	//
	// 只有客户端置了 SMB2_FLAGS_ASYNC_COMMAND 才回 interim。未置位时不回
	// 任何东西，客户端继续等着，最终响应到了才算完 —— 这正是 Samba 的做法
	// （smbd_smb2_request_pending_queue 仅在 req->do_async 时发 interim）。
	interim bool

	// abort 在请求被取消时关闭，等待方 select 它即可中止等待。
	abort chan struct{}

	mu sync.Mutex
	// onAbort 由等待方注册，取消时被调用一次（在锁外调用）。
	onAbort func()
	// settled 表示结果已定（Complete 或 Cancel 之一已经胜出）。
	settled bool
}

// Cancelled 报告本请求是否已被取消。等待方在退出等待前应当查一次，
// 以便回滚刚刚拿到的资源（例如已授予的锁）。
func (r *AsyncRequest) Cancelled() bool {
	select {
	case <-r.abort:
		return true
	default:
		return false
	}
}

// Aborted 返回取消信号通道，供等待方 select。
func (r *AsyncRequest) Aborted() <-chan struct{} { return r.abort }

// OnAbort 注册取消回调。只生效一次；请求已出结果时注册无效。
func (r *AsyncRequest) OnAbort(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return
	}
	r.onAbort = fn
}

// claim 竞争"给出本请求的最终结果"。胜出返回 true，且**同一请求上
// 只会有一个调用者胜出** —— Complete 与 Cancel 都走这里。
func (r *AsyncRequest) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return false
	}
	r.settled = true
	return true
}

// Complete 用给定状态与响应体完成本请求。重复调用除第一次外全部无效。
//
// build 负责把响应体追加到入参并返回新切片；返回错误时按
// STATUS_INSUFF_SERVER_RESOURCES 兜底（请求已经挂起了，不能静默不答）。
//
// st 为失败状态时忽略 build，改发标准 SMB2 ERROR Response。
//
// 返回值表示**本次调用**是否给出了最终结果。false 意味着 CANCEL 已经先一步
// 应答了 STATUS_CANCELLED —— 等待方此时必须**回滚它刚刚拿到的资源**
// （例如已授予的字节范围锁），否则那些资源会永久泄漏。
func (r *AsyncRequest) Complete(st status.Status, build func(dst []byte) ([]byte, error)) bool {
	if !r.claim() {
		return false
	}
	r.conn.async.remove(r.key)

	// 从未决表摘除之后才写响应，避免"响应已发出但仍在表里"的窗口
	// （那个窗口里迟到的 CANCEL 会试图再次应答同一个 MessageId）。
	body, hdr := r.buildResponse(st, build)
	if sink := r.conn.asyncSinkOrNil(); sink != nil {
		if err := sink.SendAsyncResponse(hdr, body, r.signKey, r.encrypted); err != nil {
			// 连接多半已经关了。这里没有别的补救手段 —— 补发失败就是
			// 送不到，不需要也不应该重投（客户端早就超时了）。
			_ = err
		}
	}
	return true
}

// buildResponse 组装异步响应的头与体。
//
// 响应头派生自**请求头**（MessageId / SessionId / TreeId 必须对得上，
// 否则客户端无法把它与原始请求对账），并补齐异步语义要求的字段：
//
//	Flags     |= SERVER_TO_REDIR（Reply 已置）；ASYNC 时再置 ASYNC_COMMAND
//	AsyncId    与 interim 响应里分配的一致（仅 ASYNC）
//	Status     最终 NTSTATUS
//	Credits    恒 1 —— 详见下方说明
//	NextCommand 0（异步响应永远单发，不参与复合链）
//
// **为什么最终响应只授 1 个 credit**：真正的 credit 授予发生在 interim
// 响应（走常规链路，按客户端请求量放量）。最终响应若再按量授予，一条请求
// 就还了两次 credit，水位会单调递增。但也不能给 0 —— protocol-notes §12
// 明确要求任何响应至少授予 1 个，否则客户端判定流控停滞而挂起。
// 取 1 是这两个约束的交集；水位由 Credits.Grant 的 max（512）封顶，
// 不会无界增长。
func (r *AsyncRequest) buildResponse(st status.Status, build func(dst []byte) ([]byte, error)) ([]byte, wire.Header) {
	hdr := r.hdr.Reply()
	if r.interim {
		hdr.Flags |= wire.FlagAsyncCommand
		hdr.AsyncID = r.key.asyncID
	}
	hdr.Status = uint32(st)
	hdr.Credits = 1
	hdr.NextCommand = 0

	if !st.IsSuccess() {
		out, err := (&wire.ErrorResponse{}).Append(nil)
		if err != nil {
			// ErrorResponse 是定长结构，编码不可能失败。
			return nil, hdr
		}
		return out, hdr
	}
	if build == nil {
		return nil, hdr
	}
	out, err := build(nil)
	if err != nil {
		// 编码失败：改发标准 ERROR Response，绝不静默不答。
		hdr.Status = uint32(status.InsuffServerResources)
		out, _ = (&wire.ErrorResponse{}).Append(nil)
		return out, hdr
	}
	return out, hdr
}

// asyncTable 是一条连接上的未决请求表。
//
// 零值可用。键的选择见 asyncKey 的注释。
type asyncTable struct {
	mu      sync.Mutex
	pending map[asyncKey]*AsyncRequest
	nextID  uint64
	closed  bool
}

// defer 登记一条未决请求。ok 为 false 表示挂不起来，调用方必须同步应答。
//
// 只有同时满足下列条件才允许挂起：
//   - 连接还没开始拆除；
//   - 未决数未达 maxPendingAsync；
//   - 该 (SessionId, MessageId) 上没有另一条未决请求
//     （客户端重用了 MessageId 就是协议错误，重登记会丢掉前一条的响应）。
func (t *asyncTable) deferReq(r *AsyncRequest, async bool, sessionID, messageID uint64) (asyncKey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return asyncKey{}, false
	}
	if len(t.pending) >= maxPendingAsync {
		return asyncKey{}, false
	}

	var key asyncKey
	if async {
		t.nextID++
		// AsyncId 从 1 开始：0 是"没有异步操作"的保留值。
		key = asyncKey{async: true, asyncID: t.nextID}
	} else {
		key = asyncKey{sessionID: sessionID, messageID: messageID}
		if _, dup := t.pending[key]; dup {
			return asyncKey{}, false
		}
	}

	if t.pending == nil {
		t.pending = make(map[asyncKey]*AsyncRequest)
	}
	t.pending[key] = r
	return key, true
}

// remove 摘除一条未决请求。幂等。
func (t *asyncTable) remove(k asyncKey) {
	t.mu.Lock()
	delete(t.pending, k)
	t.mu.Unlock()
}

// lookup 按 CANCEL 的键查找未决请求。
//
// MS-SMB2 §3.3.5.16：CANCEL 复用被取消请求的 MessageId（同步）或
// AsyncId（异步），因此键的构造规则与 deferReq 完全一致。
func (t *asyncTable) lookup(k asyncKey) *AsyncRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending[k]
}

// close 拆除整张表：全部未决请求按"已取消"处理，等待方随即退出。
//
// 不逐条补发响应 —— 连接都要关了，补发必然失败，而等待方需要一个
// 明确的"别等了"信号来释放 goroutine 与它持有的资源（例如已授予的锁）。
//
// 返回摘下来的请求，调用方负责逐个触发其取消回调（在锁外）。
func (t *asyncTable) close() []*AsyncRequest {
	t.mu.Lock()
	t.closed = true
	out := make([]*AsyncRequest, 0, len(t.pending))
	for _, r := range t.pending {
		out = append(out, r)
	}
	t.pending = nil
	t.mu.Unlock()
	return out
}

// pendingCount 返回当前挂起数，用于日志与测试。
func (t *asyncTable) pendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// cancelKey 由一条 CANCEL 请求的头构造出待取消请求的键。
//
// async 为 true（CANCEL 头置了 SMB2_FLAGS_ASYNC_COMMAND）时按 AsyncId 匹配，
// 否则按 (SessionId, MessageId) 匹配 —— 与 asyncKey 的注释同源。
func cancelKey(h wire.Header) asyncKey {
	if h.IsAsync() {
		return asyncKey{async: true, asyncID: h.AsyncID}
	}
	return asyncKey{sessionID: h.SessionID, messageID: h.MessageID}
}

// cancelPending 取消一条未决请求并让它回 STATUS_CANCELLED。
//
// 命中返回 true。未命中返回 false —— 客户端在响应与 CANCEL 交错时本来就
// 会取消到"已经答完"的请求，那不是错误（MS-SMB2 §3.3.5.16 要求静默丢弃）。
func (c *Conn) cancelPending(k asyncKey) bool {
	r := c.async.lookup(k)
	if r == nil {
		return false
	}

	// 先触发等待方的中止回调，再补发 STATUS_CANCELLED。
	// 顺序不能反：回调通常会释放等待方持有的资源（回滚已授予的锁），
	// 让它先跑完，客户端收到 CANCELLED 时服务端状态已经是干净的。
	r.mu.Lock()
	fn := r.onAbort
	r.onAbort = nil
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
	close(r.abort)

	r.Complete(status.Cancelled, nil)
	return true
}

// Defer 把当前请求挂起，稍后由调用方异步补发响应。
//
// 返回 (nil, false) 表示**挂不起来**，handler 必须立即同步应答：
//
//   - 没有注入 AsyncSink（单元测试构造的裸 Conn，或连接正在拆除）；
//   - 本条消息不是复合链的最后一条 —— 见下方限制说明；
//   - 未决数已达 maxPendingAsync；
//   - 同一 (SessionId, MessageId) 上已经有未决请求。
//
// **限制：只允许复合链的末条消息挂起。**
//
// 异步响应是**单发**的：它自己是一帧，不参与任何复合链。若允许链中间的
// 消息挂起，服务端就得把一条复合响应链拆成两段（前段立即发、后段等），
// 而客户端对"半个复合响应"的处理各家实现并不一致。末条消息挂起时，
// 前面各条的响应照常在同一帧里发出，末条要么回 interim STATUS_PENDING
// （客户端置了 ASYNC_COMMAND 时）要么干脆不回，语义干净且可验证。
//
// 真实客户端的 CHANGE_NOTIFY / 阻塞 LOCK 都是单独一帧发出的，
// 这个限制在实践中不会触发；万一触发，handler 退回同步语义。
func (c *Context) Defer() (*AsyncRequest, bool) {
	if c.async != nil {
		// 同一条请求只能挂起一次。
		return nil, false
	}
	if c.Conn.asyncSinkOrNil() == nil {
		return nil, false
	}
	// 见上方「限制」：非末条消息不挂起。
	if c.Header.NextCommand != 0 {
		return nil, false
	}

	r := &AsyncRequest{
		conn:      c.Conn,
		hdr:       c.Header,
		signKey:   c.SignKey,
		encrypted: c.Encrypted,
		interim:   c.Header.IsAsync(),
		abort:     make(chan struct{}),
	}
	key, ok := c.Conn.async.deferReq(r, c.Header.IsAsync(), c.Header.SessionID, c.Header.MessageID)
	if !ok {
		return nil, false
	}
	r.key = key
	c.async = r
	return r, true
}

// Async 返回本条请求挂起后的句柄；未挂起时返回 nil。
func (c *Context) Async() *AsyncRequest { return c.async }
