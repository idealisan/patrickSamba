// Package mdns 实现进程内的 mDNS/DNS-SD responder（RFC 6762 / RFC 6763）。
//
// 严格遵守 AGENTS.md C4：所有报文都由本进程自己在 224.0.0.251:5353 与
// [ff02::fb]:5353 上收发，不调用 avahi / Bonjour / systemd-resolved 的
// D-Bus 或 socket 接口。
//
// 字节序：DNS/mDNS 报文**全部是大端**（RFC 1035 §2.3.2），与 SMB2 报文体的
// 小端正好相反。本包内一律显式使用 binary.BigEndian，不写任何隐式转换。
package mdns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Type 是 DNS 资源记录类型（RFC 1035 §3.2.2）。
type Type uint16

const (
	TypeA    Type = 1   // RFC 1035 §3.4.1
	TypePTR  Type = 12  // RFC 1035 §3.3.12
	TypeTXT  Type = 16  // RFC 1035 §3.3.14
	TypeAAAA Type = 28  // RFC 3596 §2.1
	TypeSRV  Type = 33  // RFC 2782
	TypeNSEC Type = 47  // RFC 4034 §4；mDNS 中的用法见 RFC 6762 §6.1
	TypeANY  Type = 255 // QTYPE=* （RFC 1035 §3.2.3）
)

func (t Type) String() string {
	switch t {
	case TypeA:
		return "A"
	case TypePTR:
		return "PTR"
	case TypeTXT:
		return "TXT"
	case TypeAAAA:
		return "AAAA"
	case TypeSRV:
		return "SRV"
	case TypeNSEC:
		return "NSEC"
	case TypeANY:
		return "ANY"
	default:
		return "TYPE" + strconv.Itoa(int(t))
	}
}

// Class 是 DNS 类（RFC 1035 §3.2.4）。
type Class uint16

const (
	ClassIN  Class = 1   // Internet
	ClassANY Class = 255 // QCLASS=* （RFC 1035 §3.2.5）
)

const (
	// classFlagCacheFlush 是应答记录 rrclass 的最高位（RFC 6762 §10.2）：
	// 置 1 表示接收方应先刷掉同名同类型的旧缓存。
	classFlagCacheFlush uint16 = 0x8000
	// classFlagUnicast 是查询 qclass 的最高位（RFC 6762 §5.4）：
	// 置 1 表示 QU（请求单播回应）。
	classFlagUnicast uint16 = 0x8000
	// classMask 取出真正的 class 值。
	classMask uint16 = 0x7fff
)

// Flags 是 DNS 报文头的标志位字段（RFC 1035 §4.1.1）。
type Flags uint16

const (
	FlagResponse      Flags = 1 << 15 // QR
	FlagAuthoritative Flags = 1 << 10 // AA
	FlagTruncated     Flags = 1 << 9  // TC
)

// IsResponse 判断 QR 位。
func (f Flags) IsResponse() bool { return f&FlagResponse != 0 }

// Opcode 返回 OPCODE 字段（RFC 1035 §4.1.1，mDNS 只用 0 = QUERY）。
func (f Flags) Opcode() uint8 { return uint8(f>>11) & 0x0f }

// RCode 返回 RCODE 字段。
func (f Flags) RCode() uint8 { return uint8(f) & 0x0f }

// Truncated 判断 TC 位（RFC 6762 §7.2 用它表示"还有已知答案在后续报文里"）。
func (f Flags) Truncated() bool { return f&FlagTruncated != 0 }

// 报文格式相关上限。
const (
	// headerLen 是 DNS 报文头固定长度（RFC 1035 §4.1.1）。
	headerLen = 12
	// maxNameLen 是域名 wire 格式总长上限，含结尾的根标签（RFC 1035 §2.3.4）。
	maxNameLen = 255
	// maxLabelLen 是单个 label 的长度上限（RFC 1035 §2.3.4）。
	maxLabelLen = 63
	// maxTXTStringLen 是单条 character-string 的长度上限（RFC 1035 §3.3.14）。
	maxTXTStringLen = 255
	// maxCompressionJumps 是压缩指针跳转次数预算。
	// RFC 1035 §4.1.4 没有规定实现如何防环，这个上限与"指针必须指向更靠前
	// 的位置"一起构成双重保险。
	maxCompressionJumps = 64
)

