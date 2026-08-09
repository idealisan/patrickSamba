package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	registerCreateContext(createContextSpec{
		// DHnQ(v1 请求) 与 DH2Q(v2 请求) 必须由同一个 handler 处理：
		// 二者共享「是否授予」「响应回 DHnQ 还是 DH2Q」的判别逻辑。
		// DHnC/DH2C（重连）不在这里注册——createFile 在 newCreateContexts
		// 之前就走 handleDurableReconnect 短路了，重连请求里的 DHnC/DH2C
		// 对注册表而言是「不认识的 context」，按规矩静默忽略。
		Names: []string{wire.CreateContextDHnQ, wire.CreateContextDH2Q},
		Order: createContextOrderDurable,
		New:   func(req *wire.CreateRequest) createContextHandler { return &durableHandler{req: req} },
	})
}

// durableHandler 处理持久句柄的**授予**请求（DHnQ / DH2Q）。
//
// 重连（DHnC / DH2C）由 handleDurableReconnect 单独处理，这里不碰。
type durableHandler struct {
	createContextBase
	req *wire.CreateRequest

	// granted 记录本次 CREATE 是否真的授予了 durable handle。
	granted bool
	// respName / respData 是授予时要回的响应 context（DHnQ 或 DH2Q）。
	respName string
	respData []byte
}

// Parse 校验请求载荷长度（畸形直接让 CREATE 失败）。
func (h *durableHandler) Parse(_ *Context, name string, data []byte) error {
	switch name {
	case wire.CreateContextDHnQ:
		return wire.ParseDurableRequest(data)
	case wire.CreateContextDH2Q:
		if _, err := wire.ParseDurableRequestV2(data); err != nil {
			return err
		}
	}
	return nil
}

// Registered 在句柄登记后进执行授予判定（此时能拿到完整 *Open）。
//
// 顺序：先拒 persistent（本服务端无 CA 共享，永远不该授予）→ 再判前提
// （§3.3.5.9.6）→ 满足才真正授予并登记进 durableRegistry。前提不满足时
// **诚实不授予**（也不回响应 context），绝不伪造一个 dead handle。
func (h *durableHandler) Registered(ctx *Context, open *Open) error {
	intent, err := wire.FindDurableIntent(h.req.Contexts)
	if err != nil {
		return err
	}
	if !intent.RequestV1 && intent.RequestV2 == nil {
		return nil // 不是持久句柄请求
	}

	// persistent handle 需要 CONTINUOUS_AVAILABILITY 共享，本服务端没有，
	// 永远不能授予（诚实：不假装支持，直接拒）。§3.3.5.9.11。
	//
	// 已知争议，**暂不改**：MS-SMB2 §3.3.5.9.12 读起来是「share 不是 CA 就
	// 忽略 persistent 位、继续按普通 durable v2 处理」，也就是应当**降级**
	// 而不是让整个 CREATE 失败（客户端顺手带上这个位就连文件都打不开）。
	// 但这只是读规范推断出来的，我们没有对真实 Windows/macOS 客户端抓过包，
	// 而 AGENTS.md §9 要求「规范与真实客户端行为不一致时以真实客户端行为为
	// 准」。在拿到抓包证据之前保持现状，不把未经验证的规范解读写进生产代码。
	// 复现用例：durable_defect_test.go 的 TestQADefectPersistentFlagDegradesNotFails
	// （qadefect tag，故意留红）。
	if intent.RequestV2 != nil && intent.RequestV2.Flags.IsPersistent() {
		return status.NotSupported
	}

	if !durableGrantAllowed(h.req) {
		// 前提不满足：当普通句柄处理，不回任何 durable context。
		return nil
	}

	// 授予。
	ds := &DurableState{Granted: true, v2: intent.RequestV2 != nil}
	if ds.v2 {
		ds.guid = intent.RequestV2.CreateGUID
		ds.timeout = timeoutFromMs(intent.RequestV2.Timeout)
		h.respName = wire.CreateContextDH2Q
		h.respData = (&wire.DurableResponseV2{Timeout: msFromDuration(ds.timeout), Flags: 0}).Encode()
	} else {
		ds.timeout = defaultDurableTimeout
		h.respName = wire.CreateContextDHnQ
		h.respData = wire.EncodeDurableResponse()
	}
	ds.share = ctx.Tree.Share.Name
	ds.path = open.Path
	ds.identity = ctx.Session.Identity()
	open.Durable = ds
	if !durableRegistry.register(open) {
		// 登记表里这个键已被另一个存活句柄占着（v1 的键只有 Persistent
		// 一个维度，跨会话会撞）。此时无法保证重连时交回**正确**的句柄，
		// 于是诚实地不授予、降级为普通句柄，而不是登记上去等着串号。
		open.Durable = nil
		ctx.Log.Warn("durable 授予被拒：登记表键已被占用",
			"share", ds.share, "path", ds.path, "v2", ds.v2)
		return nil
	}
	h.granted = true
	return nil
}

// Respond 把授予响应 context 追加进 resp.Contexts。
func (h *durableHandler) Respond(_ *Context, _ *Open, resp *wire.CreateResponse, _ *vfs.Attr) error {
	if h.granted {
		resp.Contexts = append(resp.Contexts, wire.CreateContext{Name: h.respName, Data: h.respData})
	}
	return nil
}
