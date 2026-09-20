package wsd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/idealisan/patrickSamba/internal/sockopt"
)

// ---------------------------------------------------------------------------
// WS-Transfer 元数据端点（HTTP，默认 5357）
//
// WS-Discovery 只负责"这里有台设备"，设备的**详情**（显示名、制造商、
// 类型）要走 WS-Transfer 的 `Get` 从 XAddrs 指向的 HTTP 端点取。
// Windows 把主机列进「网络」之后就会来问这一下 —— 少了它，主机能列出来
// 但点进去拿不到信息，在 UI 上表现为图标异常或属性为空。
//
// soap.go 里的 UDP 部分是"被发现"，这里是"被认识"，两者是同一套协议的
// 两半。分文件放是因为它们一个是 UDP 组播、一个是 HTTP 服务端，
// 传输与并发模型完全不同，混在一起会互相污染。
// ---------------------------------------------------------------------------

const (
	// nsTransfer 是 WS-Transfer 命名空间。
	nsTransfer = "http://schemas.xmlsoap.org/ws/2004/09/transfer"

	// ActionGet / ActionGetResponse 是 WS-Transfer 的动作 URI。
	ActionGet         = nsTransfer + "/Get"
	ActionGetResponse = nsTransfer + "/GetResponse"

	// anonymousRole 是 WS-Addressing 的匿名角色地址：客户端在
	// wsa:ReplyTo 里填它，表示"把响应直接放回这个 HTTP 连接上"。
	anonymousRole = "http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous"

	// maxMetadataBody 是 Get 请求体的长度上限。
	// 真实请求只有几百字节，给 64 KiB 足够宽松又能挡住畸形输入。
	maxMetadataBody = 64 << 10

	// metadataTimeout 是这个小 HTTP 服务端的读写超时。
	// 它是局域网内的短请求通道，不该有长连接挂着。
	metadataTimeout = 10 * time.Second
)

// metadataServer 是 WS-Transfer Get 的 HTTP 端点。
type metadataServer struct {
	port int
	dev  deviceInfo
	log  *slog.Logger
	srv  *http.Server
}

// newMetadataServer 构造（尚未监听）元数据端点。
func newMetadataServer(port int, dev deviceInfo) (*metadataServer, error) {
	if port <= 0 {
		return nil, fmt.Errorf("元数据端口非法: %d", port)
	}
	m := &metadataServer{port: port, dev: dev, log: slog.Default()}
	mux := http.NewServeMux()
	// 路径带 UUID：WS-Discovery 的 XAddrs 就是这么宣告的。
	// 同时挂 "/" 兜底，因为见过客户端忽略 XAddrs 里的路径直接打根。
	mux.HandleFunc("/"+dev.UUID, m.handleGet)
	mux.HandleFunc("/", m.handleGet)
	m.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: metadataTimeout,
		ReadTimeout:       metadataTimeout,
		WriteTimeout:      metadataTimeout,
	}
	return m, nil
}

// SetLogger 注入日志句柄。
func (m *metadataServer) SetLogger(l *slog.Logger) {
	if l != nil {
		m.log = l
	}
}

// Serve 监听直到 ctx 取消。
func (m *metadataServer) Serve(ctx context.Context) error {
	// 5357 是 Windows 的约定端口，同一台机器上可能有别的 WSD 实现在跑，
	// 也可能有本程序的另一个实例 —— 用可复用的监听配置，别去抢占端口。
	lc := sockopt.ListenConfig()
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", m.port))
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- m.srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	case <-ctx.Done():
		// 关监听即可，不 gracefully 等在途请求：这是局域网内的查询通道，
		// 退出时没有需要保护的长事务。
		return m.srv.Close()
	}
}

// handleGet 应答 WS-Transfer Get。
//
// 只接受 POST + SOAP Action: .../Get。其余一律 405/400 ——
// 这是**设备元数据端点**，不是通用 Web 服务，不该对 GET 有反应
// （否则扫描器一看根路径有内容就会一直来）。
func (m *metadataServer) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "only POST (SOAP) is accepted", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMetadataBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	action := soapAction(r)
	if action != "" && action != ActionGet {
		// 声明了别的 Action：这是给别的 WS-* 服务的请求，不该由我们应答。
		http.Error(w, "unsupported SOAP action", http.StatusBadRequest)
		return
	}

	relatesTo := messageIDFrom(body)
	resp, err := m.buildGetResponse(relatesTo)
	if err != nil {
		m.log.Warn("构造元数据响应失败", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(resp)))
	_, _ = w.Write(resp)
	m.log.Debug("已应答 WS-Transfer Get", "from", r.RemoteAddr)
}

