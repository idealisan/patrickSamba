package command

import (
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// leaseReq 造一个带 RqLs 的 CREATE 请求，state 为请求的租约状态位。
func leaseReq(t *testing.T, state wire.LeaseState, v2 bool) *wire.CreateRequest {
	t.Helper()
	lc := wire.LeaseContext{V2: v2, LeaseState: state}
	lc.LeaseKey[0] = 0xAB // 非全零，避免与「没给」混淆
	return &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelLease,
		Contexts: []wire.CreateContext{
			{Name: wire.CreateContextRqLs, Data: lc.Encode()},
		},
	}
}

// TestLeaseSupportsDurable
//
// 判据：durable 的 lease 前提只看 SMB2_LEASE_HANDLE_CACHING 位。
// 反向对照：把 leaseSupportsDurable 里的 HANDLE 判据删掉（改成恒 true），
// 「只读缓存租约」与「无 RqLs」两例会跟着变 true，本用例立刻红。
func TestLeaseSupportsDurable(t *testing.T) {
	tests := []struct {
		name  string
		req   *wire.CreateRequest
		want  bool
		build func(*testing.T) *wire.CreateRequest
	}{
		{name: "无 RqLs", want: false, build: func(*testing.T) *wire.CreateRequest {
			return &wire.CreateRequest{RequestedOplockLevel: wire.OplockLevelLease}
		}},
		{name: "读写+句柄缓存", want: true, build: func(t *testing.T) *wire.CreateRequest {
			return leaseReq(t, wire.LeaseReadCaching|wire.LeaseWriteCaching|wire.LeaseHandleCaching, true)
		}},
		{name: "只要句柄缓存", want: true, build: func(t *testing.T) *wire.CreateRequest {
			return leaseReq(t, wire.LeaseHandleCaching, false)
		}},
		{name: "只读缓存租约（无句柄位）", want: false, build: func(t *testing.T) *wire.CreateRequest {
			return leaseReq(t, wire.LeaseReadCaching, true)
		}},
		{name: "RqLs 载荷畸形", want: false, build: func(*testing.T) *wire.CreateRequest {
			return &wire.CreateRequest{
				RequestedOplockLevel: wire.OplockLevelLease,
				Contexts: []wire.CreateContext{
					{Name: wire.CreateContextRqLs, Data: []byte{1, 2, 3}},
				},
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaseSupportsDurable(tc.build(t)); got != tc.want {
				t.Errorf("leaseSupportsDurable = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestDurableGrantAllowedViaLease
//
// 这是 #89 的核心：走 lease 的 CREATE（macOS 的
// RequestedOplockLevel = SMB2_OPLOCK_LEVEL_LEASE = 0xFF）必须能拿到 durable。
// 修复前 SetLeaseDurableEligible 全仓零调用，lease 分支恒 false，
// 也就是 TIME MACHINE 在真实链路上永远拿不到 durable handle。
//
// 反向对照：注掉 create_context_lease.go init 里的 SetLeaseDurableEligible
// 调用，「lease 带句柄缓存」一例立刻从 true 变 false。
func TestDurableGrantAllowedViaLease(t *testing.T) {
	if got := durableGrantAllowed(leaseReq(t,
		wire.LeaseReadCaching|wire.LeaseHandleCaching, true)); !got {
		t.Error("lease 带 HANDLE_CACHING 时应允许授予 durable，实得 false —— lease 没接进 durable")
	}
	if got := durableGrantAllowed(leaseReq(t, wire.LeaseReadCaching, true)); got {
		t.Error("lease 不带 HANDLE_CACHING 时不应允许授予 durable")
	}
	// batch oplock 路径与 lease 无关，仍要成立。
	if got := durableGrantAllowed(&wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelBatch}); !got {
		t.Error("batch oplock 路径应仍然允许授予 durable")
	}
}

// TestDurableReachableOnAppleLeaseCreate
//
// #58 的可达性断言：真 macOS 那条链路上能不能拿到 durable handle。
//
// 真机验证在本环境**做不到**（容器里没有 macOS），所以这里钉的是「协议层面
// 那条路径是通的」：按 Apple 验证第 2 步的 CREATE 形态发请求 ——
// RequestedOplockLevel = SMB2_OPLOCK_LEVEL_LEASE(0xFF) + DH2Q + 带
// HANDLE_CACHING 的 RqLs —— 断言真的授予了 durable 并回 DH2Q。
//
// 反向对照：注掉 create_context_lease.go 里那行 SetLeaseDurableEligible，
// 「Apple 形态」这一支立刻拿不到授予（而 batch oplock 那一支照旧通过），
// 正好指出断的是 lease 那一环而不是别处。
//
// 覆盖不到的部分（真机才有的时序、断线重连是否真能续上备份）如实写在这里，
// 不在注释里假装验过。
func TestDurableReachableOnAppleLeaseCreate(t *testing.T) {
	resetDurable()

	dh2q := (&wire.DurableRequestV2{Timeout: 0}).Encode()
	lease := wire.LeaseContext{
		LeaseState: wire.LeaseReadCaching | wire.LeaseWriteCaching | wire.LeaseHandleCaching,
	}
	lease.LeaseKey[0] = 0x5A

	ctx, _, _ := newDurableTestCtx(t, "alice")
	open := &Open{Persistent: 7, Volatile: 7, Path: "f.txt"}

	// Apple 形态：lease(0xFF) + DH2Q + RqLs(HANDLE)
	req := &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelLease,
		Contexts: []wire.CreateContext{
			{Name: wire.CreateContextDH2Q, Data: dh2q},
			{Name: wire.CreateContextRqLs, Data: lease.Encode()},
		},
	}
	resp := grantDurable(t, ctx, open, req)
	if open.Durable == nil || !open.Durable.Granted {
		t.Fatal("走 lease 的 CREATE 没拿到 durable —— Time Machine 这条链路断着")
	}
	if !hasCreateContext(resp.Contexts, wire.CreateContextDH2Q) {
		t.Error("授予了 durable 却没回 DH2Q 响应 context")
	}

	// 同样的请求去掉 RqLs：lease 位还在（0xFF），但没有租约可判 → 不应授予。
	// 这条是为了防止「只要写了 0xFF 就给 durable」这种假绿。
	resetDurable()
	reqNoLease := &wire.CreateRequest{
		RequestedOplockLevel: wire.OplockLevelLease,
		Contexts:             []wire.CreateContext{{Name: wire.CreateContextDH2Q, Data: dh2q}},
	}
	open2 := &Open{Persistent: 8, Volatile: 8, Path: "f.txt"}
	h := &durableHandler{req: reqNoLease}
	if err := h.Parse(ctx, wire.CreateContextDH2Q, dh2q); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := h.Registered(ctx, open2); err != nil {
		t.Fatalf("Registered: %v", err)
	}
	if open2.Durable != nil && open2.Durable.Granted {
		t.Error("没有 RqLs 却授予了 durable —— 判据放水了")
	}
}

// TestLeaseContextRegistered
//
// 注册表必须认识 RqLs：不认识的话它会被当成「未知 context 静默忽略」，
// 请求侧的畸形载荷就再也无人校验。
//
// 反向对照：删掉 create_context_lease.go 的 registerCreateContext，
// 本用例在「RqLs 已注册」这一条上变红。
func TestLeaseContextRegistered(t *testing.T) {
	if _, ok := createContextByName[wire.CreateContextRqLs]; !ok {
		t.Fatal("RqLs 没有注册进 create context 注册表")
	}

	// 畸形载荷必须在 Parse 阶段（打开文件之前）就被拒。
	cc, err := newCreateContexts(&Context{}, &wire.CreateRequest{
		Contexts: []wire.CreateContext{
			{Name: wire.CreateContextRqLs, Data: []byte{1, 2, 3}},
		},
	})
	if err == nil {
		t.Fatal("畸形 RqLs 载荷应让 CREATE 失败（INVALID_PARAMETER），实得通过")
	}
	if cc != nil {
		t.Error("Parse 失败时不应返回可用的 handler 集合")
	}

	// 合法载荷要能通过，且不影响后续阶段。
	if _, err := newCreateContexts(&Context{}, leaseReq(t, wire.LeaseHandleCaching, true)); err != nil {
		t.Errorf("合法 RqLs 载荷被拒: %v", err)
	}
}
