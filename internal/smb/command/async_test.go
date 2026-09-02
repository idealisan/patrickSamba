package command

import (
	"sync"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件钉的是异步未决请求表（async.go）—— CHANGE_NOTIFY 与阻塞锁异步
// 等待共用的前置设施。
//
// 每个用例都写成**可证伪**的形式：断言的是"做了什么"，而不是"没崩"。
// 变异自检方向：把 deferReq 的 dup 检查删掉 → TestDeferRejectsDuplicateMessageID 变红；
// 把 Complete 的 claim 去掉 → TestCompleteIsExactlyOnce 变红；
// 把 Conn.Close 里的 abortPending 去掉 → TestConnCloseAbortsPending 变红。

// recordedAsync 是 AsyncSink 的一次调用记录。
type recordedAsync struct {
	hdr     wire.Header
	body    []byte
	signKey []byte
	enc     bool
}

// fakeAsyncSink 记录补发的异步响应而不真的写 socket。
type fakeAsyncSink struct {
	mu   sync.Mutex
	sent []recordedAsync
}

func (s *fakeAsyncSink) SendAsyncResponse(hdr wire.Header, body []byte, signKey []byte, encrypted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, recordedAsync{hdr: hdr, body: body, signKey: signKey, enc: encrypted})
	return nil
}

func (s *fakeAsyncSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *fakeAsyncSink) last() recordedAsync {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return recordedAsync{}
	}
	return s.sent[len(s.sent)-1]
}

// newAsyncTestConn 造一条注入了假 sink 的连接。
func newAsyncTestConn(sink AsyncSink) *Conn {
	c := NewConn(&Settings{}, "test", "test")
	c.SetAsyncSink(sink)
	return c
}

// deferCtx 造一条可用于挂起的请求上下文。mods 在 Defer 之前改 Header。
func deferCtx(c *Conn, sessionID, messageID uint64, mods func(h *wire.Header)) *Context {
	hdr := wire.Header{
		Command:   wire.CommandLock,
		SessionID: sessionID,
		MessageID: messageID,
	}
	if mods != nil {
		mods(&hdr)
	}
	return NewContext(c, &Chain{}, hdr, nil, nil)
}

func TestDeferRegistersPendingRequest(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ctx := deferCtx(c, 7, 42, nil)

	ar, ok := ctx.Defer()
	if !ok {
		t.Fatal("注入了 sink 的末条请求应当能挂起")
	}
	if ctx.Async() != ar {
		t.Fatal("Defer 必须把句柄挂到 ctx.async 上，Dispatch 靠它识别挂起")
	}
	if got := c.async.pendingCount(); got != 1 {
		t.Fatalf("挂起后未决数应为 1，实际 %d", got)
	}
	// 同步请求不回 interim：hdr 不该带 ASYNC_COMMAND。
	if ar.interim {
		t.Fatal("未置 SMB2_FLAGS_ASYNC_COMMAND 的请求不应回 interim")
	}
}

func TestDeferRejectsWithoutSink(t *testing.T) {
	// 未注入 sink：单元测试构造裸 Conn 的典型情形，必须退回同步语义
	// 而不是挂起后永远没人补发。
	c := NewConn(&Settings{}, "test", "test")
	ctx := deferCtx(c, 1, 1, nil)
	if _, ok := ctx.Defer(); ok {
		t.Fatal("没有 AsyncSink 时 Defer 必须失败")
	}
}

// TestDeferAllowsMidChain：v0.7.2 起复合链**中间**的消息也能挂起。
//
// internal/server 会检测到"本条挂起且后面还有消息"，把已处理部分的响应
// 先发出，存下剩余字节与链状态，等异步完成后续跑（chainPause / resumeChain）。
// 此前 Defer 对非末条直接返回失败，导致链中间的 CREATE 只能退回同步语义。
func TestDeferAllowsMidChain(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	// NextCommand != 0 = 复合链中间的消息。
	ctx := deferCtx(c, 1, 1, func(h *wire.Header) { h.NextCommand = 128 })
	ar, ok := ctx.Defer()
	if !ok {
		t.Fatal("复合链中间的消息应当允许挂起（由 server 层续跑剩下的部分）")
	}
	if got := c.async.pendingCount(); got != 1 {
		t.Fatalf("挂起后应进表，实际 %d", got)
	}
	// 挂起后本帧不为它产生响应 —— 响应走异步补发，剩余部分走续跑。
	if ctx.Suppressed() {
		t.Fatal("未置 ASYNC_COMMAND 时仍应有 interim 处理路径")
	}
	_ = ar
}

