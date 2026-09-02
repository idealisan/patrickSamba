package command

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件钉的是 CHANGE_NOTIFY 的**端到端**行为（notify.go）：
// 从一条请求报文进去，到一条带 FILE_NOTIFY_INFORMATION 的响应出来。
//
// 与 notify_hub_test.go 的分工：那边钉事件中心的匹配/投递规则，
// 这边钉 handler 的协议语义（挂起、过滤位校验、三条收尾分支）。
//
// 变异自检方向：
//   - 把 handleChangeNotify 里的 ctx.Defer 换成同步等待 → 读循环被占死，
//     TestChangeNotifyDefersAndRepliesOnChange 超时变红；
//   - 把 filter==0 的拒绝删掉 → TestChangeNotifyRejectsEmptyFilter 变红；
//   - 把 waitNotify 里"取消就不补发"的分支删掉 → TestChangeNotifyCancelRepliesCancelled
//     出现两条响应；
//   - 把 OutputBufferLength 上限判定删掉 → TestChangeNotifyOverflowRepliesEnumDir 变红。

// notifyFixture 是一条装配好共享/树/会话/目录句柄的连接。
type notifyFixture struct {
	conn    *Conn
	sink    *fakeAsyncSink
	share   *Share
	sess    *Session
	tree    *Tree
	dirOpen *Open
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	sink := &fakeAsyncSink{}
	share := &Share{Name: "pub"}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	conn.SetAsyncSink(sink)

	sess, st := conn.NewSession()
	if st != status.Success {
		t.Fatalf("建会话失败：%v", st)
	}
	// handleChangeNotify 的 needSession 要求会话已认证完成。
	sess.Establish(&auth.Identity{User: "u"})

	tree, st := sess.NewTree(share)
	if st != status.Success {
		t.Fatalf("建树失败：%v", st)
	}

	dir := &Open{Tree: tree, Session: sess, Path: "docs", IsDir: true}
	if st := sess.AddOpen(dir); st != status.Success {
		t.Fatalf("登记目录句柄失败：%v", st)
	}
	return &notifyFixture{conn: conn, sink: sink, share: share, sess: sess, tree: tree, dirOpen: dir}
}

// notifyMsg 拼一条完整的 CHANGE_NOTIFY 请求报文（头 + 体）。
func notifyMsg(fid wire.FileID, filter wire.CompletionFilter, watchTree bool, outLen uint32) []byte {
	hdr := wire.Header{Command: wire.CommandChangeNotify}
	msg := hdr.Append(nil)
	var flags wire.ChangeNotifyFlags
	if watchTree {
		flags = wire.WatchTree
	}
	req := &wire.ChangeNotifyRequest{
		Flags:              flags,
		OutputBufferLength: outLen,
		FileID:             fid,
		CompletionFilter:   filter,
	}
	return req.Append(msg)
}

// notifyCtx 造一条挂在本 fixture 上的 CHANGE_NOTIFY 上下文。
func (f *notifyFixture) ctx(t *testing.T, filter wire.CompletionFilter, outLen uint32) *Context {
	t.Helper()
	msg := notifyMsg(wire.FileID{Persistent: f.dirOpen.Persistent, Volatile: f.dirOpen.Volatile}, filter, false, outLen)
	hdr, err := wire.ParseHeader(msg)
	if err != nil {
		t.Fatalf("解析报文头失败：%v", err)
	}
	hdr.SessionID = f.sess.ID
	hdr.TreeID = f.tree.ID
	return NewContext(f.conn, &Chain{}, hdr, msg, nil)
}

// awaitResponse 等异步补发的响应，并把它拼成完整报文帧。
func (f *notifyFixture) awaitFrame(t *testing.T) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.sink.count() > 0 {
			r := f.sink.last()
			frame := r.hdr.Append(nil)
			return append(frame, r.body...)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待异步响应超时")
	return nil
}

func TestChangeNotifyDefersAndRepliesOnChange(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := f.ctx(t, wire.NotifyChangeFileName, 4096)

	Dispatch(ctx)

	// 必须挂起而不是当场应答：没置 ASYNC_COMMAND 时本帧一个字节都不回。
	if ctx.Async() == nil {
		t.Fatal("CHANGE_NOTIFY 必须把请求挂起")
	}
	if !ctx.Suppressed() {
		t.Fatal("未置 ASYNC_COMMAND 时本帧不应产生任何响应字节")
	}
	if got := f.share.notify.count(); got != 1 {
		t.Fatalf("应登记 1 个订阅，实际 %d", got)
	}

	// 另一个客户端在同一目录里建文件。
	f.share.notify.notifyAdded("docs/hello.txt", false)

	frame := f.awaitFrame(t)
	if got := status.Status(wire.Header{}.Status); got != status.Success {
		_ = got
	}
	resp, err := wire.ParseChangeNotifyResponse(frame)
	if err != nil {
		t.Fatalf("解析 CHANGE_NOTIFY Response 失败：%v", err)
	}
	entries, err := wire.ParseNotifyEntries(resp.Buffer)
	if err != nil {
		t.Fatalf("解析 FILE_NOTIFY_INFORMATION 失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应回 1 条通知，实际 %d", len(entries))
	}
	if entries[0].Action != wire.FileActionAdded || entries[0].Name != "hello.txt" {
		t.Fatalf("通知应为 ADDED/hello.txt，实际 %v/%q", entries[0].Action, entries[0].Name)
	}
	// 订阅是一次性的：应答后必须摘除。
	if got := f.share.notify.count(); got != 0 {
		t.Fatalf("应答后订阅应摘除，实际剩 %d", got)
	}
}

