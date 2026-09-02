package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/mdns"
	"github.com/finalappstore/stupidsamba/internal/nbns"
	"github.com/finalappstore/stupidsamba/internal/wsd"
)

// ---------------------------------------------------------------------------
// 服务发现的统一启停
//
// 三个发现组件覆盖的是**不同的客户端**：
//
//	mDNS            macOS / Linux（Bonjour）
//	WS-Discovery    Windows「网络」
//	NetBIOS         `\\NAME` 解析与网上邻居浏览列表
//
// 它们分属三个互不相识的包（internal/mdns、internal/wsd、internal/nbns），
// 唯一的共同点是"能被启动、能被停下"。本文件是**唯一**把它们放在一起的
// 地方 —— 耦合只发生在这里，而且方向是单向的：cmd 依赖它们，
// 它们不依赖 cmd，彼此之间也没有任何 import 关系。
// ---------------------------------------------------------------------------

// errDiscoveryDisabled 表示对应组件在配置里没开。
//
// 用**本文件自己的**哨兵，而不是去认三个包各自的 ErrDisabled：
// 那样调用方就得 import 三个包的类型只为比一个错误值，
// 等于把三个包的内部约定又拉回到 cmd 层。三个包各自保留自己的哨兵
// （供它们自己的调用方使用），cmd 层只认这一个。
var errDiscoveryDisabled = errors.New("discovery: 组件未启用")

// discovery 是各发现组件的统一生命周期视图。
//
// mdns.Responder / wsd.Responder / nbns.Responder 都天然满足它
// （New → SetLogger → Start → Stop），不需要任何适配层 ——
// 这是当初刻意让三个包的生命周期同形换来的。
type discovery interface {
	Start(ctx context.Context) error
	Stop()
}

// startDiscovery 按配置启动全部发现组件。
//
// 单个组件起不来**不影响其它组件，也不影响文件共享本身** ——
// 发现只影响"能不能被自动发现"，客户端仍可用 IP 直连。
// 因此这里收集所有失败、逐条 WARN，而不是让进程退出。
// 返回已成功启动的组件，供退出时统一停下。
func startDiscovery(cfg *config.Config, log *slog.Logger) []discovery {
	var out []discovery

	add := func(name string, newFn func() (discovery, error)) {
		d, err := newFn()
		if err != nil {
			if errors.Is(err, errDiscoveryDisabled) && !errors.Is(err, wsd.ErrDisabled) &&
				!errors.Is(err, nbns.ErrDisabled) {
				log.Info("服务发现组件未启用（配置里显式关闭）", "component", name)
				return
			}
			// 「想开但开不起来」：按项目原则**不崩溃**，记一条 WARN 后
			// 继续跑其他功能。常见的开不起来：端口被占用、非 root 绑不上
			// 特权端口、当前环境没有符合要求的网卡、端口上已有别的实现。
			// 三套协议彼此独立，倒掉一个不影响另外两个，也不影响文件共享。
			log.Warn("服务发现组件无法启用，该途径的自动发现不可用（其他功能照常）",
				"component", name, "err", err)
			return
		}
		// 刻意传 Background 而不是信号 ctx：ctx 取消会让组件直接停摆，
		// 而退出时我们要的是先撤回宣告（mDNS goodbye），统一由 Stop 收尾。
		if err := d.Start(context.Background()); err != nil {
			// 与上面 New 失败同一条降级路径：不崩溃，记 WARN 后继续。
			// 差别只在出错环节（这里是 bind / JoinGroup 阶段），
			// 因此特权端口的提示挂在这里。
			log.Warn("服务发现组件无法启用，该途径的自动发现不可用（其他功能照常）",
				"component", name, "err", privilegedPortHint(err))
			return
		}
		out = append(out, d)
	}

	add("mdns", func() (discovery, error) { return newMDNS(cfg, log) })
	add("ws-discovery", func() (discovery, error) { return newWSD(cfg, log) })
	add("nbns", func() (discovery, error) { return newNBNS(cfg, log) })

	return out
}

