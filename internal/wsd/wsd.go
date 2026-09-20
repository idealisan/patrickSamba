// Package wsd 实现进程内的 WS-Discovery 响应器。
//
// 目的：让 Windows 的「网络」（Network Neighborhood）能自动发现本机。
// 客户端往组播地址发一条 `Probe`，我们回一条 `ProbeMatch` 把自己报上去。
//
// 约束（AGENTS.md）：
//   - C3：不 fork/exec 任何外部进程。社区里常见的做法是跑一个 `wsdd`
//     Python 守护进程，本包把它要做的事自己实现掉了。
//   - C4：自己在 239.255.255.250:3702 上收发报文，不依赖系统服务。
//   - C1/C2：禁 CGO，静态链接。因此用 golang.org/x/net/{ipv4,ipv6}
//     与 golang.org/x/sys 做组播与地址复用（系统调用号封装，非 CGO）。
//
// 依赖方向（低耦合）：本包只依赖标准库 + 两个叶子包
// （internal/netiface 网卡选择、internal/sockopt 地址复用），
// **不** import internal/config，也不 import internal/mdns。
// 配置由 cmd 层映射进 Options，本包因此可以脱离配置文件单独测试。
package wsd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/idealisan/patrickSamba/internal/netiface"
	"github.com/idealisan/patrickSamba/internal/sockopt"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	// Port 是 WS-Discovery 的 UDP 端口（WS-Discovery §2.4）。
	Port = 3702

	// DefaultMetadataPort 是 WS-Transfer Get 的 HTTP 端点端口。
	//
	// 5357 是 Windows 的「Function Discovery」约定端口，wsdd 也用它。
	DefaultMetadataPort = 5357

	// maxResponseDelay 是响应前的随机延迟上限（WS-Discovery §5.2 的
	// APP_MAX_DELAY）。不加这个延迟，局域网里所有设备会在收到同一条
	// Probe 的同一瞬间一起回包，撞在一起丢一片。
	maxResponseDelay = 500 * time.Millisecond

	// maxDatagramSize 是 UDP 报文长度上限。WS-Discovery 没规定，
	// 取一个远大于典型 Probe 报文的值，够用且不会撑爆内存。
	maxDatagramSize = 65535

	// metadataVersion 是元数据版本号。本服务不变更元数据，
	// 恒为 1（客户端只在版本变化时重新拉取）。
	metadataVersion = 1
)

var (
	// groupIPv4 是 WS-Discovery 的 IPv4 组播组（WS-Discovery §2.4）。
	groupIPv4 = net.IPv4(239, 255, 255, 250)
	// groupIPv6 是 IPv6 的链路本地组播组 ff02::c。
	groupIPv6 = net.ParseIP("ff02::c")
)

// ErrDisabled 表示 WS-Discovery 没有启用。
//
// 两种情形都用它，调用方按 err 的**包装文本**区分：
//   - 配置里关掉了（`enabled: false`）→ 直接返回本哨兵，属预期，记 INFO；
//   - 环境不具备开不起来（如没有可用组播网卡）→ 包装本哨兵并带上原因，
//     属降级，记 WARN。
var ErrDisabled = errors.New("wsd: 未启用")

// Options 是 WS-Discovery 的自包含配置。
//
// 刻意**不**直接吃 internal/config.Config：那样协议实现就被绑在配置文件
// 的结构上了（改一个字段名要动协议包）。由 cmd 层做一次映射，本包只认
// 自己需要的这几个字段，测试也不需要构造一份完整配置。
type Options struct {
	// Enabled 开关。
	Enabled bool
	// Interfaces 限定网卡名，留空表示所有支持组播的网卡。
	Interfaces []string
	// Name 是本机名（NetBIOS 名）。用于派生稳定的设备 UUID 与日志。
	Name string
	// UUID 是设备标识；留空则按 Name 派生一个稳定的 UUIDv5。
	//
	// 显式指定主要用于多实例场景：同一台机器上跑多个实例时，
	// 派生值会撞（同名字），需要各自钉一个。
	UUID string
	// MetadataPort 是 WS-Transfer Get 的 HTTP 端口。
	// 0 表示 DefaultMetadataPort；负数表示不启用元数据端点
	// （只应答 Probe/Resolve，Windows 仍能列出主机，只是点开取不到详情）。
	MetadataPort int
}

