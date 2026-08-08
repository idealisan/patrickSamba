package vfs

import "testing"

func TestMatchDOSBasic(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// 空模式 / 全匹配
		{"", "anything.txt", true},
		{"*", "anything.txt", true},
		{"*", "", true},

		// 精确匹配（大小写不敏感 —— SMB 语义）
		{"readme.txt", "readme.txt", true},
		{"README.TXT", "readme.txt", true},
		{"readme.txt", "readme.txt.bak", false},
		{"readme", "readme.txt", false},

		// `*`
		{"*.txt", "readme.txt", true},
		{"*.txt", "readme.txtx", false},
		{"*.txt", "readme.TXT", true},
		{"a*", "abc", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abd", false},
		{"*a*b*", "xxaxxbxx", true},
		{"*a*b*", "xxbxxaxx", false},

		// `?` 严格匹配一个字符
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "abbc", false},
		{"???", "abc", true},
		{"???", "ab", false},

		// 多字节：`?` 必须匹配整个字符而不是一个字节
		{"?", "汉", true},
		{"??", "汉", false},
		{"汉*", "汉字.txt", true},
	}
	for _, c := range cases {
		if got := MatchDOS(c.pattern, c.name); got != c.want {
			t.Errorf("MatchDOS(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchDOSSpecialChars(t *testing.T) {
	// DOS_STAR `<` / DOS_QM `>` / DOS_DOT `"`。
	// 期望值依据 MS-FSA §2.1.4.4 的语义描述与 Samba ms_fnmatch 行为。
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// `"` 匹配一个点，或名字末尾的零个字符 —— 这正是 `*.*`
		// 能匹配无扩展名文件的原因（客户端把它翻译成 `*"*`）。
		{`a"txt`, "a.txt", true},
		{`a"`, "a.", true},
		{`a"`, "a", true},
		{`a"txt`, "axtxt", false},

		// `<` 是客户端对 `*.` 的翻译：匹配到并吃掉最后一个点为止。
		// 所以单独的 `<` 语义是「没有扩展名的文件」。
		{"<", "abc", true},
		{"<", "abc.", true},
		{"<", "abc.txt", false},
		{"a<", "abc", true},
		{"a<", "a.", true},
		{"a<", "a.txt", false},
		{"<.txt", "abc.txt", true},
		{"<txt", "abc.txt", true},

		// `>` 匹配单个字符，名字末尾可匹配零个
		{">", "a", true},
		{">", "", true},
		{">>>", "ab", true},
		{">>>", "abcd", false},
		{"a>c", "abc", true},
	}
	for _, c := range cases {
		if got := MatchDOS(c.pattern, c.name); got != c.want {
			t.Errorf("MatchDOS(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchDOSDotEntries(t *testing.T) {
	// Windows 要求枚举结果里带 "." 与 ".."，它们必须能被 "*" 命中。
	if !MatchDOS("*", ".") || !MatchDOS("*", "..") {
		t.Error(`"*" 应当匹配 "." 与 ".."`)
	}
}

// TestMatchDOSNoBlowup 保证构造出来的病态输入不会把 CPU 打满。
// 若没有预算限制，这个用例会跑到天荒地老。
func TestMatchDOSNoBlowup(t *testing.T) {
	pattern := "*a*a*a*a*a*a*a*a*a*a*b"
	name := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_ = MatchDOS(pattern, name) // 只要能在瞬间返回即可
}

func TestHasWildcard(t *testing.T) {
	for _, p := range []string{"*", "a?b", "a<b", "a>b", `a"b`, "*.txt"} {
		if !HasWildcard(p) {
			t.Errorf("HasWildcard(%q) 应为 true", p)
		}
	}
	for _, p := range []string{"", "readme.txt", "a.b.c"} {
		if HasWildcard(p) {
			t.Errorf("HasWildcard(%q) 应为 false", p)
		}
	}
}
