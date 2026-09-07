package command

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件钉的是 oplock / lease 的授予与打破（oplock_grant.go / oplock_state.go）。
//
// 变异自检方向：
//   - 把 planOplock 里的 break 检查整块删掉 →
//     TestOplockBreakDefersConflictingCreate 变红（第二个客户端直接放行，
//     读到持有者还攥在本地缓存里的旧内容 —— 这正是本特性要防的事故）；
//   - 把 grantIfDeferrable 的 NextCommand 判断挪到 planOplock 开头 →
//     TestMidChainCreateStillBreaksExistingOplock 变红（同上，且更隐蔽）；
//   - 把 ackOplock / ackLease 的查表去掉 → 两个 ack 用例变红；
//   - 把 Settings.Oplocks 的判断去掉 → TestOplockNotGrantedWhenDisabled 变红。

// oplockEnv 是两个客户端 + 一份真实文件系统的测试环境。
type oplockEnv struct {
	share  *Share
	a      *Context
	b      *Context
	connA  *Conn
	connB  *Conn
	sink   *fakeAsyncSink
	breakA *fakeBreakSender
}

// newOplockEnvNoSink 与 newOplockEnv 的差别：B **没有**注入异步 sink。
//
// 用于验证降级路径 —— 拿不到挂起通路时，需要 break 的打开必须回
// STATUS_SHARING_VIOLATION，而不是放行走掉（那会造成脏读）。
func newOplockEnvNoSink(t *testing.T, oplocks bool) *oplockEnv {
	t.Helper()
	root := t.TempDir()
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}

	breakA := &fakeBreakSender{}
	e := &oplockEnv{share: share, breakA: breakA}
	e.connA, e.a = newOplockClient(t, share, oplocks, nil, breakA)
	e.connB, e.b = newOplockClient(t, share, oplocks, nil, nil)
	return e
}

func newOplockEnv(t *testing.T, oplocks bool) *oplockEnv {
	t.Helper()
	root := t.TempDir()
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}

	sink := &fakeAsyncSink{}
	breakA := &fakeBreakSender{}
	e := &oplockEnv{share: share, sink: sink, breakA: breakA}

	// A 只装 break sender（它是持有者，会收到 break 通知）。
	e.connA, e.a = newOplockClient(t, share, oplocks, nil, breakA)
	// B 只装 async sink（它是被推迟的那一个，要补发响应）。
	e.connB, e.b = newOplockClient(t, share, oplocks, sink, nil)
	return e
}

func newOplockClient(t *testing.T, share *Share, oplocks bool,
	sink AsyncSink, breaks BreakSender) (*Conn, *Context) {
	t.Helper()
	conn := NewConn(&Settings{Shares: []*Share{share}, Oplocks: oplocks}, "test", "test")
	if sink != nil {
		conn.SetAsyncSink(sink)
	}
	if breaks != nil {
		conn.SetBreakSender(breaks)
	}
	sess, st := conn.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession: %v", st)
	}
	sess.Establish(&auth.Identity{User: "u"})
	tree, st := sess.NewTree(share)
	if st != status.Success {
		t.Fatalf("NewTree: %v", st)
	}
	return conn, &Context{
		Conn:    conn,
		Chain:   &Chain{},
		Session: sess,
		Tree:    tree,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// openWith 走真实 createFile 打开一个文件，返回句柄与 CREATE 响应。
func openWith(t *testing.T, ctx *Context, name string, access wire.Access,
	share wire.ShareAccess, req *wire.CreateRequest) (*Open, *wire.CreateResponse) {
	t.Helper()
	if req == nil {
		req = &wire.CreateRequest{}
	}
	req.Name = name
	req.DesiredAccess = access
	req.ShareAccess = share
	if req.CreateDisposition == 0 {
		req.CreateDisposition = wire.FileOpenIf
	}
	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	if err := createFile(ctx, req); err != nil {
		return nil, nil
	}
	resp, err := wire.ParseCreateResponse(ctx.Out)
	if err != nil {
		t.Fatalf("解析 CREATE Response 失败：%v", err)
	}
	return ctx.Chain.LastOpen, resp
}

// accessRW 是"要读写"的访问掩码；shareAll 复用 share_access_test.go 里的同名常量。
const accessRW = wire.FileReadData | wire.FileWriteData

func TestOplockNotGrantedWhenDisabled(t *testing.T) {
	e := newOplockEnv(t, false)
	_, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch})
	if resp == nil {
		t.Fatal("打开失败")
	}
	// 默认关闭时必须与引入本特性之前**完全一致**。
	if resp.OplockLevel != wire.OplockLevelNone {
		t.Fatalf("关闭时应回 NONE，实际 %v", resp.OplockLevel)
	}
	if got := e.share.oplocks.count(); got != 0 {
		t.Fatalf("关闭时不应产生条目，实际 %d", got)
	}
}