// Responder 是 WS-Discovery 响应器。
//
// 生命周期与 mdns.Responder 同形（New → SetLogger → Start → Stop），
// 这样 cmd 层可以用同一个接口驱动全部发现组件，而它们彼此并不知道
// 对方存在。
type Responder struct {
	opts     Options
	log      *slog.Logger
	dev      deviceInfo
	ifaces   []net.Interface
	metaPort int
	meta     *metadataServer

	conn4 *ipv4.PacketConn
	conn6 *ipv6.PacketConn

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New 构造响应器。未启用时返回 ErrDisabled。
func New(o Options) (*Responder, error) {
	if !o.Enabled {
		return nil, ErrDisabled
	}
	ifaces, err := netiface.Select(o.Interfaces, netiface.NeedMulticast)
	if err != nil {
		return nil, fmt.Errorf("wsd: %w", err)
	}
	// 一张可用网卡都没有 = 这套协议在当前环境里起不到作用。
	// 此时**不启动**（返回 ErrDisabled）而不是照常 bind：那会白白占住
	// 3702/5357 两个端口，却一条报文也收不到、一个宣告也发不出去。
	// 按"开不了就降级继续"的原则，调用方记一条 WARN 后继续跑其他功能。
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("%w: 没有可用的组播网卡", ErrDisabled)
	}

	uuid := o.UUID
	if uuid == "" {
		uuid = DeriveUUID(o.Name)
	}
	metaPort := o.MetadataPort
	if metaPort == 0 {
		metaPort = DefaultMetadataPort
	}

	return &Responder{
		opts:     o,
		log:      slog.Default(),
		ifaces:   ifaces,
		metaPort: metaPort,
		dev: deviceInfo{
			UUID:            uuid,
			MetadataVersion: metadataVersion,
			FriendlyName:    friendlyName(o.Name),
		},
	}, nil
}

// friendlyName 生成元数据里的显示名。
//
// 名字为空时给一个固定串而不是留空：Windows 对空的 FriendlyName 会显示
// 成一串 UUID，那比「stupidSamba」难认得多。
func friendlyName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "stupidSamba"
	}
	return name
}

// SetLogger 注入日志句柄。
func (r *Responder) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l.With("component", "wsd")
	}
}

// Start 启动组播接收与（可选的）元数据端点。
//
// 刻意传 Background 而不是信号 ctx：ctx 取消会让响应器直接停摆，
// 收尾统一由 Stop 负责（与 mdns 同样的取舍）。
func (r *Responder) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	if err := r.openSockets(); err != nil {
		cancel()
		return err
	}

	if r.metaPort > 0 {
		ms, err := newMetadataServer(r.metaPort, r.dev)
		if err != nil {
			r.log.Warn("WS-Discovery 元数据端点启动失败，主机仍可被发现但点开取不到详情",
				"port", r.metaPort, "err", err)
		} else {
			ms.SetLogger(r.log)
			r.meta = ms
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				_ = ms.Serve(ctx)
			}()
		}
	}

	if r.conn4 != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.readLoop4(ctx)
		}()
	}
	if r.conn6 != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.readLoop6(ctx)
		}()
	}

	r.log.Info("WS-Discovery 已启动",
		"uuid", r.dev.UUID,
		"port", Port,
		"metadata_port", r.metaPort,
		"ifaces", ifaceNames(r.ifaces))
	return nil
}

// Stop 停止响应器并释放套接字。可重复调用。
func (r *Responder) Stop() {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		// 先关套接字：读循环正阻塞在 ReadFrom 上，只 cancel 不会唤醒它
		// （UDP 读不响应 ctx 取消）。
		if r.conn4 != nil {
			_ = r.conn4.Close()
		}
		if r.conn6 != nil {
			_ = r.conn6.Close()
		}
		r.wg.Wait()
		r.log.Debug("WS-Discovery 已停止")
	})
}

