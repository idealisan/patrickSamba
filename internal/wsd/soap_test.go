package wsd

import (
	"encoding/xml"
	"strings"
	"testing"
)

// 本文件钉的是 WS-Discovery 的 SOAP 编解码。
//
// 变异自检方向：
//   - 把 buildMatch 里 RelatesTo 与 MessageID 的取值改回同一个 →
//     TestBuildMatchSeparatesMessageIDFromRelatesTo 变红（那会让客户端
//     把第二条之后的响应当重复丢弃）；
//   - 把 escapeText 去掉 → TestBuildMatchEscapesHostName 变红
//     （含 & 的主机名会产出非良构 XML，客户端直接解析失败）；
//   - 把 parseTypes 的前缀解析去掉 → TestParseRequestResolvesTypes 变红；
//   - 把 matchesOne 的命名空间留空兜底删掉 → TestMatchesFallsBackToLocalName 变红。

const probeTemplate = `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="%s"
    xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"
    xmlns:wsd="http://schemas.xmlsoap.org/ws/2005/04/discovery"
    xmlns:wsdp="http://schemas.xmlsoap.org/ws/2006/02/devprof"
    xmlns:pub="http://schemas.microsoft.com/windows/2006/09/09/pub">
  <soap:Header>
    <wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>
    <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>
    <wsa:MessageID>urn:uuid:req-1234</wsa:MessageID>
  </soap:Header>
  <soap:Body>
    <wsd:Probe><wsd:Types>%s</wsd:Types></wsd:Probe>
  </soap:Body>
</soap:Envelope>`

func probeXML(soapNS, types string) []byte {
	return []byte(strings.Replace(
		strings.Replace(probeTemplate, "%s", soapNS, 1), "%s", types, 1))
}

