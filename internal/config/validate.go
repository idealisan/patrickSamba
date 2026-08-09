package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// isAbsPath 判断是否为**本平台**的绝对路径。
// 用 filepath 而非 path，以便 Windows 上 "C:\share" 也算绝对路径（AGENTS.md C7）。
func isAbsPath(p string) bool { return filepath.IsAbs(p) }

// quotaMinWarn 是 quota_bytes 的建议下限（1 GiB）。
// 低于此值的卷容量上报对 Time Machine 不实用（会反复失败、空间抖动），
// 但用户可能确有意图，所以只是 WARN，不报错。
const quotaMinWarn = 1 << 30

// FieldError 是单个字段的校验错误。
//
// Field 用 YAML 路径表示（如 "shares[1].path"），方便用户直接定位到配置行。
type FieldError struct {
	Field string
	Msg   string
}

func (e FieldError) Error() string { return e.Field + ": " + e.Msg }

// ValidationErrors 汇总一次校验中发现的**全部**问题。
//
// 一次性报出所有错误，避免用户改一个跑一次。
type ValidationErrors []FieldError

func (e ValidationErrors) Error() string {
	if len(e) == 1 {
		return "配置校验失败: " + e[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "配置校验失败，共 %d 处问题:", len(e))
	for _, fe := range e {
		b.WriteString("\n  - " + fe.Error())
	}
	return b.String()
}

// add 追加一条错误。
func (e *ValidationErrors) add(field, format string, args ...any) {
	*e = append(*e, FieldError{Field: field, Msg: fmt.Sprintf(format, args...)})
}

// 合法方言字符串，从低到高排列。索引即优先级。
//
// MS-SMB2 §2.2.3：0x0202 / 0x0210 / 0x0300 / 0x0302 / 0x0311。
// 这里只做字符串校验，不引入 internal/smb（config 不依赖协议层）。
var dialectOrder = []string{"2.0.2", "2.1", "3.0", "3.0.2", "3.1.1"}

// DialectNames 返回全部合法的方言字符串（从低到高）。
func DialectNames() []string {
	out := make([]string, len(dialectOrder))
	copy(out, dialectOrder)
	return out
}

// dialectRank 返回方言的序号，非法返回 -1。
func dialectRank(s string) int {
	for i, d := range dialectOrder {
		if d == s {
			return i
		}
	}
	return -1
}

// 共享名非法字符。
//
// MS-SRVS §2.2.4.22 / Windows 约定：共享名不得包含以下字符，
// 也不得包含控制字符。长度上限 80 个字符。
const shareNameInvalidChars = `\/:*?"<>|+;,=[]` + "\x00"

// ShareNameMaxLen 是共享名最大长度（MS-SRVS 约定 80）。
const ShareNameMaxLen = 80

// Validate 校验配置。调用前应先执行 ApplyDefaults。
//
// 返回的 error 若非 nil，其动态类型一定是 ValidationErrors。
func Validate(c *Config) error {
	var errs ValidationErrors

	validateServer(c, &errs)
	validateListen(c, &errs)
	validateShares(c, &errs)
	validateAuth(c, &errs)
	validateMDNS(c, &errs)
	validateLog(c, &errs)

	if len(errs) == 0 {
		return nil
	}
	return errs
}

func validateServer(c *Config, errs *ValidationErrors) {
	if c.Server.Name == "" {
		errs.add("server.name", "不能为空（留空时会自动使用系统主机名，取不到主机名请显式指定）")
	} else if len(c.Server.Name) > 15 {
		errs.add("server.name", "长度 %d 超过 NetBIOS 上限 15 字节: %q", len(c.Server.Name), c.Server.Name)
	}
	if strings.ContainsAny(c.Server.Name, shareNameInvalidChars+" .") {
		errs.add("server.name", "含非法字符（不能有空格、点或 %s）: %q", shareNameInvalidChars, c.Server.Name)
	}

	if c.Server.Domain == "" {
		errs.add("server.domain", "不能为空，默认应为 %q", DefaultDomain)
	} else if len(c.Server.Domain) > 15 {
		errs.add("server.domain", "长度 %d 超过 NetBIOS 上限 15 字节: %q", len(c.Server.Domain), c.Server.Domain)
	}

	minRank := dialectRank(c.Server.MinDialect)
	maxRank := dialectRank(c.Server.MaxDialect)
	if minRank < 0 {
		errs.add("server.min_dialect", "非法方言 %q，可选值: %s", c.Server.MinDialect, strings.Join(dialectOrder, ", "))
	}
	if maxRank < 0 {
		errs.add("server.max_dialect", "非法方言 %q，可选值: %s", c.Server.MaxDialect, strings.Join(dialectOrder, ", "))
	}
	if minRank >= 0 && maxRank >= 0 && minRank > maxRank {
		errs.add("server.min_dialect", "min_dialect (%s) 不能高于 max_dialect (%s)", c.Server.MinDialect, c.Server.MaxDialect)
	}

	// SMB3 加密最低要求 3.0（MS-SMB2 §3.3.5.4）。
	//
	// max_dialect 与 min_dialect 都要卡：
	//   - max_dialect < 3.0 → 谁都加密不了，配置自相矛盾；
	//   - min_dialect < 3.0 → 区间里的 2.x 全是死档位。协商层对它们会
	//     fail closed 直接 ACCESS_DENIED（见 command/negotiate.go），
	//     用户在运行期只能看到"连不上"，根本猜不到是这个组合导致的。
	//     所以启动时就报错，而**不是**悄悄把 min_dialect 抬到 3.0 ——
	//     静默改写用户写下的配置是魔法行为，本项目一律用"启动时一次性
	//     校验 + 人话错误"处理这类矛盾（与上面 max_dialect 那条对称）。
	//
	// 与 Samba 的差异（为什么我们更严格）：Samba 允许
	// encryption_required=true 与 min_dialect=2.x 并存——它不会在配置期报错，
	// 而是让 2.x 客户端在协商/会话建立阶段自然失败（拿不到加密能力），
	// 行为由客户端决定是否继续，服务端不主动拦。我们选择在**启动期**就硬失败，
	// 把矛盾暴露在配置里，而不是藏在运行期的"连不上"里。
	//
	// 代价：这条 fail-closed（2.x 客户端被拒）完全由 command/negotiate.go 的拒绝
	// 逻辑保证，配置层无法表达、运行期也没有兜底开关。换言之一旦协商层的拒绝逻辑
	// 被改坏，没有任何配置能救，只有 test/ 下的回归测试会变红。因此该组合的正确
	// 性**只能靠测试覆盖**——改动 negotiate.go 时必须同步跑对应用例
	// （test/ 中 encryption_required + 2.x 的协商测试）。
	if c.Server.EncryptionRequired && maxRank >= 0 && maxRank < dialectRank("3.0") {
		errs.add("server.encryption_required", "要求加密但 max_dialect 为 %s，SMB3 加密最低需要 3.0", c.Server.MaxDialect)
	}
	if c.Server.EncryptionRequired && minRank >= 0 && minRank < dialectRank("3.0") {
		errs.add("server.min_dialect",
			"server.encryption_required 为 true 时 min_dialect 不能低于 3.0（当前 %s）——"+
				"SMB 2.x 没有加密能力，这些客户端会被直接拒绝。"+
				"请设 min_dialect: \"3.0\"，或关闭 encryption_required",
			c.Server.MinDialect)
	}

	if c.Server.MaxConnections < 0 {
		errs.add("server.max_connections", "不能为负数（0 表示使用默认上限 256），当前 %d", c.Server.MaxConnections)
	}
}

func validateListen(c *Config, errs *ValidationErrors) {
	// 地址会被逐个 net.Listen，只要有一个绑不上整个服务就起不来。
	// 内核给出的 "address already in use" 对运维毫无信息量，
	// 这里把已知会冲突的组合在启动前就拦下来并说清楚原因。
	seen := make(map[string]int, len(c.Listen.Addresses))
	wildcard := -1 // 第一个通配地址（0.0.0.0 / ::）的下标

	for i, a := range c.Listen.Addresses {
		field := fmt.Sprintf("listen.addresses[%d]", i)
		if a == "" {
			errs.add(field, "不能为空字符串（若要监听全部地址请把 addresses 整个留空）")
			continue
		}
		ip := net.ParseIP(a)
		if ip == nil {
			errs.add(field, "不是合法 IP 地址: %q（这里只接受 IP，不接受主机名，端口写在 listen.port）", a)
			continue
		}

		// 用解析后的形式做键：'::1' 与 '0:0:0:0:0:0:0:1' 是同一个地址。
		key := ip.String()
		if prev, ok := seen[key]; ok {
			errs.add(field, "地址 %q 与 listen.addresses[%d] 重复，同一地址不能监听两次", a, prev)
			continue
		}
		seen[key] = i

		if ip.IsUnspecified() {
			if wildcard < 0 {
				wildcard = i
			}
		}
	}

	// 通配地址已经覆盖了本机全部地址，再列任何地址都会撞端口。
	// 特别常见的写法是同时写 0.0.0.0 与 ::，直觉上"一个管 v4 一个管 v6"，
	// 但 Go 的 "tcp" 监听是双栈的：先绑 :: 就已经把 0.0.0.0 也占了，
	// 第二个 net.Listen 必然 EADDRINUSE。
	if wildcard >= 0 && len(seen) > 1 {
		errs.add(fmt.Sprintf("listen.addresses[%d]", wildcard),
			"通配地址 %q 已经覆盖本机全部地址，不能再与其他地址并列（Go 的 tcp 监听是双栈的，"+
				"同时写 0.0.0.0 和 :: 也会抢同一个端口）。要监听全部地址请把 addresses 整个留空，"+
				"要监听指定地址就别写通配地址",
			c.Listen.Addresses[wildcard])
	}

	if c.Listen.Port < 1 || c.Listen.Port > 65535 {
		errs.add("listen.port", "端口必须在 1-65535 之间，当前 %d", c.Listen.Port)
	}
}

func validateShares(c *Config, errs *ValidationErrors) {
	if len(c.Shares) == 0 {
		errs.add("shares", "至少要配置一个共享目录，否则服务没有任何可用内容")
		return
	}

	seen := make(map[string]int, len(c.Shares))
	for i := range c.Shares {
		s := &c.Shares[i]
		prefix := fmt.Sprintf("shares[%d]", i)
		validateShareName(s, i, prefix, seen, errs)
		validateSharePath(s, prefix, errs)
		validateShareMetadataPath(s, prefix, errs)

		if s.TimeMachine && s.ReadOnly {
			errs.add(prefix+".time_machine", "共享 %q 标记为 Time Machine 目标但同时是只读，备份会失败", s.Name)
		}
	}
}

func validateShareName(s *Share, idx int, prefix string, seen map[string]int, errs *ValidationErrors) {
	if s.Name == "" {
		errs.add(prefix+".name", "共享名不能为空")
		return
	}
	if len(s.Name) > ShareNameMaxLen {
		errs.add(prefix+".name", "共享名长度 %d 超过上限 %d: %q", len(s.Name), ShareNameMaxLen, s.Name)
	}
	if strings.ContainsAny(s.Name, shareNameInvalidChars) {
		errs.add(prefix+".name", "共享名 %q 含非法字符（不允许 %s）", s.Name, shareNameInvalidChars)
	}
	for _, r := range s.Name {
		if r < 0x20 || r == 0x7f {
			errs.add(prefix+".name", "共享名 %q 含控制字符 U+%04X", s.Name, r)
			break
		}
	}

	// 保留名：IPC$ 由服务端内建（srvsvc 命名管道），不允许用户占用。
	lower := strings.ToLower(s.Name)
	if lower == "ipc$" {
		errs.add(prefix+".name", "%q 是服务端保留共享名，请换一个", s.Name)
	}

	// SMB 共享名大小写不敏感，重名判定也必须不敏感。
	if prev, ok := seen[lower]; ok {
		errs.add(prefix+".name", "共享名 %q 与 shares[%d] 重复（共享名大小写不敏感）", s.Name, prev)
	} else {
		seen[lower] = idx
	}
}

func validateSharePath(s *Share, prefix string, errs *ValidationErrors) {
	if s.Path == "" {
		errs.add(prefix+".path", "共享 %q 未指定本地目录路径", s.Name)
		return
	}
	if !isAbsPath(s.Path) {
		errs.add(prefix+".path", "共享 %q 的路径必须是绝对路径，当前 %q", s.Name, s.Path)
		return
	}

	fi, err := os.Stat(s.Path)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			errs.add(prefix+".path", "共享 %q 的目录不存在: %s", s.Name, s.Path)
		case os.IsPermission(err):
			errs.add(prefix+".path", "共享 %q 的目录无权访问: %s", s.Name, s.Path)
		default:
			errs.add(prefix+".path", "共享 %q 的目录无法访问: %v", s.Name, err)
		}
		return
	}
	if !fi.IsDir() {
		errs.add(prefix+".path", "共享 %q 的路径不是目录: %s", s.Name, s.Path)
	}
}

