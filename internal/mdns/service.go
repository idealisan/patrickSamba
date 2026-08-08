package mdns

import (
	"fmt"
	"net"
	"strings"
)

const (
	// localDomain 是 mDNS 的本地域（RFC 6762 §3）。
	localDomain = "local."

	// metaQueryName 是 DNS-SD 的服务类型枚举名（RFC 6763 §9）。
	// 客户端查询它可以拿到本机提供的全部服务类型。
	metaQueryName = "_services._dns-sd._udp.local."
)

// 记录 TTL（RFC 6762 §10）。
//
// 规范建议：含主机名的记录用 120 秒（主机地址可能变），
// 其余记录用 75 分钟（4500 秒）。
const (
	ttlHost    uint32 = 120
	ttlShared  uint32 = 4500
	ttlGoodbye uint32 = 0
)

// serviceDef 是一个与实例名无关的服务定义。
//
// 实例名可能因为冲突而被改写（RFC 6762 §9），所以定义里不含实例名，
// 由 recordSet 在生成记录时拼上当前实例名。
type serviceDef struct {
	// Type 是 DNS-SD 服务类型，形如 "_smb._tcp"（RFC 6763 §4.1.2）。
	Type string
	// Port 是 SRV 记录里的端口。DNS-SD 允许 0，表示"这个服务没有真实端口，
	// 信息全在 TXT 里"（_device-info._tcp / _adisk._tcp 就是这种）。
	Port uint16
	// TXT 是 TXT 记录内容，每项形如 "key=value"（RFC 6763 §6.3）。
	TXT []string
}

// typeName 返回服务类型的完整域名，如 "_smb._tcp.local."。
func (d serviceDef) typeName() string {
	return d.Type + "." + localDomain
}

// validate 检查服务定义是否能被正确编码。
func (d serviceDef) validate() error {
	// RFC 6763 §4.1.2：类型由 "_应用协议._传输协议" 两个 label 组成。
	parts := strings.Split(d.Type, ".")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "_") ||
		(parts[1] != "_tcp" && parts[1] != "_udp") {
		return fmt.Errorf("mdns: 服务类型 %q 不合法，应形如 \"_smb._tcp\"", d.Type)
	}
	if len(parts[0]) < 2 || len(parts[0]) > 16 {
		// RFC 6763 §7：应用协议名（不含下划线）最长 15 个字符。
		return fmt.Errorf("mdns: 服务类型 %q 的应用协议名长度非法（含下划线应为 2-16）", d.Type)
	}
	for _, kv := range d.TXT {
		if len(kv) > maxTXTStringLen {
			return fmt.Errorf("mdns: 服务 %q 的 TXT 条目 %d 字节，超过上限 %d", d.Type, len(kv), maxTXTStringLen)
		}
	}
	return nil
}

// recordSet 是本 responder 宣告的全部记录。
//
// 实例名与主机名会在冲突时被改写，因此这里同时保存"基名"与"当前名"。
type recordSet struct {
	baseInstance string // 配置里的实例名
	baseHost     string // 配置里的主机 label（不含 .local.）

	instance  string // 当前实例名
	hostLabel string // 当前主机 label

	defs []serviceDef
}

func newRecordSet(instance, hostLabel string, defs []serviceDef) (*recordSet, error) {
	for _, d := range defs {
		if err := d.validate(); err != nil {
			return nil, err
		}
	}
	return &recordSet{
		baseInstance: instance,
		baseHost:     hostLabel,
		instance:     instance,
		hostLabel:    hostLabel,
		defs:         defs,
	}, nil
}

// hostname 返回主机名，如 "MYBOX.local."。
func (rs *recordSet) hostname() string {
	return EscapeLabel(rs.hostLabel) + "." + localDomain
}

// instanceName 返回某个服务的实例名，如 "MYBOX._smb._tcp.local."。
func (rs *recordSet) instanceName(d serviceDef) string {
	return EscapeLabel(rs.instance) + "." + d.typeName()
}

// rename 按 RFC 6762 §9 的建议改名：在原名后追加 "-2"、"-3" …
//
// n 是第几次冲突（从 1 开始），n=1 得到 "-2"。
func (rs *recordSet) rename(n int) {
	suffix := fmt.Sprintf("-%d", n+1)
	rs.instance = truncateLabel(rs.baseInstance, maxLabelLen-len(suffix)) + suffix
	rs.hostLabel = truncateLabel(rs.baseHost, maxLabelLen-len(suffix)) + suffix
}

// truncateLabel 按字节截断，保证加上后缀后仍然是合法的 DNS label。
func truncateLabel(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 按 UTF-8 边界回退，避免把一个多字节字符切成两半。
	b := []byte(s[:max])
	for len(b) > 0 && b[len(b)-1]&0xc0 == 0x80 {
		b = b[:len(b)-1]
	}
	return string(b)
}

// ownsName 判断某个名字是否由本 responder 负责应答（大小写不敏感）。
func (rs *recordSet) ownsName(name string) bool {
	if EqualName(name, rs.hostname()) || EqualName(name, metaQueryName) {
		return true
	}
	for _, d := range rs.defs {
		if EqualName(name, d.typeName()) || EqualName(name, rs.instanceName(d)) {
			return true
		}
	}
	return false
}

