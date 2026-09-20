package command

import (
	"log/slog"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// TestOplockBreakStillRegistered 守住 handler 从 notify.go 搬到 oplock.go
// 这次移动。
//
// register 对重复注册是 panic，所以"搬漏了"表现为 init 期崩溃，很显眼；
// 真正会被静默吃掉的是**反过来**——两边都删干净了，OPLOCK_BREAK 从此
// 走默认 handler 回 STATUS_NOT_SUPPORTED，编译和现有测试全绿。
func TestOplockBreakStillRegistered(t *testing.T) {
	if !Registered(wire.CommandOplockBreak) {
		t.Fatal("OPLOCK_BREAK 未注册：handler 在搬移中丢了")
	}
	if NoResponse(wire.CommandOplockBreak) {
		t.Fatal("OPLOCK_BREAK 被登记成无响应命令，客户端会挂住等回包")
	}
}

// TestHandleOplockBreakRejectsAck 锁定当前（PR-1）的诚实行为：
// 本服务不授予任何 oplock/lease，所以任何 break 确认都是协议违规。
//
// 三条路径分开断言，因为它们返回的**不是同一个**状态码：
//   - oplock 族 / lease 族确认 → STATUS_INVALID_OPLOCK_PROTOCOL（§3.3.5.22.1）
//   - StructureSize 认不出来   → STATUS_INVALID_PARAMETER（连是哪族都不知道）
//
// PR-2 真正开始授予之后，前两条会改成"查表 → 找到就降级、找不到才报错"。
// 到那时这个用例必须一起改，改不动就说明授予路径没接上确认路径。
func TestHandleOplockBreakRejectsAck(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want status.Status
	}{
		{
			// §2.2.24.1 oplock 族确认：StructureSize=24。
			name: "oplock 族确认",
			body: oplockAckBody(wire.OplockLevelII),
			want: status.InvalidOplockProtocol,
		},
		{
			// §2.2.24.2 lease 族确认：StructureSize=36。
			name: "lease 族确认",
			body: leaseAckBody(),
			want: status.InvalidOplockProtocol,
		},
		{
			// 44 是 Lease Break **Notification** 的大小，客户端不该发它。
			name: "StructureSize 44（服务端才发的通知）",
			body: append([]byte{44, 0}, make([]byte, 42)...),
			want: status.InvalidParameter,
		},
		{
			name: "StructureSize 认不出来",
			body: append([]byte{0x63, 0x00}, make([]byte, 22)...),
			want: status.InvalidParameter,
		},
		{
			name: "体只有 1 字节，连 StructureSize 都读不全",
			body: []byte{24},
			want: status.InvalidParameter,
		},
		{
			// StructureSize 说是 24，但后面的字节不够 —— Peek 判成 oplock 族，
			// Parse 再兜住长度。这条专门验"Peek 之后还得 Parse"没被省掉。
			name: "StructureSize=24 但体被截断",
			body: []byte{24, 0, 0x01, 0, 0, 0, 0, 0},
			want: status.InvalidParameter,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newOplockTestContext(t, tc.body)
			err := handleOplockBreak(ctx)
			if err != tc.want {
				t.Fatalf("返回 %v，期望 %v", err, tc.want)
			}
		})
	}
}

// TestBreakSenderInjection 验证注入/取值这对方法本身。
//
// 未注入时必须返回 nil 而不是 panic —— 单元测试与将来的非网络场景
// 都会构造裸 Conn，协议层要能靠 nil 判断"这条连接推不了 break"。
func TestBreakSenderInjection(t *testing.T) {
	c := NewConn(&Settings{}, "test", "test")
	if c.BreakSender() != nil {
		t.Fatal("裸 Conn 的 BreakSender 应为 nil")
	}

	fake := &fakeBreakSender{}
	c.SetBreakSender(fake)
	if c.BreakSender() != fake {
		t.Fatal("SetBreakSender 之后取不回同一个实现")
	}

	// 两族各走一遍，确认接口的两个方法都接得上。
	if err := c.BreakSender().SendOplockBreak(BreakTarget{}, wire.OplockBreak{}); err != nil {
		t.Fatalf("SendOplockBreak: %v", err)
	}
	if err := c.BreakSender().SendLeaseBreak(BreakTarget{}, wire.LeaseBreakNotification{}); err != nil {
		t.Fatalf("SendLeaseBreak: %v", err)
	}
	if fake.oplocks != 1 || fake.leases != 1 {
		t.Fatalf("oplock=%d lease=%d，期望各 1 —— 两个方法可能接串了",
			fake.oplocks, fake.leases)
	}

	c.SetBreakSender(nil)
	if c.BreakSender() != nil {
		t.Fatal("SetBreakSender(nil) 应能关闭推送能力")
	}
}

// TestBreakTargetIsValueCopy 说明 BreakTarget 是**值**语义。
//
// break 是异步发的，发的时候原来的 session/tree 可能已经没了。
// 这里断言把 target 传出去之后再改本地变量不会影响已经排队的那一份 ——
// 如果哪天有人把它改成 *BreakTarget，这个用例会红。
func TestBreakTargetIsValueCopy(t *testing.T) {
	c := NewConn(&Settings{}, "test", "test")
	fake := &fakeBreakSender{}
	c.SetBreakSender(fake)

	tgt := BreakTarget{SessionID: 7, TreeID: 3}
	if err := c.BreakSender().SendOplockBreak(tgt, wire.OplockBreak{}); err != nil {
		t.Fatalf("SendOplockBreak: %v", err)
	}
	tgt.SessionID = 999

	if fake.lastTarget.SessionID != 7 {
		t.Fatalf("排队中的 target 被事后修改影响到了：SessionID=%d，期望 7",
			fake.lastTarget.SessionID)
	}
}

// ---- 测试辅助 ----

type fakeBreakSender struct {
	lastTarget BreakTarget
	oplocks    int
	leases     int

	// lastOplock / lastLease 记下最近一次的报文体。
	// 授予路径（oplock_grant_test.go）要断言"要求降到哪个级别"，
	// 只计数不够 —— 计数看不出降级方向给错了。
	lastOplock wire.OplockBreak
	lastLease  wire.LeaseBreakNotification
}

func (f *fakeBreakSender) SendOplockBreak(t BreakTarget, b wire.OplockBreak) error {
	f.lastTarget = t
	f.lastOplock = b
	f.oplocks++
	return nil
}

func (f *fakeBreakSender) SendLeaseBreak(t BreakTarget, b wire.LeaseBreakNotification) error {
	f.lastTarget = t
	f.lastLease = b
	f.leases++
	return nil
}

// oplockAckBody 拼 §2.2.24.1 的 24 字节体。
func oplockAckBody(level wire.OplockLevel) []byte {
	b := make([]byte, 24)
	b[0], b[1] = 24, 0 // StructureSize，小端
	b[2] = byte(level)
	return b
}

// leaseAckBody 拼 §2.2.24.2 的 36 字节体。
func leaseAckBody() []byte {
	b := make([]byte, 36)
	b[0], b[1] = 36, 0 // StructureSize，小端
	return b
}

// newOplockTestContext 造一个只够 handleOplockBreak 用的 Context：
// 它只读 ctx.Msg / ctx.Log / ctx.Header。
func newOplockTestContext(t *testing.T, body []byte) *Context {
	t.Helper()
	h := wire.Header{Command: wire.CommandOplockBreak, SessionID: 1}
	msg := append(h.Append(nil), body...)
	return &Context{
		Header: h,
		Msg:    msg,
		Log:    slog.Default(),
	}
}
