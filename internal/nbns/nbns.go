// Package nbns 实现进程内的 NetBIOS 名称服务（nmbd 的核心子集）。
//
// 目的：让 `\\NAME` 能被解析，并让本机出现在 Windows 的「网上邻居」/
// 「网络」与 macOS 的 SMB 浏览列表里。
//
// 只做「让别人能看见我」所需的那一半：
//
//	UDP 137  应答名字查询（NB）与节点状态（NBSTAT）
//	UDP 138  主动发主机宣告（Host Announcement）
//
// **刻意不做** Samba nmbd 的全量功能：WINS 服务器、浏览主控选举、
// 域主控浏览、名字注册冲突仲裁、备份列表请求。那些属于"接入一个 NetBIOS
// 工作组并参与其治理"，范围远超本项目的目标（详见 packet.go 文件头）。
//
// 约束与依赖方向同 internal/wsd：不 fork/exec 外部进程（C3），
// 自己在 137/138 上收发（C4），只依赖标准库与两个叶子包
// （internal/netiface、internal/sockopt），**不** import internal/config，
// 也不 import internal/mdns / internal/wsd —— 三个发现组件互不相识。
package nbns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/netiface"
	"github.com/finalappstore/stupidsamba/internal/sockopt"
)

// 默认值。宣告周期取 4 分钟：宣告是"我还在"的保活，太稀疏会让刚开机的
// 客户端等太久才看见我们，太密则白白刷局域网。
const (
	// DefaultAnnounceInterval 是主机宣告的发送间隔。
	DefaultAnnounceInterval = 4 * time.Minute

	// initialAnnounceDelay 是启动后第一波宣告的延迟。
	//
	// 不立刻发：刚起来时网卡地址可能还没稳定（DHCP 还没拿完），
	// 立刻宣告出去的是个可能马上失效的地址。
	initialAnnounceDelay = 2 * time.Second

	// maxDatagramSize 是 NBNS 报文长度上限（RFC 1002 §4.2.2 允许 576，
	// 但节点状态响应可以更大；给 2 KiB 足够且能挡住畸形输入）。
	maxDatagramSize = 2048
)

// ErrDisabled 表示 NetBIOS 没有启用。语义同 wsd.ErrDisabled：
// 直接返回 = 配置关闭（预期，INFO）；包装后返回 = 开不起来（降级，WARN）。
var ErrDisabled = errors.New("nbns: 未启用")

// Options 是 NetBIOS 服务的自包含配置（不 import config，理由同 wsd.Options）。
type Options struct {
	// Enabled 开关。
	Enabled bool
	// Interfaces 限定网卡名，留空表示所有支持广播的网卡。
	Interfaces []string
	// Name 是本机 NetBIOS 名，超过 15 字节会被截断。
	Name string
	// Workgroup 是工作组名，留空用 DefaultWorkgroup。
	Workgroup string
	// Comment 是宣告里的注释（网上邻居里显示的说明文字）。
	Comment string
	// AnnounceInterval 是宣告间隔，0 表示 DefaultAnnounceInterval。
	AnnounceInterval time.Duration
}

// DefaultWorkgroup 是默认工作组名，与 SMB 侧 config 的默认值保持一致。
const DefaultWorkgroup = "WORKGROUP"

// Responder 是 NetBIOS 名称服务响应器。
//
// 生命周期与 mdns.Responder / wsd.Responder 同形，便于 cmd 层统一驱动。
type Responder struct {
	opts   Options
	log    *slog.Logger
	ifaces []net.Interface

	// names 是本节点持有的名字表（NBSTAT 与查询匹配共用）。
	names []Name
	// groupNames 是组名（应答时 NB_FLAGS 要置 G 位）。
	groupNames []Name

	// conn 监听 137（同时收到单播与广播查询）。
	conn *net.UDPConn
	// announce 用于发送 138 的广播宣告；绑定 138 失败时退化为临时端口。
	announce *net.UDPConn

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once

	// announceWarnOnce 让"宣告发不出去"只告警一次。
	announceWarnOnce sync.Once
}

