package netiface

import (
	"net"
	"testing"
)

// 本文件钉的是地址判定的口径。三个发现组件（mDNS / WS-Discovery / NetBIOS）
// 共用这一份逻辑，所以它漂了会同时影响三处，表现为"某个客户端能发现、
// 另一个发现不了"—— 那类问题极难定位，必须在这里钉死。
//
// 变异自检方向：
//   - 把 AddrsFrom 里的 IsLoopback 过滤去掉 → TestAddrsFromFilters 变红；
//   - 把 BroadcastsFrom 的 /32 跳过删掉 → TestBroadcastsFromSkipsPointToPoint 变红
//     （/32 的广播地址算出来就是自己，发到那里等于发给自己）；
//   - 把掩码取反按位或改成直接赋值 → TestBroadcastsFrom 变红。

// ipnet 按 "主机地址/前缀" 造一个 *net.IPNet。
//
// ⚠️ 必须显式把 IP 换回主机地址：net.ParseCIDr 返回的 IPNet.IP 是**网络号**
// （"192.168.1.10/24" 会给你 192.168.1.0），而真实网卡的 Addrs() 给的是
// 主机地址。直接用 ParseCIDR 的返回值，测的就不是代码而是这个夹具的坑。
func ipnet(s string) *net.IPNet {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

func TestAddrsFromFilters(t *testing.T) {
	addrs := []net.Addr{
		ipnet("192.168.1.10/24"),        // 保留：常规内网地址
		ipnet("169.254.33.7/16"),        // 保留：链路本地（mDNS 的作用范围就是单链路）
		ipnet("fe80::1/64"),             // 保留：IPv6 链路本地
		ipnet("127.0.0.1/8"),            // 丢弃：loopback
		&net.IPAddr{IP: net.IPv4zero},   // 丢弃：未指定地址
		&net.IPAddr{IP: net.IPv4allsys}, // 丢弃：组播地址
	}

	v4, v6 := AddrsFrom(addrs)

	if len(v4) != 2 {
		t.Fatalf("v4 应保留 2 个（192.168.1.10 与 169.254.33.7），实际 %d：%v", len(v4), v4)
	}
	if !v4[0].Equal(net.IPv4(192, 168, 1, 10)) || !v4[1].Equal(net.IPv4(169, 254, 33, 7)) {
		t.Fatalf("v4 内容不符：%v", v4)
	}
	if len(v6) != 1 {
		t.Fatalf("v6 应保留 1 个，实际 %d：%v", len(v6), v6)
	}
}

// TestAddrsFromSeparatesIPv4AndIPv6：IPv4 必须走 v4 而不是被当成 v6
// （net.IP 的 16 字节表示会让 To4/To16 判定出错，这里钉住分流的顺序）。
func TestAddrsFromSeparatesIPv4AndIPv6(t *testing.T) {
	v4, v6 := AddrsFrom([]net.Addr{ipnet("10.0.0.5/8"), ipnet("2001:db8::5/64")})
	if len(v4) != 1 || len(v6) != 1 {
		t.Fatalf("应各得 1 个，实际 v4=%d v6=%d", len(v4), len(v6))
	}
	if !v4[0].Equal(net.IPv4(10, 0, 0, 5)) {
		t.Fatalf("v4 应为 10.0.0.5，实际 %v", v4[0])
	}
}

func TestBroadcastsFrom(t *testing.T) {
	got := BroadcastsFrom([]net.Addr{
		ipnet("192.168.1.10/24"),
		ipnet("10.20.30.40/16"),
	})
	if len(got) != 2 {
		t.Fatalf("应算出 2 个广播地址，实际 %d：%v", len(got), got)
	}
	if !got[0].Equal(net.IPv4(192, 168, 1, 255)) {
		t.Fatalf("192.168.1.10/24 的广播地址应为 192.168.1.255，实际 %v", got[0])
	}
	if !got[1].Equal(net.IPv4(10, 20, 255, 255)) {
		t.Fatalf("10.20.30.40/16 的广播地址应为 10.20.255.255，实际 %v", got[1])
	}
}

// TestBroadcastsFromSkipsPointToPoint：/32 没有广播地址。
//
// 掩码全 1 时"广播地址"就是主机地址自己 —— 往那儿发等于发给自己，
// NetBIOS 宣告会变成一个自我循环，不该发。
func TestBroadcastsFromSkipsPointToPoint(t *testing.T) {
	got := BroadcastsFrom([]net.Addr{ipnet("203.0.113.5/32")})
	if len(got) != 0 {
		t.Fatalf("/32 不应产生广播地址，实际 %v", got)
	}
}

// TestBroadcastsFromIgnoresIPv6AndLoopback：NetBIOS 根本没有 IPv6 广播的概念
// （IPv6 用组播取代广播，报文格式里也没有放 IPv6 的位置）。
func TestBroadcastsFromIgnoresIPv6AndLoopback(t *testing.T) {
	got := BroadcastsFrom([]net.Addr{
		ipnet("2001:db8::5/64"),
		ipnet("127.0.0.1/8"),
	})
	if len(got) != 0 {
		t.Fatalf("IPv6 与 loopback 都不应产生广播地址，实际 %v", got)
	}
}

// TestSelectRejectsUnknownInterface：显式点名不存在的网卡是配置错误，
// 必须报错而不是静默跳过 —— 静默跳过会让"配了却不生效"无从排查。
func TestSelectRejectsUnknownInterface(t *testing.T) {
	_, err := Select([]string{"definitely-no-such-iface-42"}, NeedMulticast)
	if err == nil {
		t.Fatal("点名不存在的网卡应当报错")
	}
}

// TestSelectOnRealInterfaces：在有网卡的环境里自检一遍不报错，
// 并且**不返回 loopback**（自动挑选模式下）。
func TestSelectOnRealInterfaces(t *testing.T) {
	ifaces, err := Select(nil, NeedMulticast)
	if err != nil {
		t.Fatalf("Select 失败：%v", err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			t.Fatalf("自动挑选模式不应返回 loopback 网卡 %q", ifi.Name)
		}
		if ifi.Flags&net.FlagMulticast == 0 {
			t.Fatalf("要求 NeedMulticast 却返回了不支持组播的网卡 %q", ifi.Name)
		}
	}
}
