package command

import (
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
// 顺序：判前提（§3.3.5.9.6）→ 满足才真正授予并登记进 durableRegistry。
// 前提不满足时**诚实不授予**（也不回响应 context），绝不伪造一个 dead
// handle。请求里的 persistent 位一律降级处理，不会让 CREATE 失败（见下）。
func (h *durableHandler) Registered(ctx *Context, open *Open) error {
	intent, err := wire.FindDurableIntent(h.req.Contexts)
	if err != nil {
		return err
	}
	if !intent.RequestV1 && intent.RequestV2 == nil {
		return nil // 不是持久句柄请求
	}

	// DH2Q 的 SMB2_DHANDLE_FLAG_PERSISTENT（§2.2.13.2.11）在这里**被忽略**，
	// 不是被拒绝 —— 我们授予普通 durable v2，响应 Flags 不置 persistent 位。
	//
	// 该位只是一个「条件升级」请求：按 §3.3.5.9.12，只有在服务端宣告了
	// SMB2_GLOBAL_CAP_PERSISTENT_HANDLES **且** TreeConnect.Share.IsCA 时才
	// 升级为 persistent handle。两个条件我们都不满足（不宣告该能力、没有 CA
	// 共享），所以走普通 durable v2 授予流程。规范里这条分支**没有失败出口**。
	//
	// 原实现在这里 return STATUS_NOT_SUPPORTED，会把「顺手带上 persistent 位」
	// 的客户端整个 CREATE 打掉 —— 文件根本打不开，而不是退化成不太好用。
	//
	// 交叉验证（AGENTS.md §9：拿真实实现的行为兜底，不靠对规范的单方面解读）：
	// Samba source3/smbd/smb2_create.c 是独立实现，行为一致 ——
	//   L1481  durable_requested = true            无条件先设好
	//   L1492  if (flag & PERSISTENT) && (server_caps & CAP_PERSISTENT_HANDLES)
	//          && (tcon_caps & CONTINUOUS_AVAILABILITY) && oplock ∈ {NONE,LEASE}
	//              → persistent_requested = true   纯粹的条件升级，**没有 else**
	//   L1988  响应侧只有真授予了 persistent 才把该位写进 DH2Q 响应
	// 顺带一条 Samba 的额外约束（我们不实现，只记录）：即便支持 persistent，
	// 请求了 oplock 时它也拒绝升级，理由是 persistent handle 配 oplock 在
	// Windows 上是坏的。
	//
	// 回归保护：durable_qa_test.go TestQADurablePersistentFlagDegrades。

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
