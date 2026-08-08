package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// mDNS 的链路本地组播地址与端口（RFC 6762 §3）。
//
// AGENTS.md C4：我们自己在这两个地址上收发报文，不经过 avahi/Bonjour。
const mdnsPort = 5353

var (
	mdnsGroupIPv4 = net.IPv4(224, 0, 0, 251)
	mdnsGroupIPv6 = net.IP{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xfb} // ff02::fb
)

var (
	mdnsAddrIPv4 = &net.UDPAddr{IP: mdnsGroupIPv4, Port: mdnsPort}
	mdnsAddrIPv6 = &net.UDPAddr{IP: mdnsGroupIPv6, Port: mdnsPort}
)

const (
	// mdnsTTL 是 mDNS 报文必须使用的 IP TTL / hop limit（RFC 6762 §11）。
	// 255 同时用于接收侧校验：TTL 不是 255 的报文说明经过了路由器转发。
	mdnsTTL = 255
	// maxPacketSize 是 mDNS 报文长度上限（RFC 6762 §17）。
	maxPacketSize = 9000
)

// packet 是一个收到的 mDNS 报文及其上下文。
type packet struct {
	msg     *Message
	src     *net.UDPAddr
	ifIndex int  // 收包网卡索引，0 表示平台不支持获取
	v6      bool // 是否从 IPv6 socket 收到
	// multicast 表示报文的目的地址是组播地址。
	// 单播查询要按 RFC 6762 §5.5 处理（一律单播回应）。
	multicast bool
}

// conn 管理已加入 mDNS 组播组的 IPv4/IPv6 socket。
type conn struct {
	v4  *ipv4.PacketConn
	v6  *ipv6.PacketConn
	log *slog.Logger

	// ifaces 是成功加入了组播组的网卡（至少 v4/v6 之一成功）。
	ifaces []net.Interface

	// sendMu 保护 SetMulticastInterface + WriteTo 这一对操作。
	// 用 setsockopt 选出网卡再发送，比控制消息更可移植
	// （Windows 上 x/net 的控制消息支持不完整）。
	sendMu sync.Mutex

	closeOnce sync.Once
}

// openConn 创建 mDNS socket 并在指定网卡上加入组播组。
//
// ifaceNames 为空表示使用全部可用网卡。
func openConn(ifaceNames []string, log *slog.Logger) (*conn, error) {
	ifaces, err := usableInterfaces(ifaceNames)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("mdns: 没有找到可用于组播的网卡（需要 UP 且支持 MULTICAST）")
	}

	c := &conn{log: log}

	joined := make(map[string]bool, len(ifaces))

	if p4, err := listenIPv4(); err != nil {
		log.Warn("mDNS: IPv4 socket 创建失败，将只使用 IPv6", "err", err)
	} else {
		c.v4 = p4
		for i := range ifaces {
			if err := p4.JoinGroup(&ifaces[i], mdnsAddrIPv4); err != nil {
				log.Debug("mDNS: 加入 IPv4 组播组失败", "iface", ifaces[i].Name, "err", err)
				continue
			}
			joined[ifaces[i].Name] = true
		}
	}

	if p6, err := listenIPv6(); err != nil {
		log.Warn("mDNS: IPv6 socket 创建失败，将只使用 IPv4", "err", err)
	} else {
		c.v6 = p6
		for i := range ifaces {
			if err := p6.JoinGroup(&ifaces[i], mdnsAddrIPv6); err != nil {
				log.Debug("mDNS: 加入 IPv6 组播组失败", "iface", ifaces[i].Name, "err", err)
				continue
			}
			joined[ifaces[i].Name] = true
		}
	}

	for i := range ifaces {
		if joined[ifaces[i].Name] {
			c.ifaces = append(c.ifaces, ifaces[i])
		}
	}

	if len(c.ifaces) == 0 {
		c.Close()
		return nil, fmt.Errorf("mdns: 所有网卡都无法加入组播组（224.0.0.251 / ff02::fb）")
	}

	log.Info("mDNS: 已加入组播组", "interfaces", interfaceNames(c.ifaces),
		"ipv4", c.v4 != nil, "ipv6", c.v6 != nil)
	return c, nil
}

