// Package dialect 定义 SMB 方言及其协商策略与能力矩阵。
//
// 本包**无状态、无 IO**，只回答两类问题：
//  1. 给定客户端提供的方言列表与本端配置区间，应当选中哪个方言？
//  2. 选中的方言具备哪些能力（多信用、加密、preauth、签名算法……）？
//
// 依据 MS-SMB2 §2.2.3（NEGOTIATE Request Dialects）与 §3.3.5.4。
package dialect

import (
	"fmt"
	"sort"
	"strings"
)

// Dialect 是 SMB2 NEGOTIATE 中的 DialectRevision（16 位，线上小端）。
type Dialect uint16

// MS-SMB2 §2.2.3 SMB2 NEGOTIATE Request — Dialects。
const (
	// SMB202 是 SMB 2.0.2（Vista / Server 2008）。
	SMB202 Dialect = 0x0202
	// SMB210 是 SMB 2.1（Win7 / Server 2008 R2）。
	SMB210 Dialect = 0x0210
	// SMB300 是 SMB 3.0（Win8 / Server 2012）。
	SMB300 Dialect = 0x0300
	// SMB302 是 SMB 3.0.2（Win8.1 / Server 2012 R2）。
	SMB302 Dialect = 0x0302
	// SMB311 是 SMB 3.1.1（Win10 / Server 2016 及以后）。
	SMB311 Dialect = 0x0311

	// SMB2Wildcard (0x02FF) 只用于回应 SMB1 多协议协商中的 "SMB 2.???"，
	// 表示"我支持 SMB2，请你再发一个真正的 SMB2 NEGOTIATE"。
	// 它**不是**一个可用于数据传输的方言（MS-SMB2 §3.3.5.3.1）。
	SMB2Wildcard Dialect = 0x02FF
)

// All 是本服务端实现的全部可用方言，**按从低到高排列**。
// 不含 SMB2Wildcard（它不是可用方言）。
var All = []Dialect{SMB202, SMB210, SMB300, SMB302, SMB311}

var names = map[Dialect]string{
	SMB202:       "2.0.2",
	SMB210:       "2.1",
	SMB300:       "3.0",
	SMB302:       "3.0.2",
	SMB311:       "3.1.1",
	SMB2Wildcard: "2.???",
}

// String 返回人类可读的方言名，如 "3.1.1"。
func (d Dialect) String() string {
	if n, ok := names[d]; ok {
		return n
	}
	return fmt.Sprintf("unknown(0x%04X)", uint16(d))
}

// Parse 解析配置文件里的方言字符串（如 "3.1.1"、"2.0.2"）。
// 也接受带 "SMB" 前缀的形式（"SMB3.1.1"）。
func Parse(s string) (Dialect, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "SMB"), "smb")
	t = strings.TrimSpace(t)
	for d, n := range names {
		if d == SMB2Wildcard {
			continue
		}
		if n == t {
			return d, nil
		}
	}
	// 允许一些常见别名。
	switch strings.ToLower(t) {
	case "2", "2.0":
		return SMB202, nil
	case "3":
		return SMB300, nil
	}
	return 0, fmt.Errorf("dialect: 无法识别的方言 %q（可用值：2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1）", s)
}

// Supported 报告本服务端是否实现了该方言。
func (d Dialect) Supported() bool {
	for _, x := range All {
		if x == d {
			return true
		}
	}
	return false
}

// IsSMB3 报告是否为 3.x 方言（决定签名算法与是否可能加密）。
func (d Dialect) IsSMB3() bool { return d >= SMB300 && d != SMB2Wildcard }

// SupportsMultiCredit 报告是否支持多信用 / LARGE_MTU。
//
// MS-SMB2 §3.3.5.4：SMB 2.0.2 不支持 large MTU，CreditCharge 字段保留为 0，
// 单次读写上限受限于 64 KiB。2.1 起支持。
func (d Dialect) SupportsMultiCredit() bool { return d >= SMB210 && d != SMB2Wildcard }

// SupportsEncryption 报告方言本身是否具备 SMB3 加密能力。
// 3.0/3.0.2 用 AES-128-CCM；3.1.1 用协商出的 cipher。
func (d Dialect) SupportsEncryption() bool { return d.IsSMB3() }

// SupportsPreauthIntegrity 报告是否使用 3.1.1 的 preauth integrity hash。
func (d Dialect) SupportsPreauthIntegrity() bool { return d == SMB311 }

// SupportsNegotiateContexts 报告 NEGOTIATE 报文是否携带 negotiate context 列表。
func (d Dialect) SupportsNegotiateContexts() bool { return d == SMB311 }

// SupportsLeasing 报告方言是否具备 lease 能力（2.1 起）。
// 注意：本服务端当前不授予 lease，此方法只表达方言层面的可能性。
func (d Dialect) SupportsLeasing() bool { return d >= SMB210 && d != SMB2Wildcard }

// SigningAlgorithm 是签名算法选择。
type SigningAlgorithm int