func TestOplockGrantedToSoleOpener(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  wire.OplockLevel
		level wire.OplockLevel
	}{
		{"batch", wire.OplockLevelBatch, wire.OplockLevelBatch},
		{"exclusive", wire.OplockLevelExclusive, wire.OplockLevelExclusive},
		{"level II", wire.OplockLevelII, wire.OplockLevelII},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOplockEnv(t, true)
			o, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
				&wire.CreateRequest{RequestedOplockLevel: tc.level})
			if resp == nil {
				t.Fatal("打开失败")
			}
			if resp.OplockLevel != tc.want {
				t.Fatalf("应授予 %v，实际 %v", tc.want, resp.OplockLevel)
			}
			if !e.share.oplocks.has(o) {
				t.Fatal("授予后表里应能按句柄找到条目")
			}
		})
	}
}

// TestOplockNotGrantedWhenOtherOpener：别人已经打开着就不授予写缓存类许可，
// 否则第二个客户端读到的可能是第一个客户端本地缓存里的旧内容。
func TestOplockNotGrantedWhenOtherOpener(t *testing.T) {
	e := newOplockEnv(t, true)
	if _, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 打开失败")
	}
	// A 关掉再让 B 开：A 关闭会收回许可，B 应当能拿到。
	e.a.Chain.LastOpen.close()

	_, respB := openWith(t, e.b, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch})
	if respB == nil {
		t.Fatal("B 打开失败")
	}
	if respB.OplockLevel != wire.OplockLevelBatch {
		t.Fatalf("独占打开者关闭后应能授予，实际 %v", respB.OplockLevel)
	}
}

// TestSecondOpenerDoesNotShareCache：A 持有许可时 B 的打开**不授予**
// （表按对象只留一条条目，两条并存意味着两套互不知情的缓存）。
// TestConcurrentReadersEachGetReadCache 钉住「多个并发只读者各持一份读缓存」。
//
// 这是**标准做法**（Samba / Windows）：Level II oplock 与 R lease 都不是排他的，
// 因为只读者谁也不会改数据。v0.7.2 之前表里每文件只留一条条目，第二个只读
// 客户端什么都拿不到 —— 正确性没问题，但白丢一份收益，而且与标准不一致。
//
// 变异自检方向：
//   - 把 planOplock 的 default 分支改成恒不授予 → 本用例变红；
//   - 把表改回单条目（grant 覆盖而不是 append）→ countFor 断言变红。
func TestConcurrentReadersEachGetReadCache(t *testing.T) {
	e := newOplockEnv(t, true)
	a, respA := openWith(t, e.a, "f.txt", wire.FileReadData, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelII})
	if a == nil || respA == nil {
		t.Fatal("A 打开失败")
	}
	if respA.OplockLevel != wire.OplockLevelII {
		t.Fatalf("首个只读打开者应拿到 Level II，实际 %v", respA.OplockLevel)
	}

	// B 也是只读：与 A 的读缓存不冲突，**应当同样拿到读缓存**。
	b, respB := openWith(t, e.b, "f.txt", wire.FileReadData, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelII})
	if b == nil || respB == nil {
		t.Fatal("B 打开失败")
	}
	if respB.OplockLevel != wire.OplockLevelII {
		t.Fatalf("第二个只读打开者应同样拿到 Level II，实际 %v", respB.OplockLevel)
	}

	// 关键是**两条并存**，不是覆盖成一条。
	key := oplockKey{fileID: a.oplockFileID, stream: a.Stream}
	if got := e.share.oplocks.countFor(key); got != 2 {
		t.Fatalf("同一文件上应有 2 条读缓存条目，实际 %d", got)
	}
	if !e.share.oplocks.has(a) || !e.share.oplocks.has(b) {
		t.Fatal("两个句柄都应能在表里找到自己的条目")
	}
}

