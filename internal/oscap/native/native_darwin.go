//go:build darwin

package native

// native_darwin.go —— macOS 上的原生能力清单。
//
// 与 internal/oscap/probe_darwin.go 逐项对齐：
//
//	能力              probe_darwin.go             本文件
//	xattr             probeXattr                  posixXattr
//	named_stream      probeXattr（承载在 xattr 上）  posixStreams
//	sparse_file       固定 false                   nil ← 见下
//	stable_file_id    probeInode                  posixIDs
//	creation_time     probeBirthTimeDarwin        darwinTimes（可读可写）
//	dos_attributes    固定 false                   nil

import "github.com/finalappstore/stupidsamba/internal/oscap"

func newSet(o oscap.Options) (oscap.Set, error) {
	return oscap.Set{
		Xattr:   &posixXattr{readOnly: o.ReadOnly},
		Streams: &posixStreams{readOnly: o.ReadOnly},
		IDs:     posixIDs{},
		Times:   &darwinTimes{readOnly: o.ReadOnly},

		// 稀疏文件**刻意留 nil**，即便 APFS 支持稀疏、macOS 10.11+ 有
		// F_PUNCHHOLE。原因是 x/sys/unix 没有导出 fpunchhole_t，硬拼要用
		// unsafe，而本项目在 macOS 上跑不了服务端测试（§3 的 macOS 一栏是
		// 拿 Finder 当**客户端**做验收）—— 一份既不能验证又要用 unsafe 的
		// 打洞实现，出问题的形态是「Time Machine 回收 band 时数据被写坏」，
		// 代价远大于降级到 builtin 带来的那点性能损失。
		//
		// 这与 probe_darwin.go 的判定一致（那里也固定 false 并留了同一条
		// TODO）。将来补齐时**两处要一起改**。
		Sparse: nil,

		// macOS 的 UF_HIDDEN 等 BSD 文件标志与 DOS 属性位语义并不重合，
		// 不能拿来充数。留 nil 交给 builtin。
		DOS: nil,

		// 元数据迁移是无操作（migration.go 有逐项清单）。
		Migration: noopMigration{},

		Close: nil,
	}, nil
}
