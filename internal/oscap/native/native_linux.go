//go:build linux

package native

// native_linux.go —— Linux 上的原生能力清单。
//
// **这张表必须与 internal/oscap/probe_linux.go 逐项对齐**（见包注释纪律 3）：
// 探测报 true 而这里是 nil，会让 filesystem_mode: native 启动通过、运行时
// 才降级；反过来这里做了而探测报 false，则是白做一份没人用的实现。
//
//	能力              probe_linux.go              本文件
//	xattr             probeXattr                  posixXattr
//	named_stream      probeXattr（承载在 xattr 上）  posixStreams
//	sparse_file       probeSparseLinux            linuxSparse
//	stable_file_id    probeInode                  posixIDs
//	creation_time     probeBirthTimeLinux         linuxTimes（只读，见该文件说明）
//	dos_attributes    固定 false                   nil ← 见下

import "github.com/finalappstore/stupidsamba/internal/oscap"

func newSet(o oscap.Options) (oscap.Set, error) {
	return oscap.Set{
		Xattr:   &posixXattr{readOnly: o.ReadOnly},
		Sparse:  &linuxSparse{readOnly: o.ReadOnly},
		Streams: &posixStreams{readOnly: o.ReadOnly},
		IDs:     posixIDs{},
		Times:   linuxTimes{},

		// DOS 属性位**刻意留 nil**，与 probe_linux.go 一致。
		//
		// 别想着「用 user.DOSATTRIB 存一份」就算 native：那是我们自己发明的
		// 存储格式，只不过借了 xattr 当载体，本质是旁路存储 —— 按本项目的
		// 划分它属于 builtin。native 的定义是「借助 OS **既有**的能力表达
		// 同一份语义」，而 Linux 内核对 DOSATTRIB 一无所知。
		DOS: nil,

		// 无需释放任何资源：本平台的实现都是无状态的（每个方法自带 Ref），
		// 不持有 fd、不开旁路存储。Close 留 nil 即可。
		Close: nil,
	}, nil
}
