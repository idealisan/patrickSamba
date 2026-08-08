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
	if c.Server.EncryptionRequired && maxRank >= 0 && maxRank < dialectRank("3.0") {
		errs.add("server.encryption_required", "要求加密但 max_dialect 为 %s，SMB3 加密最低需要 3.0", c.Server.MaxDialect)
	}

	if c.Server.MaxConnections < 0 {
		errs.add("server.max_connections", "不能为负数（0 表示不限），当前 %d", c.Server.MaxConnections)
	}
}

func validateListen(c *Config, errs *ValidationErrors) {
	for i, a := range c.Listen.Addresses {
		field := fmt.Sprintf("listen.addresses[%d]", i)
		if a == "" {
			errs.add(field, "不能为空字符串（若要监听全部地址请把 addresses 整个留空）")
			continue
		}
		if net.ParseIP(a) == nil {
			errs.add(field, "不是合法 IP 地址: %q（这里只接受 IP，不接受主机名，端口写在 listen.port）", a)
		}
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
		for j, name := range s.ValidUsers {
			if _, ok := seen[strings.ToLower(name)]; !ok {
				errs.add(fmt.Sprintf("shares[%d].valid_users[%d]", i, j),
					"共享 %q 引用了未定义的用户 %q（请先在 auth.users 里定义）", s.Name, name)
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
	if c.Log.File != "" && !isAbsPath(c.Log.File) {
		errs.add("log.file", "日志文件必须是绝对路径，当前 %q", c.Log.File)
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