// TestWriteOpenerBreaksReadCaches：要写的人进来时，**读缓存也必须打破**。
//
// 这一条是数据正确性的分界线：读者缓存着旧内容，写者改完文件后
// 读者再读就会拿到旧数据 —— 而没有任何一方会报错。
func TestWriteOpenerBreaksReadCaches(t *testing.T) {
	e := newOplockEnv(t, true)
	a, _ := openWith(t, e.a, "f.txt", wire.FileReadData, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelII})
	if a == nil {
		t.Fatal("A 打开失败")
	}

	// B 要写：必须打破 A 的读缓存，因此这次打开要被推迟。
	e.b.Out = make([]byte, wire.HeaderSize)
	e.b.Chain = &Chain{}
	err := createFile(e.b, &wire.CreateRequest{
		Name:                 "f.txt",
		DesiredAccess:        accessRW,
		ShareAccess:          shareAll,
		CreateDisposition:    wire.FileOpenIf,
		RequestedOplockLevel: wire.OplockLevelBatch,
	})
	if err != nil {
		t.Fatalf("B 的 CREATE 不应当场失败，实际 %v", err)
	}
	if e.b.Async() == nil {
		t.Fatal("写者进来时应先打破读缓存，因此 B 的 CREATE 必须挂起")
	}
	// 打破目标是 NONE：读缓存也留不住。
	if got := e.breakA.lastOplock.OplockLevel; got != wire.OplockLevelNone {
		t.Fatalf("写者进来时读缓存应被要求降到 NONE，实际 %v", got)
	}
}

func TestLeaseGrantedAndEchoed(t *testing.T) {
	e := newOplockEnv(t, true)
	var key [16]byte
	key[0] = 0xAB
	lc := &wire.LeaseContext{
		LeaseKey:   key,
		LeaseState: wire.LeaseReadCaching | wire.LeaseHandleCaching | wire.LeaseWriteCaching,
	}

	o, resp := openWith(t, e.a, "f.txt", accessRW, shareAll, &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelLease,
		Contexts: []wire.CreateContext{
			{Name: wire.CreateContextRqLs, Data: lc.Encode()},
		},
	})
	if resp == nil {
		t.Fatal("打开失败")
	}
	if resp.OplockLevel != wire.OplockLevelLease {
		t.Fatalf("租约族的 OplockLevel 应为 0xFF(LEASE)，实际 %v", resp.OplockLevel)
	}
	data, ok := wire.FindCreateContext(resp.Contexts, wire.CreateContextRqLs)
	if !ok {
		t.Fatal("响应里必须带回 RqLs create context")
	}
	got, err := wire.ParseLeaseContext(data)
	if err != nil {
		t.Fatalf("解析响应租约失败：%v", err)
	}
	if got.LeaseKey != key {
		t.Fatalf("LeaseKey 应原样回显，实际 %v", got.LeaseKey)
	}
	if got.LeaseState != lc.LeaseState {
		t.Fatalf("租约状态应为 %v，实际 %v", lc.LeaseState, got.LeaseState)
	}
	if !e.share.oplocks.has(o) {
		t.Fatal("租约应登记进表")
	}
}

