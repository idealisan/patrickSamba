package vfs

// path.go —— **本项目最重要的安全边界**（AGENTS.md §8）。
//
// SMB 客户端可以发送任意字节串作为路径。本文件负责把这些不可信输入
// 变成一个「保证位于共享根之内」的宿主机路径，任何一步失败都必须返回
// 错误而不是 panic。
//
// 威胁模型：
//
//  1. 词法穿越：`..`、`a/../../b`、绝对路径 `/etc/passwd`、`C:\Windows`。
//  2. 分隔符混淆：宿主机是 Windows 时 `\` 也是分隔符，`a\..\..\x` 能逃逸。
//  3. 符号链接逃逸：攻击者（或普通用户）在共享内放一个指向 `/etc` 的
//     软链，纯字符串规范化完全拦不住。
//  4. 设备名：宿主机是 Windows 时打开 `CON`/`COM1` 会打开真实设备。
//  5. NUL 截断：Go 字符串可含 NUL，但底层 syscall 以 NUL 结尾，
//     `"a\x00/../../etc"` 在不同层可能被解释成不同路径。
//
// 对应的防御分别是：分量级词法解析（1）、统一把 `\` 当分隔符（2）、
// 逐级 lstat + EvalSymlinks 包含性校验（3）、保留名黑名单（4）、
// 显式拒绝 NUL（5）。
//
// ## 符号链接方案的权衡
//
// 有两种做法：
//
//	(a) 逐级 lstat，遇到符号链接就用 filepath.EvalSymlinks 求值并校验
//	    结果仍在 root 内；
//	(b) Linux 专有的 openat2(RESOLVE_BENEATH) 或逐级 openat + O_NOFOLLOW。
//
// 本实现选 (a)，理由：
//
//   - 跨平台（AGENTS.md C7 要求 linux/darwin/windows 都能编译运行），
//     (b) 的 openat2 只有 Linux 5.6+ 有，darwin/windows 都没有对应物；
//   - 纯 Go 标准库即可，不引入平台分支的复杂度；
//   - VFS 的每个操作（Stat/Remove/Rename/…）都要一个**路径**而不是 fd，
//     用 (b) 的 fd 方案得把整个后端改成 *at 系列调用，收益不抵成本。
//
// 代价是 (a) 存在 TOCTOU 窗口：校验通过之后、真正 open 之前，攻击者
// 可以把某一级目录换成软链。缓解措施：
//
//   - 打开最终对象时始终带 O_NOFOLLOW（见 local.go），使「最后一跳」
//     不可能被换成软链；
//   - 共享根内的写入者本来就是已通过认证的 SMB 用户，且其能创建的
//     软链只在本共享内可见；
//   - 这与 Samba 默认的 `follow symlinks = yes` + `wide links = no`
//     的安全模型等价，属于业界可接受水位。
//
// TODO: Linux 上可以再加一条 openat2(RESOLVE_BENEATH) 的快路径彻底消除
// TOCTOU，等 local.go 稳定后再做（需要把 LocalFS 改成持有 root 的 dirfd）。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// MaxComponentLen 是单个路径分量的最大字节数。
	// NTFS 与绝大多数 POSIX 文件系统都是 255。
	MaxComponentLen = 255

	// MaxPathLen 是整条共享内相对路径的最大字节数。
	// Linux PATH_MAX 为 4096（含结尾 NUL），这里对相对路径取同一量级，
	// 再叠加共享根前缀由 syscall 自己兜底。
	MaxPathLen = 4096
)

// invalidNameChars 是 Windows 文件名中非法的字符集合（MS-FSCC §2.1.5
// "Pathname"）：`" * / : < > ? \ |`。
//
// 说明：
//   - `/` 与 `\` 在到达这里之前已被当作分隔符切开，不会出现在分量里；
//     若仍出现说明上层传了转义过的怪东西，直接拒。
//   - `:` 是 alternate data stream 的分隔符，必须由上层（server 层）
//     先拆成 OpenRequest.Path + OpenRequest.Stream，到这里不允许再出现。
const invalidNameChars = `"*/:<>?\|`

