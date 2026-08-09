package vfs

// oscap_seam_test.go —— 证明 vfs 与 internal/oscap 之间那条接缝**真的通电**。
//
// # 这个文件想防住的到底是什么
//
// 不是「扩展属性能不能读写」——那件事在接线之前就已经是绿的。
// 要防的是本仓库反复出现的那类隐形失败：**代码写了、测试绿了、
// 但那段代码在真实数据路径上一次都没被执行过。**
// oscap 这个库本身就是活生生的例子：port + 双适配器全套写完、31 个单测全绿，
// 而产品代码里没有一处 import 它，`filesystem_mode` 设了等于没设。
//
// 所以本文件的每条断言都配了**证伪手段**：
//
//   - 接线是否真的经过 oscap：注入一个会记账/会返回哨兵错误的 Provider，
//     它必须被调用到（TestXattrGoesThroughProvider / TestSeamReverseControl）。
//     反向对照的意义在于：如果哪天有人把 xattrAt 改回直接系统调用，
//     这两条会立刻红，而所有「功能性」用例依旧全绿。
//   - 矩阵是否真的生效：portable 档必须真的落到 builtin
//     （TestPortableUsesBuiltinMatrix），而不是「配置项存在」。
//   - 落盘位置是否真的变了：见 oscap_seam_unix_test.go，用原始系统调用
//     向内核求证 portable 档在宿主机上**一个扩展属性都没写**。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
	"github.com/finalappstore/stupidsamba/internal/oscap/builtin"
)