// TestOplockBreakDefersConflictingCreate：B 要写 A 持有写缓存的文件 →
// 必须先打破 A 的缓存并且**等确认**，期间 B 的 CREATE 被挂起。
func TestOplockBreakDefersConflictingCreate(t *testing.T) {
	e := newOplockEnv(t, true)
	a, _ := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch})
	if a == nil {
		t.Fatal("A 打开失败")
	}

	// B 以写方式打开同一文件。
	e.b.Out = make([]byte, wire.HeaderSize)
	e.b.Chain = &Chain{}
	err := createFile(e.b, &wire.CreateRequest{
		Name:                 "f.txt",
		DesiredAccess:        accessRW,
		ShareAccess:          shareAll,
		CreateDisposition:    wire.FileOpenIf,
		RequestedOplockLevel: wire.OplockLevelBatch,
	})
	if err != nil {
		t.Fatalf("B 的 CREATE 不应当场失败，实际 %v", err)
	}
	if e.b.Async() == nil {
		t.Fatal("需要打破缓存许可时 B 的 CREATE 必须挂起")
	}
	if e.sink.count() != 0 {
		t.Fatal("确认到达前不得补发响应")
	}
	// 必须已经向 A 发过 break 通知，且要求降到 NONE（B 要写）。
	if got := e.breakA.oplocks; got != 1 {
		t.Fatalf("应向 A 发 1 条 break 通知，实际 %d", got)
	}
	if got := e.breakA.lastOplock.OplockLevel; got != wire.OplockLevelNone {
		t.Fatalf("B 要写，应要求 A 降到 NONE，实际 %v", got)
	}

	// A 回确认（走真实 OPLOCK_BREAK handler）。
	ackCtx := &Context{
		Conn: e.connA, Chain: &Chain{}, Session: e.a.Session, Tree: e.a.Tree,
		Log: e.a.Log,
	}
	ackHdr := wire.Header{Command: wire.CommandOplockBreak, SessionID: e.a.Session.ID, TreeID: e.a.Tree.ID}
	ackMsg := append(ackHdr.Append(nil), (&wire.OplockBreak{
		OplockLevel: wire.OplockLevelNone,
		FileID:      wire.FileID{Persistent: a.Persistent, Volatile: a.Volatile},
	}).Append(nil)...)
	h, _ := wire.ParseHeader(ackMsg)
	ackCtx.Header, ackCtx.Msg = h, ackMsg
	ackCtx.Out = make([]byte, wire.HeaderSize)
	Dispatch(ackCtx)
	if ackCtx.Status != status.Success {
		t.Fatalf("A 的 break 确认应被接受，实际 %v", ackCtx.Status)
	}

	// 确认之后 B 的 CREATE 应当完成。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.sink.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if e.sink.count() != 1 {
		t.Fatalf("确认后应补发 1 条响应，实际 %d 条", e.sink.count())
	}
	if got := status.Status(e.sink.last().hdr.Status); got != status.Success {
		t.Fatalf("B 的 CREATE 应成功，实际 %v", got)
	}
}

// TestMidChainCreateStillBreaksExistingOplock 钉一条极易写错的判定。
//
// 曾经（v0.7.0）把「复合链中间就返回」的短路写在 planOplock **开头**，
// 于是链中间的 CREATE 连带跳过了 break 检查 —— 别的客户端读到的可能是
// 持有者还攥在本地缓存里的旧内容，而服务端日志一切正常。
func TestMidChainCreateStillBreaksExistingOplock(t *testing.T) {
	e := newOplockEnv(t, true)
	if _, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 打开失败")
	}

	// B 的 CREATE 是复合链中间的一条（NextCommand != 0）。
	e.b.Header = wire.Header{NextCommand: 128}
	e.b.Out = make([]byte, wire.HeaderSize)
	e.b.Chain = &Chain{}
	err := createFile(e.b, &wire.CreateRequest{
		Name:                 "f.txt",
		DesiredAccess:        accessRW,
		ShareAccess:          shareAll,
		CreateDisposition:    wire.FileOpenIf,
		RequestedOplockLevel: wire.OplockLevelBatch,
	})
	// v0.7.2 起：链中间**也能挂起**（server 层会续跑剩下的消息），
	// 所以这里是"挂起"而不是回绝。v0.7.0/1 时这里回 SHARING_VIOLATION。
	if err != nil {
		t.Fatalf("链中间的 CREATE 应当被挂起而不是当场失败，实际 %v", err)
	}
	if e.b.Async() == nil {
		t.Fatal("冲突时必须挂起等待 break 确认")
	}
	// 关键：break **已经发出**了 —— 若被链位置的短路吃掉，这里会是 0 条。
	if got := e.breakA.oplocks; got != 1 {
		t.Fatalf("链中间的 CREATE 同样必须发出 break，实际 %d 条", got)
	}
	// 没有可挂起通路时（无 async sink）才回退到回绝。
	plain := newOplockEnvNoSink(t, true)
	if _, resp := openWith(t, plain.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 打开失败")
	}
	plain.b.Header = wire.Header{NextCommand: 128}
	plain.b.Out = make([]byte, wire.HeaderSize)
	plain.b.Chain = &Chain{}
	if err := createFile(plain.b, &wire.CreateRequest{
		Name:                 "f.txt",
		DesiredAccess:        accessRW,
		ShareAccess:          shareAll,
		CreateDisposition:    wire.FileOpenIf,
		RequestedOplockLevel: wire.OplockLevelBatch,
	}); err != status.SharingViolation {
		t.Fatalf("挂不起来时应回 STATUS_SHARING_VIOLATION，实际 %v", err)
	}
}

