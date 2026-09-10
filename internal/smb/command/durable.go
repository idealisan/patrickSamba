package command

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// durable.go —— 持久句柄（durable handle）的服务端状态与登记表。
//
// 规范：MS-SMB2 §3.3.5.9（CREATE 处理）里的 .7（v1 授予）/ .9（重连）/
// .11（v2 授予）/ .12（v2 重连）。
//
// 持久句柄的核心诉求是「客户端断线后能在超时内重连、拿回同一个句柄」，
// 因此它必须活在连接与会话的生命周期之外：连接断开（Conn.Close →
// Session.Close）时普通句柄被关闭，持久句柄则要搬进 durableRegistry 进入
// 「等待重连」态，直到被重连认领或超时回收。本服务端是单进程单实例，
// 用一个包级登记表即可，无需引入 server 级对象。

// ---------------------------------------------------------------------------
// 加锁次序（改这个文件之前必读）
//
//	o.mu → r.mu     Open.close() 内部会调 durableTable.remove()。
//	s.mu → r.mu     Session.RemoveTree/Close 先摘句柄，再调 disconnect()。
//
// 由此得出一条铁律：**持有 r.mu 时绝对不许再去取 o.mu 或 s.mu**，否则与上面
// 两条形成加锁顺序反转。具体到本文件：
//
//   - reap() 必须「锁内摘表、锁外 close」。在 r.mu 内调 o.close() 会经
//     remove() 再次申请 r.mu，sync.Mutex 不可重入 —— 直接自死锁。
//   - reconnect() 必须先释放 r.mu 再调 session.rebindOpen()（它取 s.mu）。
//
// 反过来，只写 DurableState 自己字段（key / Invalidated）的操作**必须**放在
// r.mu 内：它们不碰任何别的锁，不会把死锁请回来，而放在锁外就是 data race。
// ---------------------------------------------------------------------------

// durableRegistry 是跨连接存活的持久句柄登记表。
var durableRegistry = newDurableRegistry()

// defaultDurableTimeout 是客户端请求 Timeout=0 时服务端采用的默认保留时长。
//
// 用 var 而非 const：单测可以把她调得很小来触发超时路径。选 60s 是 Windows
// 常见默认值量级；macOS/Windows 客户端通常会显式给值。
var defaultDurableTimeout = 60 * time.Second

// DurableState 挂在 Open.Durable 上，描述一个句柄的持久化能力。
//
// 只有 Granted==true 的句柄才算「持久句柄」；其余只是普通句柄。
type DurableState struct {
	// Granted 表示本句柄已被授予 durable 能力（§3.3.5.9.6 前提满足）。
	Granted bool

	// v2 表示按 durable handle v2 协议（DH2Q/DH2C）；false 为 v1（DHnQ/DHnC）。
	v2 bool

	// guid 是 v2 的 CreateGuid，重连时用于配对。
	guid [16]byte

	// share/path 是授予时的共享名与文件相对路径，重连校验用。
	share string
	path  string

	// identity 是授予时的身份，重连必须同一身份（§3.3.5.9.9）。
	identity *auth.Identity

	// timeout 是「等待重连」态的保留时长。
	timeout time.Duration

	// Invalidated 表示因 lease break 等导致 durable 资格丧失，不可再重连。
	//
	// **由 durableRegistry.mu 保护**（读写都要在 r.mu 内），不要裸读裸写。
	Invalidated bool

	// key 是登记表里的键，便于 disconnect/remove 定位（reconnect 后置空）。
	//
	// **由 durableRegistry.mu 保护**，同上。
	key string
}

// InvalidateDurable 在 lease break 等导致 durable 资格丧失时调用
// （tm-lease 的 break handler 会调它），把句柄从可重连表摘除并标记作废。
//
// 调用方只需持有 *Open，不必知道登记表内部细节。
func (o *Open) InvalidateDurable() {
	durableRegistry.invalidate(o)
}