// hostJoin 把共享内相对路径（'/' 分隔）拼成宿主机路径。
func hostJoin(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// hostPath 是 hostJoin 的 LocalFS 版本。
func hostPath(fs *LocalFS, rel string) string { return hostJoin(fs.Root(), rel) }

// writeHostFile 在**还没有 LocalFS**的时候往宿主目录里放一个文件。
func writeHostFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := hostJoin(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newModeFS 建一个指定 filesystem_mode 的临时共享。
func newModeFS(t *testing.T, mode oscap.Mode) *LocalFS {
	t.Helper()
	fs, err := NewLocalFS(LocalConfig{
		Root:            t.TempDir(),
		CaseInsensitive: true,
		FilesystemMode:  mode,
	})
	if err != nil {
		t.Fatalf("NewLocalFS(mode=%s): %v", mode, err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// newCapsFS 建一个使用注入 Provider 的临时共享。
func newCapsFS(t *testing.T, caps oscap.Provider) *LocalFS {
	t.Helper()
	fs, err := NewLocalFS(LocalConfig{
		Root:            t.TempDir(),
		CaseInsensitive: true,
		Caps:            caps,
	})
	if err != nil {
		t.Fatalf("NewLocalFS(注入 Provider): %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// ------------------------------------------------------------ 受控 Provider

// errSeamSentinel 是只可能来自本文件的错误。
//
// 用哨兵而不是「随便一个错误」：这样断言能区分「我们注入的实现被调用了」
// 与「碰巧也失败了」。后者在真实环境里完全可能发生（比如宿主没 xattr），
// 而一条分不清这两者的断言证明不了任何事。
var errSeamSentinel = errors.New("oscap-seam: 哨兵错误（只可能来自注入的假 Provider）")

// countingXattr 记账并转发。
type countingXattr struct {
	inner oscap.Xattr
	get   int
	set   int
	rm    int
	list  int

	// lastRef 记下最后一次调用拿到的 Ref，用来验证 vfs 传下去的是宿主机
	// 真实路径而不是客户端路径（传错的话 native 会在别处读写，
	// 而两边都用同一个错路径时功能测试照样全绿）。
	lastRef oscap.Ref
}

func (c *countingXattr) GetXattr(ref oscap.Ref, name string) ([]byte, error) {
	c.get++
	c.lastRef = ref
	return c.inner.GetXattr(ref, name)
}

func (c *countingXattr) SetXattr(ref oscap.Ref, name string, value []byte) error {
	c.set++
	c.lastRef = ref
	return c.inner.SetXattr(ref, name, value)
}

func (c *countingXattr) RemoveXattr(ref oscap.Ref, name string) error {
	c.rm++
	c.lastRef = ref
	return c.inner.RemoveXattr(ref, name)
}

func (c *countingXattr) ListXattr(ref oscap.Ref) ([]string, error) {
	c.list++
	c.lastRef = ref
	return c.inner.ListXattr(ref)
}

// sentinelXattr 一律失败。
type sentinelXattr struct{}

func (sentinelXattr) GetXattr(oscap.Ref, string) ([]byte, error) { return nil, errSeamSentinel }
func (sentinelXattr) SetXattr(oscap.Ref, string, []byte) error   { return errSeamSentinel }
func (sentinelXattr) RemoveXattr(oscap.Ref, string) error        { return errSeamSentinel }
func (sentinelXattr) ListXattr(oscap.Ref) ([]string, error)      { return nil, errSeamSentinel }

// wrapProvider 换掉一个 Provider 的 Xattr()，其余原样转发。
type wrapProvider struct {
	oscap.Provider
	xattr oscap.Xattr
}

func (w wrapProvider) Xattr() oscap.Xattr { return w.xattr }

// newPortableProvider 造一个纯 builtin 的 Provider（不碰宿主可选能力）。
func newPortableProvider(t *testing.T, root string) oscap.Provider {
	t.Helper()
	p, err := oscap.Open(oscap.ModePortable, oscap.Options{Root: root}, nil, builtin.New)
	if err != nil {
		t.Fatalf("组装 portable Provider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// ------------------------------------------------------------ 接线断言

// TestXattrGoesThroughProvider 证明扩展属性**确实**经过 oscap.Provider。
//
// 这是本次接线唯一无法用功能性断言替代的一条：xattr 读写在接线前后
// 都能成功，所以「成功」证明不了接线。只有「我们注入的实现被调用到了」
// 才能证明。
func TestXattrGoesThroughProvider(t *testing.T) {
	root := t.TempDir()
	base := newPortableProvider(t, root)
	c := &countingXattr{inner: base.Xattr()}

	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, Caps: wrapProvider{base, c}})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	writeFile(t, fs, "doc.txt", "hello")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	host := fs.Root() + string(os.PathSeparator) + "doc.txt"
	if err := fs.writeAfpInfo(host, ai); err != nil {
		t.Fatalf("写 AfpInfo: %v", err)
	}
	if c.set == 0 {
		t.Fatal("写 FinderInfo 没有经过 oscap.Xattr.SetXattr —— 接缝没通电")
	}
	if _, err := fs.readAfpInfo(host); err != nil {
		t.Fatalf("读 AfpInfo: %v", err)
	}
	if c.get == 0 {
		t.Error("读 FinderInfo 没有经过 oscap.Xattr.GetXattr")
	}

	// Ref 里必须是宿主机路径。传成客户端相对路径时功能照样正常
	// （两边一致地错），只有在这里对一次才发现得了。
	if c.lastRef.Path != host {
		t.Errorf("传给 oscap 的 Ref.Path = %q，期望宿主机路径 %q", c.lastRef.Path, host)
	}
}

// TestSeamReverseControl 是上一条的**反向对照**。
//
// 一个从来没红过的断言与没有断言是一回事。这里把 Provider 换成一律返回
// 哨兵错误的实现：如果 xattr 读写还能成功，就说明它根本没走 Provider
// （例如有人把 xattrAt 改回了直接系统调用），必须红。
func TestSeamReverseControl(t *testing.T) {
	root := t.TempDir()
	base := newPortableProvider(t, root)
	fs, err := NewLocalFS(LocalConfig{
		Root: root, CaseInsensitive: true,
		Caps: wrapProvider{base, sentinelXattr{}},
	})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	writeFile(t, fs, "doc.txt", "hello")

	host := fs.Root() + string(os.PathSeparator) + "doc.txt"
	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	if err := fs.writeAfpInfo(host, ai); !errors.Is(err, errSeamSentinel) {
		t.Fatalf("注入的失败 Provider 没有生效，写 FinderInfo 得到 %v；"+
			"说明 xattr 写入没走 oscap.Provider", err)
	}
	if _, err := fs.readAfpInfo(host); !errors.Is(err, errSeamSentinel) {
		t.Fatalf("注入的失败 Provider 没有生效，读 FinderInfo 得到 %v", err)
	}
}

// TestPortableUsesBuiltinMatrix 钉住 filesystem_mode 真的能改变谁来干活。
//
// 光有配置项不算数：本仓库出过「策略开关只测了允许那条路径、拒绝那条
// 压根没接线」的事故。所以两个档位都要断言，而且断言的是**生效矩阵**
// （Provider.Matrix()），不是我们传进去的那个 Mode。
func TestPortableUsesBuiltinMatrix(t *testing.T) {
	portable := newModeFS(t, oscap.ModePortable)
	m := portable.caps.Matrix()
	for _, c := range oscap.Capabilities() {
		if got := m.Kind(c); got != oscap.KindBuiltin {
			t.Errorf("portable 档 %v 由 %v 承载，期望 builtin", c, got)
		}
	}

	// 反过来：auto 档在有扩展属性的宿主上必须真的用 native，
	// 否则 portable 那条断言就只是「两个档位都走 builtin」的同义反复。
	auto := newModeFS(t, oscap.ModeAuto)
	if hostXattrSupported(auto.Root()) {
		if got := auto.caps.Matrix().Kind(oscap.CapXattr); got != oscap.KindNative {
			t.Errorf("宿主支持扩展属性，auto 档却把 CapXattr 交给 %v", got)
		}
	}
}

// TestNativeModeFailsFastOnPosix 钉住 native 档在 POSIX 上的**真实**行为。
//
// 这不是缺陷而是设计：POSIX 没有 DOS 属性位（oscap/probe_linux.go 里
// CapDOSAttributes 无条件为 false），而 native 档承诺「不静默降级」，
// 于是启动即报 UnsupportedError。
//
// 把它写成断言而不是留给用户去撞，是因为反过来更危险：哪天有人给
// native 档加了「探测不到就退 builtin」的兜底，`filesystem_mode: native`
// 会立刻退化成一个**看起来在用却在偷偷降级**的开关 —— 而它存在的全部
// 意义就是在测试里钉死走的是哪条路。那种改动必须让这条用例红。
func TestNativeModeFailsFastOnPosix(t *testing.T) {
	if !hostXattrSupported(t.TempDir()) {
		// 宿主连扩展属性都没有时失败原因会混进 CapXattr，
		// 断言就不再是「只因为 DOS 属性位而失败」了。
		t.Skip("宿主不支持扩展属性，本用例要钉的是「仅 DOS 属性位缺席」这一种失败")
	}
	_, err := NewLocalFS(LocalConfig{Root: t.TempDir(), FilesystemMode: oscap.ModeNative})
	if err == nil {
		t.Fatal("native 档在 POSIX 上应当启动即失败（DOS 属性位无原生实现），却成功了")
	}
	var ue *oscap.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("期望 *oscap.UnsupportedError，得到 %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "DOS") && !strings.Contains(err.Error(), "dos") {
		t.Errorf("错误信息应指名是 DOS 属性位缺席，实际: %v", err)
	}
}

// TestNativeModeHasNoUsablePlatform 是上一条的补全：native 档在**任何**平台
// 上都启动即失败，不是只有 POSIX。
//
// 上一条用例名字里的 "OnPosix" 没写错，但会让人以为 Windows 是可用的那一家。
// 不是：每个平台各自都有至少一项能力被源码无条件判为不支持 ——
// linux/darwin 的 CapDOSAttributes、darwin 还有 CapSparseFile、
// windows 的 CapXattr（probe_windows.go 拒绝拿 NTFS ADS 冒充 xattr，
// 语义不等价），非三大平台走 probe_other.go 六项全 false。
// 而 native 档要求六项全部走原生（oscap/matrix.go 的 UnsupportedError）。
//
// 这条用例是 CHANGELOG / README / configs/example.yaml / AGENTS.md 四处
// 「native 接线后在三个平台上都会恒定启动失败，与文件系统无关」这句断言的
// **唯一门禁**。哪天有人补齐了本平台缺的原生实现，或给 native 加了静默降级，
// 它会当场变红 —— 提醒改的人同步去改那四处，别让文档腐烂成谎话。
//
// 它刻意**不**断言是哪一项能力缺席：那是平台相关的，钉死就只能在 POSIX 上跑，
// 也就重新退回上一条用例的覆盖面。
func TestNativeModeHasNoUsablePlatform(t *testing.T) {
	_, err := NewLocalFS(LocalConfig{Root: t.TempDir(), FilesystemMode: oscap.ModeNative})
	if err == nil {
		t.Fatalf("native 档在 %s 上应当启动即失败（本平台至少有一项能力无原生实现），却成功了。"+
			"若这是有意为之，请同步改掉 CHANGELOG / README / configs/example.yaml / AGENTS.md 四处的断言", runtime.GOOS)
	}
	var ue *oscap.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("期望 *oscap.UnsupportedError（fail-fast 而非静默降级），得到 %T: %v", err, err)
	}
}

// TestSameRootTwoSharesShareOneStore 覆盖同一个共享根被导出两次。
//
// 真实配置里这很常见（同一目录一个可写共享、一个只读共享）。
// 两个 LocalFS 会算出同一个 builtin 库路径，而 bbolt 用 flock 互斥 ——
// 不做进程内复用的话第二个会**卡满 5 秒**然后报「是否已被另一个实例占用」，
// 而占用者就是自己，这句提示会把排查引向完全错误的方向。
func TestSameRootTwoSharesShareOneStore(t *testing.T) {
	root := t.TempDir()
	rw, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, FilesystemMode: oscap.ModePortable})
	if err != nil {
		t.Fatalf("第一个共享: %v", err)
	}
	t.Cleanup(func() { _ = rw.Close() })

	ro, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, ReadOnly: true, FilesystemMode: oscap.ModePortable})
	if err != nil {
		t.Fatalf("同一个根再开一个只读共享失败（旁路存储没做进程内复用？）: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	// 只读那一侧必须仍然拒绝写入 —— 复用的是**库句柄**，不是权限。
	writeFile(t, rw, "f", "x")
	host := rw.Root() + string(os.PathSeparator) + "f"
	if err := rw.xattrAt(host, nil).Set("probe", []byte{1}); err != nil {
		t.Fatalf("可写共享写扩展属性: %v", err)
	}
	if err := ro.xattrAt(host, nil).Set("probe2", []byte{1}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读共享写扩展属性应得 ErrReadOnly，得到 %v", err)
	}
	// 两侧看的是同一份数据。
	if _, err := ro.xattrAt(host, nil).Get("probe"); err != nil {
		t.Errorf("只读共享应能读到可写共享刚写的值: %v", err)
	}
}

// TestStoreReleasedWhenAllSharesClose 是上一条的**反向对照**。
//
// 缺了它，TestSameRootTwoSharesShareOneStore 在「把 flock 整个删掉」时
// 也会通过 —— 那条用例只证明「不再卡」，不证明「锁还在」。
// 这里证明的是另一半：引用计数归零时底层库真的被关掉了，锁真的还回去了，
// 所以紧接着的一次打开必须**立刻**成功，而不是等 5 秒超时。
//
// 判据用「耗时上限」而不是「有没有报错」：漏关时表现恰恰是**最终也成功**
// （前一个句柄被 GC/进程退出释放），只是中间卡了整整一个 flock 超时。
// 只断言 err == nil 的用例抓不到它。
//
// 阈值取 2 秒：flock 超时是 5 秒，正常打开是毫秒级，中间隔着一个数量级以上，
// 不是那种「在慢机器上会抖」的墙钟比值判据。
func TestStoreReleasedWhenAllSharesClose(t *testing.T) {
	root := t.TempDir()
	open := func() *LocalFS {
		t.Helper()
		fs, err := NewLocalFS(LocalConfig{
			Root: root, CaseInsensitive: true, FilesystemMode: oscap.ModePortable,
		})
		if err != nil {
			t.Fatalf("NewLocalFS: %v", err)
		}
		return fs
	}

	a, b := open(), open()
	if err := a.Close(); err != nil {
		t.Fatalf("关第一个: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("关第二个: %v", err)
	}

	start := time.Now()
	c := open()
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = c.Close() })

	if elapsed > 2*time.Second {
		t.Errorf("全部关闭后重新打开耗时 %s —— 旁路存储的文件锁没有随引用计数归零而释放"+
			"（正常是毫秒级；接近 5 秒说明在等 flock 超时）", elapsed)
	}
}

// ------------------------------------------------------- 命名流的接线断言

// sentinelStreams 一律失败，用法同 sentinelXattr。
type sentinelStreams struct{}

func (sentinelStreams) ListStreams(oscap.Ref) ([]oscap.StreamInfo, error) {
	return nil, errSeamSentinel
}

func (sentinelStreams) OpenStream(oscap.Ref, string, oscap.StreamFlags) (oscap.StreamHandle, error) {
	return nil, errSeamSentinel
}

func (sentinelStreams) RemoveStream(oscap.Ref, string) error { return errSeamSentinel }

// wrapStreamProvider 换掉一个 Provider 的 Streams()，其余原样转发。
type wrapStreamProvider struct {
	oscap.Provider
	streams oscap.NamedStream
}

func (w wrapStreamProvider) Streams() oscap.NamedStream { return w.streams }

// TestNamedStreamGoesThroughProvider 与它的反向对照。
//
// 命名流是本次接线里**更容易接漏**的一项：它在 native 侧同样落在扩展属性上，
// 所以「留着旧的直连代码」与「接上 oscap」在功能上完全无法区分 ——
// 两种写法读写都成功、往返都一致、落盘字节还一模一样。
// 唯一能区分的就是下面这条：把 Provider 换掉，它必须当场失效。
func TestNamedStreamGoesThroughProvider(t *testing.T) {
	root := t.TempDir()
	base := newPortableProvider(t, root)
	fs, err := NewLocalFS(LocalConfig{
		Root: root, CaseInsensitive: true,
		Caps: wrapStreamProvider{base, sentinelStreams{}},
	})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	writeFile(t, fs, "f", "x")

	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "s", Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	}); !errors.Is(err, errSeamSentinel) {
		t.Errorf("开命名流得到 %v；期望哨兵错误 —— 说明它没走 oscap.Provider", err)
	}

	// 枚举那条路按契约「失败即空列表」，所以不能断言错误，
	// 只能断言「没有凭空冒出流」。
	for name := range streamNames(t, fs, "f") {
		if name != DefaultStreamName {
			t.Errorf("Provider 已失效却枚举出命名流 %q —— 枚举没走 Provider", name)
		}
	}
}

// TestAppleFastPathGoesThroughProvider 覆盖 readdir_attr 的快路径。
//
// 这条路径（optional.go 的 appleInfoAt，两个入口：按路径的 AppleInfo 与
// 目录句柄内的 AppleInfoAt）原本绕开通用访问器、直接一次 getxattr 读取
// 402 字节的 metadata blob。它是「接了但没接全」的高危点：只改
// Handle.Xattr 而漏掉这里，portable 档下大目录枚举会继续偷偷直读宿主 xattr，
// 而所有功能用例照样全绿。
//
// 用计数而不是注入错误：appleInfoAt 对「读不到 FinderInfo」是**刻意宽容**的
// （一个坏掉的 FinderInfo 不该让整条目录项失败），注错误进去看不出区别。
func TestAppleFastPathGoesThroughProvider(t *testing.T) {
	root := t.TempDir()
	base := newPortableProvider(t, root)
	c := &countingXattr{inner: base.Xattr()}
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, Caps: wrapProvider{base, c}})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	writeFile(t, fs, "d/a", "x")

	before := c.get
	if _, _, err := fs.AppleInfo("d/a"); err != nil {
		t.Fatalf("AppleInfo: %v", err)
	}
	if c.get == before {
		t.Error("AppleInfo 一次都没调用 oscap.Xattr —— readdir_attr 快路径仍在直连宿主 xattr")
	}

	h, _, err := fs.Open(&OpenRequest{
		Path: "d", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	before = c.get
	if _, _, err := h.(DirAppleMetadata).AppleInfoAt("a"); err != nil {
		t.Fatalf("AppleInfoAt: %v", err)
	}
	if c.get == before {
		t.Error("目录句柄内的 AppleInfoAt 没有经过 oscap.Xattr")
	}
}

// TestBothModesSameBehaviour 让**同一张行为表**在两档下各跑一遍。
//
// 只测默认档是假阳性温床：builtin 存在的全部理由就是「与 native 语义一致」，
// 而两份各自为政的用例会让它们慢慢长歪且无人察觉。
//
// native 那一侧用 ModeAuto + 矩阵断言，而不是 ModeNative：POSIX 上
// CapDOSAttributes 没有原生实现，ModeNative 必然启动失败（见
// TestNativeModeFailsFastOnPosix），用它根本建不出 LocalFS。
// ModeAuto 走的是**产品装配路径**，配上 requireKind 同样能钉死走的哪条路。
func TestBothModesSameBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode oscap.Mode
		want oscap.Kind
	}{
		{"native", oscap.ModeAuto, oscap.KindNative},
		{"builtin", oscap.ModePortable, oscap.KindBuiltin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newModeFS(t, tc.mode)
			if tc.want == oscap.KindNative && !hostXattrSupported(fs.Root()) {
				t.Skipf("宿主不支持扩展属性，auto 档在这里不会选 native（矩阵: %s）", fs.caps.Matrix())
			}
			requireKind(t, fs, oscap.CapXattr, tc.want)
			requireKind(t, fs, oscap.CapNamedStream, tc.want)

			xattrContract(t, fs)
			streamContract(t, fs)
		})
	}
}

