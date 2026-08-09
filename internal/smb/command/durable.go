package command

import (
	"fmt"
	"sync"
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
	Invalidated bool

	// key 是登记表里的键，便于 disconnect/remove 定位（reconnect 后置空）。
	key string
}

// InvalidateDurable 在 lease break 等导致 durable 资格丧失时调用
// （tm-lease 的 break handler 会调它），把句柄从可重连表摘除并标记作废。
//
// 调用方只需持有 *Open，不必知道登记表内部细节。
func (o *Open) InvalidateDurable() {
	if o.Durable == nil {
		return
	}
	o.Durable.Invalidated = true
	durableRegistry.remove(o)
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
}

type durableTable struct {
	mu             sync.Mutex
	entries        map[string]*durableEntry
	defaultTimeout time.Duration
}

func newDurableRegistry() *durableTable {
	return &durableTable{
		entries:        make(map[string]*durableEntry),
		defaultTimeout: defaultDurableTimeout,
	}
}

// durableKey 计算登记表键。
//
//	v1：persistent FileId（DHnC 重连时客户端带回的就是它）
//	v2：CreateGuid（不透明 16 字节，逐字节比较，绝不当 UUID 解析）
func durableKey(v2 bool, guid [16]byte, persist uint64) string {
	if !v2 {
		return "v1:" + fmt.Sprintf("%d", persist)
	}
	return "v2:" + fmt.Sprintf("%x", guid)
}

// register 在授予时登记句柄（deadline 为零，表示尚未断连）。
func (r *durableTable) register(open *Open) {
	d := open.Durable
	if d == nil || !d.Granted {
		return
	}
	r.mu.Lock()
	key := durableKey(d.v2, d.guid, open.Persistent)
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
	r.mu.Unlock()
}

// disconnect 在连接断开时调用：把句柄搬进「等待重连」态（设置 deadline）。
//
// 幂等且可重复：重连成功后句柄离开登记表，若之后再次断连，这里会
// 用 DurableState 里记下的元数据重建记录，不会丢。
func (r *durableTable) disconnect(open *Open) {
	d := open.Durable
	if d == nil || !d.Granted {
		return
	}
	r.mu.Lock()
	key := durableKey(d.v2, d.guid, open.Persistent)
	e := r.entries[key]
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
	r.mu.Unlock()
}

// remove 从句柄表摘除（显式 CLOSE 或作废时调用）。
func (r *durableTable) remove(open *Open) {
	if open.Durable == nil {
		return
	}
	r.mu.Lock()
	if open.Durable.key != "" {
		delete(r.entries, open.Durable.key)
	}
	r.mu.Unlock()
}

// reap 回收所有已过期的等待记录（惰性调用，避免常驻 goroutine）。
func (r *durableTable) reap(now time.Time) {
	r.mu.Lock()
	for k, e := range r.entries {
		if !e.deadline.IsZero() && now.After(e.deadline) {
			delete(r.entries, k)
		}
	}
	r.mu.Unlock()
}

// reconnect 认领一个等待重连的持久句柄。
//
// 返回 (*Open, status.Success) 表示成功；其它 status 表示失败原因，
// *Open 为 nil。校验项（§3.3.5.9.9）：
//   - 记录存在、未作废、未过期、且已处于「等待重连」态（deadline 非零）；
//   - 重连到的共享名与授予时一致；
//   - 重连身份与授予时一致。
func (r *durableTable) reconnect(session *Session, intent *wire.DurableIntent, shareName string) (*Open, status.Status) {
	var key string
	switch {
	case intent.ReconnectV1 != nil:
		key = durableKey(false, [16]byte{}, intent.ReconnectV1.Persistent)
	case intent.ReconnectV2 != nil:
		key = durableKey(true, intent.ReconnectV2.CreateGUID, 0)
	default:
		return nil, status.ObjectNameNotFound
	}

	r.reap(time.Now())

	r.mu.Lock()
	e := r.entries[key]
	if e == nil {
		r.mu.Unlock()
		return nil, status.ObjectNameNotFound
	}
	if e.open.Durable.Invalidated {
		delete(r.entries, key)
		r.mu.Unlock()
		return nil, status.ObjectNameNotFound
	}
	// deadline 为零表示句柄仍在正常使用、尚未断连：重连无效。
	if e.deadline.IsZero() || time.Now().After(e.deadline) {
		delete(r.entries, key)
		r.mu.Unlock()
		return nil, status.ObjectNameNotFound
	}
	if e.share != shareName {
		r.mu.Unlock()
		return nil, status.ObjectPathInvalid
	}
	if !sameIdentity(e.identity, session.Identity()) {
		r.mu.Unlock()
		return nil, status.AccessDenied
	}

	open := e.open
	delete(r.entries, key)
	r.mu.Unlock()

	// 重新认领：搬回新会话的句柄表，清掉等待态。
	open.Session = session
	session.rebindOpen(open)
	open.Durable.disconnectedAtZero()
	return open, status.Success
}

// disconnectedAtZero 清掉等待态（reconnect 成功后调用）。
func (d *DurableState) disconnectedAtZero() {
	d.key = ""
}

// rebindOpen 把一个已存在（Volatile 不变）的句柄重新挂回会话句柄表。
//
// 重连拿回的是「同一个句柄」，FileId 必须保持不变，所以沿用原 Volatile。
func (s *Session) rebindOpen(o *Open) {
	s.mu.Lock()
	o.Session = s
	s.opens[o.Volatile] = o
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
	shareName := ""
	if ctx.Tree != nil {
		shareName = ctx.Tree.Share.Name
	}

	open, st := durableRegistry.reconnect(ctx.Session, intent, shareName)
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
