package wire

import (
	"bytes"
	"errors"
	"testing"
)

// 下面的字节向量按 MS-SMB2 §2.2.13.2.8 / §2.2.13.2.10 的**字段表**逐字段摆出，
// 不是抓包。仓库现有的 testdata/capture/ 里没有任何带 "RqLs" 的样本
// （macOS/Windows 只在服务端宣告 SMB2_GLOBAL_CAP_LEASING 时才会发它，
// 而本服务当前不宣告），所以拿不到真实报文。
//
// 为了让「按字段表拼出来的」这件事本身可被证伪，每个向量都写成
// 「偏移 + 字段名 + 值」的形式，并额外用 assertZero 锁死规范要求恒为 0 的
// 保留区 —— 如果哪天真抓到包，逐字节 diff 一眼就能看出差异在哪个字段。

// leaseKeyA 是一个各字节互不相同的 LeaseKey，字节序写反会立刻暴露。
var leaseKeyA = [16]byte{
	0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
	0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00,
}

// leaseKeyB 是 v2 的 ParentLeaseKey，与 leaseKeyA 不同，
// 防止把两个 key 抄串了还能通过测试。
var leaseKeyB = [16]byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
}

// TestLeaseContextV1Golden 锁死 SMB2_CREATE_REQUEST_LEASE（MS-SMB2 §2.2.13.2.8）
// 与 SMB2_CREATE_RESPONSE_LEASE（§2.2.14.2.10）的 32 字节布局。
func TestLeaseContextV1Golden(t *testing.T) {
	want := []byte{
		// 0 LeaseKey(16)
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00,
		// 16 LeaseState(4) 小端 = R|H = 0x03
		0x03, 0x00, 0x00, 0x00,
		// 20 LeaseFlags(4) 小端 = SMB2_LEASE_FLAG_BREAK_IN_PROGRESS
		0x02, 0x00, 0x00, 0x00,
		// 24 LeaseDuration(8) 规范要求恒 0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if len(want) != LeaseContextV1Size {
		t.Fatalf("向量长度 %d != LeaseContextV1Size %d", len(want), LeaseContextV1Size)
	}

	c := &LeaseContext{
		LeaseKey:   leaseKeyA,
		LeaseState: LeaseReadCaching | LeaseHandleCaching,
		Flags:      LeaseFlagBreakInProgress,
	}
	got := c.Encode()
	if !bytes.Equal(got, want) {
		t.Errorf("v1 编码不符\n got=% X\nwant=% X", got, want)
	}

	back, err := ParseLeaseContext(want)
	if err != nil {
		t.Fatalf("ParseLeaseContext(v1): %v", err)
	}
	if *back != *c {
		t.Errorf("v1 round-trip 不一致\n got=%+v\nwant=%+v", back, c)
	}
	if back.V2 {
		t.Error("32 字节必须判为 v1")
	}
	// v1 没有 ParentLeaseKey 字段，即便调用方乱设 flag 也必须报告无效。
	if back.HasParentLeaseKey() {
		t.Error("v1 不可能有 ParentLeaseKey")
	}
}

// TestLeaseContextV2Golden 锁死 SMB2_CREATE_REQUEST_LEASE_V2（§2.2.13.2.10）
// 与 SMB2_CREATE_RESPONSE_LEASE_V2（§2.2.14.2.11）的 52 字节布局。
func TestLeaseContextV2Golden(t *testing.T) {
	want := []byte{
		// 0 LeaseKey(16)
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00,
		// 16 LeaseState(4) = R|H|W = 0x07
		0x07, 0x00, 0x00, 0x00,
		// 20 Flags(4) = SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET
		0x04, 0x00, 0x00, 0x00,
		// 24 LeaseDuration(8) 恒 0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// 32 ParentLeaseKey(16)
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
		// 48 Epoch(2) 小端 = 0x0102 —— 两字节不同，字节序写反必炸
		0x02, 0x01,
		// 50 Reserved(2) 恒 0
		0x00, 0x00,
	}
	if len(want) != LeaseContextV2Size {
		t.Fatalf("向量长度 %d != LeaseContextV2Size %d", len(want), LeaseContextV2Size)
	}

	c := &LeaseContext{
		LeaseKey:       leaseKeyA,
		LeaseState:     LeaseReadCaching | LeaseHandleCaching | LeaseWriteCaching,
		Flags:          LeaseFlagParentLeaseKeySet,
		V2:             true,
		ParentLeaseKey: leaseKeyB,
		Epoch:          0x0102,
	}
	got := c.Encode()
	if !bytes.Equal(got, want) {
		t.Errorf("v2 编码不符\n got=% X\nwant=% X", got, want)
	}

	back, err := ParseLeaseContext(want)
	if err != nil {
		t.Fatalf("ParseLeaseContext(v2): %v", err)
	}
	if *back != *c {
		t.Errorf("v2 round-trip 不一致\n got=%+v\nwant=%+v", back, c)
	}
	if !back.V2 {
		t.Error("52 字节必须判为 v2")
	}
	if !back.HasParentLeaseKey() {
		t.Error("v2 且置了 PARENT_LEASE_KEY_SET 时 ParentLeaseKey 必须有效")
	}
}

