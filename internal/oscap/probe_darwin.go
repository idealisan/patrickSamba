//go:build darwin

package oscap

import "golang.org/x/sys/unix"

// probeXattrName 是探测用的扩展属性名。
// macOS 没有命名空间概念，原样使用（对比 probe_linux.go 的 `user.` 前缀）。
const probeXattrName = "stupidsamba.capprobe"

func probeNative(c Capability, o Options) bool {
	switch c {
	case CapXattr:
		return probeXattr(o.Root)

	case CapNamedStream:
		// 同 Linux：POSIX 上 native 命名流建立在扩展属性之上
		// （com.apple.ResourceFork 就是这么存的）。
		return probeXattr(o.Root)

	case CapSparseFile:
		// **刻意返回 false**，即便 APFS 支持稀疏文件、macOS 10.11+ 有 F_PUNCHHOLE。
		//
		// 理由是探测要对着「native adapter 能不能做到」而不是「内核有没有」：
		// x/sys/unix 没有导出 fpunchhole_t，本项目至今没有实现 darwin 打洞
		// （见 internal/vfs/sys_darwin.go:45-58 的同款说明与 TODO）。
		// 这里报 true 会让 filesystem_mode: native 启动通过、运行到 Time Machine
		// 回收 band 时才炸 —— 正是 §1.2 要禁止的静默失效。
		//
		// native/ 补齐 F_PUNCHHOLE 之后，把这里改成真实探测。
		return false

	case CapStableFileID:
		return probeInode(o.Root)

	case CapCreationTime:
		return probeBirthTimeDarwin(o.Root)

	case CapDOSAttributes:
		// 同 Linux：POSIX 没有原生 DOS 属性位。
		// macOS 的 FinderInfo 里那几个标志位不是 DOS 属性，语义并不重合。
		return false
	}
	return false
}

// probeBirthTimeDarwin 探测创建时间。
//
// darwin 的 stat(2) 直接带 st_birthtimespec，不需要 statx 那一套。
// x/sys/unix 把它命名为 Stat_t.Btim（ztypes_darwin_arm64.go:76），
// **不是** C 头文件里的 st_birthtimespec，写错名字会在交叉编译时才炸。
//
// HFS+/APFS 都有；取到 0 说明文件系统没记（例如挂载上来的 FAT/exFAT），
// 判不支持让 builtin 接管。
func probeBirthTimeDarwin(root string) bool {
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return false
	}
	return st.Btim.Sec > 0
}
