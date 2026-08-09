//go:build integration

package integration

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// compoundPart 是复合请求链中的一段。related=true 表示本段复用上一段的
// SessionId/TreeId/FileId（SMB2 FLAGS_RELATED_OPERATIONS），其 FileId 字段
// 应填 wire.CompoundFileID（全 0xFF）让服务端解析为「上一条 CREATE 的句柄」。
type compoundPart struct {
	cmd     wire.Command
	body    []byte
	related bool
}

// compound 把多段请求拼成一条复合链（NextCommand 偏移 + 8 字节对齐）并一次性
// 发出，返回服务端回应的整条链（按 NextCommand 切分后的各条消息）。
//
// 本方法刻意不签名、不加密：复合请求的签名/加密语义较特殊，且本测试关注的是
// 复合链解析与 related-ops 句柄解析，因此跑在「不强制签名」的会话上，避开
// 复合签名这一独立难点。
func (c *rawClient) compound(parts []compoundPart) ([][]byte, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("compound: 空链")
	}
	c.msgID++
	first := c.msgID

	msgs := make([][]byte, len(parts))
	for i, p := range parts {
		h := wire.Header{
			Command:   p.cmd,
			Credits:   1,
			MessageID: first + uint64(i),
			TreeID:    c.treeID,
			SessionID: c.sessID,
		}
		if p.related {
			h.Flags |= wire.FlagRelatedOps
			h.TreeID = 0
			h.SessionID = 0
		}
		m := h.Append(nil)
		m = append(m, p.body...)
		msgs[i] = m
	}

	// 拼接并对齐：每条消息后补 0 到 8 字节边界（最后一条除外）。
	var chain []byte
	offsets := make([]int, len(msgs))
	for i, m := range msgs {
		offsets[i] = len(chain)
		chain = append(chain, m...)
		if i < len(msgs)-1 {
			for len(chain)%8 != 0 {
				chain = append(chain, 0)
			}
		}
	}
	// 回填 NextCommand（相对本消息头起点）。
	for i := 0; i < len(msgs)-1; i++ {
		next := uint32(offsets[i+1] - offsets[i])
		binary.LittleEndian.PutUint32(chain[offsets[i]+0x14:], next)
	}

	if err := c.writeMessage(chain); err != nil {
		return nil, err
	}
	seg, err := c.readMessage()
	if err != nil {
		return nil, err
	}
	return splitChain(seg)
}

// splitChain 按 NextCommand 把一条响应链切成各条消息。
func splitChain(seg []byte) ([][]byte, error) {
	var out [][]byte
	pos := 0
	for {
		if pos+wire.HeaderSize > len(seg) {
			return nil, fmt.Errorf("splitChain: 截断于 %d", pos)
		}
		next := int(binary.LittleEndian.Uint32(seg[pos+0x14:]))
		end := pos + wire.HeaderSize
		if next != 0 {
			end = pos + next
		}
		if end > len(seg) {
			return nil, fmt.Errorf("splitChain: NextCommand 越界")
		}
		out = append(out, seg[pos:end])
		if next == 0 {
			break
		}
		pos = pos + next
	}
	return out, nil
}

