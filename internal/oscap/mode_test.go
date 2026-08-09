package oscap

import (
	"errors"
	"io/fs"
	"testing"
)

func TestParseModeAccepts(t *testing.T) {
	cases := map[string]Mode{
		"auto":     ModeAuto,
		"native":   ModeNative,
		"portable": ModePortable,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q) 意外失败: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %v, 期望 %v", in, got, want)
		}
		if got.String() != in {
			t.Errorf("Mode(%v).String() = %q, 期望 %q（往返必须自洽）", got, got.String(), in)
		}
	}
}

// TestParseModeRejects 是上一条的反向对照。
//
// 特别要拒掉的是 "Auto" / " auto" 这类"差一点点"的写法：
// 静默 ToLower/TrimSpace 会让用户以为设的是 A、服务在按 B 跑，
// 与 config.Load 的严格模式（未知字段直接报错）是同一种态度。
func TestParseModeRejects(t *testing.T) {
	for _, in := range []string{"", "Auto", "AUTO", " auto", "auto ", "builtin", "nativ", "0"} {
		got, err := ParseMode(in)
		if err == nil {
			t.Errorf("ParseMode(%q) 应当报错，却返回了 %v", in, got)
			continue
		}
		if !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("ParseMode(%q) 的错误应当可被 errors.Is(err, fs.ErrInvalid) 判定: %v", in, err)
		}
		if got != DefaultMode {
			t.Errorf("ParseMode(%q) 失败时应返回 DefaultMode，却返回 %v", in, got)
		}
	}
}

func TestModeNames(t *testing.T) {
	got := ModeNames()
	want := []string{"auto", "native", "portable"}
	if len(got) != len(want) {
		t.Fatalf("ModeNames() = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ModeNames() = %v, 期望 %v（顺序也要稳定，它会出现在配置报错里）", got, want)
		}
	}
	// 每个名字都必须能被 ParseMode 认回来，否则报给用户的"可选值"是错的。
	for _, n := range got {
		if _, err := ParseMode(n); err != nil {
			t.Errorf("ModeNames() 报出的 %q 却无法被 ParseMode 解析: %v", n, err)
		}
	}
}

func TestDefaultModeIsAutoAndIsZeroValue(t *testing.T) {
	if DefaultMode != ModeAuto {
		t.Fatalf("DefaultMode = %v, 期望 auto", DefaultMode)
	}
	var zero Mode
	if zero != ModeAuto {
		t.Fatal("Mode 的零值必须是 auto —— 忘记赋值时要落在最保守可用的一侧")
	}
}