func TestDeferRejectsDuplicateMessageID(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	if _, ok := deferCtx(c, 5, 9, nil).Defer(); !ok {
		t.Fatal("首次挂起应成功")
	}
	// 同一个 (SessionId, MessageId) 再来一次：客户端复用 MessageId 是协议错误，
	// 重登记会让前一条永远收不到响应。
	if _, ok := deferCtx(c, 5, 9, nil).Defer(); ok {
		t.Fatal("重复的 (SessionId, MessageId) 不得重复登记")
	}
}

func TestCompleteIsExactlyOnce(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ctx := deferCtx(c, 3, 11, nil)
	ar, ok := ctx.Defer()
	if !ok {
		t.Fatal("挂起失败")
	}

	build := func(dst []byte) ([]byte, error) { return (&wire.LockResponse{}).Append(dst), nil }
	if !ar.Complete(status.Success, build) {
		t.Fatal("首次 Complete 应当胜出")
	}
	if ar.Complete(status.Success, build) {
		t.Fatal("第二次 Complete 必须无效（一条请求只能有一条最终响应）")
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("补发次数应为 1，实际 %d", got)
	}
	if got := c.async.pendingCount(); got != 0 {
		t.Fatalf("完成后必须从未决表摘除，实际剩 %d", got)
	}
	// 最终响应必须沿用请求的 MessageId / SessionId，否则客户端无法对账。
	if got := sink.last().hdr.MessageID; got != 11 {
		t.Fatalf("最终响应 MessageId 应为 11，实际 %d", got)
	}
	if got := sink.last().hdr.SessionID; got != 3 {
		t.Fatalf("最终响应 SessionId 应为 3，实际 %d", got)
	}
	// protocol-notes §12：任何响应至少授予 1 个 credit。
	if got := sink.last().hdr.Credits; got < 1 {
		t.Fatalf("最终响应 Credits 必须 >= 1，实际 %d", got)
	}
}

func TestCompleteErrorSendsErrorResponse(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ar, ok := deferCtx(c, 1, 2, nil).Defer()
	if !ok {
		t.Fatal("挂起失败")
	}
	// build 为 nil：失败状态本就不该带业务响应体。
	ar.Complete(status.LockNotGranted, nil)

	f := sink.last()
	if got := status.Status(f.hdr.Status); got != status.LockNotGranted {
		t.Fatalf("状态应为 LOCK_NOT_GRANTED，实际 %v", got)
	}
	// 失败响应必须是标准 SMB2 ERROR Response（9 字节结构 + 1 字节占位）。
	if len(f.body) != 9 {
		t.Fatalf("ERROR Response 体长应为 9，实际 %d", len(f.body))
	}
}

func TestCancelPendingRepliesCancelled(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ar, ok := deferCtx(c, 8, 21, nil).Defer()
	if !ok {
		t.Fatal("挂起失败")
	}
	aborted := false
	ar.OnAbort(func() { aborted = true })

	if !c.cancelPending(cancelKey(wire.Header{SessionID: 8, MessageID: 21})) {
		t.Fatal("按 (SessionId, MessageId) 应能命中未决请求")
	}
	if !aborted {
		t.Fatal("取消必须触发等待方注册的 OnAbort（等待方据此释放资源）")
	}
	select {
	case <-ar.Aborted():
	default:
		t.Fatal("取消后 abort 通道必须已关闭")
	}
	if got := status.Status(sink.last().hdr.Status); got != status.Cancelled {
		t.Fatalf("取消应答应为 STATUS_CANCELLED，实际 %v", got)
	}
	if got := c.async.pendingCount(); got != 0 {
		t.Fatalf("取消后必须从未决表摘除，实际剩 %d", got)
	}
}

func TestCancelMissIsSilent(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	// 表里根本没有请求：CANCEL 必须静默丢弃，不能补发任何东西。
	if c.cancelPending(cancelKey(wire.Header{SessionID: 1, MessageID: 1})) {
		t.Fatal("未命中不应返回 true")
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("未命中时不得补发响应，实际发了 %d 条", got)
	}
}