// stopDiscovery 逆序停下全部发现组件。
//
// 逆序没有协议上的必要，只是让日志读起来与启动顺序对称，
// 排查时更容易把"谁起来了、谁没起来"对上。
func stopDiscovery(ds []discovery) {
	for i := len(ds) - 1; i >= 0; i-- {
		ds[i].Stop()
	}
}

// newMDNS 构造 mDNS responder。
func newMDNS(cfg *config.Config, log *slog.Logger) (discovery, error) {
	if !cfg.MDNS.EnabledOn() {
		return nil, errDiscoveryDisabled
	}
	r, err := mdns.New(cfg.MDNS, cfg.Listen.Port, cfg.Shares)
	if err != nil {
		return nil, err
	}
	r.SetLogger(log)
	return r, nil
}

// newWSD 构造 WS-Discovery 响应器。
//
// 这里做一次 config → wsd.Options 的映射。wsd 包刻意不认识 config，
// 所以映射只能落在 cmd 层 —— 这正是低耦合要的：协议实现不随配置
// 结构的变化而改动。
func newWSD(cfg *config.Config, log *slog.Logger) (discovery, error) {
	if !cfg.WSDiscovery.EnabledOn() {
		return nil, errDiscoveryDisabled
	}
	r, err := wsd.New(wsd.Options{
		Enabled:    true,
		Interfaces: cfg.WSDiscovery.Interfaces,
		// WS-Discovery 的显示名用 SMB 服务器名：与 mDNS 实例名、
		// NetBIOS 名保持同一个，客户端看到的三处才是同一个东西。
		Name:         cfg.Server.Name,
		UUID:         cfg.WSDiscovery.UUID,
		MetadataPort: cfg.WSDiscovery.MetadataPort,
	})
	if err != nil {
		return nil, err
	}
	r.SetLogger(log)
	return r, nil
}

// newNBNS 构造 NetBIOS 名称服务响应器。
func newNBNS(cfg *config.Config, log *slog.Logger) (discovery, error) {
	if !cfg.NetBIOS.EnabledOn() {
		return nil, errDiscoveryDisabled
	}
	var interval time.Duration
	if cfg.NetBIOS.AnnounceIntervalSeconds > 0 {
		interval = time.Duration(cfg.NetBIOS.AnnounceIntervalSeconds) * time.Second
	}
	r, err := nbns.New(nbns.Options{
		Enabled:          true,
		Interfaces:       cfg.NetBIOS.Interfaces,
		Name:             cfg.NetBIOS.Name,
		Workgroup:        cfg.NetBIOS.Workgroup,
		Comment:          cfg.NetBIOS.Comment,
		AnnounceInterval: interval,
	})
	if err != nil {
		return nil, err
	}
	r.SetLogger(log)
	return r, nil
}

// privilegedPortHint 给「绑不上特权端口」补上人话。
//
// NetBIOS 的 137/138 与 SMB 的 445 一样是 <1024 的端口，非 root 时
// bind 会失败并返回 "permission denied" —— 光看这个错误，
// 没人会想到"只要给二进制加个 capability 就行"。
// 与 main.go 的 bindError 是同一套措辞，保持两处提示一致。
func privilegedPortHint(err error) error {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return err
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != syscall.EACCES {
		// 也可能是"端口被占用"（别的 nmbd / wsdd 在跑），
		// 那种情况给特权端口的提示就是误导，原样返回。
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"NetBIOS 用 137/138 这两个特权端口（<1024），当前进程不是 root。三种解决办法：\n"+
		"  1. 以 root 运行；\n"+
		"  2. 给二进制授权：sudo setcap 'cap_net_bind_service=+ep' %s；\n"+
		"  3. 关掉 netbios.enabled（Windows 仍能通过 WS-Discovery 或 IP 直连访问）。",
		err, selfPath())
}
