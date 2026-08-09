package command

import (
	"encoding/binary"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// create_context_durable_test.go —— durable handle 的可证伪单测。
//
// 覆盖授予前提（§3.3.5.9.6）、v1/v2 授予、重连配对、超时回收、跨用户/错误
// 键拒绝、lease break 作废。每条成功路径都配有反向对照（前提不满足 / 超时 /
// 错身份），不跳过任何反向用例（AGENTS.md §3 验收门槛）。

// resetDurable 把包级登记表与默认超时复位，避免用例互相污染。
func resetDurable() {
	durableRegistry = newDurableRegistry()
	defaultDurableTimeout = 60 * time.Second
}

func newDurableTestCtx(t *testing.T, user string) (*Context, *Session, *Tree) {
	t.Helper()
	conn := NewConn(&Settings{}, "test", "test")
	s := newSession(conn, 1)
	s.Establish(&auth.Identity{User: user})
	tree, st := s.NewTree(&Share{Name: "share", Type: wire.ShareTypeDisk})
	if st != status.Success {
		t.Fatalf("NewTree: %v", st)
	}
	return &Context{
		Conn:    conn,
		Session: s,
		Tree:    tree,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, s, tree
}

// fakeHandle 只实现 Stat（重连响应需要），其余方法一律回 ErrNotSupported。
type fakeHandle struct{ attr vfs.Attr }

func (f *fakeHandle) Close() error                                    { return nil }
func (f *fakeHandle) ReadAt(p []byte, off int64) (int, error)         { return 0, vfs.ErrNotSupported }
func (f *fakeHandle) WriteAt(p []byte, off int64) (int, error)        { return 0, vfs.ErrNotSupported }
func (f *fakeHandle) Truncate(size int64) error                       { return vfs.ErrNotSupported }
func (f *fakeHandle) Sync(full bool) error                            { return nil }
func (f *fakeHandle) Stat() (*vfs.Attr, error)                        { a := f.attr; return &a, nil }
func (f *fakeHandle) SetAttr(attr *vfs.Attr, mask vfs.AttrMask) error { return vfs.ErrNotSupported }
func (f *fakeHandle) ReadDir(pattern string, restart bool, max int) ([]vfs.DirEntry, error) {
	return nil, vfs.ErrNotSupported
}
func (f *fakeHandle) Xattr() (vfs.XattrAccessor, error) { return nil, vfs.ErrNotSupported }

func hasCreateContext(ctxs []wire.CreateContext, name string) bool {
	for _, c := range ctxs {
		if c.Name == name {
			return true
		}
	}
	return false
}

// grantDurable 跑一遍授予（Parse→Registered→Respond）并返回响应。
func grantDurable(t *testing.T, ctx *Context, open *Open, req *wire.CreateRequest) *wire.CreateResponse {
	t.Helper()
	h := &durableHandler{req: req}
	for _, c := range req.Contexts {
		if err := h.Parse(ctx, c.Name, c.Data); err != nil {
			t.Fatalf("Parse %s: %v", c.Name, err)
		}
	}
	if err := h.Registered(ctx, open); err != nil {
		t.Fatalf("Registered: %v", err)
	}
	resp := &wire.CreateResponse{}
	if err := h.Respond(ctx, open, resp, &vfs.Attr{}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	return resp
}

func dhqReq(oplock wire.OplockLevel) *wire.CreateRequest {
	return &wire.CreateRequest{
		RequestedOplockLevel: oplock,
		Contexts:             []wire.CreateContext{{Name: wire.CreateContextDHnQ, Data: wire.EncodeDurableRequest()}},
	}
}

// --- 授予前提（§3.3.5.9.6）---

// 成功：batch oplock 请求 → 授予并回 DHnQ。
func TestDurableGrantV1Batch(t *testing.T) {
	resetDurable()
	ctx, _, _ := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 1, Volatile: 1, Path: "f.txt"}

	resp := grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))

	if open.Durable == nil || !open.Durable.Granted {
		t.Fatal("batch oplock 应满足授予前提")
	}
	if !hasCreateContext(resp.Contexts, wire.CreateContextDHnQ) {
		t.Error("v1 授予应回 DHnQ 响应 context")
	}
}