// validateShareMetadataPath 校验 POSIX 元数据旁路存储路径（仅 Windows 生效）。
//
// 只校验"路径本身写得对不对"，文件存不存在由 vfs 层在启动时创建。
func validateShareMetadataPath(s *Share, prefix string, errs *ValidationErrors) {
	if s.MetadataPath == "" {
		return
	}
	field := prefix + ".metadata_path"
	if !isAbsPath(s.MetadataPath) {
		errs.add(field, "共享 %q 的 metadata_path 必须是绝对路径，当前 %q", s.Name, s.MetadataPath)
		return
	}

	// 父目录必须已存在，否则 vfs 启动时创建 KV 数据库会失败。
	dir := filepath.Dir(s.MetadataPath)
	fi, err := os.Stat(dir)
	switch {
	case err != nil && os.IsNotExist(err):
		errs.add(field, "共享 %q 的 metadata_path 所在目录不存在: %s", s.Name, dir)
	case err != nil:
		errs.add(field, "共享 %q 的 metadata_path 所在目录无法访问: %v", s.Name, err)
	case !fi.IsDir():
		errs.add(field, "共享 %q 的 metadata_path 所在路径不是目录: %s", s.Name, dir)
	}
}

func validateAuth(c *Config, errs *ValidationErrors) {
	seen := make(map[string]int, len(c.Auth.Users))
	for i := range c.Auth.Users {
		u := &c.Auth.Users[i]
		prefix := fmt.Sprintf("auth.users[%d]", i)

		if u.Name == "" {
			errs.add(prefix+".name", "用户名不能为空")
		} else {
			lower := strings.ToLower(u.Name)
			if prev, ok := seen[lower]; ok {
				errs.add(prefix+".name", "用户名 %q 与 auth.users[%d] 重复（用户名大小写不敏感）", u.Name, prev)
			} else {
				seen[lower] = i
			}
			if lower == "guest" {
				errs.add(prefix+".name", "%q 是保留用户名，guest 行为由 auth.allow_guest 控制", u.Name)
			}
		}

		switch {
		case u.Password == "" && u.NTHash == "":
			errs.add(prefix, "用户 %q 必须提供 password 或 nt_hash 之一", u.Name)
		case u.NTHash != "":
			if !isHex32(u.NTHash) {
				errs.add(prefix+".nt_hash", "用户 %q 的 nt_hash 必须是 32 位十六进制字符串（MD4(UTF16LE(password))），当前 %d 个字符", u.Name, len(u.NTHash))
			}
		}
	}

	if !c.Auth.AllowGuest && len(c.Auth.Users) == 0 {
		errs.add("auth", "allow_guest 为 false 且未配置任何用户，没有人能登录这台服务器")
	}

	// valid_users 引用的用户必须存在，否则该共享谁都进不去。
	for i := range c.Shares {
		s := &c.Shares[i]
		listed := make(map[string]int, len(s.ValidUsers))
		for j, name := range s.ValidUsers {
			field := fmt.Sprintf("shares[%d].valid_users[%d]", i, j)
			if strings.TrimSpace(name) == "" {
				errs.add(field, "共享 %q 的 valid_users 里有空条目（留空的 valid_users 表示所有已认证用户，"+
					"不要写空字符串）", s.Name)
				continue
			}
			lower := strings.ToLower(name)
			if prev, ok := listed[lower]; ok {
				errs.add(field, "共享 %q 的 valid_users 里 %q 与第 %d 项重复", s.Name, name, prev)
				continue
			}
			listed[lower] = j
			if _, ok := seen[lower]; !ok {
				errs.add(field, "共享 %q 引用了未定义的用户 %q（请先在 auth.users 里定义）", s.Name, name)
			}
		}
	}
}

