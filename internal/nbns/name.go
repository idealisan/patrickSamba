package nbns

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// NetBIOS 名字（RFC 1001 §14 / RFC 1002 §4.1）
//
// NetBIOS 名是 **16 字节**定长：名字用空格右填充到 15 字节，第 16 个字节是
// **后缀**（服务类型）。同一个名字配上不同后缀就是不同的 NetBIOS 名 ——
// "MYHOST<0x20>"（文件服务）与 "MYHOST<0x00>"（工作站服务）是两个名字。
//
// 上线格式用「一级编码」（half-ASCII）：每个字节拆成两个半字节，
// 各加上 'A' 变成可打印字符，16 字节 → 32 字节。报文里的 Name 字段是
// 1 字节长度前缀（恒 0x20）+ 32 字节编码 + 1 字节 0x00，共 **34 字节**。
// ---------------------------------------------------------------------------

// NameSize 是 NetBIOS 名的定长字节数（含后缀）。
const NameSize = 16

// wireNameSize 是报文里 Name 字段的长度：0x20 + 32 + 0x00。
const wireNameSize = 34

// 常见的 NetBIOS 后缀（RFC 1001 §12 / Microsoft 扩展）。
const (
	// SuffixWorkstation 是工作站服务名，Windows 用它做主机发现。
	SuffixWorkstation byte = 0x00
	// SuffixFileServer 是文件服务器服务名 —— `\\NAME` 解析的就是这个。
	SuffixFileServer byte = 0x20
	// SuffixMessenger 是信使服务名，老 Windows 的弹消息用到。
	SuffixMessenger byte = 0x03
	// SuffixBrowserGroup 是普通组名，浏览器选举与宣告的目的地。
	SuffixBrowserGroup byte = 0x1e
)

// Name 是一个 NetBIOS 名（名字 + 后缀）。
type Name struct {
	// Label 是名字部分（≤15 字节），比较时大小写不敏感。
	Label string
	// Suffix 是第 16 字节的服务类型。
	Suffix byte
}

// Bytes 返回 16 字节的 NetBIOS 名（空格右填充 + 后缀）。
func (n Name) Bytes() [NameSize]byte {
	var out [NameSize]byte
	label := strings.ToUpper(n.Label)
	if len(label) > NameSize-1 {
		label = label[:NameSize-1]
	}
	copy(out[:], label)
	// 剩余字节留空格（0x20）—— 一级编码不会把空格变成别的字符，
	// 但定长名字的填充必须是空格，这是 NetBIOS 的约定。
	for i := len(label); i < NameSize-1; i++ {
		out[i] = ' '
	}
	out[NameSize-1] = n.Suffix
	return out
}

// wire 返回报文里 34 字节的 Name 字段。
func (n Name) wire() []byte {
	nb := n.Bytes()
	out := make([]byte, 0, wireNameSize)
	out = append(out, 0x20)
	out = append(out, encodeLevel1(nb)...)
	out = append(out, 0x00)
	return out
}

// String 返回可读形式，形如 "MYHOST<20>"（与 nbtstat 的显示一致）。
func (n Name) String() string {
	return fmt.Sprintf("%s<%02x>", strings.ToUpper(n.Label), n.Suffix)
}

// encodeLevel1 做一级编码：16 字节 → 32 字节。
//
// 这个编码的目的不是压缩也不是加密，而是让 NetBIOS 名在 DNS 报文字段里
// 保持**可打印且不含点号** —— 早期实现直接把它塞进 DNS 标签里。
func encodeLevel1(nb [NameSize]byte) []byte {
	out := make([]byte, 0, NameSize*2)
	for _, b := range nb {
		out = append(out, 'A'+(b>>4), 'A'+(b&0x0f))
	}
	return out
}

// decodeLevel1 做一级解码：32 字节 → 16 字节。
//
// 返回 false 表示遇到了非法字符（不是 'A'..'P'）—— 那不是 NetBIOS 名。
func decodeLevel1(b []byte) ([NameSize]byte, bool) {
	var out [NameSize]byte
	if len(b) != NameSize*2 {
		return out, false
	}
	for i := 0; i < NameSize; i++ {
		hi, ok1 := nibble(b[i*2])
		lo, ok2 := nibble(b[i*2+1])
		if !ok1 || !ok2 {
			return out, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

// nibble 把一个一级编码字符还原成半字节。
func nibble(c byte) (byte, bool) {
	if c < 'A' || c > 'P' {
		return 0, false
	}
	return c - 'A', true
}

// namePadding 是名字部分的填充字节集合。
//
// 除了空格（0x20），**NUL（0x00）也必须算填充**：实测 Samba 的
// `nmblookup -A` 发来的节点状态查询里，通配符名字是 `*<0x00>×14`
// 而不是 `*<空格>×14`。只按空格 trim 的话，那个名字会变成
// "*\x00\x00…"，于是任何"这个名字是不是通配符"的判定都失配，
// 表现为节点状态查询**永远不答**（nmblookup 侧看到 "No reply"）。
const namePadding = " \x00"

// labelOf 从 16 字节 NetBIOS 名里取回名字部分（去掉填充与后缀）。
func labelOf(nb [NameSize]byte) string {
	return strings.Trim(string(nb[:NameSize-1]), namePadding)
}

// parseWireName 从报文里的 Name 字段解析出 NetBIOS 名。
func parseWireName(b []byte) (Name, bool) {
	if len(b) < wireNameSize {
		return Name{}, false
	}
	// 长度前缀必须是 0x20（32）—— 一级编码后的固定长度。
	if b[0] != 0x20 {
		// 也见过 DNS 压缩指针（0xC0 开头），本实现不支持（见下）。
		return Name{}, false
	}
	nb, ok := decodeLevel1(b[1 : 1+NameSize*2])
	if !ok {
		return Name{}, false
	}
	return Name{Label: labelOf(nb), Suffix: nb[NameSize-1]}, true
}

// isWildcard 报告这是不是节点状态查询用的通配符名（`*`）。
//
// `nbtstat -A` / `nmblookup -A` 发来的节点状态查询把名字字段写成 `*`，
// 语义是"告诉我这台机器上都有什么名字"，而不是问某个具体名字。
// 按名字匹配去拦它，这一路就永远答不上来。
func (n Name) isWildcard() bool {
	return strings.Trim(n.Label, namePadding) == "*"
}

// namesEqual 比较两个 NetBIOS 名：名字部分大小写不敏感，后缀必须一致。
//
// 大小写不敏感是 NetBIOS 的语义（它本来就是大写世界）——
// 按字节严格比较会让 "myserver" 查不到 "MYSERVER"。
func namesEqual(a, b Name) bool {
	return a.Suffix == b.Suffix && strings.EqualFold(a.Label, b.Label)
}