// uniqueNames 返回需要 probing 的名字（RFC 6762 §8.1）。
//
// 只有"唯一记录集"才需要探测：主机名与各服务实例名。
// 服务类型的 PTR 是共享记录，多台机器本来就该同时存在，不参与探测。
func (rs *recordSet) uniqueNames() []string {
	names := make([]string, 0, len(rs.defs)+1)
	names = append(names, rs.hostname())
	for _, d := range rs.defs {
		names = append(names, rs.instanceName(d))
	}
	return names
}

// serviceRecords 返回与网卡无关的记录：
// 服务类型枚举 PTR（元查询）、服务实例 PTR、SRV、TXT。
func (rs *recordSet) serviceRecords() []Record {
	out := make([]Record, 0, len(rs.defs)*4)
	seenType := make(map[string]bool, len(rs.defs))

	for _, d := range rs.defs {
		typeName := d.typeName()
		inst := rs.instanceName(d)

		// RFC 6763 §9：_services._dns-sd._udp.local. PTR -> 服务类型。
		// 同一类型只需要一条。
		if !seenType[strings.ToLower(typeName)] {
			seenType[strings.ToLower(typeName)] = true
			out = append(out, Record{
				Name: metaQueryName, Class: ClassIN, TTL: ttlShared,
				Data: PTR{Target: typeName},
			})
		}

		// RFC 6763 §4.1：服务类型 PTR -> 服务实例。共享记录，不置 cache-flush。
		out = append(out, Record{
			Name: typeName, Class: ClassIN, TTL: ttlShared,
			Data: PTR{Target: inst},
		})

		// SRV / TXT 是本实例独占的唯一记录，置 cache-flush（RFC 6762 §10.2）。
		out = append(out, Record{
			Name: inst, Class: ClassIN, CacheFlush: true, TTL: ttlHost,
			Data: SRV{Priority: 0, Weight: 0, Port: d.Port, Target: rs.hostname()},
		})
		out = append(out, Record{
			Name: inst, Class: ClassIN, CacheFlush: true, TTL: ttlShared,
			Data: TXT{Strings: d.TXT},
		})
	}
	return out
}

// addressRecords 返回某张网卡上的 A/AAAA 记录。
//
// RFC 6762 §6：应答必须只包含在收包链路上有效的地址，
// 所以地址记录是按网卡生成的，不能一股脑全发出去。
func (rs *recordSet) addressRecords(ifi *net.Interface) []Record {
	v4, v6 := interfaceIPs(ifi)
	out := make([]Record, 0, len(v4)+len(v6))
	host := rs.hostname()
	for _, ip := range v4 {
		out = append(out, Record{
			Name: host, Class: ClassIN, CacheFlush: true, TTL: ttlHost,
			Data: A{IP: ip},
		})
	}
	for _, ip := range v6 {
		out = append(out, Record{
			Name: host, Class: ClassIN, CacheFlush: true, TTL: ttlHost,
			Data: AAAA{IP: ip},
		})
	}
	return out
}

// nsecRecords 返回否定回答记录（RFC 6762 §6.1）：
// 明确告诉对方"这些名字上只有这些类型"，避免客户端为不存在的类型反复重试。
func (rs *recordSet) nsecRecords(ifi *net.Interface) []Record {
	out := make([]Record, 0, len(rs.defs)+1)

	host := rs.hostname()
	var hostTypes []Type
	if ifi != nil {
		v4, v6 := interfaceIPs(ifi)
		if len(v4) > 0 {
			hostTypes = append(hostTypes, TypeA)
		}
		if len(v6) > 0 {
			hostTypes = append(hostTypes, TypeAAAA)
		}
	}
	if len(hostTypes) > 0 {
		out = append(out, Record{
			Name: host, Class: ClassIN, CacheFlush: true, TTL: ttlHost,
			Data: NSEC{NextDomain: host, Types: hostTypes},
		})
	}

	for _, d := range rs.defs {
		inst := rs.instanceName(d)
		out = append(out, Record{
			Name: inst, Class: ClassIN, CacheFlush: true, TTL: ttlHost,
			Data: NSEC{NextDomain: inst, Types: []Type{TypeTXT, TypeSRV}},
		})
	}
	return out
}

// allRecords 返回宣告（announcing）与告别（goodbye）时要发的全部记录。
func (rs *recordSet) allRecords(ifi *net.Interface) []Record {
	out := rs.serviceRecords()
	out = append(out, rs.addressRecords(ifi)...)
	out = append(out, rs.nsecRecords(ifi)...)
	return out
}

// withTTL 复制一份记录并统一改写 TTL。goodbye 报文用 TTL=0（RFC 6762 §10.1）。
func withTTL(records []Record, ttl uint32) []Record {
	out := make([]Record, len(records))
	copy(out, records)
	for i := range out {
		out[i].TTL = ttl
	}
	return out
}