// 解析错误。全部是可预期的外部输入错误，调用方应丢弃报文而不是崩溃。
var (
	ErrTruncated   = errors.New("mdns: 报文被截断")
	ErrNameTooLong = errors.New("mdns: 域名超过 255 字节")
	ErrBadPointer  = errors.New("mdns: 非法的压缩指针（必须指向报文中更靠前的位置）")
	ErrManyJumps   = errors.New("mdns: 压缩指针跳转次数过多")
	ErrBadLabel    = errors.New("mdns: 非法的 label 类型（只支持 0x00 与 0xC0）")
	ErrNoRData     = errors.New("mdns: 资源记录缺少 rdata")
)

// ---------------------------------------------------------------- 域名

// splitName 把表示形式域名拆成各 label 的原始字节。
//
// 支持 RFC 1035 §5.1 的转义：`\.`、`\\`、`\DDD`（三位十进制）。
// 末尾的点表示根，可有可无（本包一律当作 FQDN 处理）。
func splitName(name string) ([][]byte, error) {
	if name == "" || name == "." {
		return nil, nil
	}
	var (
		labels [][]byte
		label  []byte
	)
	emit := func() error {
		if len(label) == 0 {
			return fmt.Errorf("mdns: 域名 %q 含空 label", name)
		}
		if len(label) > maxLabelLen {
			return fmt.Errorf("mdns: 域名 %q 的 label 长度 %d 超过上限 %d", name, len(label), maxLabelLen)
		}
		labels = append(labels, label)
		label = nil
		return nil
	}

	for i := 0; i < len(name); {
		switch c := name[i]; {
		case c == '\\':
			i++
			if i >= len(name) {
				return nil, fmt.Errorf("mdns: 域名 %q 以孤立的反斜杠结尾", name)
			}
			if isDigit(name[i]) {
				if i+2 >= len(name) || !isDigit(name[i+1]) || !isDigit(name[i+2]) {
					return nil, fmt.Errorf("mdns: 域名 %q 中的 \\DDD 转义不完整", name)
				}
				v := int(name[i]-'0')*100 + int(name[i+1]-'0')*10 + int(name[i+2]-'0')
				if v > 255 {
					return nil, fmt.Errorf("mdns: 域名 %q 中的 \\%03d 超出字节范围", name, v)
				}
				label = append(label, byte(v))
				i += 3
			} else {
				label = append(label, name[i])
				i++
			}
		case c == '.':
			if err := emit(); err != nil {
				return nil, err
			}
			i++
		default:
			label = append(label, c)
			i++
		}
	}
	if len(label) > 0 {
		if err := emit(); err != nil {
			return nil, err
		}
	}
	return labels, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// packName 把表示形式域名编码成 wire 格式。
//
// 不做压缩：RFC 1035 §4.1.4 的压缩是可选优化，不压缩永远合法，
// 而且能让编码结果确定、可做 golden test。
func packName(b []byte, name string) ([]byte, error) {
	labels, err := splitName(name)
	if err != nil {
		return nil, err
	}
	n := 1 // 结尾的根标签
	for _, l := range labels {
		n += 1 + len(l)
	}
	if n > maxNameLen {
		return nil, fmt.Errorf("mdns: 域名 %q 编码后 %d 字节，超过上限 %d: %w", name, n, maxNameLen, ErrNameTooLong)
	}
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0), nil
}

// appendEscapedLabel 把一个 label 的原始字节写成表示形式（转义点、反斜杠与不可打印字符）。
func appendEscapedLabel(sb *strings.Builder, label []byte) {
	for _, c := range label {
		switch {
		case c == '.' || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c < 0x20 || c > 0x7e:
			sb.WriteByte('\\')
			sb.WriteByte('0' + c/100)
			sb.WriteByte('0' + (c/10)%10)
			sb.WriteByte('0' + c%10)
		default:
			sb.WriteByte(c)
		}
	}
}