// TestOplockAckWithoutPendingBreakIsProtocolError：没有正在等待的 break
// 时收到确认，按规范回 STATUS_INVALID_OPLOCK_PROTOCOL。
func TestOplockAckWithoutPendingBreakIsProtocolError(t *testing.T) {
	e := newOplockEnv(t, true)
	a, _ := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch})
	if a == nil {
		t.Fatal("A 打开失败")
	}
	ackCtx := &Context{
		Conn: e.connA, Chain: &Chain{}, Session: e.a.Session, Tree: e.a.Tree, Log: e.a.Log,
	}
	ackHdr := wire.Header{Command: wire.CommandOplockBreak, SessionID: e.a.Session.ID, TreeID: e.a.Tree.ID}
	ackMsg := append(ackHdr.Append(nil), (&wire.OplockBreak{
		OplockLevel: wire.OplockLevelII,
		FileID:      wire.FileID{Persistent: a.Persistent, Volatile: a.Volatile},
	}).Append(nil)...)
	h, _ := wire.ParseHeader(ackMsg)
	ackCtx.Header, ackCtx.Msg = h, ackMsg
	ackCtx.Out = make([]byte, wire.HeaderSize)
	Dispatch(ackCtx)
	if ackCtx.Status != status.InvalidOplockProtocol {
		t.Fatalf("应回 STATUS_INVALID_OPLOCK_PROTOCOL，实际 %v", ackCtx.Status)
	}
}

// TestOplockReleasedOnHandleClose：句柄关闭必须收回缓存许可，
// 否则别的对象复用同一 fileID 时会继承一份"有人在缓存"的假象。
func TestOplockReleasedOnHandleClose(t *testing.T) {
	e := newOplockEnv(t, true)
	o, _ := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch})
	if o == nil {
		t.Fatal("打开失败")
	}
	if !e.share.oplocks.has(o) {
		t.Fatal("授予后应有条目")
	}
	o.close()
	if got := e.share.oplocks.count(); got != 0 {
		t.Fatalf("句柄关闭后应清空，实际剩 %d", got)
	}
}

// TestSameSessionNeverBreaksOwnOplock：同一会话自己再开一次不会触发 break ——
// 客户端知道自己在动这个文件，打破自己的缓存没有意义。
func TestSameSessionNeverBreaksOwnOplock(t *testing.T) {
	e := newOplockEnv(t, true)
	if _, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 打开失败")
	}
	// 同一会话再开一次（不同句柄）。
	if _, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 第二次打开失败")
	}
	if got := e.breakA.oplocks; got != 0 {
		t.Fatalf("同一会话不应触发 break，实际发了 %d 条", got)
	}
}

// TestOplockRejectsUnknownLeaseState：租约状态位出现未知位时不猜含义。
func TestOplockRejectsUnknownLeaseState(t *testing.T) {
	e := newOplockEnv(t, true)
	lc := &wire.LeaseContext{LeaseState: wire.LeaseReadCaching | 0x08}
	e.a.Out = make([]byte, wire.HeaderSize)
	e.a.Chain = &Chain{}
	err := createFile(e.a, &wire.CreateRequest{
		Name:                 "f.txt",
		DesiredAccess:        accessRW,
		ShareAccess:          shareAll,
		CreateDisposition:    wire.FileOpenIf,
		RequestedOplockLevel: wire.OplockLevelLease,
		Contexts: []wire.CreateContext{
			{Name: wire.CreateContextRqLs, Data: lc.Encode()},
		},
	})
	if err != status.InvalidParameter {
		t.Fatalf("未知租约状态位应回 STATUS_INVALID_PARAMETER，实际 %v", err)
	}
}

// has 报告表里是否有某个句柄的条目（测试辅助，顺带验证 count 的实现）。
// has 报告表里是否有该句柄的条目。
func (t *oplockTable) has(o *Open) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, list := range t.m {
		for _, e := range list {
			if e.owner == o {
				return true
			}
		}
	}
	return false
}

