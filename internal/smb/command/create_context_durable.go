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
	durableRegistry.register(open)
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
