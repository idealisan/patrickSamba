package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// isAbsPath 判断是否为**本平台**的绝对路径。
// 用 filepath 而非 path，以便 Windows 上 "C:\share" 也算绝对路径（AGENTS.md C7）。
//
// 适用于必须在**本机**存在的路径（share.path、log.file）——这些路径要被
// os.Stat / os.OpenFile 真正打开，用本平台语义判断才有意义。
// 只对**目标平台**有意义的路径（metadata_path）请用 isAbsPathOn。
func isAbsPath(p string) bool { return filepath.IsAbs(p) }

// isAbsPathOn 按**指定平台**的语义判断路径是否绝对。
//
// 存在的理由：metadata_path 是一个"只在 Windows 上生效"的字段，它的绝对性
// 必须按 Windows 语义判断，而不是按当前运行平台。filepath.IsAbs 在 Linux 上
// 编译进来的是 POSIX 版本，会把 "C:\ProgramData\x" 判成相对路径 —— 这正是
// 本函数要避免的误判。同时平台作为参数传入（而非读 runtime.GOOS），
// 使得在 Linux 上也能测到 Windows 分支。
func isAbsPathOn(p string, windows bool) bool {
	if !windows {
		return strings.HasPrefix(p, "/")
	}
	return isAbsWindowsPath(p)
}

