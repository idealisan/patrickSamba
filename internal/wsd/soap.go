package wsd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// WS-Discovery 的 SOAP 报文编解码（SOAP-over-UDP，MS-WSD / WS-Discovery 1.1）
//
// Windows 的「网络」（Network Neighborhood）靠这套协议发现局域网里的主机：
// 客户端往组播地址发一条 `Probe`，服务端回一条 `ProbeMatch`。
// 没有它，Windows 只能靠 NetBIOS 浏览列表或手工 `\\IP` 访问。
//
// 为什么自己拼 XML 而不用模板：模板里插变量很容易漏掉转义，而主机名是
// 用户可控的（配置里 `server.name` 允许 15 字节内任意字符）——
// 一个 `&` 就能让客户端解析失败，而且是那种"只有某些主机连不上"的怪问题。
// 所有动态值一律走 xml.EscapeText。
// ---------------------------------------------------------------------------

// 命名空间。WS-Discovery 规定 SOAP-over-UDP 用 SOAP 1.2，但现实里
// 也见过 SOAP 1.1 的信封，所以两种都收（见 envelope 的注释）。
const (
	nsSOAP12  = "http://www.w3.org/2003/05/soap-envelope"
	nsSOAP11  = "http://schemas.xmlsoap.org/soap/envelope/"
	nsAddr    = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	nsDisc    = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	nsDevProf = "http://schemas.xmlsoap.org/ws/2006/02/devprof"
	// nsPub 是 Windows 的 Function Discovery 用的「publication」命名空间，
	// 类型 pub:Computer 就来自这里 —— 它才是"在 Windows 网络里显示为一台
	// 电脑"的关键，光有 wsdp:Device 只会被识别成一个通用设备。
	nsPub = "http://schemas.microsoft.com/windows/2006/09/09/pub"
)

// Action URI（WS-Discovery §5）。
const (
	ActionProbe        = nsDisc + "/Probe"
	ActionProbeMatch   = nsDisc + "/ProbeMatch"
	ActionResolve      = nsDisc + "/Resolve"
	ActionResolveMatch = nsDisc + "/ResolveMatch"
	ActionHello        = nsDisc + "/Hello"
	ActionBye          = nsDisc + "/Bye"
)

// discoveryAddress 是组播报文里 wsa:To / wsa:ReplyTo 用的约定地址
// （WS-Discovery §5.2：匿名端点）。
const discoveryAddress = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"

// envelope 是收到的 SOAP 信封。
//
// 只用**本地名**匹配 `Envelope`：SOAP 1.1 与 1.2 的命名空间不同，
// 写死任一个都会漏掉另一批客户端。命名空间在 parseEnvelope 里另行校验，
// 不在结构标签里卡死。
type envelope struct {
	XMLName xml.Name       `xml:"Envelope"`
	Header  soapHeader     `xml:"Header"`
	Body    soapBody       `xml:"Body"`
	NS      prefixBindings // 从原始字节里扫出来的 xmlns:前缀 → 命名空间
}

type soapHeader struct {
	Action    string       `xml:"Action"`
	MessageID string       `xml:"MessageID"`
	To        string       `xml:"To"`
	ReplyTo   *endpointRef `xml:"ReplyTo"`
}

type soapBody struct {
	Probe   *probeMsg   `xml:"Probe"`
	Resolve *resolveMsg `xml:"Resolve"`
}

type probeMsg struct {
	Types string `xml:"Types"`
}

type resolveMsg struct {
	EndpointRef *endpointRef `xml:"EndpointReference"`
}

type endpointRef struct {
	Address string `xml:"Address"`
}

// request 是解析后的请求要点。
type request struct {
	// action 是 wsa:Action。
	action string
	// messageID 是 wsa:MessageID，回包要用它填 wsa:RelatesTo。
	messageID string
	// types 是 Probe 请求的 QName 列表（已按前缀解析成 "ns" + "local" 对）。
	types []qname
	// resolveAddress 是 Resolve 请求里的端点地址（urn:uuid:...）。
	resolveAddress string
}