// 反向对照：没有 batch oplock、也没有 lease → 诚实不授予，不回任何 durable context。
func TestDurableGrantV1NoPrecondition(t *testing.T) {
	resetDurable()
	ctx, _, _ := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 1, Volatile: 1, Path: "f.txt"}

	resp := grantDurable(t, ctx, open, dhqReq(wire.OplockLevelNone))

	if open.Durable != nil && open.Durable.Granted {
		t.Error("前提不满足时不应授予 durable")
	}
	if hasCreateContext(resp.Contexts, wire.CreateContextDHnQ) {
		t.Error("前提不满足时不应回 DHnQ")
	}
}

// v2 带 persistent flag、本服务端无 CA 共享 → **忽略该位**，降级授予普通
// durable v2，响应里的 persistent 位不置（§3.3.5.9.12，无失败出口）。
//
// 本用例原名 TestDurableGrantV2PersistentRejected，断言的是「整个 CREATE
// 失败」。那个行为是错的：客户端顺手带上 persistent 位就连文件都打不开。
// 改正依据见 create_context_durable.go 里的注释（含 Samba 的交叉验证）。
func TestDurableGrantV2PersistentDegrades(t *testing.T) {
	resetDurable()
	ctx, _, _ := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 2, Volatile: 2, Path: "f.txt"}

	req := &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelBatch,
		Contexts: []wire.CreateContext{{
			Name: wire.CreateContextDH2Q,
			Data: (&wire.DurableRequestV2{Timeout: 0, Flags: wire.DurableHandlePersistent}).Encode(),
		}},
	}
	h := &durableHandler{req: req}
	if err := h.Parse(ctx, wire.CreateContextDH2Q, req.Contexts[0].Data); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := h.Registered(ctx, open); err != nil {
		t.Fatalf("persistent 位应被忽略而非让 CREATE 失败，实得 %v", err)
	}
	if open.Durable == nil || !open.Durable.Granted {
		t.Fatal("应降级授予普通 durable v2")
	}
	if !open.Durable.v2 {
		t.Error("DH2Q 请求应授予 v2，不是 v1")
	}

	// 响应侧：必须回 DH2Q，且 Flags 里**不能**有 persistent 位 —— 否则等于
	// 谎称授予了 persistent handle，客户端会据此放弃自己的重试逻辑。
	resp := &wire.CreateResponse{}
	if err := h.Respond(ctx, open, resp, nil); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !hasCreateContext(resp.Contexts, wire.CreateContextDH2Q) {
		t.Fatal("降级授予后应回 DH2Q 响应 context")
	}
	for _, c := range resp.Contexts {
		if c.Name != wire.CreateContextDH2Q {
			continue
		}
		// DurableResponseV2 线格式：Timeout(4) + Flags(4)，小端。
		if len(c.Data) != 8 {
			t.Fatalf("DH2Q 响应应为 8 字节，实得 %d", len(c.Data))
		}
		flags := binary.LittleEndian.Uint32(c.Data[4:8])
		if flags&uint32(wire.DurableHandlePersistent) != 0 {
			t.Errorf("响应里置了 persistent 位(flags=%#x) —— 谎称支持 persistent handle", flags)
		}
	}
}

// --- 重连 ---

// 成功：授予 → 断连 → 重连，拿回同一个句柄，FileId 不变，且不可二次重连。
func TestDurableReconnectV1Success(t *testing.T) {
	resetDurable()
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 7, Volatile: 7, Path: "f.txt"}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open) // 模拟连接断开

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 7, Volatile: 7}}
	got, st := durableRegistry.reconnect(s, tree, intent)
	if st != status.Success {
		t.Fatalf("重连失败: %v", st)
	}
	if got != open {
		t.Error("重连应拿回同一个 open")
	}
	if got.Persistent != 7 || got.Volatile != 7 {
		t.Errorf("重连后 FileId 应保持一致，实得 %d/%d", got.Persistent, got.Volatile)
	}
	if got.Session != s {
		t.Error("重连后应挂回新会话")
	}
	// 二次重连：已认领，不应再成功。
	if _, st2 := durableRegistry.reconnect(s, tree, intent); st2 == status.Success {
		t.Error("重连成功后不应可再次重连")
	}
}

// 反向对照：句柄仍在正常使用（未断连）→ 重连无效。
func TestDurableReconnectWithoutDisconnect(t *testing.T) {
	resetDurable()
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 7, Volatile: 7, Path: "f.txt"}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	// 注意：没有调用 disconnect。

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 7, Volatile: 7}}
	if _, st := durableRegistry.reconnect(s, tree, intent); st == status.Success {
		t.Error("未断连的句柄不应被重连认领")
	}
}

