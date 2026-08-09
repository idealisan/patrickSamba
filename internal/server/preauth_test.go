package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// sessionSetupSeeds 只取真实抓包里客户端方向的 SESSION_SETUP 报文。
//
// SESSION_SETUP 里裹着 SPNEGO/NTLMSSP token，是**唯一**一个未认证客户端
// 就能触达的复杂解析器（NEGOTIATE 的结构简单得多）。它的解析代码在
// internal/auth 里，不归本 agent 改，但攻击面归本 agent 审。
func sessionSetupSeeds(t *testing.T) [][]byte {
	t.Helper()

	var out [][]byte
	err := filepath.Walk(captureRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(p) != ".bin" {
			return err
		}
		name := filepath.Base(p)
		if !strings.Contains(name, "-c2s-") || !strings.Contains(name, "SESSION_SETUP") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if len(b) > wire.HeaderSize {
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历抓包目录失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没有找到 SESSION_SETUP 抓包，认证前变异测试没有种子")
	}
	return out
}

// securityBlobMutations 针对 SESSION_SETUP 的安全 blob 做变异。
//
// SESSION_SETUP Request 体（MS-SMB2 §2.2.5）：
//
//	 0 StructureSize(2)=25   2 Flags(1)   3 SecurityMode(1)
//	 4 Capabilities(4)       8 Channel(4)
//	12 SecurityBufferOffset(2)  14 SecurityBufferLength(2)
//	16 PreviousSessionId(8)  24 Buffer（SPNEGO token）
//
// 重点打三类：offset/length 撒谎（越界读的经典入口）、
// blob 内部逐字节翻转（ASN.1/DER 解析器的入口）、任意位置截断。
func securityBlobMutations(seed []byte) [][]byte {
	var out [][]byte
	add := func(b []byte) {
		if len(b) > 0 {
			out = append(out, b)
		}
	}

	// 1) SecurityBufferOffset / SecurityBufferLength 撒谎。
	//    这两个字段直接决定从哪里切多长的切片给 SPNEGO 解析器。
	for _, off := range []int{wire.HeaderSize + 12, wire.HeaderSize + 14} {
		if off+2 > len(seed) {
			continue
		}
		for _, v := range [][]byte{
			{0x00, 0x00},
			{0xFF, 0xFF},
			{0xFF, 0x7F},
			{0x00, 0x80},
			{0x01, 0x00},
			{byte(len(seed)), byte(len(seed) >> 8)},
			{byte(len(seed) + 1), byte((len(seed) + 1) >> 8)},
		} {
			m := append([]byte(nil), seed...)
			copy(m[off:], v)
			add(m)
		}
	}

	// 2) blob 区域（体偏移 24 起）逐字节翻转。SPNEGO 是 DER，
	//    tag/length 字节被改就是长度撒谎，最容易越界。
	for i := wire.HeaderSize + 24; i < len(seed); i++ {
		for _, v := range []byte{0xFF, 0x00, 0x80, 0x30, 0xA0} {
			if seed[i] == v {
				continue
			}
			m := append([]byte(nil), seed...)
			m[i] = v
			add(m)
		}
	}

	// 3) 在 blob 中间的任意位置截断（DER 结构声称的长度会超出实际数据）。
	for n := wire.HeaderSize + 24; n < len(seed); n++ {
		add(append([]byte(nil), seed[:n]...))
	}

	return out
}

// TestPreauthSeedLayout 校验种子确实是 SESSION_SETUP，
// 免得上面那些按偏移写死的变异打在了错误的字段上（那样测试就是白跑）。
func TestPreauthSeedLayout(t *testing.T) {
	for _, seed := range sessionSetupSeeds(t) {
		// SMB2 头偏移 12 是 Command（2 字节小端），SESSION_SETUP = 0x0001。
		if cmd := uint16(seed[12]) | uint16(seed[13])<<8; cmd != 0x0001 {
			t.Fatalf("种子不是 SESSION_SETUP：Command = %#x", cmd)
		}
		// 体偏移 0 是 StructureSize，SESSION_SETUP Request 固定 25。
		if ss := uint16(seed[wire.HeaderSize]) | uint16(seed[wire.HeaderSize+1])<<8; ss != 25 {
			t.Fatalf("SESSION_SETUP Request 的 StructureSize 应为 25，实际 %d", ss)
		}
	}
}

// TestPreauthSessionSetupNoPanic 覆盖**未认证**客户端能触达的最大攻击面：
// 连接完成 NEGOTIATE 之后、认证成功之前，畸形的 SPNEGO/NTLMSSP token
// 绝不能把进程打崩。
//
// 认证解析代码在 internal/auth（不归本 agent 改），这里是它的外部黑盒回归。
func TestPreauthSessionSetupNoPanic(t *testing.T) {
	seeds := sessionSetupSeeds(t)

	// 每个种子用一条独立连接：SESSION_SETUP 是有状态的多轮握手，
	// 复用连接会让后面的变体全部走进"状态不对"的早退分支，测不到解析器。
	n := 0
	for si, seed := range seeds {
		c := newFullTestConn(t)
		// 先把连接推进到"已协商、未认证"这个真实的攻击状态。
		negotiateOn(t, c)

		for mi, m := range securityBlobMutations(seed) {
			feedNoPanic(t, c, fmt.Sprintf("认证前 SESSION_SETUP 种子 #%d 变体 #%d", si, mi), m)
			n++
		}
	}
	t.Logf("已喂入 %d 个畸形 SESSION_SETUP 变体（已协商、未认证）", n)
}

// negotiateOn 用真实抓包的 NEGOTIATE 把连接推进到已协商状态。
func negotiateOn(t *testing.T, c *Connection) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captureRoot, "negotiate-smb311", "001-c2s-NEGOTIATE.bin"))
	if err != nil {
		t.Fatalf("读取 NEGOTIATE 抓包失败: %v", err)
	}
	if _, err := c.handleFrame(b); err != nil {
		t.Fatalf("NEGOTIATE 失败: %v", err)
	}
	if !c.state.NegotiateDone {
		t.Fatal("NEGOTIATE 之后连接仍处于未协商状态")
	}
}