// requireKind 断言某项能力**实际**由哪一侧提供，不成立就判红。
//
// 绝不 skip：这条断言的全部意义就是钉死「这一轮跑的是哪条路」。
// 一个会自己跳过的模式断言，等于把两档测试悄悄变成同一档跑两遍。
func requireKind(t *testing.T, fs *LocalFS, c oscap.Capability, want oscap.Kind) {
	t.Helper()
	if got := fs.caps.Matrix().Kind(c); got != want {
		t.Fatalf("能力 %v 由 %v 提供，期望 %v；完整矩阵: %s", c, got, want, fs.caps.Matrix())
	}
}

// xattrContract 是扩展属性的行为契约，两档共用同一份。
func xattrContract(t *testing.T, fs *LocalFS) {
	t.Helper()
	writeFile(t, fs, "obj", "content")
	x := fs.xattrAt(hostPath(fs, "obj"), nil)

	if _, err := x.Get("com.apple.metadata:kMDItemFinderComment"); !errors.Is(err, ErrNotFound) {
		t.Errorf("读不存在的属性 = %v，期望 ErrNotFound", err)
	}

	want := []byte("hello\x00world")
	if err := x.Set("com.apple.quarantine", want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := x.Get("com.apple.quarantine"); err != nil || string(got) != string(want) {
		t.Errorf("往返 = %q, err=%v；期望 %q", got, err, want)
	}

	// 「存在但为空」与「不存在」必须区分得开（ports.go 明文规定）。
	if err := x.Set("empty.one", nil); err != nil {
		t.Fatalf("Set 空值: %v", err)
	}
	if v, err := x.Get("empty.one"); err != nil || len(v) != 0 {
		t.Errorf("空值属性 = %q, err=%v；期望读到一个零长度的值", v, err)
	}

	// List 报出来的名字拿去 Get 必须取得到（双向对称）。
	names, err := x.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
		if _, err := x.Get(n); err != nil {
			t.Errorf("List 报出 %q 却 Get 不到: %v（名字编解码不对称）", n, err)
		}
	}
	for _, w := range []string{"com.apple.quarantine", "empty.one"} {
		if !seen[w] {
			t.Errorf("List 少了 %q，实际 %v", w, names)
		}
	}

	if err := x.Remove("com.apple.quarantine"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := x.Remove("com.apple.quarantine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除 = %v，期望 ErrNotFound", err)
	}
}

