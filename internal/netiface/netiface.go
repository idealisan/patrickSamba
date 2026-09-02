// Package netiface 回答一个所有服务发现组件都要问的问题：
// **本机有哪些网卡可以用来广播/组播，它们上面有哪些地址。**
//
// 它是刻意做成**叶子包**的：只依赖标准库，不 import config，更不 import
// 任何协议实现（mdns / wsd / nbns）。依赖方向永远单向：
//
//	mdns ─┐
//	wsd   ├─→ netiface
//	nbns ─┘
//
// 三个发现组件**互不相识**。要不是有这个包，网卡枚举与地址筛选的逻辑就会
// 在各组件里各写一遍，然后各自漂移（比如一个跳过 loopback、另一个不跳），
// 表现是"同一个服务在 mDNS 里能发现、在 WS-Discovery 里发现不了"。
// 判定口径只留一份，是所有组件行为一致的前提。
package netiface

import (
	"fmt"
	"net"
)

// Feature 是网卡需要具备的能力位（可组合）。
type Feature uint

const (
	// NeedMulticast 要求网卡支持组播（mDNS 与 WS-Discovery 都需要）。
	NeedMulticast Feature = 1 << iota
	// NeedBroadcast 要求网卡支持广播（NetBIOS 的 137/138 宣告需要）。
	NeedBroadcast
)

// Select 返回可用于服务发现的网卡。
//
// names 留空表示"所有符合条件的网卡"，此时跳过 DOWN 的与 loopback 的；
// names 非空时按名字精确挑选，**不**跳过 loopback —— 显式点名 loopback
// 是运维的有意选择（容器里可能只有 lo），此时应当照做，只是通常拿不到
// 可用地址。
//
// 显式点名时，网卡缺失 / 未 UP / 不具备所需能力都算**配置错误**，直接返回
// 错误而不是静默跳过：静默跳过会让"配了却不生效"变成最难查的那类问题。
func Select(names []string, want Feature) ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("枚举网卡失败: %w", err)
	}

	if len(names) > 0 {
		byName := make(map[string]net.Interface, len(all))
		for _, ifi := range all {
			byName[ifi.Name] = ifi
		}
		out := make([]net.Interface, 0, len(names))
		for _, n := range names {
			ifi, ok := byName[n]
			if !ok {
				return nil, fmt.Errorf("配置里指定的网卡 %q 不存在", n)
			}
			if ifi.Flags&net.FlagUp == 0 {
				return nil, fmt.Errorf("网卡 %q 当前不是 UP 状态", n)
			}
			if want&NeedMulticast != 0 && ifi.Flags&net.FlagMulticast == 0 {
				return nil, fmt.Errorf("网卡 %q 不支持组播", n)
			}
			if want&NeedBroadcast != 0 && ifi.Flags&net.FlagBroadcast == 0 {
				return nil, fmt.Errorf("网卡 %q 不支持广播", n)
			}
			out = append(out, ifi)
		}
		return out, nil
	}

	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if want&NeedMulticast != 0 && ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if want&NeedBroadcast != 0 && ifi.Flags&net.FlagBroadcast == 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out, nil
}

// Addrs 返回网卡上可用于对外宣告的地址（IPv4 / IPv6 分开）。
//
// 链路本地地址（169.254/16、fe80::/10）**保留**：mDNS 的作用范围本来就是
// 单条链路，Bonjour 也会把它们一并宣告。WS-Discovery 与 NetBIOS 同理 ——
// 没有 DHCP 的直连场景下它们是唯一可用的地址。
//
// 过滤掉 loopback / 未指定 / 组播地址：宣告这些出去只会污染客户端的列表。
func Addrs(ifi net.Interface) (v4, v6 []net.IP) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, nil
	}
	return AddrsFrom(addrs)
}

// AddrsFrom 是 Addrs 的纯函数版本：只做地址筛选，不碰网卡对象。
//
// 拆出来的理由是**可测**：net.Interface 造不出假货（Addrs() 会真的去问内核），
// 而 []net.Addr 可以随便构造。把判定逻辑压在这层，网卡相关的一切就都能
// 在没有真实网卡的 CI 容器里单测了 —— 本项目的开发容器正是这种环境。
func AddrsFrom(addrs []net.Addr) (v4, v6 []net.IP) {
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			v4 = append(v4, ip4)
		} else if ip16 := ip.To16(); ip16 != nil {
			v6 = append(v6, ip16)
		}
	}
	return v4, v6
}

// Broadcasts 返回网卡上各 IPv4 子网的广播地址，供 NetBIOS 的
// 138 端口主机宣告使用。
//
// 只处理 IPv4：NetBIOS over TCP/IP 根本没有 IPv6 的广播概念
// （IPv6 用组播取代广播，而 NetBIOS 的报文格式里也没有 IPv6 的位置）。
func Broadcasts(ifi net.Interface) []net.IP {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	return BroadcastsFrom(addrs)
}

// BroadcastsFrom 是 Broadcasts 的纯函数版本，理由同 AddrsFrom。
func BroadcastsFrom(addrs []net.Addr) []net.IP {
	var out []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() {
			continue
		}
		// 用掩码算出子网广播地址：ones 是网络位长度，剩下的是主机位。
		// /32（点对点）没有广播地址，掩码全 1 时直接跳过。
		ones, bits := ipnet.Mask.Size()
		if bits == 0 || ones >= bits {
			continue
		}
		bcast := make(net.IP, len(ip4))
		copy(bcast, ip4)
		for i := 0; i < len(bcast); i++ {
			bcast[i] |= ^ipnet.Mask[i]
		}
		out = append(out, bcast)
	}
	return out
}