func TestCancelMatchesAsyncByAsyncID(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	// 客户端置了 SMB2_FLAGS_ASYNC_COMMAND：按 AsyncId 匹配，且要回 interim。
	hdr := wire.Header{SessionID: 4, MessageID: 77, Flags: wire.FlagAsyncCommand}
	ar, ok := NewContext(c, &Chain{}, hdr, nil, nil).Defer()
	if !ok {
		t.Fatal("挂起失败")
	}
	if !ar.interim {
		t.Fatal("客户端置了 ASYNC_COMMAND 就必须回 interim STATUS_PENDING")
	}
	asyncID := ar.key.asyncID
	if asyncID == 0 {
		t.Fatal("服务端分配的 AsyncId 不得为 0（0 是保留值）")
	}

	if !c.cancelPending(cancelKey(wire.Header{Flags: wire.FlagAsyncCommand, AsyncID: asyncID})) {
		t.Fatal("按 AsyncId 应能命中未决请求")
	}
	// 最终响应必须带回同一个 AsyncId 与 ASYNC_COMMAND 标志。
	f := sink.last()
	if !f.hdr.IsAsync() {
		t.Fatal("异步最终响应必须置 SMB2_FLAGS_ASYNC_COMMAND")
	}
	if f.hdr.AsyncID != asyncID {
		t.Fatalf("最终响应 AsyncId 应为 %d，实际 %d", asyncID, f.hdr.AsyncID)
	}
}

func TestCancelIgnoresMessageIDForAsyncRequests(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	hdr := wire.Header{SessionID: 4, MessageID: 77, Flags: wire.FlagAsyncCommand}
	ar, _ := NewContext(c, &Chain{}, hdr, nil, nil).Defer()

	// 用同步键（SessionId, MessageId）去取消一条异步请求：必须**不命中**。
	// 两套键空间混用会张冠李戴 —— AsyncId 与 MessageId 数值相等是可能的。
	if c.cancelPending(cancelKey(wire.Header{SessionID: 4, MessageID: 77})) {
		t.Fatal("异步请求不得被同步键命中")
	}
	if sink.count() != 0 {
		t.Fatal("误命中会补发错误的 STATUS_CANCELLED")
	}
	_ = ar
}

func TestConnCloseAbortsPending(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ar, ok := deferCtx(c, 1, 1, nil).Defer()
	if !ok {
		t.Fatal("挂起失败")
	}
	aborted := false
	ar.OnAbort(func() { aborted = true })

	c.Close()

	if !aborted {
		t.Fatal("连接关闭必须中止全部挂起的等待者（否则 goroutine 与它持有的资源一起泄漏）")
	}
	select {
	case <-ar.Aborted():
	case <-time.After(time.Second):
		t.Fatal("连接关闭后 abort 通道应当已关闭")
	}
	// 关闭时不补发响应 —— 连接都没了，补发必然失败。
	if got := sink.count(); got != 0 {
		t.Fatalf("连接关闭不得补发响应，实际发了 %d 条", got)
	}
}

func TestAsyncCarriesSigningAndEncryptionContext(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	ctx := deferCtx(c, 6, 13, nil)
	ctx.SignKey = []byte("0123456789abcdef")
	ctx.Encrypted = true
	ar, ok := ctx.Defer()
	if !ok {
		t.Fatal("挂起失败")
	}
	ar.Complete(status.Success, nil)

	f := sink.last()
	if string(f.signKey) != "0123456789abcdef" {
		t.Fatalf("补发必须带回原请求的签名密钥，实际 %q", f.signKey)
	}
	if !f.enc {
		t.Fatal("原请求来自加密信封时，补发的响应也必须加密")
	}
}

// TestDeferRespectsPendingLimit：未决数达到上限后必须拒绝挂起，
// 否则一个只发不收的客户端能用 CHANGE_NOTIFY 把服务端挂满。
func TestDeferRespectsPendingLimit(t *testing.T) {
	sink := &fakeAsyncSink{}
	c := newAsyncTestConn(sink)
	var last *AsyncRequest
	for i := 0; i < maxPendingAsync; i++ {
		ar, ok := deferCtx(c, 1, uint64(i+1), nil).Defer()
		if !ok {
			t.Fatalf("第 %d 条挂起应当成功（上限 %d）", i+1, maxPendingAsync)
		}
		last = ar
	}
	if _, ok := deferCtx(c, 1, uint64(maxPendingAsync+1), nil).Defer(); ok {
		t.Fatal("达到上限后必须拒绝挂起")
	}
	// 腾出一个名额后又能挂 —— 上限是计数不是永久熔断。
	last.Complete(status.Cancelled, nil)
	if _, ok := deferCtx(c, 1, uint64(maxPendingAsync+1), nil).Defer(); !ok {
		t.Fatal("腾出名额后应当能继续挂起")
	}
}