// openSockets 建立 IPv4 / IPv6 的组播接收套接字。
//
// 两个地址族各开一个，任一个失败都不致命（很多环境只有 IPv4），
// 但**两个都失败就是错误**，必须让调用方知道服务发现没起来。
func (r *Responder) openSockets() error {
	lc := sockopt.ListenConfig()

	var errs []error
	pc4, err4 := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", Port))
	if err4 == nil {
		p := ipv4.NewPacketConn(pc4)
		_ = p.SetMulticastLoopback(true)
		if err := p.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
			// 控制消息拿不到就退化成「不知道收包网卡」，XAddrs 用首选地址。
			r.log.Debug("IPv4 控制消息不可用，XAddrs 将退化为首选地址", "err", err)
		}
		for i := range r.ifaces {
			if err := p.JoinGroup(&r.ifaces[i], &net.UDPAddr{IP: groupIPv4}); err != nil {
				r.log.Debug("加入 IPv4 组播组失败", "iface", r.ifaces[i].Name, "err", err)
			}
		}
		r.conn4 = p
	} else {
		errs = append(errs, err4)
	}

	pc6, err6 := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf(":%d", Port))
	if err6 == nil {
		p := ipv6.NewPacketConn(pc6)
		_ = p.SetMulticastLoopback(true)
		if err := p.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst, true); err != nil {
			r.log.Debug("IPv6 控制消息不可用", "err", err)
		}
		for i := range r.ifaces {
			if err := p.JoinGroup(&r.ifaces[i], &net.UDPAddr{IP: groupIPv6}); err != nil {
				r.log.Debug("加入 IPv6 组播组失败", "iface", r.ifaces[i].Name, "err", err)
			}
		}
		r.conn6 = p
	} else {
		errs = append(errs, err6)
	}

	if len(errs) == 2 {
		return fmt.Errorf("wsd: IPv4 与 IPv6 组播套接字都起不来（v4: %v; v6: %v）", err4, err6)
	}
	return nil
}

func (r *Responder) readLoop4(ctx context.Context) {
	buf := make([]byte, maxDatagramSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, cm, src, err := r.conn4.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 单个读失败不该停掉整个循环（端口上有别的程序在发怪包是常态）。
			continue
		}
		idx := -1
		if cm != nil {
			idx = cm.IfIndex
		}
		r.handle(buf[:n], src, idx)
	}
}

func (r *Responder) readLoop6(ctx context.Context) {
	buf := make([]byte, maxDatagramSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, cm, src, err := r.conn6.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		idx := -1
		if cm != nil {
			idx = cm.IfIndex
		}
		r.handle(buf[:n], src, idx)
	}
}

// handle 处理一条请求并在匹配时安排应答。
//
// 匹配判定（WS-Discovery §5.2）：
//   - Probe：Types 为空或命中本机类型 → 回 ProbeMatch；
//   - Resolve：请求里的端点地址等于本机 UUID → 回 ResolveMatch；
//   - 其余（Hello / Bye / 未知）：忽略。这些是别的设备的宣告，
//     我们不做主动发现，收到了也没有可记录的状态。
func (r *Responder) handle(b []byte, src net.Addr, ifIndex int) {
	req, err := parseRequest(b)
	if err != nil {
		// 3702 端口上什么都可能发过来（其它 WSD 实现、扫描器），
		// 解析失败是常态，记 debug 即可，不能刷屏。
		r.log.Debug("忽略无法解析的 WS-Discovery 报文", "from", src, "err", err)
		return
	}

	var action string
	switch req.action {
	case ActionProbe:
		if !r.dev.matches(req.types) {
			return
		}
		action = ActionProbeMatch
	case ActionResolve:
		if !r.dev.isAddress(req.resolveAddress) {
			return
		}
		action = ActionResolveMatch
	default:
		return
	}

	// 随机延迟：让同一条 Probe 的多个应答错开（APP_MAX_DELAY）。
	//
	// 每条请求起一个 goroutine 睡一觉再回，而不是在接收侧排队 ——
	// 排队会让后到的请求等前面那个的延迟，等于把随机延迟变成了串行延迟。
	go r.replyAfterDelay(action, req.messageID, src, ifIndex)
}

