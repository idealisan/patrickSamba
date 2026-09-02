package config

import (
	"testing"
)

// 本文件钉的是「能开就尽量开」这条默认规则。
//
// 规则本身：默认值为 true 的开关一律用 *bool，以便区分「未设置」与
// 「显式 false」。用 bool 的话零值就是 false，等于默认关闭 ——
// 那是本项目明确不要的行为。
//
// 变异自检方向：
//   - 把任一 Default*Enabled 常量改成 false → TestDiscoveryDefaultsOn 变红；
//   - 把 EnabledOn 里的 `if x == nil { return Default }` 顺序弄反 →
//     TestExplicitFalseIsRespected 变红。

func TestDiscoveryDefaultsOn(t *testing.T) {
	var c Config
	ApplyDefaults(&c)

	// 三套发现协议**不是互斥的**：它们覆盖不同的客户端，默认应当全部开启，
	// 有条件就都开。
	if !c.MDNS.EnabledOn() {
		t.Fatal("mdns 默认应当开启")
	}
	if !c.WSDiscovery.EnabledOn() {
		t.Fatal("ws_discovery 默认应当开启")
	}
	if !c.NetBIOS.EnabledOn() {
		t.Fatal("netbios 默认应当开启")
	}
	if !c.Server.OplocksOn() {
		t.Fatal("server.oplocks 默认应当开启")
	}
}

// TestExplicitFalseIsRespected：默认值是 true，但用户显式写 false 必须生效 ——
// 否则"想关也关不掉"，那比默认关闭更糟。
func TestExplicitFalseIsRespected(t *testing.T) {
	c := Config{
		MDNS:        MDNS{Enabled: BoolPtr(false)},
		WSDiscovery: WSDiscovery{Enabled: BoolPtr(false)},
		NetBIOS:     NetBIOS{Enabled: BoolPtr(false)},
	}
	c.Server.Oplocks = BoolPtr(false)
	ApplyDefaults(&c)

	if c.MDNS.EnabledOn() {
		t.Fatal("mdns 显式 false 应生效")
	}
	if c.WSDiscovery.EnabledOn() {
		t.Fatal("ws_discovery 显式 false 应生效")
	}
	if c.NetBIOS.EnabledOn() {
		t.Fatal("netbios 显式 false 应生效")
	}
	if c.Server.OplocksOn() {
		t.Fatal("server.oplocks 显式 false 应生效")
	}
}

// TestExplicitTrueIsRespected：显式 true 与默认 true 不能区分时无所谓，
// 但要保证解引用路径本身是对的（nil 与非 nil 两条都走到）。
func TestExplicitTrueIsRespected(t *testing.T) {
	c := Config{
		MDNS:        MDNS{Enabled: BoolPtr(true)},
		WSDiscovery: WSDiscovery{Enabled: BoolPtr(true)},
		NetBIOS:     NetBIOS{Enabled: BoolPtr(true)},
	}
	c.Server.Oplocks = BoolPtr(true)
	if !c.MDNS.EnabledOn() || !c.WSDiscovery.EnabledOn() ||
		!c.NetBIOS.EnabledOn() || !c.Server.OplocksOn() {
		t.Fatal("显式 true 应当全部生效")
	}
}

// TestBoolPtrIsIndependent：两次调用不能返回同一个指针 ——
// 共用一个指针的话，改一处会连带改掉另一处。
func TestBoolPtrIsIndependent(t *testing.T) {
	a := BoolPtr(true)
	b := BoolPtr(false)
	if a == b {
		t.Fatal("BoolPtr 两次调用不应返回同一个指针")
	}
	if *a != true || *b != false {
		t.Fatal("BoolPtr 的值不对")
	}
}