// durableWaiting 报告本句柄是否正处于「等待重连」态 —— 也就是**客户端已经断开**。
//
// 判据是登记表里的键非空：disconnect 时写入，重连成功或过期回收时清空
// （reap 与 reconnect 都会把它置空）。
//
// 这个判据是 oplock break 路径必需的：断开的持有者不可能回 break 确认，
// 而且**必须**作废它的 durable 登记 —— 否则它重连回来，拿着一份本地缓存的
// 脏数据继续写，而服务端早就把这个文件放给别人了（那条路径不会报错，
// 是静默的脏数据）。见 docs/tm-prereview-20260907.md 第 2 条。
func (o *Open) durableWaiting() bool {
	d := o.Durable
	if d == nil {
		return false
	}
	durableRegistry.mu.Lock()
	defer durableRegistry.mu.Unlock()
	return d.key != ""
}

// durableEntry 是登记表里的一条记录：一个正在「等待重连」的持久句柄，
// 以及重连校验所需的全部上下文。
type durableEntry struct {
	open     *Open
	v2       bool
	guid     [16]byte
	persist  uint64 // v1：persistent FileId
	share    string
	path     string
	identity *auth.Identity
	timeout  time.Duration
	deadline time.Time // 进入等待重连态的过期时刻；零值表示尚未断连

	// timer 是本条记录的过期清扫定时器（一次性，不是常驻 goroutine）。
	//
	// 由来：reap() 原本只在 register() 与 reconnect() 里被顺带调用，
	// 于是「最后一批句柄断连之后服务端再没有 SMB 流量」时，那一批记录
	// 要留到下一次有人连进来才被回收，且**没有任何一方会报错**。
	//
	// team-lead 的约束是「不要起常驻定时器 goroutine」（本项目里
	// 『起了 goroutine 但没人管它生命周期』是另一类坑），所以这里用
	// 有明确宿主与销毁点的一次性定时器：宿主是这条记录，销毁点在所有
	// 摘表路径（dropLocked）。
	timer *time.Timer
}

// dropLocked 摘掉一条记录并销毁它的定时器。调用前必须持有 r.mu。
//
// 所有摘表路径都必须走这里：漏掉 Stop() 的后果不只是「多跑一次空的
// reap」，而是定时器继续持有 r 与 *Open，把本该回收的句柄再续命一轮。
func (r *durableTable) dropLocked(key string) {
	if e := r.entries[key]; e != nil && e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	delete(r.entries, key)
}

type durableTable struct {
	mu sync.Mutex
	// entries 由 mu 保护；每条记录里的 open.Durable.key / .Invalidated
	// 也一并由 mu 保护（见文件头的加锁次序说明）。
	entries map[string]*durableEntry
}

func newDurableRegistry() *durableTable {
	return &durableTable{entries: make(map[string]*durableEntry)}
}

// invalidate 标记句柄丧失 durable 资格并摘除其登记。
//
// Invalidated 与 key 都在 r.mu 内改动 —— 原实现在锁外写 Invalidated，
// 与 reconnect 里的读构成 data race（`go test -race` 可复现）。
func (r *durableTable) invalidate(open *Open) {
	if open == nil || open.Durable == nil {
		return
	}
	r.mu.Lock()
	d := open.Durable
	d.Invalidated = true
	r.detachLocked(open)
	r.mu.Unlock()
}

// detachLocked 摘除 open 的登记项并清空它的键。调用前必须持有 r.mu。
//
// 只在「表里那条记录确实属于这个 open」时才删：键碰撞的情况下按键裸删会把
// **别人的**登记删掉，受害者从此再也无法重连，而且日志里什么都看不到。
func (r *durableTable) detachLocked(open *Open) {
	d := open.Durable
	if d == nil || d.key == "" {
		return
	}
	if e := r.entries[d.key]; e != nil && e.open == open {
		r.dropLocked(d.key)
	}
	d.key = ""
}