// New 构造响应器。未启用时返回 ErrDisabled。
func New(o Options) (*Responder, error) {
	if !o.Enabled {
		return nil, ErrDisabled
	}
	ifaces, err := netiface.Select(o.Interfaces, netiface.NeedBroadcast)
	if err != nil {
		return nil, fmt.Errorf("nbns: %w", err)
	}
	// 同 wsd：没有可用网卡就不启动，别白占 137/138（还都是特权端口）。
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("%w: 没有可用的广播网卡", ErrDisabled)
	}
	if o.Workgroup == "" {
		o.Workgroup = DefaultWorkgroup
	}
	if o.AnnounceInterval <= 0 {
		o.AnnounceInterval = DefaultAnnounceInterval
	}
	if o.Name == "" {
		return nil, errors.New("nbns: 缺少本机名（NetBIOS 名）")
	}

	r := &Responder{
		opts:   o,
		log:    slog.Default(),
		ifaces: ifaces,
		// 名字表顺序与 nmbd 一致：<名><20> 是主条目。
		names: []Name{
			{Label: o.Name, Suffix: SuffixFileServer},
			{Label: o.Name, Suffix: SuffixWorkstation},
			{Label: o.Name, Suffix: SuffixMessenger},
		},
		groupNames: []Name{
			{Label: o.Workgroup, Suffix: SuffixWorkstation},
		},
	}
	return r, nil
}

// SetLogger 注入日志句柄。
func (r *Responder) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l.With("component", "nbns")
	}
}

// Start 开始监听 137 并周期性发宣告。
func (r *Responder) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	if err := r.openSockets(); err != nil {
		cancel()
		return err
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.readLoop(ctx)
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.announceLoop(ctx)
	}()

	r.log.Info("NetBIOS 名称服务已启动",
		"name", r.opts.Name,
		"workgroup", r.opts.Workgroup,
		"port", PortNameService,
		"announce_interval", r.opts.AnnounceInterval,
		"ifaces", ifaceNames(r.ifaces))
	return nil
}

// Stop 停止响应器并释放套接字。可重复调用。
func (r *Responder) Stop() {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		// 同样要先关套接字：UDP 读不响应 ctx 取消。
		if r.conn != nil {
			_ = r.conn.Close()
		}
		if r.announce != nil {
			_ = r.announce.Close()
		}
		r.wg.Wait()
		r.log.Debug("NetBIOS 名称服务已停止")
	})
}

// openSockets 建立 137 的监听套接字与宣告用的广播套接字。
func (r *Responder) openSockets() error {
	nsCfg := sockopt.ListenConfig()
	pc, err := nsCfg.ListenPacket(context.Background(), "udp4",
		fmt.Sprintf(":%d", PortNameService))
	if err != nil {
		return fmt.Errorf("nbns: 监听 UDP %d 失败: %w", PortNameService, err)
	}
	r.conn = pc.(*net.UDPConn)

	// 宣告优先绑 138（Windows 的浏览服务只认源端口 138 的宣告）。
	// 绑不上（非 root 环境）就退化成临时端口——宣告仍会发出去，
	// 只是部分严格的客户端可能不采信。因此只告警不失败。
	bcCfg := sockopt.BroadcastConfig()
	bc, err := bcCfg.ListenPacket(context.Background(), "udp4",
		fmt.Sprintf(":%d", PortDatagram))
	if err != nil {
		r.log.Warn("无法绑定 UDP 138，主机宣告将改用临时端口（Windows 可能不采信）；以 root 运行或 setcap 可解决",
			"err", err)
		bc, err = bcCfg.ListenPacket(context.Background(), "udp4", ":0")
		if err != nil {
			return fmt.Errorf("nbns: 建立广播套接字失败: %w", err)
		}
	}
	r.announce = bc.(*net.UDPConn)
	return nil
}

// readLoop 处理 137 上的查询。
func (r *Responder) readLoop(ctx context.Context) {
	buf := make([]byte, maxDatagramSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		r.handleQuery(buf[:n], src)
	}
}