// streamContract 是命名流的行为契约，两档共用同一份。
func streamContract(t *testing.T, fs *LocalFS) {
	t.Helper()
	writeFile(t, fs, "doc2.txt", "main data")

	h := openGeneric(t, fs, "doc2.txt", "meta", OpenAlways)
	if _, err := h.WriteAt([]byte("STREAMDATA"), 0); err != nil {
		t.Fatalf("写流: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 枚举结果里的键是 SMB 线格式的完整流名（`:meta:$DATA`），不是裸的 "meta"。
	// 直接用 "meta" 查会恒为 miss —— 这是一条会静默变成「永远不成立」的断言，
	// 而不是会报错的写法，所以这里显式走 StreamName() 构造。
	got := streamNames(t, fs, "doc2.txt")
	if size, ok := got[StreamName("meta")]; !ok || size != int64(len("STREAMDATA")) {
		t.Errorf("枚举 meta: size=%d ok=%v，期望 10；完整结果 %v", size, ok, got)
	}
	if _, ok := got[DefaultStreamName]; !ok {
		t.Errorf("枚举结果缺主数据流: %v", got)
	}

	h2 := openGeneric(t, fs, "doc2.txt", "meta", OpenExisting)
	buf := make([]byte, 32)
	n, err := h2.ReadAt(buf, 0)
	if err != nil && n == 0 {
		t.Fatalf("读流: %v", err)
	}
	if string(buf[:n]) != "STREAMDATA" {
		t.Errorf("流内容 = %q，期望 STREAMDATA", buf[:n])
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	// 主数据流不能被命名流写坏 —— 两者是不同的字节区间。
	main, _, err := fs.Open(&OpenRequest{
		Path: "doc2.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = main.Close() }()
	mb := make([]byte, 32)
	mn, _ := main.ReadAt(mb, 0)
	if string(mb[:mn]) != "main data" {
		t.Errorf("主数据流 = %q，期望 main data（命名流把主数据写坏了）", mb[:mn])
	}
}