// buildGetResponse 构造 GetResponse 的 SOAP 信封。
func (m *metadataServer) buildGetResponse(relatesTo string) ([]byte, error) {
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
	buf.WriteString(`<wsa:RelatesTo>`)
	if err := escapeText(&buf, relatesTo); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsa:RelatesTo>`)
	buf.WriteString(`<wsa:To>` + anonymousRole + `</wsa:To>`)
	buf.WriteString(`<wsa:Action>` + ActionGetResponse + `</wsa:Action>`)
	buf.WriteString(`<wsa:MessageID>urn:uuid:` + newID + `</wsa:MessageID>`)
	buf.WriteString(`</soap:Header>`)
	buf.WriteString(`<soap:Body><wsd:Metadata>`)

	// ThisDevice：设备本身的描述。
	buf.WriteString(`<wsd:MetadataSection Dialect="` + nsDevProf + `/ThisDevice">`)
	buf.WriteString(`<wsdp:ThisDevice><wsdp:FriendlyName>`)
	if err := escapeText(&buf, m.dev.FriendlyName); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsdp:FriendlyName>`)
	buf.WriteString(`<wsdp:FirmwareVersion></wsdp:FirmwareVersion>`)
	buf.WriteString(`<wsdp:SerialNumber></wsdp:SerialNumber>`)
	buf.WriteString(`</wsdp:ThisDevice></wsd:MetadataSection>`)

	// ThisModel：型号与厂商。
	buf.WriteString(`<wsd:MetadataSection Dialect="` + nsDevProf + `/ThisModel">`)
	buf.WriteString(`<wsdp:ThisModel>`)
	buf.WriteString(`<wsdp:Manufacturer>stupidSamba</wsdp:Manufacturer>`)
	buf.WriteString(`<wsdp:ManufacturerUrl>https://cnb.cool/finalappstore/stupidSamba</wsdp:ManufacturerUrl>`)
	buf.WriteString(`<wsdp:ModelName>stupidSamba File Server</wsdp:ModelName>`)
	if m.dev.PresentationURL != "" {
		buf.WriteString(`<wsdp:PresentationUrl>`)
		if err := escapeText(&buf, m.dev.PresentationURL); err != nil {
			return nil, err
		}
		buf.WriteString(`</wsdp:PresentationUrl>`)
	}
	buf.WriteString(`</wsdp:ThisModel></wsd:MetadataSection>`)

	// Relationship：把它声明成一台**主机**（pub:Computer）。
	// 这一段才是 Windows 决定"显示成电脑图标"的依据。
	buf.WriteString(`<wsd:MetadataSection Dialect="` + nsDevProf + `/Relationship">`)
	buf.WriteString(`<wsd:Relationship Type="` + nsDevProf + `/host">`)
	buf.WriteString(`<wsd:Host>`)
	buf.WriteString(`<wsa:EndpointReference><wsa:Address>urn:uuid:`)
	if err := escapeText(&buf, m.dev.UUID); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsa:Address></wsa:EndpointReference>`)
	buf.WriteString(`<wsd:Types>pub:Computer</wsd:Types>`)
	buf.WriteString(`<wsd:ServiceId>urn:uuid:`)
	if err := escapeText(&buf, m.dev.UUID); err != nil {
		return nil, err
	}
	buf.WriteString(`</wsd:ServiceId>`)
	buf.WriteString(`</wsd:Host></wsd:Relationship></wsd:MetadataSection>`)

	buf.WriteString(`</wsd:Metadata></soap:Body></soap:Envelope>`)
	return buf.Bytes(), nil
}

// soapAction 从 HTTP 头或 SOAP 信封里取 Action。
//
// 两处都看：SOAP 1.2 over HTTP 要求 `Content-Type` 带 action 参数，
// 但不少客户端只写在信封里。都拿不到时返回空串，由调用方按"不限制"处理。
func soapAction(r *http.Request) string {
	if v := r.Header.Get("SOAPAction"); v != "" {
		return strings.Trim(v, `"`)
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.Index(strings.ToLower(ct), "action="); i >= 0 {
		v := ct[i+len("action="):]
		if j := strings.IndexByte(v, ';'); j >= 0 {
			v = v[:j]
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// messageIDFrom 从请求信封里取出 wsa:MessageID，用于填 RelatesTo。
//
// 取不到就返回空串 —— RelatesTo 是"应当"而非"必须"，
// 客户端不会因为缺它就解析失败。
func messageIDFrom(body []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	inMessageID := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inMessageID = t.Name.Local == "MessageID"
		case xml.CharData:
			if inMessageID {
				return strings.TrimSpace(string(t))
			}
		}
	}
}
