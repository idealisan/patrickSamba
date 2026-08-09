package mdns

import (
	"net"
	"testing"
	"time"
)

func udpAddr(t *testing.T, s string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		t.Fatalf("解析地址 %q: %v", s, err)
	}
	return a
}

func ptrRecord(name, target string, ttl uint32) Record {
	return Record{Name: name, Class: ClassIN, TTL: ttl, Data: PTR{Target: target}}
}

// TestKnownAnswerStashMergesAcrossPackets 验证 RFC 6762 §7.2 的跨报文合并：
// 首包（TC=1）与续包的已知答案要能被一并取出。
func TestKnownAnswerStashMergesAcrossPackets(t *testing.T) {
	s := newKnownAnswerStash()
	src := udpAddr(t, "192.0.2.10:5353")
	now := time.Now()

	s.add(src, []Record{ptrRecord("_smb._tcp.local.", "a._smb._tcp.local.", 4500)}, now)
	s.add(src, []Record{ptrRecord("_smb._tcp.local.", "b._smb._tcp.local.", 4500)}, now.Add(50*time.Millisecond))

	got := s.get(src, now.Add(100*time.Millisecond))
	if len(got) != 2 {
		t.Fatalf("已知答案数 = %d, 期望 2（首包 + 续包）", len(got))
	}
}

// TestKnownAnswerStashIsolatesSources 验证不同查询者的已知答案不会串台 ——
// 串台会导致本该发给 B 的记录被 A 的缓存"抑制"掉。
func TestKnownAnswerStashIsolatesSources(t *testing.T) {
	s := newKnownAnswerStash()
	now := time.Now()
	a := udpAddr(t, "192.0.2.10:5353")
	b := udpAddr(t, "192.0.2.11:5353")

	s.add(a, []Record{ptrRecord("_smb._tcp.local.", "x._smb._tcp.local.", 4500)}, now)

	if got := s.get(b, now); len(got) != 0 {
		t.Errorf("查询者 B 取到了 %d 条属于 A 的已知答案", len(got))
	}
	// 同一 IP 的不同源端口也算不同查询者（同机上的两个进程）。
	if got := s.get(udpAddr(t, "192.0.2.10:12345"), now); len(got) != 0 {
		t.Errorf("不同源端口取到了 %d 条已知答案", len(got))
	}
}

// TestKnownAnswerStashExpires 验证暂存会过期：
// 不过期的话上一轮查询的已知答案会误抑制下一轮该发的记录。
func TestKnownAnswerStashExpires(t *testing.T) {
	s := newKnownAnswerStash()
	src := udpAddr(t, "192.0.2.10:5353")
	now := time.Now()

	s.add(src, []Record{ptrRecord("_smb._tcp.local.", "x._smb._tcp.local.", 4500)}, now)

	if got := s.get(src, now.Add(knownAnswerTTL-time.Millisecond)); len(got) != 1 {
		t.Errorf("过期前取到 %d 条, 期望 1", len(got))
	}
	if got := s.get(src, now.Add(knownAnswerTTL)); len(got) != 0 {
		t.Errorf("过期后仍取到 %d 条", len(got))
	}
}