// qname 是一个 XML 限定名。
type qname struct {
	space string
	local string
}

// parseRequest 解析一条 WS-Discovery 请求。
func parseRequest(b []byte) (*request, error) {
	var env envelope
	if err := xml.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("解析 SOAP 信封失败: %w", err)
	}
	// 信封命名空间必须是 SOAP 1.1 或 1.2。本地名匹配会放过任意命名空间，
	// 这里补上收口，免得把完全不相干的 XML 当成 WS-Discovery 处理。
	if env.XMLName.Space != nsSOAP12 && env.XMLName.Space != nsSOAP11 {
		return nil, fmt.Errorf("SOAP 信封命名空间 %q 不受支持", env.XMLName.Space)
	}
	env.NS = scanNamespaces(b)

	req := &request{
		action:    strings.TrimSpace(env.Header.Action),
		messageID: strings.TrimSpace(env.Header.MessageID),
	}
	switch req.action {
	case ActionProbe:
		if env.Body.Probe != nil {
			req.types = parseTypes(env.Body.Probe.Types, env.NS)
		}
	case ActionResolve:
		if env.Body.Resolve != nil && env.Body.Resolve.EndpointRef != nil {
			req.resolveAddress = strings.TrimSpace(env.Body.Resolve.EndpointRef.Address)
		}
	}
	return req, nil
}

// prefixBindings 是 前缀 → 命名空间 的映射。
//
// 为什么需要它：Probe 里的 `<wsd:Types>wsdp:Device pub:Computer</wsd:Types>`
// 是 QName 列表，前缀绑定在信封元素上。encoding/xml 不暴露 xmlns 属性，
// 只能自己从原始字节里扫。
type prefixBindings map[string]string

// scanNamespaces 扫出原始 XML 里的 xmlns[:前缀] 声明。
//
// 用手工扫描而不是完整 XML 解析：这里只需要属性层的信息，而 Go 的
// encoding/xml 拿不到命名空间前缀声明（它把它们当普通属性吞掉了，
// 且 xmlns 属性本身不会出现在任何结构体字段里）。
func scanNamespaces(b []byte) prefixBindings {
	out := prefixBindings{}
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range start.Attr {
			switch {
			case a.Name.Local == "xmlns":
				out[""] = a.Value
			case a.Name.Space == "xmlns":
				out[a.Name.Local] = a.Value
			}
		}
	}
}

// parseTypes 把 "wsdp:Device pub:Computer" 解析成 QName 列表。
//
// 前缀解析不到命名空间时**退回按本地名匹配**：真实客户端（尤其老版本
// Windows）偶尔会用非标准前缀甚至省略前缀，严格拒收会让主机在对方
// 的网络列表里彻底消失 —— 而匹配宽松一点的代价只是多答一条 ProbeMatch。
// 这个取舍与 AGENTS.md §9「以真实客户端行为为准」是同一条。
func parseTypes(s string, ns prefixBindings) []qname {
	var out []qname
	for _, field := range strings.Fields(s) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		prefix, local, found := strings.Cut(field, ":")
		if !found {
			out = append(out, qname{local: field})
			continue
		}
		out = append(out, qname{space: ns[prefix], local: local})
	}
	return out
}

// ---- 响应构造 ----

// deviceInfo 是构造响应所需的设备信息。
type deviceInfo struct {
	// UUID 是设备的稳定标识（不带 urn:uuid: 前缀）。
	UUID string
	// XAddrs 是元数据端点的 URL 列表（空格分隔），由 Responder 在应答时
	// 按收包网卡现算（见 Responder.xaddrs）—— 它依赖网卡，不该存死。
	XAddrs string
	// MetadataVersion 是元数据版本号。
	MetadataVersion uint64
	// FriendlyName 是元数据里的显示名（Windows 网络列表里看到的名字）。
	FriendlyName string
	// PresentationURL 是可选的「打开设备网页」链接。
	//
	// 本服务没有 Web 界面，留空即可；这里留出字段是因为 WS-Discovery 的
	// ThisModel 里它是常规项，将来要挂也不必改结构。
	PresentationURL string
}