const (
	// SigningHMACSHA256 用于 2.0.2 / 2.1，取 HMAC-SHA256 的前 16 字节。
	SigningHMACSHA256 SigningAlgorithm = iota
	// SigningAESCMAC 用于 3.0 / 3.0.2 / 3.1.1（AES-128-CMAC，RFC 4493）。
	SigningAESCMAC
)

// Signing 返回该方言默认使用的签名算法（protocol-notes §6）。
//
// 3.1.1 可以通过 SIGNING_CAPABILITIES negotiate context 协商成 AES-GMAC，
// 那属于协商结果，不由方言单独决定，因此不在本函数体现。
func (d Dialect) Signing() SigningAlgorithm {
	if d.IsSMB3() {
		return SigningAESCMAC
	}
	return SigningHMACSHA256
}

// 传输尺寸上限（protocol-notes §4 的建议值）。
const (
	// MaxTransactSizeLarge 是支持 LARGE_MTU 的方言使用的 1 MiB 上限。
	MaxTransactSizeLarge uint32 = 0x00100000
	// MaxTransactSizeSmall 是 SMB 2.0.2 使用的 64 KiB 上限。
	MaxTransactSizeSmall uint32 = 0x00010000
)

// MaxTransactSize 返回该方言下 NEGOTIATE Response 应宣告的
// MaxTransactSize / MaxReadSize / MaxWriteSize（三者取同值）。
func (d Dialect) MaxTransactSize() uint32 {
	if d.SupportsMultiCredit() {
		return MaxTransactSizeLarge
	}
	return MaxTransactSizeSmall
}

// Capabilities 位（MS-SMB2 §2.2.3）。
const (
	CapDFS               uint32 = 0x00000001
	CapLeasing           uint32 = 0x00000002
	CapLargeMTU          uint32 = 0x00000004
	CapMultiChannel      uint32 = 0x00000008
	CapPersistentHandles uint32 = 0x00000010
	CapDirectoryLeasing  uint32 = 0x00000020
	CapEncryption        uint32 = 0x00000040
)

// ServerCapabilities 返回 NEGOTIATE Response 中应当宣告的 Capabilities。
//
// 刻意**不宣告** CAP_DFS：我们不实现 DFS，宣告了客户端会发
// FSCTL_DFS_GET_REFERRALS 并期待有效应答（protocol-notes §7）。
// 同样不宣告 MULTI_CHANNEL / PERSISTENT_HANDLES / DIRECTORY_LEASING，
// 因为这些能力都没有实现，宣告即撒谎，会导致客户端行为异常。
//
// encryption 参数由上层根据配置与 3.1.1 cipher 协商结果传入；
// 仅 3.0/3.0.2 通过本位宣告加密能力，3.1.1 改用 negotiate context。
//
// leasing 参数由上层按 server.oplocks 传入。租约要 SMB 2.1 起才有
// （MS-SMB2 §3.3.5.9.11 的租约语义依赖 2.1 的 create context），
// 2.0.2 下即使开了也不宣告 —— 宣告了客户端会发 RqLs 而我们不认。
func (d Dialect) ServerCapabilities(encryption, leasing bool) uint32 {
	var caps uint32
	if d.SupportsMultiCredit() {
		caps |= CapLargeMTU
	}
	if leasing && d >= SMB210 {
		caps |= CapLeasing
	}
	// MS-SMB2 §3.3.5.4：3.1.1 的加密能力通过 ENCRYPTION_CAPABILITIES
	// negotiate context 表达，Capabilities 里的 CAP_ENCRYPTION 位
	// 只对 3.0/3.0.2 有意义。
	if encryption && (d == SMB300 || d == SMB302) {
		caps |= CapEncryption
	}
	return caps
}

// Negotiate 从客户端提供的方言列表中选出最终方言。
//
// 规则（MS-SMB2 §3.3.5.4）：取**双方交集中的最高方言**。
// min/max 是本端配置允许的闭区间；传 0 表示不限制。
//
// 返回 ok=false 表示无交集，调用方应回 STATUS_NOT_SUPPORTED 并断连。
//
// 注意：客户端列表里可能出现 0x02FF（仅当它是 SMB1 多协议协商的产物），
// 本函数会忽略它 —— 真正的 SMB2 NEGOTIATE 不应包含通配方言。
func Negotiate(client []Dialect, min, max Dialect) (Dialect, bool) {
	if min == 0 {
		min = All[0]
	}
	if max == 0 {
		max = All[len(All)-1]
	}
	if min > max {
		min, max = max, min
	}

	best := Dialect(0)
	found := false
	for _, c := range client {
		if c == SMB2Wildcard || !c.Supported() {
			continue
		}
		if c < min || c > max {
			continue
		}
		if !found || c > best {
			best, found = c, true
		}
	}
	return best, found
}

// Range 返回 [min, max] 区间内本端支持的方言列表（升序）。
// 用于日志与自检。
func Range(min, max Dialect) []Dialect {
	if min == 0 {
		min = All[0]
	}
	if max == 0 {
		max = All[len(All)-1]
	}
	if min > max {
		min, max = max, min
	}
	var out []Dialect
	for _, d := range All {
		if d >= min && d <= max {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
