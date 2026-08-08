//go:build integration

// 端到端验证：真的在 224.0.0.251:5353 上收发报文（AGENTS.md C4）。
//
// 需要能加入组播组的运行环境，因此用 integration build tag 隔离：
//
//	go test -tags integration ./internal/mdns/ -run TestResponder -v
package mdns

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/finalappstore/stupidsamba/internal/config"
)

// TestResponderAnswersLegacyQuery 用一个源端口 ≠5353 的普通 UDP socket 发查询，
// 按 RFC 6762 §5.5 我们必须**单播**回应，于是不需要客户端也加入组播组。
func TestResponderAnswersLegacyQuery(t *testing.T) {
	const instance = "SSITEST"

	r, err := New(config.MDNS{
		Enabled:  true,
		Instance: instance,
		Apple: config.AppleMDNS{
			Enabled:              true,
			Model:                "TimeCapsule8,119",
			AdvertiseTimeMachine: true,
		},
	}, 4445, []config.Share{
		{Name: "data"},
		{Name: "backup", TimeMachine: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := r.Start(context.Background()); err != nil {
		t.Skipf("跳过：当前环境无法加入 mDNS 组播组: %v", err)
	}
	defer r.Stop()

	// 等探测 + 首次宣告结束，此时才会应答查询。
	deadline := time.Now().Add(5 * time.Second)
	for r.currentState() != stateResponding {
		if time.Now().After(deadline) {
			t.Fatal("responder 迟迟没有进入应答状态")
		}
		time.Sleep(50 * time.Millisecond)
	}

	pc := newQuerySocket(t)
	defer pc.Close()

	q := &Message{Questions: []Question{
		{Name: serviceTypeSMB + "." + localDomain, Type: TypePTR, Class: ClassIN},
	}}
	raw, err := q.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := pc.WriteToUDP(raw, &net.UDPAddr{IP: mdnsGroupIPv4, Port: mdnsPort}); err != nil {
		t.Skipf("跳过：发不出组播报文: %v", err)
	}

	// 收集回应里的记录，直到超时。
	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, maxPacketSize)

	var got []Record
	for {
		n, _, err := pc.ReadFromUDP(buf)
		if err != nil {
			break
		}
		m, err := Unpack(buf[:n])
		if err != nil || !m.Flags.IsResponse() {
			continue
		}
		got = append(got, m.Answers...)
		got = append(got, m.Additionals...)
		if hasType(got, TypePTR) && hasType(got, TypeSRV) && hasType(got, TypeTXT) && hasType(got, TypeA) {
			break
		}
	}

	if len(got) == 0 {
		t.Fatal("没有收到任何回应")
	}

	wantInstance := instance + "." + serviceTypeSMB + "." + localDomain
	var ptr, srv, txt, a bool
	for _, rec := range got {
		switch d := rec.Data.(type) {
		case PTR:
			if EqualName(rec.Name, serviceTypeSMB+"."+localDomain) && EqualName(d.Target, wantInstance) {
				ptr = true
			}
		case SRV:
			if EqualName(rec.Name, wantInstance) {
				if d.Port != 4445 {
					t.Errorf("SRV 端口 = %d, 期望 4445", d.Port)
				}
				srv = true
			}
		case TXT:
			if EqualName(rec.Name, wantInstance) {
				txt = true
			}
		case A:
			a = true
		}
		// RFC 6762 §6.7：传统单播回应的 TTL 必须压到 10 秒以内。
		if rec.TTL > legacyTTL {
			t.Errorf("记录 %s 的 TTL=%d 超过传统单播上限 %d", rec.Name, rec.TTL, legacyTTL)
		}
		// §6.7：传统客户端不认识 cache-flush 位。
		if rec.CacheFlush {
			t.Errorf("记录 %s 对传统单播查询带了 cache-flush 位", rec.Name)
		}
	}

	if !ptr {
		t.Errorf("回应里缺少指向 %s 的 PTR", wantInstance)
	}
	if !srv {
		t.Errorf("回应里缺少 %s 的 SRV", wantInstance)
	}
	if !txt {
		t.Errorf("回应里缺少 %s 的 TXT", wantInstance)
	}
	if !a {
		t.Errorf("回应里缺少 A 记录（附加段应当带上地址）")
	}
}

// TestResponderMetaQuery 验证 DNS-SD 服务类型枚举（RFC 6763 §9）：
// 查询 _services._dns-sd._udp.local. 应当列出我们提供的所有服务类型。
func TestResponderMetaQuery(t *testing.T) {
	r, err := New(config.MDNS{
		Enabled:  true,
		Instance: "SSMETA",
		Apple:    config.AppleMDNS{Enabled: true, Model: "MacSamba"},
	}, 4446, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := r.Start(context.Background()); err != nil {
		t.Skipf("跳过：当前环境无法加入 mDNS 组播组: %v", err)
	}
	defer r.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for r.currentState() != stateResponding {
		if time.Now().After(deadline) {
			t.Fatal("responder 迟迟没有进入应答状态")
		}
		time.Sleep(50 * time.Millisecond)
	}

	pc := newQuerySocket(t)
	defer pc.Close()

	q := &Message{Questions: []Question{{Name: metaQueryName, Type: TypePTR, Class: ClassIN}}}
	raw, err := q.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := pc.WriteToUDP(raw, &net.UDPAddr{IP: mdnsGroupIPv4, Port: mdnsPort}); err != nil {
		t.Skipf("跳过：发不出组播报文: %v", err)
	}

	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, maxPacketSize)

	types := map[string]bool{}
	for {
		n, _, err := pc.ReadFromUDP(buf)
		if err != nil {
			break
		}
		m, err := Unpack(buf[:n])
		if err != nil || !m.Flags.IsResponse() {
			continue
		}
		for _, rec := range m.Answers {
			if !EqualName(rec.Name, metaQueryName) {
				continue
			}
			if d, ok := rec.Data.(PTR); ok {
				types[strings.ToLower(d.Target)] = true
			}
		}
		if len(types) >= 2 {
			break
		}
	}

	for _, want := range []string{serviceTypeSMB, serviceTypeDeviceInfo} {
		if !types[strings.ToLower(want+"."+localDomain)] {
			t.Errorf("服务类型枚举里缺少 %s（收到 %v）", want, keysOf(types))
		}
	}
}

// newQuerySocket 建一个普通（未加入组播组）的 UDP socket 用来发查询。
//
// 关键：组播 TTL 必须设成 255。RFC 6762 §11 要求响应者丢弃 TTL≠255 的报文，
// 而 Go 的默认组播 TTL 是 1 —— 忘了设的话我们自己的 responder 会正确地
// 把测试报文丢掉，表现为"没有收到任何回应"。
func newQuerySocket(t *testing.T) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("客户端 socket: %v", err)
	}
	p := ipv4.NewPacketConn(pc)
	if err := p.SetMulticastTTL(mdnsTTL); err != nil {
		pc.Close()
		t.Fatalf("设置组播 TTL: %v", err)
	}
	_ = p.SetMulticastLoopback(true)
	return pc
}

func hasType(recs []Record, t Type) bool {
	for _, r := range recs {
		if r.Type() == t {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
