package server

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// splitChain 把一个复合响应帧按 NextCommand 拆成若干条响应头。
func splitChain(t *testing.T, resp []byte) []wire.Header {
	t.Helper()

	var out []wire.Header
	pos := 0
	for {
		h, err := wire.ParseHeader(resp[pos:])
		if err != nil {
			t.Fatalf("解析偏移 %d 处的响应头失败: %v", pos, err)
		}
		out = append(out, h)
		if h.NextCommand == 0 {
			return out
		}
		pos += int(h.NextCommand)
		if pos >= len(resp) {
			t.Fatalf("NextCommand 链在偏移 %d 处越出响应帧（%d 字节）", pos, len(resp))
		}
	}
}

// closeBody 是 SMB2 CLOSE Request 的报文体（MS-SMB2 §2.2.15）：
// StructureSize(2)=24 + Flags(2) + Reserved(4) + FileId(16)。
// fid 为 true 时 FileId 填全 0xFF（复合链"复用上一个句柄"）。
func closeBody(compoundFileID bool) []byte {
	b := make([]byte, 24)
	b[0] = 24
	if compoundFileID {
		for i := 8; i < 24; i++ {
			b[i] = 0xFF
		}
	}
	return b
}

// TestCompoundFirstRelatedRejected：链中**首条**消息不允许置
// SMB2_FLAGS_RELATED_OPERATIONS（MS-SMB2 §3.3.5.2.7.2），
// 必须回 STATUS_INVALID_PARAMETER 而不是被当成正常请求处理。
func TestCompoundFirstRelatedRejected(t *testing.T) {
	c := newTestConn()

	resp, err := c.handleSMB2Chain(
		buildMsg(wire.CommandEcho, 1, wire.FlagRelatedOps, echoBody()))
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}

	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	if status.Status(h.Status) != status.InvalidParameter {
		t.Fatalf("首条 RELATED 期望 STATUS_INVALID_PARAMETER，实际 %s",
			status.Status(h.Status))
	}
}

// TestCompoundFirstRelatedPoisonsChain：首条非法置 RELATED 时整条链都失败
// （规范："the server SHOULD fail processing the compound chain request"）。
func TestCompoundFirstRelatedPoisonsChain(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandEcho, 1, wire.FlagRelatedOps, echoBody()),
		buildMsg(wire.CommandEcho, 2, wire.FlagRelatedOps, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	hdrs := splitChain(t, resp)
	if len(hdrs) != 2 {
		t.Fatalf("期望 2 条响应，实际 %d 条", len(hdrs))
	}
	for i, h := range hdrs {
		if status.Status(h.Status) != status.InvalidParameter {
			t.Fatalf("第 %d 条期望 STATUS_INVALID_PARAMETER，实际 %s",
				i+1, status.Status(h.Status))
		}
	}
}

// TestCompoundUnrelatedIndependent：unrelated 请求各自独立，
// 前面失败不影响后面（MS-SMB2 §3.3.5.2.7.1）。
//
// 第一条 CLOSE 没有会话必然失败，第二条 ECHO 是 unrelated，必须成功。
func TestCompoundUnrelatedIndependent(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandClose, 1, 0, closeBody(false)),
		buildMsg(wire.CommandEcho, 2, 0, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	hdrs := splitChain(t, resp)
	if len(hdrs) != 2 {
		t.Fatalf("期望 2 条响应，实际 %d 条", len(hdrs))
	}
	if status.Status(hdrs[0].Status) == status.Success {
		t.Fatal("无会话的 CLOSE 应当失败")
	}
	if status.Status(hdrs[1].Status) != status.Success {
		t.Fatalf("unrelated ECHO 不应被前一条的失败影响，实际 %s",
			status.Status(hdrs[1].Status))
	}
}

// TestCompoundUnrelatedResetsFailedChain 覆盖之前的真 bug：
// Chain.Failed 一旦置位就永不复位，导致 [失败, unrelated, related]
// 里最后那条 related 被**上上条**的错误毒死。
//
// MS-SMB2 Appendix A <117>：遇到未置 RELATED 的请求即视为新链开始，
// 因此第三条 related 应当继承的是第二条（成功的 ECHO），
// 而不是第一条的失败。
func TestCompoundUnrelatedResetsFailedChain(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandClose, 1, 0, closeBody(false)),
		buildMsg(wire.CommandEcho, 2, 0, echoBody()),
		buildMsg(wire.CommandEcho, 3, wire.FlagRelatedOps, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	hdrs := splitChain(t, resp)
	if len(hdrs) != 3 {
		t.Fatalf("期望 3 条响应，实际 %d 条", len(hdrs))
	}
	if status.Status(hdrs[1].Status) != status.Success {
		t.Fatalf("第二条 unrelated ECHO 期望成功，实际 %s", status.Status(hdrs[1].Status))
	}
	if status.Status(hdrs[2].Status) != status.Success {
		t.Fatalf("第三条 related ECHO 继承的是第二条（成功），不该失败，实际 %s",
			status.Status(hdrs[2].Status))
	}
}

// TestCompoundRelatedAfterFailurePropagates：前序失败时后续 related
// 消息不得执行。当前操作需要会话/树而链中没有 → STATUS_INVALID_PARAMETER
// （MS-SMB2 §3.3.5.2.7.2 原文）。
func TestCompoundRelatedAfterFailurePropagates(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandClose, 1, 0, closeBody(false)),
		buildMsg(wire.CommandClose, 2, wire.FlagRelatedOps, closeBody(true)),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	hdrs := splitChain(t, resp)
	if len(hdrs) != 2 {
		t.Fatalf("期望 2 条响应，实际 %d 条", len(hdrs))
	}
	if status.Status(hdrs[1].Status) != status.InvalidParameter {
		t.Fatalf("需要 SessionId/TreeId 却无从继承时期望 STATUS_INVALID_PARAMETER，实际 %s",
			status.Status(hdrs[1].Status))
	}
}

// TestCompoundResponseChainWellFormed：复合响应链每段必须 8 字节对齐，
// 且非末条的 NextCommand 精确指向下一段（MS-SMB2 §3.3.5.2.7）。
func TestCompoundResponseChainWellFormed(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandEcho, 1, 0, echoBody()),
		buildMsg(wire.CommandEcho, 2, 0, echoBody()),
		buildMsg(wire.CommandEcho, 3, 0, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}

	pos := 0
	for i := 0; ; i++ {
		h, err := wire.ParseHeader(resp[pos:])
		if err != nil {
			t.Fatalf("解析第 %d 段失败: %v", i+1, err)
		}
		if pos%8 != 0 {
			t.Fatalf("第 %d 段起点 %d 未 8 字节对齐", i+1, pos)
		}
		if h.NextCommand == 0 {
			if i != 2 {
				t.Fatalf("期望 3 段，实际在第 %d 段结束", i+1)
			}
			break
		}
		if h.NextCommand%8 != 0 {
			t.Fatalf("第 %d 段 NextCommand=%d 未 8 字节对齐", i+1, h.NextCommand)
		}
		pos += int(h.NextCommand)
	}
	// 末段之后不应有多余字节（除对齐外）。
	if len(resp)-pos != wire.HeaderSize+4 {
		t.Fatalf("末段长度期望 %d，实际 %d", wire.HeaderSize+4, len(resp)-pos)
	}
}
