package mdns

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/idealisan/patrickSamba/internal/config"
)

// New 根据配置构造一个 mDNS responder。
//
// 本包与 SMB 协议层完全解耦（AGENTS.md §5）：只吃配置、端口和共享列表，
// 不 import internal/server 或 internal/smb 里的任何东西。
//
// port 是 SMB 服务端口，用于 _smb._tcp 的 SRV 记录。
func New(cfg config.MDNS, port int, shares []config.Share) (*Responder, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("mdns: 端口 %d 不在 1-65535 范围内", port)
	}
	instance := strings.TrimSpace(cfg.Instance)
	if instance == "" {
		return nil, fmt.Errorf("mdns: 实例名为空（config.ApplyDefaults 会用 server.name 填充，请检查配置加载流程）")
	}

	defs := []serviceDef{smbServiceDef(uint16(port))}

	if cfg.Apple.EnabledOn() {
		model := cfg.Apple.Model
		if model == "" {
			model = config.DefaultAppleModel
		}
		defs = append(defs, deviceInfoServiceDef(model))

		if cfg.Apple.AdvertiseTimeMachine {
			if def, ok := adiskServiceDef(timeMachineVolumes(shares)); ok {
				defs = append(defs, def)
			}
		}
	}

	rs, err := newRecordSet(instance, hostLabel(instance), defs)
	if err != nil {
		return nil, err
	}

	return &Responder{
		rs:          rs,
		ifaceNames:  cfg.Interfaces,
		log:         slog.Default(),
		conflictCh:  make(chan struct{}, 1),
		readdressCh: make(chan struct{}, 1),
		known:       newKnownAnswerStash(),
	}, nil
}

// timeMachineVolumes 挑出标记为 Time Machine 目标的共享名。
func timeMachineVolumes(shares []config.Share) []string {
	var out []string
	for i := range shares {
		if shares[i].TimeMachine {
			out = append(out, shares[i].Name)
		}
	}
	return out
}

// hostLabel 把实例名转换成可用作主机名的 DNS label。
//
// DNS-SD 实例名允许空格等任意 UTF-8（RFC 6763 §4.1.1），但主机名（A/AAAA
// 记录的名字）会被人直接敲进地址栏，保守起见只保留 [A-Za-z0-9-]，
// 其余字符换成连字符（RFC 1123 §2.1 的主机名字符集）。
func hostLabel(instance string) string {
	var sb strings.Builder
	for _, ch := range instance {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			sb.WriteRune(ch)
		case ch == '-' || ch == '_':
			sb.WriteByte('-')
		default:
			// 非 ASCII 与标点统统折叠成一个连字符。
			sb.WriteByte('-')
		}
	}
	// 去掉首尾连字符：主机名不能以连字符开头或结尾。
	label := strings.Trim(sb.String(), "-")
	if label == "" {
		label = "stupidsamba"
	}
	return truncateLabel(label, maxLabelLen)
}