// TestLeaseContextParentKeyGating 覆盖 HasParentLeaseKey 的两个条件
// （必须同时是 v2 且置了 SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET）。
//
// 这个 gating 不是形式主义：v2 报文里 ParentLeaseKey 字段**永远存在**，
// 但只有置了 flag 时它才有意义。不看 flag 就用，会把客户端留在那 16 字节里的
// 垃圾（或全 0）当成一个真实的父租约 key，进而把无关的文件挂到同一棵
// 目录租约树上。
func TestLeaseContextParentKeyGating(t *testing.T) {
	cases := []struct {
		name string
		c    LeaseContext
		want bool
	}{
		{"v1 无 flag", LeaseContext{}, false},
		{"v1 有 flag（字段根本不存在）", LeaseContext{Flags: LeaseFlagParentLeaseKeySet}, false},
		{"v2 无 flag", LeaseContext{V2: true}, false},
		{"v2 有 flag", LeaseContext{V2: true, Flags: LeaseFlagParentLeaseKeySet}, true},
		{"v2 只有 BREAK_IN_PROGRESS", LeaseContext{V2: true, Flags: LeaseFlagBreakInProgress}, false},
	}
	for _, tc := range cases {
		if got := tc.c.HasParentLeaseKey(); got != tc.want {
			t.Errorf("%s: HasParentLeaseKey = %v, 期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestLeaseContextRejectsOtherLengths 是本文件最重要的用例。
//
// 版本判别**只能**靠 DataLength（规范原文：v1 与 v2 共用同一个 context 名字
// "RqLs"，"the server differentiates these requests based on the value of the
// DataLength field"）。因此长度校验就是这里唯一的防线：既不能猜、不能截断、
// 不能补零，更不能因为长度不对而 panic —— 载荷完全来自网络（AGENTS.md §8）。
func TestLeaseContextRejectsOtherLengths(t *testing.T) {
	for n := 0; n <= LeaseContextV2Size+16; n++ {
		if n == LeaseContextV1Size || n == LeaseContextV2Size {
			continue
		}
		c, err := ParseLeaseContext(make([]byte, n))
		if err == nil {
			t.Errorf("%d 字节载荷应被拒绝，却解析出 %+v", n, c)
			continue
		}
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%d 字节载荷的错误应包裹 ErrMalformed，实际 %v", n, err)
		}
	}
	// nil 载荷同样不能 panic。
	if _, err := ParseLeaseContext(nil); err == nil {
		t.Error("nil 载荷应被拒绝")
	}
}

// TestLeaseContextIgnoresReservedFields 锁死「客户端置 0、服务端忽略」的处理：
// LeaseDuration(v1/v2 偏移 24) 与 Reserved(v2 偏移 50) 即便客户端填了非 0，
// 也不得影响解析结果，且服务端编码时必须恒写 0。
//
// 反过来说这也是一条**不许偷偷保存**的断言：如果哪天有人给 LeaseContext 加了
// LeaseDuration 字段并原样回显，这个用例会失败。规范要求响应里这两处必须为 0。
func TestLeaseContextIgnoresReservedFields(t *testing.T) {
	dirty := make([]byte, LeaseContextV2Size)
	copy(dirty[0:16], leaseKeyA[:])
	le.PutUint32(dirty[16:], uint32(LeaseReadCaching))
	// 24..32 LeaseDuration 填满 0xFF
	for i := 24; i < 32; i++ {
		dirty[i] = 0xFF
	}
	copy(dirty[32:48], leaseKeyB[:])
	le.PutUint16(dirty[48:], 7)
	// 50..52 Reserved 填非 0
	dirty[50], dirty[51] = 0xAA, 0xBB

	c, err := ParseLeaseContext(dirty)
	if err != nil {
		t.Fatalf("带脏保留区的载荷应当照常解析: %v", err)
	}
	if c.LeaseState != LeaseReadCaching || c.Epoch != 7 || c.ParentLeaseKey != leaseKeyB {
		t.Errorf("保留区的脏数据污染了真实字段: %+v", c)
	}

	// 重新编码后保留区必须归零。
	out := c.Encode()
	assertZero(t, out[24:32], "LeaseDuration")
	assertZero(t, out[50:52], "Reserved")
}

func assertZero(t *testing.T, b []byte, what string) {
	t.Helper()
	for i, v := range b {
		if v != 0 {
			t.Errorf("%s[%d] = %#x, 规范要求恒为 0", what, i, v)
		}
	}
}

// TestFindLeaseContext 覆盖 create context 链里查找 "RqLs" 的三种结局。
//
// 「客户端没请求租约」是**最常见**的情况，必须是 (nil, nil) 而不是错误 ——
// 把它当错误会让每一个普通 CREATE 都失败。
func TestFindLeaseContext(t *testing.T) {
	t.Run("不存在", func(t *testing.T) {
		c, err := FindLeaseContext([]CreateContext{
			{Name: CreateContextQFid},
			{Name: CreateContextMxAc},
		})
		if err != nil {
			t.Fatalf("没有 RqLs 不应报错: %v", err)
		}
		if c != nil {
			t.Errorf("没有 RqLs 应返回 nil，实际 %+v", c)
		}
	})

	t.Run("空链", func(t *testing.T) {
		c, err := FindLeaseContext(nil)
		if c != nil || err != nil {
			t.Errorf("空链应返回 (nil, nil)，实际 (%+v, %v)", c, err)
		}
	})

	t.Run("v1 存在", func(t *testing.T) {
		want := &LeaseContext{LeaseKey: leaseKeyA, LeaseState: LeaseReadCaching}
		got, err := FindLeaseContext([]CreateContext{
			{Name: CreateContextQFid},
			{Name: CreateContextRqLs, Data: want.Encode()},
		})
		if err != nil {
			t.Fatalf("FindLeaseContext: %v", err)
		}
		if got == nil || *got != *want {
			t.Errorf("解析结果 %+v, 期望 %+v", got, want)
		}
	})

	t.Run("载荷畸形要报错", func(t *testing.T) {
		// 长度既非 32 也非 52：必须报错，不能当成"没请求租约"静默放过。
		// 静默放过等于把一个明确要求缓存的客户端当成不要缓存的，
		// 而客户端那边认为自己拿到了服务端的沉默同意。
		got, err := FindLeaseContext([]CreateContext{
			{Name: CreateContextRqLs, Data: make([]byte, 40)},
		})
		if err == nil {
			t.Fatalf("畸形载荷应报错，实际解析出 %+v", got)
		}
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("错误应包裹 ErrMalformed，实际 %v", err)
		}
	})
}

// TestLeaseStateBits 锁死 RWH 三个状态位的取值（MS-SMB2 §2.2.13.2.8）。
//
// 位值写错不会让任何编解码测试失败（它们只是 uint32），但会让服务端授予
// 一个与客户端理解完全不同的缓存权限 —— 例如把 W 当成 H，客户端就会在
// 服务端以为"只缓存句柄"的时候缓存写数据。所以必须单独钉死。
func TestLeaseStateBits(t *testing.T) {
	cases := []struct {
		s    LeaseState
		want uint32
		name string
	}{
		{LeaseNone, 0x00, "SMB2_LEASE_NONE"},
		{LeaseReadCaching, 0x01, "SMB2_LEASE_READ_CACHING"},
		{LeaseHandleCaching, 0x02, "SMB2_LEASE_HANDLE_CACHING"},
		{LeaseWriteCaching, 0x04, "SMB2_LEASE_WRITE_CACHING"},
	}
	for _, tc := range cases {
		if uint32(tc.s) != tc.want {
			t.Errorf("%s = %#x, 规范值 %#x", tc.name, uint32(tc.s), tc.want)
		}
	}
	// RWH 全置 = 0x07，这是 lease break 报文里最常见的 CurrentLeaseState。
	if all := LeaseReadCaching | LeaseHandleCaching | LeaseWriteCaching; all != 0x07 {
		t.Errorf("R|H|W = %#x, 期望 0x07", uint32(all))
	}
}

// TestLeaseFlagsNamespaces 记录并锁死一个极易踩的坑：LeaseFlags 这个 Go 类型
// 被**三个互不相同的位域命名空间**共用（见 lease_context.go 的注释）。
//
//	Lease Break Notification（§2.2.23.2）   ACK_REQUIRED        0x01
//	租约 create context（§2.2.13.2.10）     PARENT_LEASE_KEY_SET 0x04
//	租约 create context 响应（§2.2.14.2.10）BREAK_IN_PROGRESS    0x02
//
// 它们恰好互不重叠，所以混用不会立刻炸 —— 这正是危险之处。
func TestLeaseFlagsNamespaces(t *testing.T) {
	cases := []struct {
		f    LeaseFlags
		want uint32
		name string
	}{
		{LeaseBreakAckRequired, 0x01, "SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED"},
		{LeaseFlagBreakInProgress, 0x02, "SMB2_LEASE_FLAG_BREAK_IN_PROGRESS"},
		{LeaseFlagParentLeaseKeySet, 0x04, "SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET"},
	}
	for _, tc := range cases {
		if uint32(tc.f) != tc.want {
			t.Errorf("%s = %#x, 规范值 %#x", tc.name, uint32(tc.f), tc.want)
		}
	}
}