// handleQuery 应答一条查询。
//
// 应答对象（RFC 1002 §4.2.2）：
//   - 匹配本节点唯一名 → 肯定应答（NB_FLAGS 唯一）
//   - 匹配组名 → 肯定应答（NB_FLAGS 置 G）
//   - qtype = NBSTAT 且名字属于本节点 → 节点状态应答
//   - 其余一律**不答**。NetBIOS 查询是广播的，乱答会污染别人的名字解析
//     （尤其不能对不属于自己的名字做"好心"的转发应答）。
func (r *Responder) handleQuery(b []byte, src *net.UDPAddr) {
	q := parseQuery(b)
	if q == nil {
		// 137 端口上什么都可能发过来，解析不了是常态，不记日志。
		return
	}
	if r.replyFor(q, src) {
		return
	}
	// 走到这里说明这条查询我们不打算应答。留一条 debug：排查"客户端查了
	// 但没答"这类问题时，这是唯一的线索 —— 得能分清是"不该答"还是
	// "该答却漏了"。
	r.log.Debug("忽略 NBNS 查询", "name", q.name.String(),
		"qtype", q.qtype, "from", src)
}

// replyFor 尝试应答一条查询，返回是否应答了。
//
// 应答对象（RFC 1002 §4.2.2）：
//   - qtype = NBSTAT 且名字属于本节点**或是通配符** → 节点状态应答
//   - 匹配本节点唯一名 → 肯定应答（NB_FLAGS 唯一）
//   - 匹配组名 → 肯定应答（NB_FLAGS 置 G）
//   - 其余一律**不答**。NetBIOS 查询是广播的，乱答会污染别人的名字解析
//     （尤其不能对不属于自己的名字做"好心"的转发应答）。
func (r *Responder) replyFor(q *nameQuery, src *net.UDPAddr) bool {
	switch q.qtype {
	case qTypeNBStat:
		// 节点状态查询（`nbtstat -A` / `nmblookup -A`）通常把名字写成通配符
		// `*`，查的不是"某个名字在不在"，而是"这台机器上都有哪些名字"。
		// 只在我们持有该名字时才应答，会让这一路**永远不答** ——
		// 实测 nmblookup -A 得到的就是 "No reply"。
		if !r.owns(q.name) && !q.name.isWildcard() {
			return false
		}
		all := append(append([]Name{}, r.names...), r.groupNames...)
		resp := buildNodeStatusResponse(q, all)
		if resp == nil {
			return false
		}
		r.reply(resp, src)
		r.log.Debug("已应答节点状态查询", "name", q.name.String(), "to", src)
		return true

	case qTypeNB:
		if r.ownsUnique(q.name) {
			if resp := buildNameResponse(q, r.addressFor(src), false); resp != nil {
				r.reply(resp, src)
				r.log.Debug("已应答名字查询", "name", q.name.String(),
					"ip", r.addressFor(src), "to", src)
				return true
			}
			return false
		}
		if r.ownsGroup(q.name) {
			if resp := buildNameResponse(q, r.addressFor(src), true); resp != nil {
				r.reply(resp, src)
				r.log.Debug("已应答组名查询", "name", q.name.String(), "to", src)
				return true
			}
		}
	}
	return false
}

// reply 把应答写回查询方。
//
// 一律**单播**回源地址，即使查询是广播来的：对广播查询做广播应答会让
// 局域网里所有主机都收到一份它们没问过的应答（RFC 1002 §4.2.2 也是这么要求的）。
// 源端口是 137 时才回 137，否则回客户端的临时端口（客户端行为不一致，
// 跟着它的源端口走最稳）。
func (r *Responder) reply(resp []byte, dst *net.UDPAddr) {
	if _, err := r.conn.WriteToUDP(resp, dst); err != nil {
		r.log.Debug("发送 NBNS 应答失败", "to", dst, "err", err)
	}
}

