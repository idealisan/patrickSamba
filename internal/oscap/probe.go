package oscap

// probe.go —— 原生能力探测的跨平台入口。
//
// 平台实现在 probe_linux.go / probe_darwin.go / probe_windows.go /
// probe_other.go 里，各自提供 probeNative(Capability, Options) bool。
//
// # 探测的判据是「native adapter 到底能不能做到」，不是「内核有没有这个特性」
//
// 举例：macOS 有 F_PUNCHHOLE，但 internal/vfs/sys_darwin.go 至今没实现它
// （x/sys/unix 没导出 fpunchhole_t）。那么 darwin 上 CapSparseFile 就该探测为
// false，让 builtin 接管 —— 探测报 true 而 adapter 做不到，会在 native 模式下
// 变成「启动通过、运行时报 ErrNotSupported」，正是 §1.2 要禁止的静默失效。
//
// 所以：**native/ 补齐某项能力时，必须回来同步放开这里的判定**。
// 这条耦合是刻意的，它把「探测」与「实现」绑成一件事，而不是两处各自演化。

import "os"

// ProbeNative 是默认 Prober：在 o.Root 上真实探测能力 c。
//
// 三条硬保证：
//   - **绝不 panic**（下面的 recover 是最后一道保险，不是常态路径）；
//   - **不留垃圾**：探测用的临时文件/临时扩展属性在返回前清理干净；
//   - **不改用户数据**：只在共享根下建自己的临时文件，不碰任何既有对象的内容。
//
// 为什么要 recover：探测只在启动时每个共享跑一次，代价可以忽略；
// 而一个未预料的 panic 让服务**起不来**，远比降级到 builtin（照样能跑）更糟。
// 判成「不支持」是安全侧的答案。
func ProbeNative(c Capability, o Options) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	if !c.Valid() || o.Root == "" {
		return false
	}
	return probeNative(c, o)
}

// probeFilePrefix 是探测临时文件的名字前缀。
//
// 刻意带上项目名并以点开头：万一进程在探测中途被杀导致残留，
// 运维一眼能看出这是谁留下的，而且点开头在 POSIX 下默认不显示、
// 在 SMB 侧会被映射成 HIDDEN，不至于突兀地出现在客户端的目录列表里。
const probeFilePrefix = ".stupidsamba-capprobe-"

// withProbeFile 在 root 下建一个临时文件交给 fn，返回前删除。
//
// root 不可写时返回 false：连临时文件都建不出来，就没有任何办法**如实**
// 判定原生能力。此时宁可全部落到 builtin —— 猜一个 true 会让 native 模式
// 在运行期才炸。
func withProbeFile(root string, fn func(f *os.File) bool) bool {
	f, err := os.CreateTemp(root, probeFilePrefix+"*")
	if err != nil {
		return false
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()
	return fn(f)
}