// TestKnownAnswerStashLimits 验证资源上限（AGENTS.md §8）：
// 恶意对端不停发续包不能把内存吃光。
func TestKnownAnswerStashLimits(t *testing.T) {
	s := newKnownAnswerStash()
	now := time.Now()
	src := udpAddr(t, "192.0.2.10:5353")

	for i := 0; i < maxKnownAnswersPerSrc*2; i++ {
		s.add(src, []Record{ptrRecord("_smb._tcp.local.", "x._smb._tcp.local.", 4500)}, now)
	}
	if got := len(s.get(src, now)); got != maxKnownAnswersPerSrc {
		t.Errorf("单个查询者暂存 %d 条, 期望封顶在 %d", got, maxKnownAnswersPerSrc)
	}

	for i := 0; i < maxKnownAnswerSources*2; i++ {
		a := &net.UDPAddr{IP: net.IPv4(198, 51, 100, byte(i%256)), Port: 5353 + i}
		s.add(a, []Record{ptrRecord("_smb._tcp.local.", "y._smb._tcp.local.", 4500)}, now)
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n > maxKnownAnswerSources {
		t.Errorf("暂存了 %d 个查询者, 超过上限 %d", n, maxKnownAnswerSources)
	}
}

func queryPacket(t *testing.T, srcAddr string, multicast bool, m *Message) packet {
	t.Helper()
	return packet{msg: m, src: udpAddr(t, srcAddr), multicast: multicast}
}

// TestReplyModeFor 覆盖 RFC 6762 §5.4/§5.5/§6/§6.7/§7.2 的应答方式决策。
func TestReplyModeFor(t *testing.T) {
	q := func(unicast bool) []Question {
		return []Question{{Name: "_smb._tcp.local.", Type: TypePTR, Class: ClassIN, Unicast: unicast}}
	}

	tests := []struct {
		name     string
		pkt      packet
		shared   bool
		unicast  bool
		legacy   bool
		delayMin time.Duration
		delayMax time.Duration
	}{
		{
			// §6：组播来的共享记录查询，延迟 20-120ms 错峰。
			name:     "组播共享记录查询延迟错峰",
			pkt:      queryPacket(t, "192.0.2.10:5353", true, &Message{Questions: q(false)}),
			shared:   true,
			delayMin: sharedReplyDelayMin,
			delayMax: sharedReplyDelayMax,
		},
		{
			// §6：唯一记录（cache-flush）的单问题查询立即应答。
			name: "组播唯一记录查询立即应答",
			pkt:  queryPacket(t, "192.0.2.10:5353", true, &Message{Questions: q(false)}),
		},
		{
			// §5.4：QU 位要求单播回应，单播没有风暴问题，不延迟。
			name:    "QU 位单播回应",
			pkt:     queryPacket(t, "192.0.2.10:5353", true, &Message{Questions: q(true)}),
			shared:  true,
			unicast: true,
		},
		{
			// §5.5：直接单播发给我们的查询要单播回应 —— 对方不在组播组里。
			name:    "定向单播查询单播回应",
			pkt:     queryPacket(t, "192.0.2.10:5353", false, &Message{Questions: q(false)}),
			shared:  true,
			unicast: true,
		},
		{
			// §6.7：源端口不是 5353 的传统单播查询。
			name:    "传统单播查询",
			pkt:     queryPacket(t, "192.0.2.10:34567", true, &Message{Questions: q(false)}),
			unicast: true,
			legacy:  true,
		},
		{
			// §7.2：TC 位说明还有已知答案在续包里，等 400-500ms。
			name: "TC 位查询等待续包",
			pkt: queryPacket(t, "192.0.2.10:5353", true,
				&Message{Flags: FlagTruncated, Questions: q(false)}),
			delayMin: tcReplyDelayMin,
			delayMax: tcReplyDelayMax,
		},
		{
			// TC 优先于单播：不等续包的话已知答案抑制就失效了。
			name: "TC 位对单播查询同样等待",
			pkt: queryPacket(t, "192.0.2.10:5353", true,
				&Message{Flags: FlagTruncated, Questions: q(true)}),
			unicast:  true,
			delayMin: tcReplyDelayMin,
			delayMax: tcReplyDelayMax,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := replyModeFor(tt.pkt, tt.shared)
			if m.unicast != tt.unicast {
				t.Errorf("unicast = %v, 期望 %v", m.unicast, tt.unicast)
			}
			if m.legacy != tt.legacy {
				t.Errorf("legacy = %v, 期望 %v", m.legacy, tt.legacy)
			}
			if m.delayMin != tt.delayMin || m.delayMax != tt.delayMax {
				t.Errorf("延迟区间 = [%v, %v), 期望 [%v, %v)",
					m.delayMin, m.delayMax, tt.delayMin, tt.delayMax)
			}
			// delay() 必须落在区间内。
			for i := 0; i < 50; i++ {
				d := m.delay()
				if d < tt.delayMin || (tt.delayMax > tt.delayMin && d >= tt.delayMax) {
					t.Fatalf("delay() = %v, 超出 [%v, %v)", d, tt.delayMin, tt.delayMax)
				}
			}
		})
	}
}

// TestSuppressKnownAnswers 验证 RFC 6762 §7.1：
// 对方缓存里还新鲜（剩余 TTL > 我们 TTL 的一半）的记录不重发，
// 快过期的仍要重发。
func TestSuppressKnownAnswers(t *testing.T) {
	ours := []Record{
		ptrRecord("_smb._tcp.local.", "a._smb._tcp.local.", 4500),
		ptrRecord("_smb._tcp.local.", "b._smb._tcp.local.", 4500),
	}

	fresh := []Record{ptrRecord("_smb._tcp.local.", "a._smb._tcp.local.", 4000)}
	got := suppressKnownAnswers(ours, fresh)
	if len(got) != 1 {
		t.Fatalf("抑制后剩 %d 条, 期望 1", len(got))
	}
	if d := got[0].Data.(PTR); d.Target != "b._smb._tcp.local." {
		t.Errorf("剩下的是 %q, 期望 b._smb._tcp.local.", d.Target)
	}

	// 剩余 TTL 只有一半以下：对方缓存快过期了，必须重发。
	stale := []Record{ptrRecord("_smb._tcp.local.", "a._smb._tcp.local.", 4500/2)}
	if got := suppressKnownAnswers(ours, stale); len(got) != 2 {
		t.Errorf("对方缓存将过期时抑制后剩 %d 条, 期望 2（全部重发）", len(got))
	}
}