func validateMDNS(c *Config, errs *ValidationErrors) {
	if !c.MDNS.Enabled {
		return
	}

	// RFC 6763 §4.1.1：实例名是一个 DNS label，UTF-8 编码后不得超过 63 字节。
	if n := len(c.MDNS.Instance); n > 63 {
		errs.add("mdns.instance", "实例名 UTF-8 编码后 %d 字节，超过 DNS label 上限 63 字节", n)
	}
	if strings.ContainsAny(c.MDNS.Instance, "\x00.") {
		errs.add("mdns.instance", "实例名不能含点号或 NUL: %q", c.MDNS.Instance)
	}

	for i, name := range c.MDNS.Interfaces {
		field := fmt.Sprintf("mdns.interfaces[%d]", i)
		if name == "" {
			errs.add(field, "网卡名不能为空（若要使用全部网卡请把 interfaces 整个留空）")
			continue
		}
		if _, err := net.InterfaceByName(name); err != nil {
			errs.add(field, "找不到网卡 %q: %v", name, err)
		}
	}

	if c.MDNS.Apple.Enabled && c.MDNS.Apple.Model == "" {
		errs.add("mdns.apple.model", "启用 Apple 扩展时 model 不能为空（决定 Finder 中显示的图标）")
	}
	// _device-info._tcp 的 model= 是单条 TXT 记录，长度上限 255 字节（RFC 6763 §6.1）。
	if n := len(c.MDNS.Apple.Model); n > 200 {
		errs.add("mdns.apple.model", "model 过长（%d 字节），单条 TXT 记录上限 255 字节", n)
	}

	if c.MDNS.Apple.AdvertiseTimeMachine && !c.MDNS.Apple.Enabled {
		errs.add("mdns.apple.advertise_time_machine", "需要同时设置 mdns.apple.enabled: true")
	}
	if c.MDNS.Apple.AdvertiseTimeMachine && !hasTimeMachineShare(c) {
		errs.add("mdns.apple.advertise_time_machine", "已开启 Time Machine 广播，但没有任何共享设置 time_machine: true")
	}
}