// SplitPath 校验并规范化一条**共享内相对路径**，返回各路径分量。
//
// 输入约定见 OpenRequest.Path：'/' 分隔、不以 '/' 开头、空串表示共享根。
// 为了防御宿主机为 Windows 时的分隔符混淆，'\' 也一律按分隔符处理。
//
// 返回的分量序列不含 "."、".." 与空分量；根目录返回长度为 0 的切片。
func SplitPath(p string) ([]string, error) {
	if len(p) > MaxPathLen {
		return nil, fmt.Errorf("%w: 路径过长 (%d 字节 > %d)", ErrInvalidPath, len(p), MaxPathLen)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return nil, fmt.Errorf("%w: 路径含 NUL 字节", ErrInvalidPath)
	}
	if !utf8.ValidString(p) {
		// SMB 的路径是 UTF-16LE，wire 层解码后应当是合法 UTF-8。
		// 非法 UTF-8 说明解码有问题或客户端在构造畸形输入，直接拒绝，
		// 避免后续大小写折叠 / 通配符匹配出现不可预期行为。
		return nil, fmt.Errorf("%w: 路径不是合法 UTF-8", ErrInvalidPath)
	}

	// 统一分隔符：Windows 宿主上 '\' 同样是分隔符，若按普通字符处理，
	// `a\..\..\etc` 会在词法检查中漏网、却被 Windows 内核解释为逃逸。
	p = strings.ReplaceAll(p, `\`, "/")

	if p == "" {
		return nil, nil
	}
	if strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("%w: 不接受绝对路径 %q", ErrInvalidPath, p)
	}

	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for i, comp := range raw {
		switch comp {
		case "":
			// 允许结尾多余的分隔符（部分客户端会发 "dir\"），
			// 但中间的空分量（"a//b"）是畸形输入，拒绝 —— Windows 亦然。
			if i == len(raw)-1 {
				continue
			}
			return nil, fmt.Errorf("%w: 路径含空分量 %q", ErrInvalidPath, p)
		case ".":
			continue
		case "..":
			if len(out) == 0 {
				return nil, fmt.Errorf("%w: 路径 %q 试图越过共享根", ErrInvalidPath, p)
			}
			out = out[:len(out)-1]
		default:
			if err := ValidateComponent(comp); err != nil {
				return nil, err
			}
			out = append(out, comp)
		}
	}
	return out, nil
}

// CleanPath 返回规范化后的相对路径（'/' 分隔，根为空串）。
func CleanPath(p string) (string, error) {
	comps, err := SplitPath(p)
	if err != nil {
		return "", err
	}
	return strings.Join(comps, "/"), nil
}

// ValidateComponent 校验单个路径分量的**字符层面**合法性：
// 非空、不超长、无控制字符、无 SMB 非法字符，以及 winpath.go 里那套
// Windows 名字规则（设备名、宿主会归一的结尾点/空格）。
//
// ⚠️ **它不是防路径穿越的屏障。** 本函数**故意放行 "." 与 ".."** ——
// 它的唯一调用方 SplitPath 在 switch 里先行处理了那两个分量
// （见上方 :124~:130），轮不到这里。
//
// 因此「拿到一个来自客户端的名字 → ValidateComponent → filepath.Join」
// 这个模式**是有洞的**：`Join(dir, "..")` 直接就是父目录。
// 按名字寻址请改用本包的 validateChildName（optional.go），
// 或者干脆走 Resolver.Resolve 那条完整路径。
//
// 这不是假设：AppleInfoAt 的第一版就是这么写的，被自己的用例逼出来。
func ValidateComponent(name string) error {
	return validateComponent(name, hostNormalizesTrailingDotSpace)
}

// validateComponent 是 ValidateComponent 的实现，把宿主归一行为做成参数。
//
// 与 validateWindowsName 同样的理由（见 winpath.go 那段注释）：结尾点/空格
// 规则只在 Windows 宿主上生效，而本项目没有 Windows 机器。做成参数之后，
// 「接线之后 Windows 侧到底会不会拒」这件事能在 Linux 上被直接断言，
// 否则那半边逻辑等于没写（AGENTS.md §3 验收判据必须可证伪）。
func validateComponent(name string, hostTrimsTrailingDotSpace bool) error {
	if name == "" {
		return fmt.Errorf("%w: 空的路径分量", ErrInvalidPath)
	}
	if len(name) > MaxComponentLen {
		return fmt.Errorf("%w: 路径分量过长 (%d 字节 > %d)", ErrInvalidPath, len(name), MaxComponentLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 {
			// 控制字符（含 NUL）在 Windows 文件名中非法。
			return fmt.Errorf("%w: 路径分量 %q 含控制字符 0x%02x", ErrInvalidPath, name, c)
		}
		if strings.IndexByte(invalidNameChars, c) >= 0 {
			return fmt.Errorf("%w: 路径分量 %q 含非法字符 %q", ErrInvalidPath, name, rune(c))
		}
	}
	// "." 与 ".." 不满足 validateWindowsName 的输入约定：它的第 1 条规则看的是
	// 结尾字符，会把这两个连坐拒掉。而本函数按契约要放行它们（见上方说明），
	// 所以在此先行返回 —— 既满足被调方的前置条件，也保证同一个输入在三个平台
	// 上得到同一个结论，不会出现「Linux 放行、Windows 拒绝」的漂移。
	//
	// 这不是死代码：把它删掉，validateComponent("..", true) 会开始返回错误
	// （path_wiring_test.go 有对应断言）。本包现有的四个调用方都在调用前
	// 自行处理了这两个分量，所以删不删都影响不到它们 —— 正因如此才必须
	// 用参数化的 validateComponent 直接断言，光靠调用方是测不出来的。
	if name == "." || name == ".." {
		return nil
	}
	return validateWindowsName(name, hostTrimsTrailingDotSpace)
}

// Resolver 把共享内相对路径解析为宿主机绝对路径，并保证结果不逃出根目录。
//
// 并发安全：Resolver 本身无可变状态，可被多 goroutine 共享。
type Resolver struct {
	// root 是共享根的绝对路径，构造时已做过 EvalSymlinks，
	// 因此后续与解析结果做前缀比较是「实路径 vs 实路径」。
	root string

	// caseInsensitive 打开后，精确匹配失败时会扫描父目录做
	// 不区分大小写的回退查找（SMB 语义是大小写不敏感但保留大小写，
	// 而 POSIX 文件系统是大小写敏感的）。
	caseInsensitive bool
}

// NewResolver 构造一个以 root 为边界的解析器。root 必须已经存在且是目录。
func NewResolver(root string, caseInsensitive bool) (*Resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("vfs: 无法取得 %q 的绝对路径: %w", root, err)
	}
	// 根目录本身可能位于软链之下（例如 /var -> /private/var），
	// 先求值再作为比较基准，否则后续所有包含性检查都会误判。
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("vfs: 共享根 %q 不可用: %w", root, mapError(err))
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("vfs: 共享根 %q 不可用: %w", root, mapError(err))
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("vfs: 共享根 %q 不是目录: %w", root, ErrNotDir)
	}
	return &Resolver{root: filepath.Clean(real), caseInsensitive: caseInsensitive}, nil
}

// Root 返回共享根的宿主机绝对路径。
func (r *Resolver) Root() string { return r.root }

// CaseInsensitive 返回是否启用大小写不敏感回退查找。
func (r *Resolver) CaseInsensitive() bool { return r.caseInsensitive }

// Resolve 把共享内相对路径解析为宿主机绝对路径。
//
// 语义：
//   - 允许**最后一个分量**不存在（创建场景）；中间分量不存在返回 ErrNotFound。
//   - 遇到符号链接（任意一级，含最后一级）会校验其目标仍在根内，
//     否则返回 ErrPermission —— 逃逸出共享的软链一律不可访问。
//   - 返回的路径**不做**软链替换，仍是 root + 各分量拼接的结果，
//     以便调用方对最后一级使用 O_NOFOLLOW / lstat 得到软链自身的属性。
func (r *Resolver) Resolve(rel string) (string, error) {
	comps, err := SplitPath(rel)
	if err != nil {
		return "", err
	}
	return r.resolveComponents(comps)
}

// ResolveParent 解析出「父目录的宿主机路径」与「最后一个分量的名字」，
// 用于 Mkdir / Rename / Remove 这类需要单独处理末级名字的场景。
//
// 对共享根本身调用返回 ErrInvalidPath（根没有父）。
func (r *Resolver) ResolveParent(rel string) (dir string, name string, err error) {
	comps, err := SplitPath(rel)
	if err != nil {
		return "", "", err
	}
	if len(comps) == 0 {
		return "", "", fmt.Errorf("%w: 共享根没有父目录", ErrInvalidPath)
	}
	dir, err = r.resolveComponents(comps[:len(comps)-1])
	if err != nil {
		return "", "", err
	}
	name = comps[len(comps)-1]
	if r.caseInsensitive {
		if actual, ok := lookupCaseInsensitive(dir, name); ok {
			name = actual
		}
	}
	return dir, name, nil
}

// EvalFinal 把一个**已经过 Resolve 校验**的宿主机路径的最后一跳软链求值成实路径。
//
// 为什么需要它：Resolve 有意不做软链替换（好让调用方能 lstat 到软链自身），
// 但真正 open 的时候我们必须带 O_NOFOLLOW —— 否则「校验通过之后、open 之前」
// 这一段时间里最后一级可以被换成指向共享外的软链（TOCTOU）。
// 两个需求的调和办法就是：先把软链求值成**实路径**（求值过程中再确认一次
// 仍在根内），再对这个不含软链的实路径带 O_NOFOLLOW 打开。
// 这样既支持共享内的软链，最后一跳又不可能被偷换。
//
// 非软链路径原样返回。目标不存在（悬空软链）返回 ErrNotFound。
func (r *Resolver) EvalFinal(host string) (string, error) {
	fi, err := os.Lstat(host)
	if err != nil {
		return "", mapError(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return host, nil
	}
	real, err := filepath.EvalSymlinks(host)
	if err != nil {
		return "", mapError(err)
	}
	if !r.contains(real) {
		return "", fmt.Errorf("%w: 符号链接 %q 指向共享外", ErrPermission, host)
	}
	return real, nil
}

// resolveComponents 是逐级下降的核心。
func (r *Resolver) resolveComponents(comps []string) (string, error) {
	cur := r.root
	for i, comp := range comps {
		last := i == len(comps)-1
		next := filepath.Join(cur, comp)

		fi, err := os.Lstat(next)
		if err != nil && os.IsNotExist(err) && r.caseInsensitive {
			if actual, ok := lookupCaseInsensitive(cur, comp); ok {
				next = filepath.Join(cur, actual)
				fi, err = os.Lstat(next)
			}
		}
		if err != nil {
			if !os.IsNotExist(err) {
				return "", mapError(err)
			}
			if last {
				// 末级不存在是合法的（FILE_CREATE / FILE_OPEN_IF 等）。
				// cur 已确认在根内，comp 不含 ".."，因此 next 必然也在根内。
				return next, nil
			}
			return "", ErrNotFound
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			if err := r.checkSymlink(next); err != nil {
				return "", err
			}
		} else if !last && !fi.IsDir() {
			// 中间分量不是目录：POSIX 会给 ENOTDIR，这里提前给出确定的语义。
			return "", ErrNotDir
		}
		cur = next
	}
	return cur, nil
}

// checkSymlink 校验一个符号链接（含其后续链条）的最终目标仍在共享根内。
func (r *Resolver) checkSymlink(link string) error {
	real, err := filepath.EvalSymlinks(link)
	if err == nil {
		if !r.contains(real) {
			return fmt.Errorf("%w: 符号链接 %q 指向共享外", ErrPermission, link)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return mapError(err)
	}

	// 悬空软链（目标暂不存在）：EvalSymlinks 给不出实路径，
	// 退化为词法判断 —— 绝对目标一律拒绝，相对目标按父目录拼接后判包含。
	// 这样即使目标之后被创建出来，也不可能落在共享外。
	target, rerr := os.Readlink(link)
	if rerr != nil {
		return mapError(rerr)
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("%w: 符号链接 %q 指向绝对路径 %q", ErrPermission, link, target)
	}
	joined := filepath.Clean(filepath.Join(filepath.Dir(link), target))
	if !r.contains(joined) {
		return fmt.Errorf("%w: 符号链接 %q 指向共享外 %q", ErrPermission, link, target)
	}
	return nil
}

// contains 判断宿主机路径 p 是否位于共享根之内（含根本身）。
func (r *Resolver) contains(p string) bool {
	p = filepath.Clean(p)
	if p == r.root {
		return true
	}
	return strings.HasPrefix(p, r.root+string(filepath.Separator))
}

// lookupCaseInsensitive 在 dir 下做不区分大小写的名字查找。
//
// SMB 的语义是「大小写不敏感、保留大小写」，而 Linux 的 ext4/xfs 是
// 大小写敏感的。Windows 客户端经常用与磁盘上不同的大小写来打开文件
// （例如 Explorer 记住的是 `Report.TXT`，磁盘上是 `report.txt`），
// 精确匹配会误报 ENOENT。
//
// 性能：这是一次目录全扫描，只在**精确匹配失败**时才发生，
// 正常路径零开销。大目录（Time Machine 的 band 目录可达十万条）上
// 一次扫描很贵，但那种场景下客户端用的是从枚举里拿到的精确名字，
// 不会走到这里。
//
// TODO: 若实测有热点，可加一个按 (dir, mtime) 失效的小 LRU 缓存。
// 先做对再做快（不缓存就不会有「目录变了缓存没失效」的正确性问题）。
func lookupCaseInsensitive(dir, name string) (string, bool) {
	f, err := os.Open(dir)
	if err != nil {
		return "", false
	}
	defer f.Close()

	for {
		names, err := f.Readdirnames(256)
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return n, true
			}
		}
		if err != nil || len(names) == 0 {
			// io.EOF 或真实错误都在此收敛为「没找到」。
			_ = io.EOF
			return "", false
		}
	}
}
