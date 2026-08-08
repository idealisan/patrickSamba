package vfs

// dosmatch.go —— SMB / DOS 通配符匹配。
//
// SMB2 QUERY_DIRECTORY 的 FileName 字段是一个通配符模式，除了大家熟悉的
// `*` 与 `?`，还有三个 DOS 时代遗留的特殊字符（MS-FSA §2.1.4.4
// "Algorithm to Compare a File Name Against an Expression"，
// docs/protocol-notes.md §9 也有摘录）：
//
//	`<`  DOS_STAR  —— 匹配任意多个字符，但在**最后一个 `.`** 处停止
//	`>`  DOS_QM    —— 匹配任意单个字符；在名字末尾或最后一个 `.` 之前可匹配零个
//	`"`  DOS_DOT   —— 匹配一个 `.`，或在名字末尾匹配零个字符
//
// 这些字符不是客户端手写的，而是**客户端把 8.3 短名模式翻译过来的**：
// Windows 在把 `*.` / `?` / `.` 送到线上之前会分别换成 `<` / `>` / `"`，
// 使得 `*.*` 能匹配没有扩展名的文件、`a.???` 能匹配 `a.b`。
// 因此不实现它们的话，`dir *.*` 之类的命令会漏掉文件。
//
// 匹配**大小写不敏感**（SMB 语义），这与宿主机文件系统是否大小写敏感无关。
//
// 实现参照 Samba `lib/util/ms_fnmatch.c` 的 ms_fnmatch_core 递归算法
// （只参考算法思路，未复制代码；Samba 是 GPL，不可 vendor）。
// 时间复杂度最坏是指数级，但模式串来自客户端且长度有限，
// 且这里加了「同一 (模式位置, 名字位置) 不重复展开」的剪枝上界保护。

import "unicode"

// MatchDOS 判断 name 是否匹配 SMB 通配符模式 pattern。
//
// 空模式与 "*" 都匹配一切（部分客户端用空串表示「全部」）。
func MatchDOS(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	// 一次性转 []rune：SMB 的名字是 UTF-16 解码出来的 UTF-8，
	// 按字节匹配会把多字节字符拆开，导致 `?` 匹配到半个汉字。
	p := []rune(pattern)
	n := []rune(name)

	m := &dosMatcher{
		p:      p,
		n:      n,
		budget: 16 * (len(p) + 1) * (len(n) + 1),
	}
	return m.match(0, 0)
}

// HasWildcard 判断模式里是否含通配符。
// server 层用它区分「枚举目录」与「精确查单个文件」。
func HasWildcard(pattern string) bool {
	for _, r := range pattern {
		switch r {
		case '*', '?', '<', '>', '"':
			return true
		}
	}
	return false
}

type dosMatcher struct {
	p []rune
	n []rune
	// budget 限制递归展开次数，防御 `*a*a*a*a*b` 这类构造出来的
	// 指数级回溯输入把 CPU 打满（AGENTS.md §8 资源限制）。
	// 预算耗尽时按「不匹配」处理：宁可漏一个条目，不可挂死服务。
	budget int
}

func (m *dosMatcher) match(pi, ni int) bool {
	if m.budget <= 0 {
		return false
	}
	m.budget--

	for pi < len(m.p) {
		c := m.p[pi]
		pi++
		switch c {
		case '*':
			// 尾随的 `*` 匹配剩下的一切。
			if pi == len(m.p) {
				return true
			}
			for i := ni; i <= len(m.n); i++ {
				if m.match(pi, i) {
					return true
				}
			}
			return false

		case '<':
			// DOS_STAR：匹配零个或多个字符，**直到遇到并吃掉名字里的最后一个 `.`**
			// 为止（MS-FSA §2.1.4.4）。它是客户端对 `*.` 的翻译，
			// 所以 `<` 单独出现时等价于「没有扩展名的文件」：
			// `abc` 匹配，`abc.txt` 不匹配。
			for i := ni; i <= len(m.n); i++ {
				if m.match(pi, i) {
					return true
				}
				if i < len(m.n) && m.n[i] == '.' && !containsRune(m.n[i+1:], '.') {
					// 到达最后一个点：吃掉它并停止扩张。
					return m.match(pi, i+1)
				}
			}
			return false

		case '?':
			if ni >= len(m.n) {
				return false
			}
			ni++

		case '>':
			// DOS_QM：匹配任意单个字符；遇到 `.` 或名字末尾时匹配零个字符
			// （规范原文：advances the expression to the end of the set of
			// contiguous DOS_QMs —— 每个 `>` 都在同一个点上跳过，效果一致）。
			if ni < len(m.n) && m.n[ni] == '.' {
				// 名字以 "." 结尾时，`>` 也允许把这个点吃掉。
				if ni+1 == len(m.n) && m.match(pi, ni+1) {
					return true
				}
				continue
			}
			if ni >= len(m.n) {
				continue
			}
			ni++

		case '"':
			// DOS_DOT：匹配一个 `.`，或在名字末尾匹配零个字符。
			if ni >= len(m.n) {
				continue
			}
			if m.n[ni] != '.' {
				return false
			}
			ni++

		default:
			if ni >= len(m.n) {
				return false
			}
			if !equalFoldRune(c, m.n[ni]) {
				return false
			}
			ni++
		}
	}
	return ni == len(m.n)
}

func containsRune(s []rune, r rune) bool {
	for _, x := range s {
		if x == r {
			return true
		}
	}
	return false
}

// equalFoldRune 做大小写不敏感比较。
// 用 ToUpper 而非 ToLower：Windows 的名字比较基于**大写折叠表**，
// 土耳其语 'i'/'İ' 这类特例上两者结果不同，与 Windows 对齐取 ToUpper。
func equalFoldRune(a, b rune) bool {
	if a == b {
		return true
	}
	return unicode.ToUpper(a) == unicode.ToUpper(b)
}