// unpackName 解析 msg 中 off 处的域名。
//
// 返回表示形式（带结尾点）与**本层**结束后的下一个偏移；遇到压缩指针时，
// 下一个偏移是指针自身之后的位置（RFC 1035 §4.1.4）。
//
// 防护：
//   - 每次读取前校验长度，绝不裸切片；
//   - 指针目标必须严格小于指针自身的位置，偏移单调递减，从原理上杜绝环路；
//   - 额外再加一个跳转次数预算做保险；
//   - 累计长度超过 255 字节即报错。
func unpackName(msg []byte, off int) (string, int, error) {
	if off < 0 {
		return "", 0, ErrTruncated
	}
	var (
		sb       strings.Builder
		next     = -1 // 第一次跳转前记录的返回位置
		jumps    int
		consumed = 1 // 结尾根标签的 1 字节
		cur      = off
	)
	for {
		if cur >= len(msg) {
			return "", 0, ErrTruncated
		}
		l := int(msg[cur])
		switch l & 0xc0 {
		case 0x00:
			if l == 0 {
				cur++
				if next < 0 {
					next = cur
				}
				if sb.Len() == 0 {
					return ".", next, nil
				}
				return sb.String(), next, nil
			}
			if cur+1+l > len(msg) {
				return "", 0, ErrTruncated
			}
			consumed += 1 + l
			if consumed > maxNameLen {
				return "", 0, ErrNameTooLong
			}
			appendEscapedLabel(&sb, msg[cur+1:cur+1+l])
			sb.WriteByte('.')
			cur += 1 + l
		case 0xc0:
			if cur+2 > len(msg) {
				return "", 0, ErrTruncated
			}
			// 指针是 14 位偏移，大端。
			ptr := int(binary.BigEndian.Uint16(msg[cur:cur+2]) & 0x3fff)
			if next < 0 {
				next = cur + 2
			}
			if ptr >= cur {
				return "", 0, ErrBadPointer
			}
			jumps++
			if jumps > maxCompressionJumps {
				return "", 0, ErrManyJumps
			}
			cur = ptr
		default:
			// 0x40（扩展 label）与 0x80（保留）都不在 mDNS 的使用范围内。
			return "", 0, ErrBadLabel
		}
	}
}

// EqualName 比较两个域名是否相同。
//
// RFC 6762 §16 与 RFC 1035 §2.3.3：DNS 名字大小写不敏感；
// 这里同时把结尾的点归一化，"local" 与 "local." 视为同一个名字。
func EqualName(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// FQDN 补上结尾的点。
func FQDN(s string) string {
	if s == "" {
		return "."
	}
	if strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

// EscapeLabel 把任意字符串转义成可以安全拼进域名的单个 label 表示形式。
//
// DNS-SD 实例名允许包含点和空格（RFC 6763 §4.1.1），拼域名前必须转义。
func EscapeLabel(s string) string {
	var sb strings.Builder
	appendEscapedLabel(&sb, []byte(s))
	return sb.String()
}

// ---------------------------------------------------------------- rdata

// RData 是一条资源记录的类型相关数据。
type RData interface {
	// Type 返回该 rdata 对应的记录类型。
	Type() Type
	// pack 把 rdata 追加到 b（大端）。
	pack(b []byte) ([]byte, error)
	fmt.Stringer
}

// A 是 IPv4 地址记录（RFC 1035 §3.4.1）。
type A struct{ IP net.IP }

func (A) Type() Type { return TypeA }

func (r A) pack(b []byte) ([]byte, error) {
	v4 := r.IP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("mdns: A 记录需要 IPv4 地址，得到 %v", r.IP)
	}
	return append(b, v4...), nil
}

func (r A) String() string { return r.IP.String() }

// AAAA 是 IPv6 地址记录（RFC 3596 §2.2）。
type AAAA struct{ IP net.IP }

func (AAAA) Type() Type { return TypeAAAA }

func (r AAAA) pack(b []byte) ([]byte, error) {
	v16 := r.IP.To16()
	if v16 == nil || r.IP.To4() != nil {
		return nil, fmt.Errorf("mdns: AAAA 记录需要 IPv6 地址，得到 %v", r.IP)
	}
	return append(b, v16...), nil
}

func (r AAAA) String() string { return r.IP.String() }

// PTR 是指针记录（RFC 1035 §3.3.12），DNS-SD 用它做服务枚举。
type PTR struct{ Target string }

func (PTR) Type() Type { return TypePTR }

func (r PTR) pack(b []byte) ([]byte, error) { return packName(b, r.Target) }

func (r PTR) String() string { return r.Target }

// SRV 是服务定位记录（RFC 2782）。
type SRV struct {
	Priority uint16
	Weight   uint16
	Port     uint16
	Target   string
}

func (SRV) Type() Type { return TypeSRV }

func (r SRV) pack(b []byte) ([]byte, error) {
	b = binary.BigEndian.AppendUint16(b, r.Priority)
	b = binary.BigEndian.AppendUint16(b, r.Weight)
	b = binary.BigEndian.AppendUint16(b, r.Port)
	return packName(b, r.Target)
}

func (r SRV) String() string {
	return fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Target)
}