func TestParseRequestProbe(t *testing.T) {
	req, err := parseRequest(probeXML(nsSOAP12, "wsdp:Device pub:Computer"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.action != ActionProbe {
		t.Fatalf("action 应为 Probe，实际 %q", req.action)
	}
	if req.messageID != "urn:uuid:req-1234" {
		t.Fatalf("messageID 应为 urn:uuid:req-1234，实际 %q", req.messageID)
	}
	if len(req.types) != 2 {
		t.Fatalf("应解析出 2 个类型，实际 %d", len(req.types))
	}
}

// TestParseRequestResolvesTypes：前缀必须按信封上的 xmlns 解析成命名空间。
func TestParseRequestResolvesTypes(t *testing.T) {
	req, _ := parseRequest(probeXML(nsSOAP12, "wsdp:Device pub:Computer"))
	want := []qname{
		{space: nsDevProf, local: "Device"},
		{space: nsPub, local: "Computer"},
	}
	for i, w := range want {
		if req.types[i] != w {
			t.Fatalf("类型 %d 应为 %+v，实际 %+v", i, w, req.types[i])
		}
	}
}

// TestParseRequestAcceptsSOAP11：现实里有客户端发 SOAP 1.1 信封。
func TestParseRequestAcceptsSOAP11(t *testing.T) {
	if _, err := parseRequest(probeXML(nsSOAP11, "wsdp:Device")); err != nil {
		t.Fatalf("SOAP 1.1 信封应被接受，实际报错: %v", err)
	}
}

// TestParseRequestRejectsForeignNamespace：3702 端口上什么都可能发过来，
// 不能把任意 XML 当 WS-Discovery 处理。
func TestParseRequestRejectsForeignNamespace(t *testing.T) {
	if _, err := parseRequest(probeXML("urn:totally:not:soap", "wsdp:Device")); err == nil {
		t.Fatal("非 SOAP 命名空间应当被拒")
	}
}

func TestParseRequestResolve(t *testing.T) {
	body := `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="` + nsSOAP12 + `"
    xmlns:wsa="` + nsAddr + `" xmlns:wsd="` + nsDisc + `">
  <soap:Header>
    <wsa:Action>` + ActionResolve + `</wsa:Action>
    <wsa:MessageID>urn:uuid:req-9</wsa:MessageID>
  </soap:Header>
  <soap:Body>
    <wsd:Resolve>
      <wsa:EndpointReference><wsa:Address>urn:uuid:dev-1</wsa:Address></wsa:EndpointReference>
    </wsd:Resolve>
  </soap:Body>
</soap:Envelope>`
	req, err := parseRequest([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.resolveAddress != "urn:uuid:dev-1" {
		t.Fatalf("resolveAddress 应为 urn:uuid:dev-1，实际 %q", req.resolveAddress)
	}
}

func TestBuildMatchIsWellFormed(t *testing.T) {
	b, err := buildMatch(ActionProbeMatch, "urn:uuid:req-1", deviceInfo{
		UUID: "dev-uuid", XAddrs: "http://192.168.1.5:5357/dev-uuid", MetadataVersion: 1,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 应答必须是**良构** XML：客户端第一件事就是解析它。
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
	}
	if err := xml.Unmarshal(b, &env); err != nil {
		t.Fatalf("产出的 XML 不是良构的: %v\n%s", err, b)
	}
	s := string(b)
	for _, want := range []string{
		ActionProbeMatch, "wsd:ProbeMatch", "urn:uuid:dev-uuid",
		"http://192.168.1.5:5357/dev-uuid", "wsdp:Device pub:Computer",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("应答里应当包含 %q，实际:\n%s", want, s)
		}
	}
}

// TestBuildMatchSeparatesMessageIDFromRelatesTo：RelatesTo 回填请求的
// MessageID，而响应自己的 MessageID 必须**另生成**。
//
// 两者共用一个值的话，客户端按 MessageID 去重会把第二条之后的响应全丢掉，
// 表现为"网络列表里时有时无"。
func TestBuildMatchSeparatesMessageIDFromRelatesTo(t *testing.T) {
	b, _ := buildMatch(ActionProbeMatch, "urn:uuid:req-abc", deviceInfo{UUID: "u"})
	s := string(b)
	if !strings.Contains(s, "<wsa:RelatesTo>urn:uuid:req-abc</wsa:RelatesTo>") {
		t.Fatalf("RelatesTo 应为请求的 MessageID，实际:\n%s", s)
	}
	if strings.Contains(s, "<wsa:MessageID>urn:uuid:req-abc</wsa:MessageID>") {
		t.Fatal("响应的 MessageID 不得复用请求的 MessageID")
	}

	// 两次构造的 MessageID 不能相同。
	b2, _ := buildMatch(ActionProbeMatch, "urn:uuid:req-abc", deviceInfo{UUID: "u"})
	if s == string(b2) {
		t.Fatal("两次应答的 MessageID 应当不同（客户端按它去重）")
	}
}

// TestBuildMatchEscapesHostName：主机名是用户可控的，必须转义。
func TestBuildMatchEscapesHostName(t *testing.T) {
	b, err := buildMatch(ActionProbeMatch, "urn:uuid:a&b<c>", deviceInfo{UUID: "u"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
	}
	if err := xml.Unmarshal(b, &env); err != nil {
		t.Fatalf("含特殊字符的 RelatesTo 未转义，产出非良构 XML: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "a&amp;b&lt;c&gt;") {
		t.Fatalf("特殊字符应被转义，实际:\n%s", b)
	}
}

func TestMatches(t *testing.T) {
	d := deviceInfo{UUID: "u"}
	// 没带 Types 的 Probe 匹配所有设备。
	if !d.matches(nil) {
		t.Fatal("不带 Types 的 Probe 应当匹配")
	}
	if !d.matches([]qname{{space: nsDevProf, local: "Device"}}) {
		t.Fatal("wsdp:Device 应当匹配")
	}
	if !d.matches([]qname{{space: nsPub, local: "Computer"}}) {
		t.Fatal("pub:Computer 应当匹配")
	}
	if d.matches([]qname{{space: nsDevProf, local: "Printer"}}) {
		t.Fatal("wsdp:Printer 不应当匹配")
	}
	// 多个类型里有一个命中就算匹配。
	if !d.matches([]qname{
		{space: nsDevProf, local: "Printer"},
		{space: nsPub, local: "Computer"},
	}) {
		t.Fatal("多类型里有一个命中就应当匹配")
	}
}

// TestMatchesFallsBackToLocalName：前缀解析不出命名空间时按本地名兜底。
// 严格拒收会让主机在某些客户端的网络列表里彻底消失，代价太大。
func TestMatchesFallsBackToLocalName(t *testing.T) {
	d := deviceInfo{UUID: "u"}
	if !d.matches([]qname{{local: "Device"}}) {
		t.Fatal("前缀解析失败时应按本地名兜底匹配")
	}
}

func TestBuildMatchResolveUsesResolveMatchElement(t *testing.T) {
	b, _ := buildMatch(ActionResolveMatch, "urn:uuid:r", deviceInfo{UUID: "u"})
	if !strings.Contains(string(b), "wsd:ResolveMatch") {
		t.Fatalf("Resolve 的应答体元素应为 ResolveMatch，实际:\n%s", b)
	}
}

// ---- UUID ----

func TestDeriveUUIDIsStable(t *testing.T) {
	a := DeriveUUID("MYHOST")
	b := DeriveUUID("MYHOST")
	if a != b {
		t.Fatalf("同名字应派生出同一个 UUID（跨重启必须稳定），实际 %q vs %q", a, b)
	}
	if DeriveUUID("OTHER") == a {
		t.Fatal("不同名字不应派生出同一个 UUID")
	}
}

func TestDeriveUUIDFormat(t *testing.T) {
	u := DeriveUUID("MYHOST")
	if len(u) != 36 {
		t.Fatalf("UUID 标准形式应为 36 字符，实际 %q（%d）", u, len(u))
	}
	// 版本位必须是 5，变体位必须是 RFC 4122。
	if u[14] != '5' {
		t.Fatalf("应为 UUIDv5（第 15 个字符是 5），实际 %q", u)
	}
	if !strings.ContainsRune("89ab", rune(u[19])) {
		t.Fatalf("变体位应为 RFC 4122（8/9/a/b），实际 %q", u)
	}
}

func TestNewMessageIDIsUnique(t *testing.T) {
	a, err := newMessageID()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	b, _ := newMessageID()
	if a == b {
		t.Fatal("两次生成的消息 ID 不应相同")
	}
	if len(a) != 36 {
		t.Fatalf("消息 ID 应为 36 字符，实际 %q", a)
	}
	if a[14] != '4' {
		t.Fatalf("消息 ID 应为 UUIDv4（第 15 个字符是 4），实际 %q", a)
	}
}

// TestScanNamespaces 钉住前缀解析本身。
func TestScanNamespaces(t *testing.T) {
	ns := scanNamespaces(probeXML(nsSOAP12, "wsdp:Device"))
	if ns["wsdp"] != nsDevProf {
		t.Fatalf("wsdp 应绑定到 %q，实际 %q", nsDevProf, ns["wsdp"])
	}
	if ns["pub"] != nsPub {
		t.Fatalf("pub 应绑定到 %q，实际 %q", nsPub, ns["pub"])
	}
}
