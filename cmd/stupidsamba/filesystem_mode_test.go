package main

// 装配层的 filesystem_mode 接线测试。
//
// # 这里防的是什么
//
// 不是"配置能不能解析"——那件事 internal/config 已经测过了。
// 防的是本仓库反复出现的那类失败：**配置项读进来了、字段填上了、运行期没人消费**，
// 于是三个取值的行为完全一样，而且**不报任何错**。
// oscap 子系统在接线之前正是这个状态：port + 双适配器全套写完、测试全绿，
// 产品代码里一处调用都没有，`filesystem_mode` 设了等于没设。
//
// 所以这里的断言全部落在「共享**实际**在用哪一侧」上（CapabilityMatrix），
// 而不是「配置结构体里那个字符串等于什么」。后者就算全绿也证明不了任何事。

import (
	"errors"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/oscap"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// buildOneShare 用给定的 filesystem_mode 装配一个临时共享，返回它的 LocalFS。
func buildOneShare(t *testing.T, mode string) *vfs.LocalFS {
	t.Helper()
	cfg := &config.Config{
		FilesystemMode: mode,
		Shares: []config.Share{
			{Name: "data", Path: t.TempDir()},
		},
	}
	shares, err := buildShares(cfg)
	if err != nil {
		t.Fatalf("buildShares(filesystem_mode=%q): %v", mode, err)
	}
	t.Cleanup(func() { closeShares(shares) })

	// shares[0] 是磁盘共享，末尾那个是 IPC$。
	fs, ok := shares[0].FS.(*vfs.LocalFS)
	if !ok {
		t.Fatalf("shares[0].FS 类型 = %T，期望 *vfs.LocalFS", shares[0].FS)
	}
	return fs
}

// TestPortableModeReachesTheShare：YAML 里写 portable，共享就必须真的落到 builtin。
//
// 这是「接缝通电」的装配层判据。逐项断言而不是只看一项：接线漏掉某一项能力
// 是真实发生过的形态（"接了但没接全"），只查 CapXattr 会给它放行。
func TestPortableModeReachesTheShare(t *testing.T) {
	m := buildOneShare(t, "portable").CapabilityMatrix()

	if m.Mode() != oscap.ModePortable {
		t.Errorf("矩阵报的 Mode = %s，期望 portable", m.Mode())
	}
	for _, c := range oscap.Capabilities() {
		if got := m.Kind(c); got != oscap.KindBuiltin {
			t.Errorf("portable 档下 %s 落到了 %s，期望 builtin —— "+
				"配置没能穿透到运行期（矩阵: %s）", c, got, m)
		}
	}
}

// TestAutoModeIsNotPortable 是上一条的**反向对照**。
//
// 少了它，一个「无论配置写什么都返回 builtin」的桩实现可以平凡地通过上一条。
// 必须证明另一个取值真的走出了不同的路，才能排除"矩阵是写死的"这种双重失败。
//
// 只断言"至少有一项不是 builtin"而不是逐项断 native：auto 的结果依宿主而定
// （容器里的 overlayfs、tmpfs、exfat 各不相同），逐项钉死会变成一条依赖环境的脆弱断言。
// 但**完全跳过是不行的** —— 一个会自己 skip 的反向对照等于没有对照。
func TestAutoModeIsNotPortable(t *testing.T) {
	m := buildOneShare(t, "auto").CapabilityMatrix()

	if m.Mode() != oscap.ModeAuto {
		t.Errorf("矩阵报的 Mode = %s，期望 auto", m.Mode())
	}
	if len(m.Caps(oscap.KindNative)) == 0 {
		t.Errorf("auto 档在本宿主上一项 native 都没选中（矩阵: %s）—— "+
			"若宿主确实一项可选能力都不支持这是合理的，但更可能是矩阵被写死成了 builtin", m)
	}
}

// TestInvalidModeFailsAssembly：非法取值必须在装配期炸掉，不能默默按 auto 跑。
//
// 校验层已经拦过一道，这里再测一次是因为**装配层不保证一定跑在校验之后**
// （测试、未来的其他入口都可能直接调 buildShares）。一个"非法值静默降级成 auto"
// 的装配层会让配置错误在生产上变成"配了但没生效"，而那是最难查的一类问题。
func TestInvalidModeFailsAssembly(t *testing.T) {
	shares, err := buildShares(&config.Config{
		FilesystemMode: "turbo",
		Shares:         []config.Share{{Name: "data", Path: t.TempDir()}},
	})
	if err == nil {
		closeShares(shares)
		t.Fatal("非法 filesystem_mode=\"turbo\" 被装配层接受了")
	}
	if !errors.Is(err, oscap.ErrInvalidArg) {
		t.Errorf("错误 = %v，期望包裹 oscap.ErrInvalidArg", err)
	}
}