// TestCompoundUnrelated 验证无关联的复合链（两条 ECHO）能被服务端整链处理
// 并返回两条独立响应。
func TestCompoundUnrelated(t *testing.T) {
	h := startServer(t, harnessOptions{})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	parts := []compoundPart{
		{cmd: wire.CommandEcho, body: (&wire.EchoRequest{}).Append(nil)},
		{cmd: wire.CommandEcho, body: (&wire.EchoRequest{}).Append(nil)},
	}
	resp, err := c.compound(parts)
	if err != nil {
		t.Fatalf("compound: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("响应链条数 = %d, 期望 2", len(resp))
	}
	for i, m := range resp {
		hh, err := wire.ParseHeader(m)
		if err != nil {
			t.Fatalf("解析响应 %d: %v", i, err)
		}
		if hh.Status != 0 {
			t.Fatalf("响应 %d 状态 = %#x, 期望 0", i, hh.Status)
		}
		if hh.Command != wire.CommandEcho {
			t.Fatalf("响应 %d 命令 = %s, 期望 ECHO", i, hh.Command)
		}
	}
}

// TestCompoundRelated 验证 related 复合链：CREATE 之后用 CompoundFileID 的
// CLOSE 能正确解析到上一条 CREATE 返回的句柄（句柄复用）。
func TestCompoundRelated(t *testing.T) {
	h := startServer(t, harnessOptions{})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("treeConnect: %v", err)
	}

	cr, err := (&wire.CreateRequest{
		ImpersonationLevel: wire.ImpersonationImpersonation,
		DesiredAccess:      wire.GenericRead | wire.GenericWrite,
		FileAttributes:     wire.FileAttributeNormal,
		ShareAccess:        wire.ShareRead | wire.ShareWrite,
		CreateDisposition:  wire.FileOpenIf,
		CreateOptions:      wire.FileNonDirectoryFile,
		Name:               "comp-related.txt",
	}).Append(nil)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	cl := (&wire.CloseRequest{FileID: wire.CompoundFileID}).Append(nil)
	parts := []compoundPart{
		{cmd: wire.CommandCreate, body: cr},
		{cmd: wire.CommandClose, body: cl, related: true},
	}
	resp, err := c.compound(parts)
	if err != nil {
		t.Fatalf("compound: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("响应链条数 = %d, 期望 2", len(resp))
	}
	ch, _ := wire.ParseHeader(resp[0])
	if ch.Status != 0 {
		t.Fatalf("CREATE 状态 = %#x, 期望 0", ch.Status)
	}
	clh, _ := wire.ParseHeader(resp[1])
	if clh.Status != 0 {
		t.Fatalf("related CLOSE 状态 = %#x, 期望 0（句柄复用失败？）", clh.Status)
	}
}

// TestCompoundRelatedPostFailure 验证 related 链中后段失败会被正确回报：
// CREATE 成功 → CLOSE（CompoundFileID）成功 → 再用 CompoundFileID CLOSE 一次
// （句柄已关，应失败）。证明 related 链逐段解析、失败状态正确回传。
func TestCompoundRelatedPostFailure(t *testing.T) {
	h := startServer(t, harnessOptions{})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("treeConnect: %v", err)
	}

	cr, _ := (&wire.CreateRequest{
		ImpersonationLevel: wire.ImpersonationImpersonation,
		DesiredAccess:      wire.GenericRead | wire.GenericWrite,
		FileAttributes:     wire.FileAttributeNormal,
		ShareAccess:        wire.ShareRead | wire.ShareWrite,
		CreateDisposition:  wire.FileOpenIf,
		CreateOptions:      wire.FileNonDirectoryFile,
		Name:               "comp-fail.txt",
	}).Append(nil)
	cl := (&wire.CloseRequest{FileID: wire.CompoundFileID}).Append(nil)
	parts := []compoundPart{
		{cmd: wire.CommandCreate, body: cr},
		{cmd: wire.CommandClose, body: cl, related: true},
		{cmd: wire.CommandClose, body: cl, related: true}, // 句柄已关，应失败
	}
	resp, err := c.compound(parts)
	if err != nil {
		t.Fatalf("compound: %v", err)
	}
	if len(resp) != 3 {
		t.Fatalf("响应链条数 = %d, 期望 3", len(resp))
	}
	h0, _ := wire.ParseHeader(resp[0])
	h1, _ := wire.ParseHeader(resp[1])
	h2, _ := wire.ParseHeader(resp[2])
	if h0.Status != 0 {
		t.Fatalf("CREATE 状态 = %#x", h0.Status)
	}
	if h1.Status != 0 {
		t.Fatalf("第一次 CLOSE 状态 = %#x（应成功）", h1.Status)
	}
	if h2.Status == 0 {
		t.Fatalf("第二次 CLOSE 状态 = 0，期望失败（句柄已关）")
	}
}
