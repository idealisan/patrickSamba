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

// reservedNames 是 Windows 的设备名（MS-DOS 遗留）。
//
// 为什么在 Linux 上也拒绝：本项目要跨平台（AGENTS.md C7），同一份路径
// 逻辑必须在 Windows 宿主上也安全 —— 在 Windows 上 open("CON") 打开的是
// 控制台设备而不是文件。统一拒绝可以避免「Linux 上能建、Windows 上变成
// 设备」这种平台相关的惊吓。代价是宿主机上真实存在的名为 `aux` 的
// POSIX 文件将不可访问，这是可接受的取舍（Windows 客户端本来也建不出来）。
var reservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

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
// 非空、不超长、无控制字符、无 SMB 非法字符、不是 Windows 保留设备名。
//
// ⚠️ **它不是防路径穿越的屏障。** 本函数**故意放行 "." 与 ".."** ——
// 它的唯一调用方 SplitPath 在 switch 里先行处理了那两个分量
// （见上方 :139~:145），轮不到这里。
//
// 因此「拿到一个来自客户端的名字 → ValidateComponent → filepath.Join」
// 这个模式**是有洞的**：`Join(dir, "..")` 直接就是父目录。
// 按名字寻址请改用本包的 validateChildName（optional.go），
// 或者干脆走 Resolver.Resolve 那条完整路径。
//
// 这不是假设：AppleInfoAt 的第一版就是这么写的，被自己的用例逼出来。
func ValidateComponent(name string) error {
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
	// 设备名判定取第一个 '.' 之前的部分：Windows 下 "CON.txt" 同样是设备。
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if _, bad := reservedNames[strings.ToUpper(base)]; bad {
		return fmt.Errorf("%w: %q 是 Windows 保留设备名", ErrInvalidPath, name)
	}
	return nil
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
	if r.caseFallback() {
		// 先试精确匹配，**只有 ENOENT 才回退**到全目录扫描。
		//
		// 这不只是优化，也修一个语义问题：lookupCaseInsensitive 返回的是
		// readdir 顺序里第一个 EqualFold 命中，目录里同时存在 "a.txt" 与
		// "A.TXT" 时，可能把客户端明确指名的那一个折叠成另一个。
		//
		// 性能上更是必须：ResolveParent 是 Remove / Rename / Link 的必经之路，
		// 而这几个操作的末级名字**绝大多数时候在磁盘上原样存在**
		// （客户端用的是枚举里拿到的名字）。无条件全扫父目录会让
		// 每一次删除/改名都付出 O(目录条目数)，在 Time Machine 的
		// .sparsebundle/bands/（十万级）上就是每次操作几毫秒。
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil && os.IsNotExist(err) {
			if actual, ok := lookupCaseInsensitive(dir, name); ok {
				name = actual
			}
		}
	}
	return dir, name, nil
}

// caseFallback 判断是否需要做大小写不敏感回退查找。
//
// 这是给平台后端留的**唯一钩子**：宿主文件系统本身就大小写不敏感时
// （NTFS、APFS 默认配置），精确 Lstat 不可能因为大小写而 miss，
// 回退扫描是纯浪费，应当整个关掉。届时把这里改成
//
//	return r.caseInsensitive && !hostCaseInsensitive
//
// 并在 *_windows.go / *_unix.go 里定义 hostCaseInsensitive 常量即可，
// 不需要动任何调用点。
func (r *Resolver) caseFallback() bool { return r.caseInsensitive }

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
		if err != nil && os.IsNotExist(err) && r.caseFallback() {
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
// 性能：这是一次目录全扫描，成本 O(目录条目数)。所有调用点都必须
// **先做精确 Lstat，只在 ENOENT 时才回退到这里**。
//
// 实测（AMD EPYC 9K65，ext4，见 path_perf_test.go）单次扫描耗时：
//
//	1k 条目 ≈ 95 µs    10k ≈ 0.9 ms    50k ≈ 4.5 ms
//
// 也就是约 90 ns/条，线性增长。
//
// ⚠️ 仍未解决的热点：**末级名字在磁盘上根本不存在**时（FILE_CREATE
// 新建文件、Mkdir 新建目录），精确 Lstat 必然 miss，于是每一次创建
// 都要全扫一遍父目录。Time Machine 往 .sparsebundle/bands/ 灌十万个
// band，这就是 O(n²)。这一条**尚未修复**，方案待定（候选：按目录缓存
// 折叠索引并由本进程的改动点自行维护）。不要以为「加了精确匹配就没事了」。
//
// 这段注释上一版写的是「正常路径零开销……不会走到这里」——那是错的，
// 而且错在两处：ResolveParent 当时根本没做精确匹配（无条件全扫），
// 创建新文件也必然走到这里。留此记录，免得下一个人再被误导。
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