// nextPersistentID 是 FileId.Persistent 的**全进程**分配器。
//
// MS-SMB2 §3.3.1.10：服务端把每个 Open 挂进 GlobalOpenTable，索引是
// Open.FileId 的 Persistent 部分，作用域是**整个服务端**，不是单个会话。
// 原实现把它设成 Session 内的计数器（session.go 的 AddOpen），于是每条
// 连接的第一个句柄 Persistent 都是 1。
//
// 这对 durable handle 是致命的：v1 重连（DHnC，§2.2.13.2.3）客户端带回来的
// 就是这个 Persistent 值，服务端只能拿它当登记表键。两个会话撞在同一个键上
// 时，后登记的会顶掉先登记的，重连时服务端把**另一个文件的句柄**交回去 ——
// 一条跨会话的数据泄漏路径，身份校验拦不住（同一用户开两条连接完全正常）。
//
// 修法只能是让 Persistent 本身全进程唯一：键必须能从客户端带回的值反推，
// 所以不存在「另起一个内部唯一键」的选项。
//
// **不变量（改这里之前必读）**：返回值永远落在 [1, 0xFFFFFFFFFFFFFFFF) 内，
// 两端都不能碰：
//
//   - 0 保留作「未分配」。Add 先加后返，首值即 1，天然避开。
//   - 全 1（0xFFFFFFFFFFFFFFFF）是 wire.CompoundFileID 的一半 —— 复合请求里
//     「复用上一条 CREATE 返回的句柄」的占位值（§3.2.4.1.4，macOS 大量使用）。
//     单调递增到它需要 1.8e19 次分配，实际不可达；而且 IsCompound() 要求
//     Persistent 与 Volatile **同时**为全 1，Volatile 是会话内计数器，更够不着。
//
// 将来若有人把这里改成「从别处取值」（复用回收的 ID、取时间戳、取随机数），
// 上面两条就不再自动成立，**必须显式排除这两个值** —— 否则一个正常句柄会被
// IsCompound() 误判成复合占位符，客户端拿到的句柄直接串号。
//
// 不考虑回绕与复用：每秒分配一百万个也要 58 万年。
var nextPersistentID atomic.Uint64

// newPersistentID 分配一个全进程唯一的 FileId.Persistent。见上面的不变量。
func newPersistentID() uint64 { return nextPersistentID.Add(1) }

// durableKey 计算登记表键。
//
//	v1：persistent FileId（DHnC 重连时客户端带回的就是它）
//	v2：CreateGuid（不透明 16 字节，逐字节比较，绝不当 UUID 解析）
//
// v1 的唯一性完全由 newPersistentID 的全局单调性保证；v2 的键是**客户端
// 自己给的** CreateGuid，服务端管不住，恶意客户端可以故意重复 —— 那一侧
// 靠 register() 的占位检查兜底。
func durableKey(v2 bool, guid [16]byte, persist uint64) string {
	if !v2 {
		return "v1:" + fmt.Sprintf("%d", persist)
	}
	return "v2:" + fmt.Sprintf("%x", guid)
}

// register 在授予时登记句柄（deadline 为零，表示尚未断连）。
//
// 返回 false 表示**不能登记**：这个键已经被另一个仍然存活的句柄占着。
// 调用方必须据此**放弃授予**（当普通句柄处理），绝不能假装授予成功 ——
// 两个句柄共用一个键时，后登记的会静默顶掉先登记的，重连时服务端就会把
// **另一个文件的句柄**交给客户端。这是一条跨会话的数据泄漏路径，
// 身份校验拦不住它（同一个用户开两条连接是完全正常的场景）。
func (r *durableTable) register(open *Open) bool {
	d := open.Durable
	if d == nil || !d.Granted {
		return false
	}

	// 顺手清一遍过期项。本表刻意不起常驻回收 goroutine（「起了个 goroutine
	// 但没人管它生命周期」在本项目是另一类坑），改为在每次新登记时做一次
	// 机会式回收 —— 有新句柄进来才可能增长，正好在这里堵住无界增长。
	// 必须在取 r.mu **之前**调用：reap 自己会加锁，且会在锁外 close 句柄。
	r.reap(time.Now())

	r.mu.Lock()
	defer r.mu.Unlock()

	key := durableKey(d.v2, d.guid, open.Persistent)
	if e := r.entries[key]; e != nil && e.open != open {
		return false
	}
	r.entries[key] = &durableEntry{
		open:     open,
		v2:       d.v2,
		guid:     d.guid,
		persist:  open.Persistent,
		share:    d.share,
		path:     d.path,
		identity: d.identity,
		timeout:  d.timeout,
	}
	d.key = key
	return true
}

