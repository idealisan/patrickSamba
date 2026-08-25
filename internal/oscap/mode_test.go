package oscap

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestParseModeAccepts(t *testing.T) {
	cases := map[string]Mode{
		"auto":     ModeAuto,
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

// TestParseModeRejectsRemovedNative 是移除档的反向对照。
//
// "native" 在 v0.5 开发版已从 filesystem_mode 移除（原契约「全部能力原生」
// 在任何平台都无法满足）。它必须**报错**而不是被静默当成别的档，
// 且报错要指名道姓：说明已移除、并给出替代取值 auto / portable ——
// 只说"非法"会让人去检查拼写，意识不到配置本身已经过时。
func TestParseModeRejectsRemovedNative(t *testing.T) {
	got, err := ParseMode("native")
	if err == nil {
		t.Fatalf("ParseMode(\"native\") 应当报错，却返回了 %v", got)
	}
	if got != DefaultMode {
		t.Errorf("ParseMode(\"native\") 失败时应返回 DefaultMode，却返回 %v", got)
	}
	msg := err.Error()
	for _, want := range []string{"auto", "portable", "v0.5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ParseMode(\"native\") 的错误信息应包含 %q:\n%s", want, msg)
		}
	}
	// 大小写与空白变体走普通非法值路径，同样必须报错（严格解析，不纠正）。
	for _, in := range []string{"Native", "NATIVE", "native ", " native"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) 应当报错", in)
		}
	}
}

func TestModeNames(t *testing.T) {
	got := ModeNames()
	want := []string{"auto", "portable"}
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