func hasTimeMachineShare(c *Config) bool {
	for i := range c.Shares {
		if c.Shares[i].TimeMachine {
			return true
		}
	}
	return false
}

func validateLog(c *Config, errs *ValidationErrors) {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs.add("log.level", "非法日志级别 %q，可选值: debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs.add("log.format", "非法日志格式 %q，可选值: text, json", c.Log.Format)
	}
	if c.Log.File == "" {
		return
	}
	if !isAbsPath(c.Log.File) {
		errs.add("log.file", "日志文件必须是绝对路径，当前 %q", c.Log.File)
		return
	}
	// 目录不存在时 os.OpenFile 会失败，但那要等到日志器初始化才暴露 ——
	// 而校验的承诺是"一次性报出全部问题"，所以提前查。
	dir := filepath.Dir(c.Log.File)
	fi, err := os.Stat(dir)
	switch {
	case err != nil && os.IsNotExist(err):
		errs.add("log.file", "日志文件所在目录不存在: %s", dir)
	case err != nil:
		errs.add("log.file", "日志文件所在目录无法访问: %v", err)
	case !fi.IsDir():
		errs.add("log.file", "日志文件所在路径不是目录: %s", dir)
	}
}

// Warnings 返回配置合法但值得提醒用户的问题。
//
// 与 Validate 分开：这些不阻止启动，但必须在日志里以 WARN 级别打出来。
func Warnings(c *Config) []string {
	var w []string

	// 445 是特权端口，非 root 且无 CAP_NET_BIND_SERVICE 时 bind 会失败，
	// 这是最常见的启动失败原因，提前提示。
	//
	// Windows 没有特权端口的概念（也没有 euid，os.Geteuid 恒返回 -1），
	// 在那里提示会是纯噪音，因此跳过。
	if c.Listen.Port < 1024 && runtime.GOOS != "windows" && os.Geteuid() != 0 {
		w = append(w, fmt.Sprintf(
			"listen.port=%d 是特权端口（<1024），当前不是 root。"+
				"请以 root 运行，或执行 setcap 'cap_net_bind_service=+ep' <二进制>，"+
				"或改用 >=1024 的端口", c.Listen.Port))
	}

	if c.Auth.AllowGuest {
		w = append(w, "auth.allow_guest=true：任何人都可以匿名访问共享，请确认这是你想要的"+
			"（注意 Windows 10/11 默认拒绝不安全的 guest 登录）")
	}

	for i := range c.Shares {
		s := &c.Shares[i]
		if s.GuestOK && !c.Auth.AllowGuest {
			w = append(w, fmt.Sprintf("shares[%d] %q 设置了 guest_ok 但 auth.allow_guest=false，该设置不会生效", i, s.Name))
		}
		// quota_bytes 太小（< 1 GiB）时 Time Machine 会反复备份失败、
		// 空间抖动，真实 NAS 都设下限。这里只 WARN 不报错：用户可能有意为之。
		if s.QuotaBytes != 0 && s.QuotaBytes < quotaMinWarn {
			w = append(w, fmt.Sprintf("shares[%d] %q 的 quota_bytes=%d 小于 1 GiB，Time Machine 在过小的卷上会反复失败", i, s.Name, s.QuotaBytes))
		}
		if s.MetadataPath == "" {
			continue
		}
		// POSIX 元数据旁路存储只在 Windows 编译进来（AGENTS.md §5 P7）。
		if runtime.GOOS != "windows" {
			w = append(w, fmt.Sprintf("shares[%d] %q 设置了 metadata_path，但该字段仅在 Windows 上生效，当前平台（%s）会忽略它",
				i, s.Name, runtime.GOOS))
		}
		if isUnderDir(s.MetadataPath, s.Path) {
			w = append(w, fmt.Sprintf("shares[%d] %q 的 metadata_path 位于共享目录内部，客户端会看到这个数据库文件，建议放到共享之外",
				i, s.Name))
		}
	}

	w = append(w, sharePathOverlapWarnings(c)...)

	for i := range c.Auth.Users {
		if c.Auth.Users[i].Password != "" {
			w = append(w, fmt.Sprintf("auth.users[%d] %q 使用明文口令，建议改用 nt_hash 避免口令落盘",
				i, c.Auth.Users[i].Name))
		}
	}

	if !c.Server.SigningRequired {
		w = append(w, "server.signing_required=false：未强制 SMB 签名，存在中间人篡改风险")
	}

	return w
}