// buildMatch 构造 ProbeMatch / ResolveMatch 的 SOAP 信封。
//
// action 决定是 ProbeMatch 还是 ResolveMatch，其余布局完全一致
// （WS-Discovery §6）。
//
// relatesTo 是**请求**的 wsa:MessageID；响应自己的 MessageID 在函数内部
// 另行生成 —— 两者不能共用一个值：客户端按 MessageID 去重，若每条响应的
// MessageID 都等于请求的那个，第二条之后会全被当成重复而丢弃。
func buildMatch(action, relatesTo string, d deviceInfo) ([]byte, error) {
	newID, err := newMessageID()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<soap:Envelope xmlns:soap="` + nsSOAP12 + `"` +
		` xmlns:wsa="` + nsAddr + `"` +
		` xmlns:wsd="` + nsDisc + `"` +
		` xmlns:wsdp="` + nsDevProf + `"` +
		` xmlns:pub="` + nsPub + `">`)
	buf.WriteString(`<soap:Header>`)
	buf.WriteString(`<wsa:To>` + discoveryAddress + `</wsa:To>`)
	buf.WriteString(`<wsa:Action>` + action + `</wsa:Action>`)
	buf.WriteString(`<wsa:MessageID>urn:uuid:` + newID + `</wsa:MessageID>`)
	// RelatesTo 把响应与请求对上号（WS-Addressing §6.2 的要求）。
	buf.WriteString(`<wsa:RelatesTo>`)
	if err := escapeText(&buf, relatesTo); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsa:RelatesTo>`)
	buf.WriteString(`</soap:Header>`)
	buf.WriteString(`<soap:Body>`)
	buf.WriteString(`<wsd:` + matchElement(action) + `>`)
	buf.WriteString(`<wsa:EndpointReference><wsa:Address>urn:uuid:`)
	if err := escapeText(&buf, d.UUID); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsa:Address></wsa:EndpointReference>`)
	buf.WriteString(`<wsd:Types>wsdp:Device pub:Computer</wsd:Types>`)
	buf.WriteString(`<wsd:XAddrs>`)
	if err := escapeText(&buf, d.XAddrs); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsd:XAddrs>`)
	buf.WriteString(fmt.Sprintf(`<wsd:MetadataVersion>%d</wsd:MetadataVersion>`, d.MetadataVersion))
	buf.WriteString(`</wsd:` + matchElement(action) + `>`)
	buf.WriteString(`</soap:Body>`)
	buf.WriteString(`</soap:Envelope>`)
	return buf.Bytes(), nil
}

// matchElement 由 Action 推出响应体的元素名。
func matchElement(action string) string {
	if action == ActionResolveMatch {
		return "ResolveMatch"
	}
	return "ProbeMatch"
}

// escapeText 写入转义后的文本。
//
// xml.EscapeText 会处理 & < > ' " —— 主机名与 XAddrs 都可能出现这些字符，
// 不加转义就是典型的 XML 注入式解析失败。
func escapeText(buf *bytes.Buffer, s string) error {
	return xml.EscapeText(buf, []byte(s))
}

// matches 报告本设备是否应当回应这次 Probe。
//
// 规则（WS-Discovery §5.2）：
//   - 请求没带 Types → 匹配所有设备；
//   - 带 Types 时，只要本机任一类型命中就算匹配。
//
// 本机声明的类型固定为 wsdp:Device 与 pub:Computer。
func (d deviceInfo) matches(types []qname) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if d.matchesOne(t) {
			return true
		}
	}
	return false
}

// matchesOne 判定单个 QName 是否命中。
//
// 命名空间为空（前缀解析失败）时按本地名比较 —— 理由见 parseTypes 的注释。
func (d deviceInfo) matchesOne(t qname) bool {
	for _, mine := range []qname{
		{space: nsDevProf, local: "Device"},
		{space: nsPub, local: "Computer"},
	} {
		if t.local != mine.local {
			continue
		}
		if t.space == "" || t.space == mine.space {
			return true
		}
	}
	return false
}