// countFor 返回某个对象上的条目条数（多读者并存时 > 1）。
func (t *oplockTable) countFor(k oplockKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m[k])
}

// count 返回表里的条目总数（跨所有对象）。
func (t *oplockTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, list := range t.m {
		n += len(list)
	}
	return n
}

// TestOplockBreakTimeoutThenCancelDoesNotPanic：break 超时的收尾必须幂等。
//
// 一次超时会连着走两步：wait 的超时分支先关一次 done，紧接着
// awaitOplockBreak 对同一条目调 cancelBreak，那又要关一次。
// 修复前第二步是 "close of closed channel" —— 而它跑在挂起 CREATE 的
// goroutine 上（不在 serve 的 recover 范围内），一次 break 超时就带走整个进程。
//
// 变异自检：把 breakWait.finish 改回裸 close(w.done) → 本例 panic 变红。
func TestOplockBreakTimeoutThenCancelDoesNotPanic(t *testing.T) {
	var tbl oplockTable
	owner := &Open{}
	e := &oplockEntry{owner: owner, level: wire.OplockLevelBatch}
	tbl.grant(oplockKey{}, e)

	w, ok := tbl.beginBreak(e, wire.OplockLevelNone, 0)
	if !ok {
		t.Fatal("首个 break 应当能登记")
	}

	// 没有任何确认，等到超时。
	if w.wait(time.Millisecond) {
		t.Fatal("无人确认时 wait 应返回 false")
	}
	// 超时的下一步就是 cancelBreak（awaitOplockBreak 的收尾）。
	tbl.cancelBreak(e)

	// 迟到的确认同样不能把通道关第二次。
	w.ack(wire.OplockLevelII, 0)

	// 收尾后条目应已按"已打破"降级：要求降到 NONE，于是整条摘掉。
	if tbl.count() != 0 {
		t.Fatalf("cancelBreak 后应无条目，实际 %d", tbl.count())
	}
	if tbl.breaking != 0 {
		t.Fatalf("cancelBreak 后进行中的 break 应归零，实际 %d", tbl.breaking)
	}
}

// TestOplockBreakAckAndTimeoutRace：确认与超时并发时也只能关一次通道。
//
// 两个 goroutine 分别代表"持有者回了确认"与"等到超时"，谁先谁后不确定；
// 无论谁赢，都不能 panic，也不能漏唤醒（wait 必须返回）。
func TestOplockBreakAckAndTimeoutRace(t *testing.T) {
	var tbl oplockTable
	owner := &Open{}
	e := &oplockEntry{owner: owner, level: wire.OplockLevelBatch}
	tbl.grant(oplockKey{}, e)

	w, ok := tbl.beginBreak(e, wire.OplockLevelNone, 0)
	if !ok {
		t.Fatal("首个 break 应当能登记")
	}

	done := make(chan bool, 1)
	go func() { done <- w.wait(time.Millisecond) }()
	go w.ack(wire.OplockLevelNone, 0)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait 既没被确认唤醒也没超时返回 —— 等待方被挂死了")
	}

	// 再关一次也不该出事（超时路径与确认路径都跑过了）。
	w.finish()
	tbl.cancelBreak(e)
}

// TestOplockBreakSentOnlyOnce：同一条 break 的通知只能发出一次。
//
// 多个等待方会拿到 beginBreak 返回的同一个 wait，各自跑在自己的 goroutine
// 上。判定必须走 markSent（表锁下置位），裸读写 sent 既是一场数据竞争，
// 也会在交错不利时给同一个持有者发两遍 break。
//
// 变异自检：把 markSent 换成 w.sent 的裸读写 → -race 下本例报竞争。
func TestOplockBreakSentOnlyOnce(t *testing.T) {
	var tbl oplockTable
	owner := &Open{}
	e := &oplockEntry{owner: owner, level: wire.OplockLevelBatch}
	tbl.grant(oplockKey{}, e)

	w, ok := tbl.beginBreak(e, wire.OplockLevelNone, 0)
	if !ok {
		t.Fatal("首个 break 应当能登记")
	}

	// 16 个并发的后续打开都要同一条 break。
	const n = 16
	var sent int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if tbl.markSent(w) {
				atomic.AddInt32(&sent, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if sent != 1 {
		t.Fatalf("break 通知应恰好发出一次，实际 %d 次", sent)
	}
}
