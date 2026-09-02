package command

import (
	"io"
	"log/slog"
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
func TestSecondOpenerDoesNotShareCache(t *testing.T) {
	e := newOplockEnv(t, true)
	a, respA := openWith(t, e.a, "f.txt", wire.FileReadData, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelII})
	if a == nil || respA == nil {
		t.Fatal("A 打开失败")
	}
	// A 的 II（读缓存）+ B 只读：不构成冲突，但也不授予第二条。
	_, respB := openWith(t, e.b, "f.txt", wire.FileReadData, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelII})
	if respB == nil {
		t.Fatal("B 打开失败")
	}
	if respB.OplockLevel != wire.OplockLevelNone {
		t.Fatalf("第二个打开者不应拿到缓存许可，实际 %v", respB.OplockLevel)
	}
	if got := e.share.oplocks.count(); got != 1 {
		t.Fatalf("表里应只有 1 条条目，实际 %d", got)
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

// TestMidChainCreateStillBreaksExistingOplock：复合链中间的 CREATE
// **拿不到**缓存许可，但绝不能因此跳过 break 检查 —— 那会直接放行一次
// 会读到脏数据的访问。
func TestMidChainCreateStillBreaksExistingOplock(t *testing.T) {
	e := newOplockEnv(t, true)
	if _, resp := openWith(t, e.a, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("A 打开失败")
	}

	// B 的 CREATE 是复合链中间的一条（NextCommand != 0），且拿不到挂起通路。
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
	// 挂不起来 → 必须回共享冲突，而不是悄无声息地放行。
	if err != status.SharingViolation {
		t.Fatalf("无法等确认时应回 STATUS_SHARING_VIOLATION，实际 %v", err)
	}
	// break 通知**已经发出**：A 收到后会让出缓存，B 重试即可成功。
	if got := e.breakA.oplocks; got != 1 {
		t.Fatalf("即便要回绝也应先发出 break，实际发了 %d 条", got)
	}

	// 回绝路径必须把这次 break **就地收尾**，否则进行中的 break 计数会
	// 永久占着（攒够上限后该共享上所有冲突打开都变成 SHARING_VIOLATION，
	// 且重开服务才能恢复）。
	//
	// 可观测后果：B 换成可挂起的普通打开后应当**直接成功**，
	// 既不需要再发一次 break，也不会被推迟。
	e.b.Header = wire.Header{}
	if _, resp := openWith(t, e.b, "f.txt", accessRW, shareAll,
		&wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelBatch}); resp == nil {
		t.Fatal("重试打开应当成功")
	}
	if e.b.Async() != nil {
		t.Fatal("重试不应再被推迟（说明上一次 break 没被收尾）")
	}
	if got := e.breakA.oplocks; got != 1 {
		t.Fatalf("重试不应再发 break，累计 %d 条（应为 1）", got)
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
func (t *oplockTable) has(o *Open) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.m {
		if e.owner == o {
			return true
		}
	}
	return false
}

func (t *oplockTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
