// Package sockopt 提供跨平台的「允许多个进程绑定同一个 UDP 端口」能力。
//
// 同样是**叶子包**：只依赖标准库与 golang.org/x/sys（系统调用号封装，
// 不需要 CGO），不依赖任何协议实现。
//
// 为什么单独成包：mDNS（5353）、WS-Discovery（3702）、NetBIOS（137/138）
// 这三个端口在真实机器上都**可能已经有别的实现在跑**，也**可能被本程序的
// 另一个实例占用**。让三个组件各自写一份带 build tag 的 setsockopt 代码，
// 是三份会各自漂移的平台相关逻辑 —— 那份风险不值得省一个包。
package sockopt

import "net"

// ListenConfig 返回一个允许地址复用的 ListenConfig。
//
// 用法：
//
//	pc, err := sockopt.ListenConfig().ListenPacket(ctx, "udp4", ":3702")
//
// 注意这只解决「bind 不冲突」，**不解决**「谁能收到包」——
// 组播报文的分发还要另外 JoinGroup（见各组件自己的实现）。
func ListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: setReuse}
}