// 反向对照：超时未重连 → 回收，重连失败。
func TestDurableReconnectTimeout(t *testing.T) {
	resetDurable()
	defaultDurableTimeout = 5 * time.Millisecond
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 7, Volatile: 7, Path: "f.txt"}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	time.Sleep(20 * time.Millisecond) // 远超 5ms 超时

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 7, Volatile: 7}}
	if _, st := durableRegistry.reconnect(s, tree, intent); st != status.ObjectNameNotFound {
		t.Errorf("超时重连应失败(OBJECT_NAME_NOT_FOUND)，实得 %v", st)
	}
}

// 反向对照：不同用户重连 → 拒绝。
func TestDurableReconnectCrossUser(t *testing.T) {
	resetDurable()
	ctxAlice, _, _ := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 9, Volatile: 9, Path: "f.txt"}

	grantDurable(t, ctxAlice, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	_, sBob, treeBob := newDurableTestCtx(t, "bob")
	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 9, Volatile: 9}}
	if _, st := durableRegistry.reconnect(sBob, treeBob, intent); st != status.AccessDenied {
		t.Errorf("跨用户重连应拒绝(ACCESS_DENIED)，实得 %v", st)
	}
}

// 反向对照：错误的 FileId → 找不到。
func TestDurableReconnectWrongKey(t *testing.T) {
	resetDurable()
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 9, Volatile: 9, Path: "f.txt"}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 999, Volatile: 999}}
	if _, st := durableRegistry.reconnect(s, tree, intent); st != status.ObjectNameNotFound {
		t.Errorf("错误 FileId 重连应失败(OBJECT_NAME_NOT_FOUND)，实得 %v", st)
	}
}

// 反向对照：lease break 让 HANDLE caching 丢失 → durable 作废，重连失败。
// 这条验证 tm-lease 的 break handler 调 open.InvalidateDurable() 的契约。
func TestDurableInvalidateOnLeaseBreak(t *testing.T) {
	resetDurable()
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 7, Volatile: 7, Path: "f.txt"}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	open.InvalidateDurable() // tm-lease 的 break handler 会调这个

	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 7, Volatile: 7}}
	if _, st := durableRegistry.reconnect(s, tree, intent); st != status.ObjectNameNotFound {
		t.Errorf("作废后重连应失败(OBJECT_NAME_NOT_FOUND)，实得 %v", st)
	}
	if open.Durable != nil && !open.Durable.Invalidated {
		t.Error("作废后应标记 Invalidated")
	}
}

// handleDurableReconnect 整条路径（含写响应）的端到端冒烟。
func TestHandleDurableReconnectEndToEnd(t *testing.T) {
	resetDurable()
	ctx, s, tree := newDurableTestCtx(t, "alice")
	open := &Open{
		Persistent: 7, Volatile: 7, Path: "f.txt",
		Handle: &fakeHandle{attr: vfs.Attr{Size: 10, FileAttributes: 0x20}},
	}

	grantDurable(t, ctx, open, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open)

	ctx.Out = nil
	intent := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 7, Volatile: 7}}
	if err := handleDurableReconnect(ctx, intent); err != nil {
		t.Fatalf("handleDurableReconnect: %v", err)
	}
	// 重连必须把句柄改绑到本次的树上。不改绑的话重连会「成功」，但之后
	// 每个命令的 resolveOpen 都会因 o.Tree != ctx.Tree 回 INVALID_PARAMETER。
	if open.Tree != tree {
		t.Error("重连后应改绑到本次 TREE_CONNECT 的树")
	}
	if open.Session != s {
		t.Error("重连后应挂回会话")
	}

	// 反向：跨用户走整条路径也应失败。
	_, sBob, treeBob := newDurableTestCtx(t, "bob")
	open2 := &Open{Persistent: 11, Volatile: 11, Path: "g.txt",
		Handle: &fakeHandle{attr: vfs.Attr{Size: 1}}}
	grantDurable(t, ctx, open2, dhqReq(wire.OplockLevelBatch))
	durableRegistry.disconnect(open2)
	ctxBob := &Context{Conn: sBob.Conn, Session: sBob, Tree: treeBob, Log: ctx.Log}
	ctxBob.Out = nil
	intent2 := &wire.DurableIntent{ReconnectV1: &wire.FileID{Persistent: 11, Volatile: 11}}
	if err := handleDurableReconnect(ctxBob, intent2); err != status.AccessDenied {
		t.Errorf("跨用户整条路径应 AccessDenied，实得 %v", err)
	}
}