func interfaceNames(ifaces []net.Interface) []string {
	out := make([]string, len(ifaces))
	for i := range ifaces {
		out[i] = ifaces[i].Name
	}
	return out
}

func listenIPv4() (*ipv4.PacketConn, error) {
	lc := net.ListenConfig{Control: setReuse}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", mdnsPort))
	if err != nil {
		return nil, err
	}
	p := ipv4.NewPacketConn(pc)
	// RFC 6762 §11：mDNS 报文的 TTL 必须是 255。
	_ = p.SetMulticastTTL(mdnsTTL)
	_ = p.SetTTL(mdnsTTL)
	// 打开回环，让同一台机器上的其他进程（以及集成测试）也能收到我们的广播。
	_ = p.SetMulticastLoopback(true)
	// 控制消息用于拿到收包网卡与目的地址；部分平台不支持，失败不致命。
	if err := p.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst|ipv4.FlagTTL, true); err != nil {
		_ = p.SetControlMessage(ipv4.FlagInterface, true)
	}
	return p, nil
}

func listenIPv6() (*ipv6.PacketConn, error) {
	lc := net.ListenConfig{Control: setReuse}
	pc, err := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf(":%d", mdnsPort))
	if err != nil {
		return nil, err
	}
	p := ipv6.NewPacketConn(pc)
	_ = p.SetMulticastHopLimit(mdnsTTL)
	_ = p.SetHopLimit(mdnsTTL)
	_ = p.SetMulticastLoopback(true)
	if err := p.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst|ipv6.FlagHopLimit, true); err != nil {
		_ = p.SetControlMessage(ipv6.FlagInterface, true)
	}
	return p, nil
}

// usableInterfaces 返回可用于 mDNS 的网卡。
//
// names 为空时自动挑选：UP、支持组播、且不是回环。
// 显式指定网卡名时放宽回环限制（方便在单机环境里做集成测试）。
func usableInterfaces(names []string) ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("mdns: 枚举网卡失败: %w", err)
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
				return nil, fmt.Errorf("mdns: 配置里指定的网卡 %q 不存在", n)
			}
			if ifi.Flags&net.FlagUp == 0 {
				return nil, fmt.Errorf("mdns: 网卡 %q 当前不是 UP 状态", n)
			}
			if ifi.Flags&net.FlagMulticast == 0 {
				return nil, fmt.Errorf("mdns: 网卡 %q 不支持组播，无法用于 mDNS", n)
			}
			out = append(out, ifi)
		}
		return out, nil
	}

	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out, nil
}

// interfaceIPs 返回网卡上可用于 A/AAAA 记录的地址。
//
// Bonjour 会把链路本地地址（169.254/16、fe80::/10）也一并宣告，
// 因为 mDNS 的作用范围本来就是单条链路，这里保持一致行为。
func interfaceIPs(ifi *net.Interface) (v4, v6 []net.IP) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, nil
	}
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

// run 启动收包循环，每个收到的报文交给 handle。ctx 取消后退出。
func (c *conn) run(ctx context.Context, handle func(packet)) {
	var wg sync.WaitGroup
	if c.v4 != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.readIPv4(handle)
		}()
	}
	if c.v6 != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.readIPv6(handle)
		}()
	}

	<-ctx.Done()
	c.Close()
	wg.Wait()
}

func (c *conn) readIPv4(handle func(packet)) {
	buf := make([]byte, maxPacketSize)
	for {
		n, cm, src, err := c.v4.ReadFrom(buf)
		if err != nil {
			return // socket 已关闭或出错，退出循环
		}
		udp, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		p := packet{src: udp, multicast: true}
		if cm != nil {
			p.ifIndex = cm.IfIndex
			if cm.Dst != nil {
				p.multicast = cm.Dst.IsMulticast()
			}
			// RFC 6762 §11：TTL 不是 255 说明报文来自链路之外，直接丢弃。
			if cm.TTL != 0 && cm.TTL != mdnsTTL {
				continue
			}
		}
		c.dispatch(buf[:n], p, handle)
	}
}

