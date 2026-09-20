package native

// native_test.go —— 与平台无关的用例：参数校验、降级答案，以及
// **探测与实现的一致性**（本包最重要的一条不变量，见下）。

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// TestNewNeverFailsOnMissingCapability 钉住 New 的错误契约。
//
// 「这台机器没这个能力」**不是**构造失败。若在这里报错，auto 模式下
// provider.New 会把整个 native 侧判为不可用（provider.go:115-126），
// 把「一项降级」放大成「全部降级」；native 模式下则直接启动失败。
// 这条在任何平台上都必须成立，包括那些一项原生能力都没有的平台。
func TestNewNeverFailsOnMissingCapability(t *testing.T) {
	set, err := New(oscap.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New 在能力不全时返回了错误: %v（应当只在真正的构造失败时报错）", err)
	}
	// 至少要有一项，否则说明 newSet 整个没接线 —— 三个目标平台都不该是空的。
	// 用**反向**判据：不是「断言某一项非 nil」（那会随平台漂移），
	// 而是「六项全 nil」这个明确的坏状态。
	if set.Xattr == nil && set.Sparse == nil && set.Streams == nil &&
		set.IDs == nil && set.Times == nil && set.DOS == nil {
		t.Fatal("New 返回了一项能力都没有的 Set：本平台的 newSet 没有接线")
	}
}

// TestNewRejectsInvalidOptions 确认 Options 校验没有被绕过。
func TestNewRejectsInvalidOptions(t *testing.T) {
	if _, err := New(oscap.Options{}); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空 Root 应当返回 ErrInvalidArg，实际 %v", err)
	}
}

// TestProbeAgreesWithImplementation 是本包与 internal/oscap/probe_*.go 之间
// 那条**刻意耦合**的守卫（probe.go 文件头：「探测的判据是 native adapter
// 到底能不能做到」）。
//
// 判据方向是单向的：**探测报 true ⇒ Set 里必须有这一项**。
// 反过来不成立且不该断言 —— adapter 做了这一项、而本机文件系统恰好不支持
// （例如 overlayfs 上打不了洞），探测报 false 是正确答案。
//
// 为什么这个用例值钱：探测报 true 而实现是 nil 时，filesystem_mode: native
// 会**启动通过、运行到用那一项时才炸**。那种失效没有任何报错时刻，
// 正是 §1.2 点名要禁止的形态，而它在编译期完全看不出来。
func TestProbeAgreesWithImplementation(t *testing.T) {
	opts := oscap.Options{Root: t.TempDir()}
	set, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	have := map[oscap.Capability]bool{
		oscap.CapXattr:         set.Xattr != nil,
		oscap.CapSparseFile:    set.Sparse != nil,
		oscap.CapNamedStream:   set.Streams != nil,
		oscap.CapStableFileID:  set.IDs != nil,
		oscap.CapCreationTime:  set.Times != nil,
		oscap.CapDOSAttributes: set.DOS != nil,
	}
	for _, c := range oscap.Capabilities() {
		if oscap.ProbeNative(c, opts) && !have[c] {
			t.Errorf("探测说 %s 原生可用，但 native.New 没有提供实现（nil）——"+
				"filesystem_mode: native 会启动通过、运行时才炸", c)
		}
	}
}

// TestCheckWindowRejectsOverflow 钉住 offset/length 的边界校验。
//
// 溢出必须在进系统调用之前拦住：off+length 溢出成负数后，
// fallocate / DeviceIoControl 收到的是一个完全不同的区间，那是一次越界写
// （AGENTS.md §8）。
func TestCheckWindowRejectsOverflow(t *testing.T) {
	cases := []struct {
		name        string
		off, length int64
		wantErr     bool
	}{
		{"正常", 0, 4096, false},
		{"零长度", 100, 0, false},
		{"最大边界刚好不溢出", math.MaxInt64 - 10, 10, false},
		{"负偏移", -1, 10, true},
		{"负长度", 0, -1, true},
		{"相加溢出", math.MaxInt64 - 10, 11, true},
		{"双最大值", math.MaxInt64, math.MaxInt64, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkWindow(c.off, c.length)
			if c.wantErr {
				if !errors.Is(err, oscap.ErrInvalidArg) {
					t.Fatalf("off=%d length=%d 应当被拒绝为 ErrInvalidArg，实际 %v",
						c.off, c.length, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("off=%d length=%d 应当通过，实际 %v", c.off, c.length, err)
			}
		})
	}
}

// TestWholeWindow 钉住「探测不可用时整段报已分配」这条降级答案。
//
// 它必须是**整段**而不是空列表：少报会让客户端以为数据丢了（ports.go）。
func TestWholeWindow(t *testing.T) {
	got := wholeWindow(100, 400)
	if len(got) != 1 || got[0].Offset != 100 || got[0].Length != 300 {
		t.Fatalf("wholeWindow(100,400) = %+v，期望单个 {100,300}", got)
	}
	if r := wholeWindow(100, 100); r != nil {
		t.Fatalf("空窗口应当返回 nil，实际 %+v", r)
	}
	if r := wholeWindow(400, 100); r != nil {
		t.Fatalf("倒置窗口应当返回 nil 而不是负长度，实际 %+v", r)
	}
}

// TestValidateStreamName 钉住命名流名字的安全校验（AGENTS.md §8）。
//
// 流名由客户端控制，会被拼进宿主机的名字空间（POSIX 上是 xattr 名，
// Windows 上是 `文件:流:$DATA` 路径）。放行分隔符等于让客户端把写入
// 引到别的对象上去。
func TestValidateStreamName(t *testing.T) {
	bad := map[string]string{
		"空串":     "",
		"当前目录":   ".",
		"上级目录":   "..",
		"含斜杠":    "a/b",
		"含反斜杠":   `a\b`,
		"含裸冒号":   "a:b",
		"含 NUL":  "a\x00b",
		"含控制字符":  "a\x01b",
		"以控制字符起": "\x1fname",
	}
	for name, v := range bad {
		if err := validateStreamName(v); !errors.Is(err, oscap.ErrInvalidArg) {
			t.Errorf("%s：validateStreamName(%q) 应当返回 ErrInvalidArg，实际 %v", name, v, err)
		}
	}

	good := []string{
		"AFP_Resource",
		"AFP_AfpInfo",
		"com.apple.metadata\uf022kMDItemFinderComment", // 私用区 U+F022，真实客户端形态
		strings.Repeat("x", 200),
	}
	for _, v := range good {
		if err := validateStreamName(v); err != nil {
			t.Errorf("validateStreamName(%q) 应当通过，实际 %v", v, err)
		}
	}
}
