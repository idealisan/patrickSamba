package oscap

// instanceid.go —— InstanceID 编进旁路库文件名用的公共规则。
//
// # 为什么放在 port 层而不是某个 adapter 里
//
// 两处旁路存储要用**同一份**规则把 InstanceID 变成文件名片段：
//
//   - internal/oscap/builtin（六项能力旁路）  .stupidsamba-oscap-<root哈希>-<实例>.db
//   - internal/vfs/metadata_windows.go        metadata-<root哈希>-<实例>.db
//     （Windows 上的 POSIX 属主/权限旁路）
//
// 两者的 bbolt 库都按 path 拿 flock（OI-1 的教训）：清洗规则若各自实现，
// 将来一处改了另一处不知道，「同一实例重启后复用同一份元数据」就会在
// 其中一侧悄悄失效。单一真源放在这里，两侧都只调用不复制。

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// maxInstanceTagLen 是清洗后的实例标签在文件名里占用的最大字节数。
//
// 文件名总长上限通常是 255 字节：root 哈希(16) + 前后缀 + 实例标签 + 实例哈希(8)
// 全部留足余量。标签只影响**可读性**（运维 ls 一眼能认出是哪个监听端点），
// 不承担唯一性 —— 那是 InstanceIDSuffix 里追加的哈希的职责，所以截断无损正确性。
const maxInstanceTagLen = 64

// SanitizeInstanceID 把 InstanceID 清洗成**文件名安全**的短标签。
//
// 规则：保留字母、数字与 '-' '_' '.'；其余字符一律折叠成单个 '_'
// （监听端点 "127.0.0.1:4451" 的冒号、IPv6 地址的冒号、Windows 路径的反斜杠、
// 空格等都在此列）；折叠出的连续下划线合并为一个，首尾的下划线去掉；
// 结果截断到 maxInstanceTagLen 字节。
//
// 输出恒为 ASCII 可打印字符且不含路径分隔符，因此可以直接拼进文件名；
// 输入为空时输出也为空。清洗**不做唯一性保证**："a:b" 与 "a_b" 会得到
// 同一个标签 —— 区分不同原始值由 InstanceIDSuffix 的哈希部分负责。
func SanitizeInstanceID(id string) string {
	if id == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(id))
	prevFolded := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
			prevFolded = false
		default:
			if !prevFolded {
				b.WriteByte('_')
				prevFolded = true
			}
		}
	}
	// 截断安全：上面的 switch 保证输出只含 ASCII，按字节切不会切碎字符。
	s := b.String()
	if len(s) > maxInstanceTagLen {
		s = s[:maxInstanceTagLen]
	}
	return strings.Trim(s, "_-")
}

// InstanceIDSuffix 把非空 InstanceID 编成一个**确定性**的文件名片段：
// "<清洗后的可读标签>-<原始值的 fnv32a 八位十六进制>"。
//
// 为什么除了清洗还要带哈希：清洗会折叠信息（"a:b" 与 "a:b2" 折叠后可能相同
// 也可能只差一个字符被截断），而「不同实例必得不同文件名」是硬要求 ——
// 哈希直接取自**原始**字符串，两个不同的 InstanceID 必然得到不同的片段，
// 清洗只负责让人眼能认出来。同一 InstanceID 两次调用必得同一结果，
// 这就是「同一实例重启后得到同一路径、复用同一份元数据」的兑现方式。
//
// id 为空时返回 ok=false：调用方应退回历史文件名（不带任何实例片段），
// 保持与旧版本逐字节一致（测试与库直连场景零变化）。
func InstanceIDSuffix(id string) (suffix string, ok bool) {
	if id == "" {
		return "", false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	s := SanitizeInstanceID(id)
	if s != "" {
		return fmt.Sprintf("%s-%08x", s, h.Sum32()), true
	}
	// 标签被清空（例如 InstanceID 全是非文件名字符）也要保留哈希：
	// 「不同实例不同名」比「好看」重要。
	return fmt.Sprintf("%08x", h.Sum32()), true
}