func (c *conn) readIPv6(handle func(packet)) {
	buf := make([]byte, maxPacketSize)
	for {
		n, cm, src, err := c.v6.ReadFrom(buf)
		if err != nil {
			return
		}
		udp, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		p := packet{src: udp, v6: true, multicast: true}
		if cm != nil {
			p.ifIndex = cm.IfIndex
			if cm.Dst != nil {
				p.multicast = cm.Dst.IsMulticast()
			}
			if cm.HopLimit != 0 && cm.HopLimit != mdnsTTL {
				continue
			}
		}
		c.dispatch(buf[:n], p, handle)
	}
}

func (c *conn) dispatch(b []byte, p packet, handle func(packet)) {
	msg, err := Unpack(b)
	if err != nil {
		// 畸形报文只记 debug：链路上什么设备都可能存在，不能因此吵闹或崩溃。
		c.log.Debug("mDNS: 丢弃畸形报文", "src", p.src, "err", err)
		return
	}
	p.msg = msg
	handle(p)
}

// sendMulticast 把报文发到所有已加入组播组的网卡。
//
// ifIndex 非 0 时只发这一张网卡（回应查询时要沿原路返回，RFC 6762 §6）。
func (c *conn) sendMulticast(m *Message, ifIndex int) {
	b, err := m.Pack()
	if err != nil {
		c.log.Error("mDNS: 报文编码失败", "err", err)
		return
	}
	for i := range c.ifaces {
		ifi := &c.ifaces[i]
		if ifIndex != 0 && ifi.Index != ifIndex {
			continue
		}
		c.writeMulticast(b, ifi)
	}
}

func (c *conn) writeMulticast(b []byte, ifi *net.Interface) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if c.v4 != nil {
		if err := c.v4.SetMulticastInterface(ifi); err == nil {
			if _, err := c.v4.WriteTo(b, nil, mdnsAddrIPv4); err != nil {
				c.log.Debug("mDNS: IPv4 组播发送失败", "iface", ifi.Name, "err", err)
			}
		}
	}
	if c.v6 != nil {
		if err := c.v6.SetMulticastInterface(ifi); err == nil {
			// IPv6 组播必须带 zone，否则内核不知道从哪张网卡出去。
			dst := &net.UDPAddr{IP: mdnsGroupIPv6, Port: mdnsPort, Zone: ifi.Name}
			if _, err := c.v6.WriteTo(b, nil, dst); err != nil {
				c.log.Debug("mDNS: IPv6 组播发送失败", "iface", ifi.Name, "err", err)
			}
		}
	}
}

// sendUnicast 单播回应（RFC 6762 §5.4 的 QU、§5.5 的非 5353 源端口）。
func (c *conn) sendUnicast(m *Message, dst *net.UDPAddr, v6 bool) {
	b, err := m.Pack()
	if err != nil {
		c.log.Error("mDNS: 报文编码失败", "err", err)
		return
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if v6 {
		if c.v6 == nil {
			return
		}
		if _, err := c.v6.WriteTo(b, nil, dst); err != nil {
			c.log.Debug("mDNS: IPv6 单播发送失败", "dst", dst, "err", err)
		}
		return
	}
	if c.v4 == nil {
		return
	}
	if _, err := c.v4.WriteTo(b, nil, dst); err != nil {
		c.log.Debug("mDNS: IPv4 单播发送失败", "dst", dst, "err", err)
	}
}

// Close 关闭全部 socket。可重复调用。
func (c *conn) Close() {
	c.closeOnce.Do(func() {
		if c.v4 != nil {
			_ = c.v4.Close()
		}
		if c.v6 != nil {
			_ = c.v6.Close()
		}
	})
}
