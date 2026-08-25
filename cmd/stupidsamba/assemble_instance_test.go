package main

// assemble_instance_test.go —— listenerInstanceID 的推导规则（OI-1）。
//
// 它的输出是旁路元数据默认落点的实例维度，两条硬保证都必须在这里成立：
// 同一配置重启得到同一个标识（复用元数据）；不同端点组合得到不同标识
// （并存进程不抢同一把 bbolt flock）。

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/config"
)

func TestListenerInstanceID(t *testing.T) {
	cases := []struct {
		name string
		l    config.Listen
		want string
	}{
		{
			// 最常见形态：单地址 + 端口，恰为 "addr:port"。
			name: "单地址",
			l:    config.Listen{Addresses: []string{"127.0.0.1"}, Port: 4451},
			want: "127.0.0.1:4451",
		},
		{
			// 多地址按配置顺序连接；顺序来自配置本身，重启后稳定。
			name: "多地址",
			l:    config.Listen{Addresses: []string{"192.168.1.5", "10.0.0.2"}, Port: 445},
			want: "192.168.1.5:445,10.0.0.2:445",
		},
		{
			// addresses 留空 = 监听所有接口：用字面量保持确定性。
			name: "空地址表",
			l:    config.Listen{Port: 4460},
			want: "0.0.0.0:4460",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := listenerInstanceID(c.l); got != c.want {
				t.Fatalf("listenerInstanceID(%+v) = %q, want %q", c.l, got, c.want)
			}
		})
	}
}

func TestListenerInstanceIDDeterministicAndDistinct(t *testing.T) {
	a := config.Listen{Addresses: []string{"127.0.0.1"}, Port: 4471}
	if listenerInstanceID(a) != listenerInstanceID(a) {
		t.Fatal("同一配置两次推导不一致 —— 重启后元数据会丢")
	}
	b := config.Listen{Addresses: []string{"127.0.0.1"}, Port: 4472}
	c := config.Listen{Addresses: []string{"127.0.0.2"}, Port: 4471}
	got := map[string]bool{
		listenerInstanceID(a): true,
		listenerInstanceID(b): true,
		listenerInstanceID(c): true,
	}
	if len(got) != 3 {
		t.Fatalf("不同端点组合必须得到不同实例标识: %v", got)
	}
}

func TestListenerInstanceIDIPv6SafeForFilenames(t *testing.T) {
	// IPv6 地址含冒号：推导值本身允许含冒号（清洗是下游文件名层的事，
	// 见 oscap.SanitizeInstanceID），但必须把 host 与端口完整带下来。
	// net.JoinHostPort 对 IPv6 字面量按惯例加方括号。
	got := listenerInstanceID(config.Listen{Addresses: []string{"::1"}, Port: 4455})
	if want := "[::1]:4455"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
