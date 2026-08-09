package wire

import (
	"strings"
	"testing"
)

// FileStreamInformation 链的对齐与截断测试。
//
// 缘起：VFS 支持通用 named stream（xattr `user.DosStream.*`）之后，
// 这条链的形状变了 —— 以前实际最多 3 条、流名都是 `::$DATA` 这种短名，
// 现在条数不设上限、单个流名可达 234 字节（UTF-16 上线翻倍到 468）。
// 「条数多」和「名字长且长度各异」正是 NextEntryOffset 链与 8 字节对齐
// 最容易翻车的地方，而原有用例只覆盖了 2 条短名。

// TestStreamInfoChainAlignmentManyEntries 用**长度刻意错开**的一批流名，
// 验证每条记录都落在 8 字节边界上、NextEntryOffset 串得起来、最后一条为 0。
//
// 名字长度取 1..N 是有意的：只有当 24+len(name) 恰好不是 8 的倍数时，
// padding 逻辑才会被真正考验；固定长度的名字可能碰巧全部对齐从而漏掉 bug。
func TestStreamInfoChainAlignmentManyEntries(t *testing.T) {
	var streams []FileStreamInfo
	streams = append(streams, FileStreamInfo{
		Name: "::$DATA", StreamSize: 13, StreamAllocationSize: 4096,
	})
	// 名字长度 1..12 逐个递增，覆盖 mod 8 的每一种余数（UTF-16 后是偶数字节，
	// 所以 24+2*len 的余数会在 0/2/4/6 之间轮转）。
	for n := 1; n <= 12; n++ {
		streams = append(streams, FileStreamInfo{
			Name:                 ":" + strings.Repeat("x", n) + ":$DATA",
			StreamSize:           int64(n),
			StreamAllocationSize: int64(n) * 2,
		})
	}
	// 再来一条接近上限的超长名（VFS 允许 234 字节流名）。
	streams = append(streams, FileStreamInfo{
		Name:                 ":" + strings.Repeat("L", 234) + ":$DATA",
		StreamSize:           1,
		StreamAllocationSize: 1,
	})

	buf := AppendStreamInfoChain(nil, streams)

	// 1. 手工走一遍链，逐条检查偏移对齐与边界。
	pos := 0
	for i := 0; ; i++ {
		if pos%8 != 0 {
			t.Fatalf("第 %d 条记录起始偏移 %d 不是 8 的倍数", i, pos)
		}
		if pos+FileStreamInfoFixedSize > len(buf) {
			t.Fatalf("第 %d 条记录的定长部分越界（pos=%d len=%d）", i, pos, len(buf))
		}
		next := int(le.Uint32(buf[pos:]))
		nameLen := int(le.Uint32(buf[pos+4:]))
		if pos+FileStreamInfoFixedSize+nameLen > len(buf) {
			t.Fatalf("第 %d 条记录的流名越界（pos=%d nameLen=%d len=%d）", i, pos, nameLen, len(buf))
		}
		if next == 0 {
			if i != len(streams)-1 {
				t.Errorf("链在第 %d 条就断了，共应有 %d 条", i, len(streams))
			}
			break
		}
		if next%8 != 0 {
			t.Errorf("第 %d 条的 NextEntryOffset = %d，不是 8 的倍数", i, next)
		}
		if next < FileStreamInfoFixedSize+nameLen {
			t.Errorf("第 %d 条的 NextEntryOffset = %d 小于本条实际长度 %d，会导致重叠",
				i, next, FileStreamInfoFixedSize+nameLen)
		}
		pos += next
	}

	// 2. 解析回来必须与原始输入完全一致（名字、大小都不能错位）。
	got, err := ParseStreamInfoChain(buf)
	if err != nil {
		t.Fatalf("ParseStreamInfoChain: %v", err)
	}
	if len(got) != len(streams) {
		t.Fatalf("解析出 %d 条，期望 %d 条", len(got), len(streams))
	}
	for i := range streams {
		if got[i] != streams[i] {
			t.Errorf("第 %d 条往返不一致:\n got %+v\nwant %+v", i, got[i], streams[i])
		}
	}
}

// TestStreamInfoChainSingleEntryNoTrailingPad：只有一条时不得有尾部填充。
//
// 尾部多填几个字节不会让解析失败（NextEntryOffset=0 就停了），
// 但会让 QUERY_INFO 响应的 OutputBufferLength 比实际内容大，
// 与真实 Samba 的字节数对不上，抓包比对时会误判成我们编码有问题。
func TestStreamInfoChainSingleEntryNoTrailingPad(t *testing.T) {
	s := FileStreamInfo{Name: ":a:$DATA", StreamSize: 1, StreamAllocationSize: 8}
	buf := AppendStreamInfoChain(nil, []FileStreamInfo{s})

	want := FileStreamInfoFixedSize + len(EncodeUTF16LE(s.Name))
	if len(buf) != want {
		t.Errorf("单条链长度 = %d, 期望 %d（不应有尾部对齐填充）", len(buf), want)
	}
	if next := le.Uint32(buf[0:]); next != 0 {
		t.Errorf("单条链的 NextEntryOffset = %d, 期望 0", next)
	}
}

// TestStreamInfoChainPrefixIsValidChain 验证「取前 n 条重新编码」得到的
// 仍是一条合法的链 —— 这是 command 层做 BUFFER_OVERFLOW 截断的前提。
//
// 截断如果直接切字节，最后一条的 NextEntryOffset 会指向缓冲区之外，
// 客户端顺着它走就会读到垃圾。command 层因此改用「重新编码前缀」，
// 这里把该做法的正确性钉住。
func TestStreamInfoChainPrefixIsValidChain(t *testing.T) {
	all := []FileStreamInfo{
		{Name: "::$DATA", StreamSize: 10},
		{Name: ":AFP_Resource:$DATA", StreamSize: 20},
		{Name: ":com.apple.quarantine:$DATA", StreamSize: 30},
		{Name: ":" + strings.Repeat("z", 100) + ":$DATA", StreamSize: 40},
	}
	full := len(AppendStreamInfoChain(nil, all))

	for n := 1; n <= len(all); n++ {
		buf := AppendStreamInfoChain(nil, all[:n])
		if n < len(all) && len(buf) >= full {
			t.Errorf("前 %d 条的编码长度 %d 不应达到全量的 %d", n, len(buf), full)
		}
		got, err := ParseStreamInfoChain(buf)
		if err != nil {
			t.Fatalf("前 %d 条解析失败: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("前 %d 条解析出 %d 条", n, len(got))
		}
		for i := 0; i < n; i++ {
			if got[i].Name != all[i].Name {
				t.Errorf("前 %d 条里第 %d 条名字 = %q, 期望 %q", n, i, got[i].Name, all[i].Name)
			}
		}
	}
}