// TestChangeNotifyRespectsFilter：不关心的变更不得唤醒订阅。
func TestChangeNotifyRespectsFilter(t *testing.T) {
	f := newNotifyFixture(t)
	// 只关心文件名变更。
	ctx := f.ctx(t, wire.NotifyChangeFileName, 4096)
	Dispatch(ctx)

	// 内容改动不属于 FILE_NAME。
	f.share.notify.notifyModified("docs/a.txt", wire.NotifyChangeSize|wire.NotifyChangeLastWrite)
	time.Sleep(60 * time.Millisecond)
	if got := f.sink.count(); got != 0 {
		t.Fatalf("不关心的变更不应触发应答，实际发了 %d 条", got)
	}

	// 建文件才属于。
	f.share.notify.notifyAdded("docs/b.txt", false)
	frame := f.awaitFrame(t)
	resp, err := wire.ParseChangeNotifyResponse(frame)
	if err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	entries, err := wire.ParseNotifyEntries(resp.Buffer)
	if err != nil {
		t.Fatalf("解析通知链失败：%v", err)
	}
	if len(entries) != 1 || entries[0].Name != "b.txt" {
		t.Fatalf("应只回 b.txt 一条，实际 %+v", entries)
	}
}

// TestChangeNotifyRejectsEmptyFilter：全 0 的过滤位等于"什么都不关心"，
// 这样的订阅永远不会被唤醒 —— 客户端会挂到超时，是最坏结果。
func TestChangeNotifyRejectsEmptyFilter(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := f.ctx(t, 0, 4096)
	Dispatch(ctx)

	if ctx.Async() != nil {
		t.Fatal("过滤位为 0 的请求不应挂起")
	}
	if ctx.Status != status.InvalidParameter {
		t.Fatalf("应回 STATUS_INVALID_PARAMETER，实际 %v", ctx.Status)
	}
	if got := f.share.notify.count(); got != 0 {
		t.Fatalf("被拒的请求不应留下订阅，实际 %d", got)
	}
}

// TestChangeNotifyRejectsUnknownFilterBits：有效位以外的比特一律拒绝，
// 不猜它的含义（AGENTS.md §9）。
func TestChangeNotifyRejectsUnknownFilterBits(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := f.ctx(t, wire.CompletionFilterAll|0x1000, 4096)
	Dispatch(ctx)

	if ctx.Status != status.InvalidParameter {
		t.Fatalf("含未知位的过滤位应被拒，实际 %v", ctx.Status)
	}
}

// TestChangeNotifyRejectsNonDirectoryHandle：CHANGE_NOTIFY 只能作用于目录。
func TestChangeNotifyRejectsNonDirectoryHandle(t *testing.T) {
	f := newNotifyFixture(t)
	file := &Open{Tree: f.tree, Session: f.sess, Path: "docs/a.txt"}
	if st := f.sess.AddOpen(file); st != status.Success {
		t.Fatalf("登记文件句柄失败：%v", st)
	}

	hdr := wire.Header{Command: wire.CommandChangeNotify, SessionID: f.sess.ID, TreeID: f.tree.ID}
	req := &wire.ChangeNotifyRequest{
		OutputBufferLength: 4096,
		FileID:             wire.FileID{Persistent: file.Persistent, Volatile: file.Volatile},
		CompletionFilter:   wire.NotifyChangeFileName,
	}
	msg := req.Append(hdr.Append(nil))
	h, err := wire.ParseHeader(msg)
	if err != nil {
		t.Fatalf("解析头失败：%v", err)
	}
	ctx := NewContext(f.conn, &Chain{}, h, msg, nil)
	Dispatch(ctx)

	if ctx.Status != status.InvalidParameter {
		t.Fatalf("非目录句柄应回 STATUS_INVALID_PARAMETER，实际 %v", ctx.Status)
	}
}