// isAbsWindowsPath 判断 Windows 绝对路径。
//
// 认两种形式（与 Go 的 path/filepath windows 版 IsAbs 对齐）：
//   - 盘符根：`C:\foo` / `C:/foo`（注意 `C:foo` 是**盘符相对**路径，不算绝对）
//   - 双分隔符开头：UNC `\\server\share\foo` 与扩展前缀 `\\?\C:\foo`
//
// 刻意比 filepath.IsAbs 宽松一点：不校验 UNC 的 `host\share` 两段是否齐全。
// 本函数的职责是拦住"明显写错的相对路径"这类配置笔误，
// UNC 细节留给运行时真正打开失败时报错，避免在校验层制造假阴性。
func isAbsWindowsPath(p string) bool {
	if len(p) >= 2 && isWindowsSlash(p[0]) && isWindowsSlash(p[1]) {
		return true
	}
	// 盘符必须是 ASCII 字母，冒号后必须紧跟分隔符。
	if len(p) >= 3 && p[1] == ':' && isWindowsSlash(p[2]) {
		c := p[0]
		return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	return false
}

// isWindowsSlash：Windows 上正反斜杠都是路径分隔符。
// 委托给 vfs.IsWindowsSlash —— Windows 分隔符的唯一真源收敛到 vfs（PR #27），
// config 不再各自持有一份定义，避免两边静默漂移。
// 注释边界（见 vfs.IsWindowsSlash）：这是「配置校验的 Windows 分隔符」语义，
// 不是路径穿越安全校验，不能当安全边界用。
func isWindowsSlash(c byte) bool { return vfs.IsWindowsSlash(c) }

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
func Validate(c *Config) error { return validateOn(c, runtime.GOOS) }

// validateOn 是 Validate 的可注入平台版本。
//
// hostOS 取 runtime.GOOS 的取值域（"windows" / "linux" / "darwin" …）。
// 把平台做成参数而不是在判定点直接读 runtime.GOOS，是为了让"Windows 专属字段"
// 的两条分支都能在**任意**平台上被测到 —— 否则 Windows 分支在 CI（Linux）里
// 永远跑不到，等于没有测试。
func validateOn(c *Config, hostOS string) error {
	var errs ValidationErrors

	validateServer(c, &errs)
	validateFilesystemMode(c, &errs)
	validateListen(c, &errs)
	validateShares(c, hostOS, &errs)
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

	// 签名算法策略（MS-SMB2 §2.2.3.1.7 / §3.3.5.4）。
	switch c.Server.SigningAlgorithm {
	case "", DefaultSigningAlgorithm, "aes-cmac":
		// auto 是默认值；aes-cmac 对方言没有额外下限 —— CMAC 本来就是
		// 3.x 的默认签名算法，2.x 的 HMAC-SHA256 与它无关。
	case "aes-gmac":
		// AES-GMAC 只能通过 3.1.1 的 SIGNING_CAPABILITIES negotiate context
		// 协商出来（§2.2.3.1.7：该 context 仅在方言列表含 3.1.1 时有效）。
		// max_dialect < 3.1.1 意味着谁都协商不出 GMAC，配置自相矛盾，
		// 启动时一次性报错（人话错误，指出字段与原因），不做静默纠正。
		if maxRank >= 0 && maxRank < dialectRank("3.1.1") {
			errs.add("server.signing_algorithm",
				"aes-gmac 要求 max_dialect >= 3.1.1（当前 %s）—— "+
					"SMB2_SIGNING_CAPABILITIES 协商只在 3.1.1 上有效。"+
					"请把 max_dialect 设为 \"3.1.1\"，或改用 aes-cmac/auto",
				c.Server.MaxDialect)
		}
	default:
		errs.add("server.signing_algorithm",
			"非法取值 %q，可选值: auto, aes-cmac, aes-gmac", c.Server.SigningAlgorithm)
	}

	if c.Server.MaxConnections < 0 {
		errs.add("server.max_connections", "不能为负数（0 表示使用默认上限 256），当前 %d", c.Server.MaxConnections)
	}
}

// validateFilesystemMode 校验 OS 能力抽象的两态开关（AGENTS.md §1.2 C9）。
//
// 判定**直接委托给 oscap.ParseMode**，不在这里另抄一份取值表：
// 合法取值只能有一个真源。两处各写一份 switch 的下场是新增取值时改了一边、
// 另一边静默拒绝（或静默放行），而这类漂移只有用户在现场才发现。
//
// oscap.ParseMode 刻意严格 —— 不 ToLower、不 trim、不认空串。空串在这里
// 到不了：ApplyDefaults 会先填成 "auto"。真到了说明调用方跳过了 ApplyDefaults，
// 那就该报错，而不是替它猜一个默认值（Validate 的契约就是"调用前先 ApplyDefaults"）。
func validateFilesystemMode(c *Config, errs *ValidationErrors) {
	if _, err := oscap.ParseMode(c.FilesystemMode); err != nil {
		// 底层 err 一并带出：写 "native"（v0.5 已移除的档）的用户会拿到
		// 指名道姓的移除原因与替代建议，而不是一句干巴巴的「非法取值」。
		errs.add("filesystem_mode",
			"非法取值 %q，可选值: %s"+
				"（auto=逐项探测自动降级；"+
				"portable=全部使用本项目自带实现，可移植性最高）：%v",
			c.FilesystemMode, strings.Join(oscap.ModeNames(), ", "), err)
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

func validateShares(c *Config, hostOS string, errs *ValidationErrors) {
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
		validateShareMetadataPath(s, prefix, hostOS, errs)

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

// validateShareMetadataPath 校验元数据旁路存储路径（**所有平台都生效**）。
//
// 只校验"路径本身写得对不对"，文件存不存在由 vfs 层在启动时创建。
//
// # 为什么从"仅 Windows 校验"改成"所有平台校验"
//
// 旧版本在非 Windows 上**完全不校验**，理由是该字段会被运行时忽略
// （当时旁路存储只在 Windows 编译进来）。PR #159 把 oscap 的 CapXattr /
// CapNamedStream 接进数据路径之后，**这个前提没了**：该字段现在还决定
// `internal/oscap/builtin` 那份旁路 bbolt 库落在哪
// （`internal/vfs/oscap_xattr.go` 的 oscapMetadataDir），
// 实测 auto 与 portable 两档在 Linux 上都会真的在该目录里建出
// `.stupidsamba-oscap-<hash>.db`。
//
// 于是不校验的代价反过来了：一个写错的路径不再是"被忽略"，
// 而是拖到 NewLocalFS 才炸，报的还是底层 bbolt 错误而不是
// "配置的哪一项写错了"，违反 AGENTS.md §6"启动时一次性给出人话错误"。
//
// # 绝对性按"运行平台"判定（AGENTS.md 记忆：路径字段的平台语义）
//
// 判定一律走 isAbsPathOn(path, hostOS == "windows")，**不用** filepath.IsAbs ——
// 后者拿的是编译期平台语义，交叉编译/测试场景下会判错，那正是 PR #18 修过的
// 同源 bug 的根源。
//
// ⚠️ 这带来一处**行为变化**：一份给 Windows 写的配置
// （metadata_path: C:\ProgramData\...）拿到 Linux 上，以前是 WARN 后照常启动，
// 现在会**启动报错**。这是刻意的，而且恰恰是 PR #18 那条理由的自然延续：
// PR #18 反对的是"一个声称被忽略的字段却能拦住启动"这种自相矛盾；
// 如今该字段**不再被忽略**，那个自相矛盾也就不存在了——
// 真正会坑人的反倒是放行：Linux 上 filepath.Dir(`C:\...`) 得到 "."，
// 数据库会被**静默**建在进程当前工作目录里。宁可启动就报错，也不要静默放错地方。
// 报错文案会点明这是"另一个平台的绝对路径"，而不是笼统说"不是绝对路径"。
func validateShareMetadataPath(s *Share, prefix string, hostOS string, errs *ValidationErrors) {
	if s.MetadataPath == "" {
		return
	}
	field := prefix + ".metadata_path"
	onWindows := hostOS == "windows"

	if !isAbsPathOn(s.MetadataPath, onWindows) {
		// 分开两种写错法：写成了"另一个平台的绝对路径"是跨平台复用配置时的
		// 高发错误，笼统报"不是绝对路径"会让人一头雾水（它在他眼里明明是绝对的）。
		if isAbsPathOn(s.MetadataPath, !onWindows) {
			errs.add(field,
				"共享 %q 的 metadata_path %q 是另一个平台的绝对路径，当前运行平台是 %s；"+
					"该字段现在在所有平台都生效（决定旁路元数据库的位置），请改成 %s 平台的绝对路径",
				s.Name, s.MetadataPath, hostOS, hostOS)
			return
		}
		errs.add(field, "共享 %q 的 metadata_path 必须是绝对路径，当前 %q", s.Name, s.MetadataPath)
		return
	}

	// 父目录必须已存在，否则 vfs 启动时创建 KV 数据库会失败。
	//
	// 只在"校验平台 == 真实运行平台"时做这一步：拿 Linux 的文件系统去 stat
	// 一个 Windows 路径没有任何意义，只会产生假错误。跨平台校验（测试里用
	// hostOS 形参注入）到上面的语法判定为止。
	if hostOS != runtime.GOOS {
		return
	}
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

	if c.MDNS.Apple.EnabledOn() && c.MDNS.Apple.Model == "" {
		errs.add("mdns.apple.model", "启用 Apple 扩展时 model 不能为空（决定 Finder 中显示的图标）")
	}
	// _device-info._tcp 的 model= 是单条 TXT 记录，长度上限 255 字节（RFC 6763 §6.1）。
	if n := len(c.MDNS.Apple.Model); n > 200 {
		errs.add("mdns.apple.model", "model 过长（%d 字节），单条 TXT 记录上限 255 字节", n)
	}

	if c.MDNS.Apple.AdvertiseTimeMachine && !c.MDNS.Apple.EnabledOn() {
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
func Warnings(c *Config) []string { return warningsOn(c, runtime.GOOS) }

// warningsOn 是 Warnings 的可注入平台版本，理由同 validateOn。
func warningsOn(c *Config, hostOS string) []string {
	var w []string

	// 445 是特权端口，非 root 且无 CAP_NET_BIND_SERVICE 时 bind 会失败，
	// 这是最常见的启动失败原因，提前提示。
	//
	// Windows 没有特权端口的概念（也没有 euid，os.Geteuid 恒返回 -1），
	// 在那里提示会是纯噪音，因此跳过。
	if c.Listen.Port < 1024 && hostOS != "windows" && os.Geteuid() != 0 {
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
		// 这里**刻意不再有**"该字段仅在 Windows 上生效 / 当前平台会忽略它"
		// 那条 WARN —— PR #159 之后它是假话。
		//
		// 该字段现在有两个消费方，其中第二个在所有平台都跑：
		//   ① POSIX 属主/权限位旁路（internal/vfs/metadata_windows.go）—— 仅 Windows；
		//   ② oscap builtin 的六项能力旁路 bbolt 库（internal/vfs/oscap_xattr.go
		//      的 oscapMetadataDir）—— **所有平台**，auto 与 portable 两档实测都会
		//      在该目录里建出 .stupidsamba-oscap-<hash>.db。
		//
		// 留这段注释而不是直接删掉那行，是因为"某字段只在某平台生效"这类断言
		// 一旦过期极难被发现（它读起来很谦虚，抽查会通过）。写明它为什么消失，
		// 免得日后有人"好心"把它加回来。
		if isUnderDir(s.MetadataPath, s.Path) {
			w = append(w, fmt.Sprintf("shares[%d] %q 的 metadata_path 位于共享目录内部，客户端会看到这个数据库文件，建议放到共享之外",
				i, s.Name))
		}
	}

	w = append(w, sharePathOverlapWarnings(c)...)
	w = append(w, quotaUsageWarnings(c)...)

	for i := range c.Auth.Users {
		if c.Auth.Users[i].Password != "" {
			w = append(w, fmt.Sprintf("auth.users[%d] %q 使用明文口令，建议改用 nt_hash 避免口令落盘",
				i, c.Auth.Users[i].Name))
		}
		// 容器镜像内置的演示账号（configs/docker.yaml）。公开凭据等于匿名，
		// 必须让用户在日志里第一眼看到这一点。
		if c.Auth.Users[i].Name == "stupidsamba" && c.Auth.Users[i].Password == "stupidsamba" {
			w = append(w, "检测到内置演示账号 stupidsamba/stupidsamba：这是公开凭据，任何知道"+
				"镜像地址的人都能读写共享。仅限本机试用；对外服务请挂载自己的配置覆盖内置配置")
		}
	}

	if !c.Server.SigningRequired {
		w = append(w, "server.signing_required=false：未强制 SMB 签名，存在中间人篡改风险")
	}

	return w
}

// quotaScanBudget 是启动自检允许花在**单个共享**用量统计上的时间上限。
//
// 这只是一次性的启动提示，不能让服务为它迟迟不监听：超出预算就放弃这一条告警
// （静默跳过 —— 宁可不提示，也不要拿一个半截数字去吓唬人）。
// 运行期真正的用量统计由 vfs 在后台自己做，与这里无关。
const quotaScanBudget = 300 * time.Millisecond

// quotaUsageWarnings 检查 quota_bytes 是不是已经不大于共享现有的用量。
//
// 为什么值得在启动时查一次：这种配置下客户端看到的可用空间恒为 0，
// macOS 会**直接拒绝启动 Time Machine 备份**，而服务端日志里不会有任何异常 ——
// 用户那边只有 Finder 上一句「备份磁盘已满」，完全无从下手。
// 启动时一句人话能省掉几个小时的排查。
//
// 依赖方向说明：config 位于 vfs 之上（AGENTS.md §5 的分层图），
// 这里只是复用 vfs 的用量统计口径（分配空间、硬链接去重、不跟随软链），
// 免得同一件事在两个包里算出两个不同的数。
func quotaUsageWarnings(c *Config) []string {
	var w []string
	for i := range c.Shares {
		s := &c.Shares[i]
		if s.QuotaBytes == 0 || s.Path == "" {
			continue
		}
		used, ok := vfs.ScanUsage(s.Path, quotaScanBudget)
		if !ok {
			// 目录不存在（Validate 会另行报错）、读不到，或者大到来不及统计。
			continue
		}
		if used < s.QuotaBytes {
			continue
		}
		w = append(w, fmt.Sprintf(
			"shares[%d] %q 的 quota_bytes=%s 不大于该目录现有用量 %s，"+
				"客户端会看到 0 可用空间，Time Machine 会直接拒绝备份。"+
				"请把 quota_bytes 调到现有用量之上，或清理 %s",
			i, s.Name, humanBytes(s.QuotaBytes), humanBytes(used), s.Path))
	}
	return w
}

// humanBytes 把字节数写成人看得懂的单位。
// 告警信息里裸写 2199023255552 没人读得出那是 2 TiB。
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
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