// replyAfterDelay 睡一个随机时长后发出应答。
//
// 应答发往请求的源地址（单播）而不是组播组：WS-Discovery §5.2 规定
// 对组播请求的应答应当单播回源地址，这样网络上就只有一条响应，
// 不会给所有主机都刷一遍。
func (r *Responder) replyAfterDelay(action, relatesTo string, src net.Addr, ifIndex int) {
	time.Sleep(time.Duration(rand.Int63n(int64(maxResponseDelay))))

	r.dev.XAddrs = r.xaddrs(ifIndex)
	body, err := buildMatch(action, relatesTo, r.dev)
	if err != nil {
		r.log.Warn("构造 WS-Discovery 应答失败", "err", err)
		return
	}
	if err := r.writeUnicast(body, src); err != nil {
		r.log.Debug("发送 WS-Discovery 应答失败", "to", src, "err", err)
		return
	}
	r.log.Debug("已应答 WS-Discovery", "action", action, "to", src)
}

// writeUnicast 把应答写到源地址。
func (r *Responder) writeUnicast(body []byte, dst net.Addr) error {
	udp, ok := dst.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("源地址不是 UDP: %T", dst)
	}
	if udp.IP.To4() != nil {
		if r.conn4 == nil {
			return errors.New("没有 IPv4 套接字")
		}
		_, err := r.conn4.WriteTo(body, nil, udp)
		return err
	}
	if r.conn6 == nil {
		return errors.New("没有 IPv6 套接字")
	}
	_, err := r.conn6.WriteTo(body, nil, udp)
	return err
}

// xaddrs 生成本机的元数据端点 URL 列表。
//
// ifIndex 是收包网卡：应答里应当给出**那张网卡**上的地址，
// 否则客户端可能拿到一个从它自己这条链路上不可达的地址。
// 拿不到 ifIndex 时退化为所有网卡上的地址全列 —— 宁可多给几个
// 让客户端挑，也不要给一个不可达的。
func (r *Responder) xaddrs(ifIndex int) string {
	if r.metaPort <= 0 {
		// 没有元数据端点时 XAddrs 只能留空。WS-Discovery 允许为空，
		// 客户端靠 ProbeMatch 本身也能把主机列出来。
		return ""
	}

	var ips []net.IP
	if ifIndex > 0 {
		if ifi, err := net.InterfaceByIndex(ifIndex); err == nil {
			v4, v6 := netiface.Addrs(*ifi)
			ips = append(ips, v4...)
			ips = append(ips, v6...)
		}
	}
	if len(ips) == 0 {
		for _, ifi := range r.ifaces {
			v4, v6 := netiface.Addrs(ifi)
			ips = append(ips, v4...)
			ips = append(ips, v6...)
		}
	}
	if len(ips) == 0 {
		return ""
	}

	// 每个地址一个 URL，空格分隔（wsd:XAddrs 是 xs:anyURI 列表）。
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		host := ip.String()
		// IPv6 在 URL 里必须加方括号，否则端口会被当成分段的一部分。
		if ip.To4() == nil {
			host = "[" + host + "]"
		}
		out = append(out, fmt.Sprintf("http://%s:%d/%s", host, r.metaPort, r.dev.UUID))
	}
	return strings.Join(out, " ")
}

// isAddress 报告 addr 是否指向本设备。
func (d deviceInfo) isAddress(addr string) bool {
	return addr == "urn:uuid:"+d.UUID
}

func ifaceNames(ifaces []net.Interface) []string {
	out := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		out = append(out, ifi.Name)
	}
	return out
}