// sharePathOverlapWarnings 提示互相重叠的共享目录。
//
// 只是 WARN 不是错误：把同一个目录导出两遍（一个只读一个可写）是合法用法。
// 但重叠会带来两个真实的坑，值得说清楚：
//   - 同一份文件在两个共享里各有一套句柄状态，锁与 oplock 互不可见；
//   - 只读共享套在可写共享里等于没有保护，客户端换个共享名就能写。
func sharePathOverlapWarnings(c *Config) []string {
	var w []string
	for i := range c.Shares {
		for j := i + 1; j < len(c.Shares); j++ {
			a, b := &c.Shares[i], &c.Shares[j]
			if a.Path == "" || b.Path == "" {
				continue
			}
			switch {
			case filepath.Clean(a.Path) == filepath.Clean(b.Path):
				w = append(w, fmt.Sprintf(
					"shares[%d] %q 与 shares[%d] %q 指向同一个目录 %s，"+
						"两个共享的文件锁与 oplock 状态互不可见",
					i, a.Name, j, b.Name, filepath.Clean(a.Path)))
			case isUnderDir(b.Path, a.Path):
				w = append(w, nestedShareWarning(j, b, i, a))
			case isUnderDir(a.Path, b.Path):
				w = append(w, nestedShareWarning(i, a, j, b))
			}
		}
	}
	return w
}

func nestedShareWarning(innerIdx int, inner *Share, outerIdx int, outer *Share) string {
	msg := fmt.Sprintf("shares[%d] %q 的目录位于 shares[%d] %q 之内（%s ⊂ %s）",
		innerIdx, inner.Name, outerIdx, outer.Name,
		filepath.Clean(inner.Path), filepath.Clean(outer.Path))
	if inner.ReadOnly && !outer.ReadOnly {
		msg += "；内层是只读共享而外层可写，客户端换个共享名就能绕过只读限制"
	}
	return msg
}

// isUnderDir 判断 p 是否位于目录 dir 之内（不含 dir 自身）。
//
// 只做词法比较，不解析符号链接 —— 这里只用于生成提示，不用于安全判定。
func isUnderDir(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

// isHex32 判断是否为 32 位十六进制字符串。
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}