// announceLoop 周期性发主机宣告。
func (r *Responder) announceLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialAnnounceDelay):
	}
	r.sendAnnouncements()

	ticker := time.NewTicker(r.opts.AnnounceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sendAnnouncements()
		}
	}
}

// sendAnnouncements 向每张网卡的广播地址发两条宣告。
//
// 两条的目的地不同，缺一条都不完整：
//
//	<工作组><0x1e>   告诉本工作组的成员"我是成员之一"
//	__MSBROWSE__<01> 告诉**所有**浏览主控"我是可浏览的主机" ——
//	                 Windows 的「Microsoft Windows Network」列表就是靠它填充的
func (r *Responder) sendAnnouncements() {
	dests := []Name{
		{Label: r.opts.Workgroup, Suffix: SuffixBrowserGroup},
		{Label: "\x01\x02__MSBROWSE__\x02", Suffix: 0x01},
	}
	source := Name{Label: r.opts.Name, Suffix: SuffixFileServer}

	sent := 0
	for i := range r.ifaces {
		ifi := r.ifaces[i]
		v4, _ := netiface.Addrs(ifi)
		if len(v4) == 0 {
			continue
		}
		ip := v4[0]
		for _, d := range dests {
			pkt, err := buildHostAnnouncement(ip, source, d, r.opts.Name, r.opts.Comment)
			if err != nil {
				r.log.Debug("构造主机宣告失败", "err", err)
				continue
			}
			for _, bcast := range netiface.Broadcasts(ifi) {
				dst := &net.UDPAddr{IP: bcast, Port: PortDatagram}
				if _, err := r.announce.WriteToUDP(pkt, dst); err != nil {
					r.log.Debug("发送主机宣告失败", "to", dst, "err", err)
					continue
				}
				sent++
			}
		}
	}
	if sent == 0 {
		// 只在第一次告警：宣告是周期性动作，没有可用广播地址时
		// 每 4 分钟刷一条同样的 WARN 只会淹没真正有用的日志。
		r.announceWarnOnce.Do(func() {
			r.log.Warn("主机宣告一条都没发出去（网卡上没有可用于广播的 IPv4？）" +
				"后续同类失败只记 debug")
		})
		r.log.Debug("本轮主机宣告未发出")
		return
	}
	r.log.Debug("已发送主机宣告", "count", sent)
}

// owns 报告名字是否属于本节点（唯一名或组名都算）。
func (r *Responder) owns(n Name) bool {
	return r.ownsUnique(n) || r.ownsGroup(n)
}

func (r *Responder) ownsUnique(n Name) bool {
	for _, mine := range r.names {
		if namesEqual(mine, n) {
			return true
		}
	}
	return false
}

func (r *Responder) ownsGroup(n Name) bool {
	for _, mine := range r.groupNames {
		if namesEqual(mine, n) {
			return true
		}
	}
	return false
}

// addressFor 选出应答里要给出的本机地址。
//
// 优先给**与查询方同网段**的地址：多网卡机器上给一个从对方那条链路
// 不可达的地址，`\\NAME` 解析出来也连不上。同网段判定用"掩码按位与是否相等"，
// 需要遍历各网卡的地址前缀；都匹配不上时退回第一个可用 IPv4。
func (r *Responder) addressFor(src *net.UDPAddr) net.IP {
	var fallback net.IP
	for i := range r.ifaces {
		ifi := r.ifaces[i]
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			if fallback == nil {
				fallback = append(net.IP(nil), ip4...)
			}
			if src == nil || src.IP == nil {
				continue
			}
			src4 := src.IP.To4()
			if src4 == nil {
				continue
			}
			// 同网段判定：两边各自与掩码求网络号后比较。
			if ipnet.Mask != nil &&
				ip4.Mask(ipnet.Mask).Equal(src4.Mask(ipnet.Mask)) {
				return ip4
			}
		}
	}
	return fallback
}

func ifaceNames(ifaces []net.Interface) []string {
	out := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		out = append(out, ifi.Name)
	}
	return out
}