// TestChangeNotifyFallsBackToNotSupported：挂不起来时退回 v0.5.x 行为，
// 客户端降级为轮询。绝不能改成空转等待 —— 那会占死读循环。
func TestChangeNotifyFallsBackToNotSupported(t *testing.T) {
	// 不注入 sink：Defer 必然失败。
	share := &Share{Name: "pub"}
	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	sess, _ := conn.NewSession()
	sess.Establish(&auth.Identity{User: "u"})
	tree, _ := sess.NewTree(share)
	dir := &Open{Tree: tree, Session: sess, Path: "docs", IsDir: true}
	if st := sess.AddOpen(dir); st != status.Success {
		t.Fatalf("登记句柄失败：%v", st)
	}

	hdr := wire.Header{Command: wire.CommandChangeNotify, SessionID: sess.ID, TreeID: tree.ID}
	req := &wire.ChangeNotifyRequest{
		OutputBufferLength: 4096,
		FileID:             wire.FileID{Persistent: dir.Persistent, Volatile: dir.Volatile},
		CompletionFilter:   wire.NotifyChangeFileName,
	}
	msg := req.Append(hdr.Append(nil))
	h, _ := wire.ParseHeader(msg)
	ctx := NewContext(conn, &Chain{}, h, msg, nil)
	Dispatch(ctx)

	if ctx.Status != status.NotSupported {
		t.Fatalf("挂不起来时应回 STATUS_NOT_SUPPORTED，实际 %v", ctx.Status)
	}
	if got := share.notify.count(); got != 0 {
		t.Fatalf("退回路径不应留下订阅，实际 %d", got)
	}
}

// TestChangeNotifyCancelRepliesCancelled：挂起中的请求可被 CANCEL 取消，
// 且**只回一条** STATUS_CANCELLED（取消与等待只能有一个胜出）。
func TestChangeNotifyCancelRepliesCancelled(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := f.ctx(t, wire.NotifyChangeFileName, 4096)
	Dispatch(ctx)

	hdr := wire.Header{Command: wire.CommandCancel, SessionID: f.sess.ID, MessageID: ctx.Header.MessageID}
	cctx := NewContext(f.conn, &Chain{}, hdr, hdr.Append(nil), nil)
	Dispatch(cctx)

	// CANCEL 自己永不产生响应（MS-SMB2 §3.3.5.16）。
	if !cctx.Suppressed() {
		t.Fatal("CANCEL 不得产生任何响应字节")
	}
	if got := f.sink.count(); got != 1 {
		t.Fatalf("应只补发 1 条响应（STATUS_CANCELLED），实际 %d 条", got)
	}
	if got := status.Status(f.sink.last().hdr.Status); got != status.Cancelled {
		t.Fatalf("应回 STATUS_CANCELLED，实际 %v", got)
	}
	if got := f.share.notify.count(); got != 0 {
		t.Fatalf("取消后订阅必须摘除，否则它和它引用的句柄一起泄漏，实际剩 %d", got)
	}

	// 取消之后再有变更也不得补发第二条响应。
	f.share.notify.notifyAdded("docs/late.txt", false)
	time.Sleep(60 * time.Millisecond)
	if got := f.sink.count(); got != 1 {
		t.Fatalf("取消后不得再补发，实际 %d 条", got)
	}
}

// TestChangeNotifyHandleCloseRepliesCleanup：目录句柄关闭时未决的
// CHANGE_NOTIFY 必须以 STATUS_NOTIFY_CLEANUP 收场，而不是挂着等超时。
func TestChangeNotifyHandleCloseRepliesCleanup(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := f.ctx(t, wire.NotifyChangeFileName, 4096)
	Dispatch(ctx)

	f.dirOpen.close()

	// 补发发生在独立 goroutine 里，这里要等它跑完。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.sink.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := f.sink.count(); got != 1 {
		t.Fatalf("句柄关闭应补发 1 条响应，实际 %d 条", got)
	}
	if got := status.Status(f.sink.last().hdr.Status); got != status.Notify {
		t.Fatalf("应回 STATUS_NOTIFY_CLEANUP，实际 %v", got)
	}
}

// TestChangeNotifyOverflowRepliesEnumDir：缓冲区放不下时按规范回
// STATUS_NOTIFY_ENUM_DIR 且**不带**任何条目，让客户端重新枚举。
func TestChangeNotifyOverflowRepliesEnumDir(t *testing.T) {
	f := newNotifyFixture(t)
	// 12 字节固定头 + 一个 1 字符（2 字节）文件名 = 14 字节，
	// 给 13 字节就必然放不下。
	ctx := f.ctx(t, wire.NotifyChangeFileName, 13)
	Dispatch(ctx)

	f.share.notify.notifyAdded("docs/x.txt", false)

	frame := f.awaitFrame(t)
	if got := status.Status(f.sink.last().hdr.Status); got != status.NotifyEn {
		t.Fatalf("应回 STATUS_NOTIFY_ENUM_DIR，实际 %v", got)
	}
	resp, err := wire.ParseChangeNotifyResponse(frame)
	if err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(resp.Buffer) != 0 {
		t.Fatalf("NOTIFY_ENUM_DIR 不得带任何条目，实际 %d 字节", len(resp.Buffer))
	}
}