// disconnect 在连接断开时调用：把句柄搬进「等待重连」态（设置 deadline）。
//
// 幂等且可重复：重连成功后句柄离开登记表，若之后再次断连，这里会
// 用 DurableState 里记下的元数据重建记录，不会丢。
//
// 返回 false 表示这个句柄**不能**转入等待态（不是持久句柄、已作废、或键被
// 别的存活句柄占着）。调用方必须据此把它当普通句柄关掉 —— 既不进等待表又
// 不关闭的话，fd 会永久泄漏。
func (r *durableTable) disconnect(open *Open) bool {
	d := open.Durable
	if d == nil || !d.Granted {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if d.Invalidated {
		return false
	}
	key := durableKey(d.v2, d.guid, open.Persistent)
	e := r.entries[key]
	if e != nil && e.open != open {
		// 键被别人占着。顶掉他等于把他的句柄变成孤儿（永不关闭、永不可
		// 重连），所以这里认输：本句柄放弃 durable，由调用方正常关闭。
		return false
	}
	if e == nil {
		e = &durableEntry{
			open:     open,
			v2:       d.v2,
			guid:     d.guid,
			persist:  open.Persistent,
			share:    d.share,
			path:     d.path,
			identity: d.identity,
			timeout:  d.timeout,
		}
		r.entries[key] = e
	}
	e.deadline = time.Now().Add(d.timeout)
	d.key = key
	r.armLocked(e)
	return true
}

// armLocked 给一条进入「等待重连」态的记录挂上过期清扫定时器。
// 调用前必须持有 r.mu。
//
// 这是**后台回收者**：没有它，最后一批句柄断连之后若服务端再无任何 SMB
// 流量，那一批记录要留到下一次有人连进来才被回收，而且没有任何一方会报错
// （既不是泄漏增长，也不是崩溃，就是静静地躺着）。
//
// 刻意用一次性 time.AfterFunc 而不是常驻 ticker goroutine：定时器有明确的
// 宿主（本条记录）与销毁点（dropLocked），不需要管理生命周期。回调只调
// reap()，而 reap 自己会加锁，与并发的 register/reconnect 天然安全。
//
// 重连成功后记录被 dropLocked 摘掉、定时器随之 Stop()，不会误杀已复活的句柄。
func (r *durableTable) armLocked(e *durableEntry) {
	if e.timer != nil {
		e.timer.Stop()
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = defaultDurableTimeout
	}
	e.timer = time.AfterFunc(timeout, func() { r.reap(time.Now()) })
}

// remove 从句柄表摘除（显式 CLOSE 或作废时调用）。带归属校验，见 detachLocked。
func (r *durableTable) remove(open *Open) {
	if open == nil || open.Durable == nil {
		return
	}
	r.mu.Lock()
	r.detachLocked(open)
	r.mu.Unlock()
}

// reap 回收所有已过期的等待记录：摘表**并关闭底层句柄**。
//
// 只 delete 不 close 是原实现的缺陷：*Open 与它持有的 fd 会永久泄漏，
// Windows 上还会一直占着文件不让删/改名，FILE_DELETE_ON_CLOSE 创建的句柄
// 也永远不会执行那次删除。
//
// 注意「锁内摘表、锁外关闭」的写法不是风格问题：Open.close() 会回头调
// durableTable.remove() 再次申请 r.mu，在临界区里调它就是自死锁。
func (r *durableTable) reap(now time.Time) {
	var expired []*Open
	r.mu.Lock()
	for k, e := range r.entries {
		if e.deadline.IsZero() || !now.After(e.deadline) {
			continue
		}
		r.dropLocked(k)
		if e.open == nil {
			continue
		}
		if d := e.open.Durable; d != nil {
			d.key = ""
			d.Invalidated = true // 超时之后不再具备重连资格
		}
		expired = append(expired, e.open)
	}
	r.mu.Unlock()

	for _, o := range expired {
		o.close()
	}
}

// reconnect 认领一个等待重连的持久句柄。
//
// tree 是本次 CREATE 所在的树连接（重连必然发生在一次新的 TREE_CONNECT 上）；
// 共享名从它取，因此不可能出现「传进来的 share 名与实际树不是同一个」这种
// 参数不一致。tree 为 nil 时按空共享名处理（一定校验失败）。
//
// 返回 (*Open, status.Success) 表示成功；其它 status 表示失败原因，
// *Open 为 nil。校验项（§3.3.5.9.9）：
//   - 记录存在；
//   - 重连到的共享名与授予时一致；
//   - 重连身份与授予时一致；
//   - 未作废、未过期、且已处于「等待重连」态（deadline 非零）。
//
// **次序不能改**：share/身份校验必须排在所有「删除登记」的分支之前。
// 原实现把作废/过期两个删除分支放在授权校验之前，于是任何一个已认证会话
// （含 guest）只要猜中键，就能把别人的登记删掉 —— v1 的键就是 1、2、3…
// 这样的小整数，猜中的成本约等于零。
func (r *durableTable) reconnect(session *Session, tree *Tree, intent *wire.DurableIntent) (*Open, status.Status) {
	var key string
	switch {
	case intent.ReconnectV1 != nil:
		key = durableKey(false, [16]byte{}, intent.ReconnectV1.Persistent)
	case intent.ReconnectV2 != nil:
		key = durableKey(true, intent.ReconnectV2.CreateGUID, 0)
	default:
		return nil, status.ObjectNameNotFound
	}
	shareName := ""
	if tree != nil && tree.Share != nil {
		shareName = tree.Share.Name
	}

	r.reap(time.Now())

	// expired 在临界区外关闭（close 会回头取 r.mu，见文件头加锁次序）。
	var expired *Open

	open, st := func() (*Open, status.Status) {
		r.mu.Lock()
		defer r.mu.Unlock()

		e := r.entries[key]
		if e == nil {
			return nil, status.ObjectNameNotFound
		}

		// —— 先授权 ——（下面才允许动登记表）
		if e.share != shareName {
			return nil, status.ObjectPathInvalid
		}
		if !sameIdentity(e.identity, session.Identity()) {
			return nil, status.AccessDenied
		}

		// —— 再驱逐 ——
		if e.open == nil || e.open.Durable == nil || e.open.Durable.Invalidated {
			r.dropLocked(key)
			return nil, status.ObjectNameNotFound
		}
		if e.deadline.IsZero() {
			// 句柄仍在正常使用、尚未断连：重连无效，但**不删记录** ——
			// 那条记录对应的句柄还活着，删了它一旦真的断连就再也接不回来。
			return nil, status.ObjectNameNotFound
		}
		if time.Now().After(e.deadline) {
			r.dropLocked(key)
			e.open.Durable.key = ""
			e.open.Durable.Invalidated = true
			expired = e.open
			return nil, status.ObjectNameNotFound
		}

		open := e.open
		r.dropLocked(key)
		// 认领成功，清掉等待态。这一步**必须在 r.mu 内**：key 是受 r.mu
		// 保护的字段，锁外写会与并发的 remove/disconnect 构成 data race
		// （-race 能稳定复现）。它只写一个字段、不取任何别的锁，
		// 放进临界区不会把死锁请回来。
		open.Durable.key = ""
		return open, status.Success
	}()

	if expired != nil {
		expired.close()
	}
	if st != status.Success {
		return nil, st
	}

	session.rebindOpen(open, tree)
	return open, status.Success
}

// rebindOpen 把一个已存在（Volatile 不变）的句柄重新挂回会话句柄表。
//
// 重连拿回的是「同一个句柄」，FileId 必须保持不变，所以沿用原 Volatile。
//
// **树必须一起改绑**：旧树随旧连接一起销毁了，而每个命令入口的 resolveOpen
// 都会校验 o.Tree == ctx.Tree（close.go）。不改绑的话，重连本身会返回成功、
// FileId 也对，但之后每一个 READ/WRITE/CLOSE 都拿到 STATUS_INVALID_PARAMETER
// —— 一个「看起来成功、实际是死的」句柄，比直接失败更难排查。
func (s *Session) rebindOpen(o *Open, t *Tree) {
	s.mu.Lock()
	// o.Tree 的写必须持 o.mu：Open.close() 的锁释放路径（open.go）会在
	// 无 s.mu 的上下文里快照读 o.Tree（CI race 门禁抓到的数据竞争）。
	// 锁序恒为 s.mu → o.mu，与既有用法一致。
	o.mu.Lock()
	o.Session = s
	if t != nil {
		o.Tree = t
	}
	o.mu.Unlock()
	s.opens[o.Volatile] = o
	// 新会话的 Volatile 计数器从 0 起，若不抬高，它后续分配到 o.Volatile 时
	// 会**覆盖掉刚认领回来的句柄**（map 同键写入，静默丢失）。
	if o.Volatile > s.nextVolatile {
		s.nextVolatile = o.Volatile
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 授予前提判定（§3.3.5.9.6）
// ---------------------------------------------------------------------------

// leaseDurableEligible 报告本次 CREATE 的 lease 是否满足授予 durable 的前提
// （lease 带 HANDLE caching 位）。默认 false：未接入 lease 模块时，durable
// 只能走 batch oplock 路径。tm-lease 通过 SetLeaseDurableEligible 注入真正的
// LeaseSupportsDurable 实现（零耦合，他不碰我的文件）。
var leaseDurableEligible = func(req *wire.CreateRequest) bool { return false }

// SetLeaseDurableEligible 由 lease 模块在 init 时调用，注入 lease→durable 的
// 前提判定。tm-lease 应传他的 LeaseSupportsDurable。
func SetLeaseDurableEligible(fn func(req *wire.CreateRequest) bool) {
	if fn != nil {
		leaseDurableEligible = fn
	}
}

// durableGrantAllowed 报告本次 CREATE 是否满足授予 durable handle 的前提
// （MS-SMB2 §3.3.5.9.6）：客户端请求了 batch oplock，或 lease 带 HANDLE
// caching 位。
//
// batch oplock 直接用请求线字段判断（无依赖、可独立测试）；lease 分支委托
// 给 lease 模块。两者任一满足即可。
func durableGrantAllowed(req *wire.CreateRequest) bool {
	if req.RequestedOplockLevel == wire.OplockLevelBatch {
		return true
	}
	return leaseDurableEligible(req)
}

// ---------------------------------------------------------------------------
// 重连的 CREATE 处理（短路正常 open 路径）
// ---------------------------------------------------------------------------

// handleDurableReconnect 处理 DHnC/DH2C 重连请求。
//
// 它**不**打开任何文件——直接认领登记表里等待重连的句柄，重写回原 FileId，
// 并把重连响应 context 附上（v1→DHnQ，v2→DH2Q）。找不到/已过期/身份不符时
// 返回对应 NTSTATUS。
func handleDurableReconnect(ctx *Context, intent *wire.DurableIntent) error {
	if ctx.Session == nil || !ctx.Session.Established() {
		return status.UserSessionDeleted
	}
	open, st := durableRegistry.reconnect(ctx.Session, ctx.Tree, intent)
	if st != status.Success {
		return st
	}

	// 句柄虽在，仍要取一次属性回填响应。
	attr, err := open.Handle.Stat()
	if err != nil {
		durableRegistry.remove(open)
		open.close()
		return status.FromVFSError(err)
	}

	resp := &wire.CreateResponse{
		OplockLevel:    wire.OplockLevelNone,
		CreateAction:   wire.FileOpened,
		CreationTime:   vfs.TimeToFiletime(attr.CreateTime),
		LastAccessTime: vfs.TimeToFiletime(attr.AccessTime),
		LastWriteTime:  vfs.TimeToFiletime(attr.WriteTime),
		ChangeTime:     vfs.TimeToFiletime(attr.ChangeTime),
		AllocationSize: uint64(attr.Alloc),
		EndOfFile:      uint64(attr.Size),
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
		FileID:         wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile},
	}
	if open.Durable.v2 {
		resp.Contexts = append(resp.Contexts, wire.CreateContext{
			Name: wire.CreateContextDH2Q,
			Data: (&wire.DurableResponseV2{Timeout: msFromDuration(open.Durable.timeout), Flags: 0}).Encode(),
		})
	} else {
		resp.Contexts = append(resp.Contexts, wire.CreateContext{
			Name: wire.CreateContextDHnQ,
			Data: wire.EncodeDurableResponse(),
		})
	}

	out, err := resp.Append(ctx.Out)
	if err != nil {
		ctx.Log.Error("编码 durable 重连 Response 失败", "err", err)
		return status.InsuffServerResources
	}
	ctx.Out = out
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func timeoutFromMs(ms uint32) time.Duration {
	if ms == 0 {
		return defaultDurableTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

func msFromDuration(d time.Duration) uint32 {
	if d <= 0 {
		return uint32(defaultDurableTimeout / time.Millisecond)
	}
	return uint32(d / time.Millisecond)
}

// sameIdentity 比较两个身份是否代表同一个用户（用于重连校验）。
func sameIdentity(a, b *auth.Identity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.User == b.User &&
		a.Domain == b.Domain &&
		a.Guest == b.Guest &&
		a.Anonymous == b.Anonymous
}