// TXT 是文本记录（RFC 1035 §3.3.14），DNS-SD 的键值对载体（RFC 6763 §6）。
type TXT struct{ Strings []string }

func (TXT) Type() Type { return TypeTXT }

func (r TXT) pack(b []byte) ([]byte, error) {
	if len(r.Strings) == 0 {
		// RFC 6763 §6.1：DNS-SD 的空 TXT 必须是一个长度为 0 的
		// character-string，而不是长度为 0 的 rdata。
		return append(b, 0), nil
	}
	for _, s := range r.Strings {
		if len(s) > maxTXTStringLen {
			return nil, fmt.Errorf("mdns: TXT 条目 %d 字节，超过上限 %d", len(s), maxTXTStringLen)
		}
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	return b, nil
}

func (r TXT) String() string { return strings.Join(r.Strings, "|") }

// NSEC 用于否定回答：声明"这个名字上只存在下列类型的记录"（RFC 6762 §6.1）。
type NSEC struct {
	NextDomain string
	Types      []Type
}

func (NSEC) Type() Type { return TypeNSEC }

func (r NSEC) pack(b []byte) ([]byte, error) {
	b, err := packName(b, r.NextDomain)
	if err != nil {
		return nil, err
	}
	return packTypeBitmap(b, r.Types), nil
}

func (r NSEC) String() string {
	parts := make([]string, 0, len(r.Types)+1)
	parts = append(parts, r.NextDomain)
	for _, t := range r.Types {
		parts = append(parts, t.String())
	}
	return strings.Join(parts, " ")
}

// Raw 承载本包不认识的记录类型，保证解析不因未知类型而失败。
type Raw struct {
	RRType Type
	Data   []byte
}

func (r Raw) Type() Type { return r.RRType }

func (r Raw) pack(b []byte) ([]byte, error) { return append(b, r.Data...), nil }

func (r Raw) String() string { return fmt.Sprintf("%d bytes", len(r.Data)) }

// packTypeBitmap 按 RFC 4034 §4.1.2 的窗口块格式编码类型位图。
func packTypeBitmap(b []byte, types []Type) []byte {
	if len(types) == 0 {
		return b
	}
	sorted := make([]Type, len(types))
	copy(sorted, types)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	for i := 0; i < len(sorted); {
		win := byte(sorted[i] >> 8)
		var bitmap [32]byte
		last := 0
		j := i
		for ; j < len(sorted) && byte(sorted[j]>>8) == win; j++ {
			lo := byte(sorted[j])
			bitmap[lo/8] |= 0x80 >> (lo % 8)
			if int(lo)/8 > last {
				last = int(lo) / 8
			}
		}
		b = append(b, win, byte(last+1))
		b = append(b, bitmap[:last+1]...)
		i = j
	}
	return b
}

// unpackTypeBitmap 解析类型位图。
func unpackTypeBitmap(b []byte) ([]Type, error) {
	var types []Type
	for off := 0; off < len(b); {
		if off+2 > len(b) {
			return nil, ErrTruncated
		}
		win := b[off]
		l := int(b[off+1])
		off += 2
		if l < 1 || l > 32 {
			return nil, fmt.Errorf("mdns: NSEC 位图窗口长度 %d 非法（应为 1-32）", l)
		}
		if off+l > len(b) {
			return nil, ErrTruncated
		}
		for i := 0; i < l; i++ {
			v := b[off+i]
			for bit := 0; bit < 8; bit++ {
				if v&(0x80>>bit) != 0 {
					types = append(types, Type(uint16(win)<<8|uint16(i*8+bit)))
				}
			}
		}
		off += l
	}
	return types, nil
}

// ---------------------------------------------------------------- 记录与问题

// Question 是一条查询问题（RFC 1035 §4.1.2）。
type Question struct {
	Name  string
	Type  Type
	Class Class
	// Unicast 是 RFC 6762 §5.4 的 QU 位：请求以单播方式回应。
	Unicast bool
}

func (q Question) String() string {
	s := fmt.Sprintf("%s %s", q.Name, q.Type)
	if q.Unicast {
		s += " (QU)"
	}
	return s
}

// Record 是一条资源记录（RFC 1035 §4.1.3）。
type Record struct {
	Name  string
	Class Class
	// CacheFlush 是 RFC 6762 §10.2 的 cache-flush 位（只在应答里有意义）。
	CacheFlush bool
	// TTL 单位为秒。TTL=0 表示该记录正在消失（RFC 6762 §10.1 goodbye）。
	TTL  uint32
	Data RData
}

// Type 返回记录类型，由 rdata 决定。
func (r Record) Type() Type {
	if r.Data == nil {
		return 0
	}
	return r.Data.Type()
}

func (r Record) String() string {
	return fmt.Sprintf("%s %d %s %v", r.Name, r.TTL, r.Type(), r.Data)
}

// SameKey 判断两条记录的 (name, type, class) 是否相同（名字大小写不敏感）。
func (r Record) SameKey(o Record) bool {
	return r.Type() == o.Type() && r.Class == o.Class && EqualName(r.Name, o.Name)
}

// Equal 判断两条记录是否等价（忽略 TTL 与 cache-flush 位）。
//
// 用于 known-answer suppression（RFC 6762 §7.1）与冲突判定（§9）。
func (r Record) Equal(o Record) bool {
	return r.SameKey(o) && rdataEqual(r.Data, o.Data)
}

// rdataEqual 比较 rdata。域名字段按 DNS 规则做大小写不敏感比较。
func rdataEqual(a, b RData) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Type() != b.Type() {
		return false
	}
	switch x := a.(type) {
	case A:
		y, ok := b.(A)
		return ok && x.IP.Equal(y.IP)
	case AAAA:
		y, ok := b.(AAAA)
		return ok && x.IP.Equal(y.IP)
	case PTR:
		y, ok := b.(PTR)
		return ok && EqualName(x.Target, y.Target)
	case SRV:
		y, ok := b.(SRV)
		return ok && x.Priority == y.Priority && x.Weight == y.Weight &&
			x.Port == y.Port && EqualName(x.Target, y.Target)
	case TXT:
		y, ok := b.(TXT)
		if !ok || len(x.Strings) != len(y.Strings) {
			return false
		}
		for i := range x.Strings {
			if x.Strings[i] != y.Strings[i] {
				return false
			}
		}
		return true
	case NSEC:
		y, ok := b.(NSEC)
		if !ok || !EqualName(x.NextDomain, y.NextDomain) || len(x.Types) != len(y.Types) {
			return false
		}
		for i := range x.Types {
			if x.Types[i] != y.Types[i] {
				return false
			}
		}
		return true
	case Raw:
		y, ok := b.(Raw)
		if !ok || len(x.Data) != len(y.Data) {
			return false
		}
		for i := range x.Data {
			if x.Data[i] != y.Data[i] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Matches 判断记录是否回答了某个问题（RFC 6762 §6）。
func (r Record) Matches(q Question) bool {
	if !EqualName(r.Name, q.Name) {
		return false
	}
	if q.Class != ClassANY && q.Class != r.Class {
		return false
	}
	return q.Type == TypeANY || q.Type == r.Type()
}

// ---------------------------------------------------------------- 报文

// Message 是一个完整的 DNS 报文（RFC 1035 §4.1）。
type Message struct {
	ID          uint16
	Flags       Flags
	Questions   []Question
	Answers     []Record
	Authorities []Record // mDNS probing 用它承载提议的记录（RFC 6762 §8.2）
	Additionals []Record
}

// Pack 把报文编码为 wire 格式（大端）。
func (m *Message) Pack() ([]byte, error) {
	b := make([]byte, 0, 512)

	b = binary.BigEndian.AppendUint16(b, m.ID)
	b = binary.BigEndian.AppendUint16(b, uint16(m.Flags))
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Questions)))
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Answers)))
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Authorities)))
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Additionals)))

	var err error
	for _, q := range m.Questions {
		if b, err = packQuestion(b, q); err != nil {
			return nil, err
		}
	}
	for _, group := range [][]Record{m.Answers, m.Authorities, m.Additionals} {
		for _, r := range group {
			if b, err = packRecord(b, r); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func packQuestion(b []byte, q Question) ([]byte, error) {
	b, err := packName(b, q.Name)
	if err != nil {
		return nil, err
	}
	b = binary.BigEndian.AppendUint16(b, uint16(q.Type))
	cls := uint16(q.Class)
	if q.Unicast {
		cls |= classFlagUnicast
	}
	return binary.BigEndian.AppendUint16(b, cls), nil
}

func packRecord(b []byte, r Record) ([]byte, error) {
	if r.Data == nil {
		return nil, ErrNoRData
	}
	b, err := packName(b, r.Name)
	if err != nil {
		return nil, err
	}
	b = binary.BigEndian.AppendUint16(b, uint16(r.Data.Type()))
	cls := uint16(r.Class)
	if r.CacheFlush {
		cls |= classFlagCacheFlush
	}
	b = binary.BigEndian.AppendUint16(b, cls)
	b = binary.BigEndian.AppendUint32(b, r.TTL)

	// rdlength 先占位，写完 rdata 再回填（大端）。
	lenOff := len(b)
	b = append(b, 0, 0)
	b, err = r.Data.pack(b)
	if err != nil {
		return nil, err
	}
	n := len(b) - lenOff - 2
	if n > 0xffff {
		return nil, fmt.Errorf("mdns: 记录 %s 的 rdata %d 字节，超过 65535", r.Name, n)
	}
	binary.BigEndian.PutUint16(b[lenOff:lenOff+2], uint16(n))
	return b, nil
}

// Unpack 解析 wire 格式报文。
//
// 任何越界、非法指针、非法长度都返回错误，绝不 panic：
// 这些字节直接来自网络，必须当成敌意输入对待（AGENTS.md §8）。
func Unpack(buf []byte) (*Message, error) {
	if len(buf) < headerLen {
		return nil, ErrTruncated
	}
	m := &Message{
		ID:    binary.BigEndian.Uint16(buf[0:2]),
		Flags: Flags(binary.BigEndian.Uint16(buf[2:4])),
	}
	var (
		qd = int(binary.BigEndian.Uint16(buf[4:6]))
		an = int(binary.BigEndian.Uint16(buf[6:8]))
		ns = int(binary.BigEndian.Uint16(buf[8:10]))
		ar = int(binary.BigEndian.Uint16(buf[10:12]))
	)
	p := &parser{msg: buf, off: headerLen}

	// 每条问题至少 5 字节、每条记录至少 11 字节，先用剩余长度粗筛，
	// 避免伪造的巨大计数值导致大块预分配。
	if qd*5+(an+ns+ar)*11 > len(buf)-headerLen {
		return nil, ErrTruncated
	}

	var err error
	if qd > 0 {
		m.Questions = make([]Question, qd)
		for i := 0; i < qd; i++ {
			if m.Questions[i], err = p.question(); err != nil {
				return nil, err
			}
		}
	}
	if m.Answers, err = p.records(an); err != nil {
		return nil, err
	}
	if m.Authorities, err = p.records(ns); err != nil {
		return nil, err
	}
	if m.Additionals, err = p.records(ar); err != nil {
		return nil, err
	}
	return m, nil
}

// parser 是一个带边界检查的顺序读取器。
type parser struct {
	msg []byte
	off int
}

func (p *parser) remaining() int { return len(p.msg) - p.off }

func (p *parser) uint16() (uint16, error) {
	if p.remaining() < 2 {
		return 0, ErrTruncated
	}
	v := binary.BigEndian.Uint16(p.msg[p.off : p.off+2])
	p.off += 2
	return v, nil
}

func (p *parser) uint32() (uint32, error) {
	if p.remaining() < 4 {
		return 0, ErrTruncated
	}
	v := binary.BigEndian.Uint32(p.msg[p.off : p.off+4])
	p.off += 4
	return v, nil
}

func (p *parser) name() (string, error) {
	name, next, err := unpackName(p.msg, p.off)
	if err != nil {
		return "", err
	}
	p.off = next
	return name, nil
}

func (p *parser) question() (Question, error) {
	var q Question
	var err error
	if q.Name, err = p.name(); err != nil {
		return q, err
	}
	t, err := p.uint16()
	if err != nil {
		return q, err
	}
	c, err := p.uint16()
	if err != nil {
		return q, err
	}
	q.Type = Type(t)
	q.Class = Class(c & classMask)
	q.Unicast = c&classFlagUnicast != 0
	return q, nil
}

func (p *parser) records(n int) ([]Record, error) {
	if n == 0 {
		return nil, nil
	}
	out := make([]Record, n)
	for i := 0; i < n; i++ {
		r, err := p.record()
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func (p *parser) record() (Record, error) {
	var r Record
	name, err := p.name()
	if err != nil {
		return r, err
	}
	r.Name = name

	t, err := p.uint16()
	if err != nil {
		return r, err
	}
	c, err := p.uint16()
	if err != nil {
		return r, err
	}
	ttl, err := p.uint32()
	if err != nil {
		return r, err
	}
	rdlen, err := p.uint16()
	if err != nil {
		return r, err
	}
	if int(rdlen) > p.remaining() {
		return r, ErrTruncated
	}

	r.Class = Class(c & classMask)
	r.CacheFlush = c&classFlagCacheFlush != 0
	r.TTL = ttl

	start := p.off
	data, err := parseRData(p.msg, Type(t), start, int(rdlen))
	if err != nil {
		return r, err
	}
	// 无论 rdata 解析消耗了多少（压缩指针会让它看起来更短），
	// 都以 rdlength 为准前进，避免一条畸形记录带偏后续所有记录。
	p.off = start + int(rdlen)
	r.Data = data
	return r, nil
}

// parseRData 解析 rdata。off/rdlen 已由调用方保证在 msg 范围内。
func parseRData(msg []byte, typ Type, off, rdlen int) (RData, error) {
	body := msg[off : off+rdlen]
	switch typ {
	case TypeA:
		if rdlen != net.IPv4len {
			return nil, fmt.Errorf("mdns: A 记录 rdlength=%d，应为 4", rdlen)
		}
		ip := make(net.IP, net.IPv4len)
		copy(ip, body)
		return A{IP: ip}, nil

	case TypeAAAA:
		if rdlen != net.IPv6len {
			return nil, fmt.Errorf("mdns: AAAA 记录 rdlength=%d，应为 16", rdlen)
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, body)
		return AAAA{IP: ip}, nil

	case TypePTR:
		// PTR 的 target 可能用压缩指针指向报文更靠前的位置，
		// 所以必须在完整报文上解析，而不是在 body 上。
		target, _, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		return PTR{Target: target}, nil

	case TypeSRV:
		if rdlen < 7 { // 2+2+2 + 至少一个根标签
			return nil, ErrTruncated
		}
		target, _, err := unpackName(msg, off+6)
		if err != nil {
			return nil, err
		}
		return SRV{
			Priority: binary.BigEndian.Uint16(body[0:2]),
			Weight:   binary.BigEndian.Uint16(body[2:4]),
			Port:     binary.BigEndian.Uint16(body[4:6]),
			Target:   target,
		}, nil

	case TypeTXT:
		var out []string
		for i := 0; i < len(body); {
			l := int(body[i])
			if i+1+l > len(body) {
				return nil, ErrTruncated
			}
			out = append(out, string(body[i+1:i+1+l]))
			i += 1 + l
		}
		return TXT{Strings: out}, nil

	case TypeNSEC:
		next, nextOff, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		if nextOff > off+rdlen {
			return nil, ErrTruncated
		}
		types, err := unpackTypeBitmap(msg[nextOff : off+rdlen])
		if err != nil {
			return nil, err
		}
		return NSEC{NextDomain: next, Types: types}, nil

	default:
		raw := make([]byte, rdlen)
		copy(raw, body)
		return Raw{RRType: typ, Data: raw}, nil
	}
}
