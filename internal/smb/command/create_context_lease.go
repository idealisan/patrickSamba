package command

import (
	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

func init() {
	// 登记 RqLs：从此 create context 注册表**认识**租约请求，
	// 不再当作「未知 context 静默忽略」。
	registerCreateContext(createContextSpec{
		Names: []string{wire.CreateContextRqLs},
		Order: createContextOrderLease,
		New:   func(*wire.CreateRequest) createContextHandler { return &leaseContextHandler{} },
	})

	// 把 lease→durable 的前提判定接进 durable 模块（零耦合：durable.go 只
	// 持有一个函数变量，不知道 lease 的存在）。
	//
	// 这一步之前 SetLeaseDurableEligible 是全仓零调用，于是
	// durableGrantAllowed 恒为 false —— 走 lease 的 CREATE（macOS 的
	// RequestedOplockLevel = SMB2_OPLOCK_LEVEL_LEASE = 0xFF）永远拿不到
	// durable handle。Time Machine 强依赖 durable，这就是那条断链。
	SetLeaseDurableEligible(leaseSupportsDurable)
}

// leaseSupportsDurable 报告本次 CREATE 申请的租约是否满足授予 durable handle
// 的前提：租约带 SMB2_LEASE_HANDLE_CACHING 位（MS-SMB2 §3.3.5.9.6）。
//
// 拿不到租约（没带 RqLs）或载荷畸形时返回 false —— 这两种情况都不是错误，
// 只是「不满足前提」，CREATE 照走普通路径。真正的畸形请求由
// leaseContextHandler.Parse 与 planOplock 各自拦。
func leaseSupportsDurable(req *wire.CreateRequest) bool {
	lc, err := wire.FindLeaseContext(req.Contexts)
	if err != nil || lc == nil {
		return false
	}
	return lc.LeaseState&wire.LeaseHandleCaching != 0
}

// leaseContextHandler 处理 "RqLs"（SMB2_CREATE_REQUEST_LEASE / _LEASE_V2）。
//
// 请求侧的载荷在 Parse 阶段就校验掉，好处是「畸形 RqLs」在**打开文件之前**
// 就被拒（无效的缓存许可不该先落一个文件出来）；授予判定与响应编码分别在
// oplock_grant.go 与 create.go，本 handler 不碰。
type leaseContextHandler struct {
	createContextBase
}

// Parse 校验 RqLs 载荷长度（32 = v1、52 = v2，见 wire/lease_context.go）。
//
// 状态位是否合法由 planOplock 判（它才知道哪些位我们认），这里只管长度：
// 长度不对连 LeaseState 都取不出来，属于明确的畸形请求。
func (h *leaseContextHandler) Parse(_ *Context, _ string, data []byte) error {
	if _, err := wire.ParseLeaseContext(data); err != nil {
		return status.InvalidParameter
	}
	return nil
}
