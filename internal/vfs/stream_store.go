package vfs

// stream_store.go —— alternate data stream 的落盘后端。
//
// 映射关系（选型理由见 appledouble.go 文件头，结论是对齐 Samba vfs_fruit
// 的两个默认模式 fruit:metadata=netatalk + fruit:resource=file）：
//
//	AFP_AfpInfo   → xattr  user.org.netatalk.Metadata  （402 字节 AppleDouble blob）
//	AFP_Resource  → 旁路文件 ._<basename>               （82 字节头 + 资源数据）
//	其它自定义流   → ErrNotSupported
//
// 为什么其它流直接不支持而不是随便找个地方存：SMB 的通用 ADS 需要在
// 枚举、改名、删除、配额统计上都保持一致，成本很高，而 Time Machine
// 只用到上面两个。宁可明确 STATUS_NOT_SUPPORTED，也不要做个半吊子实现
// 让客户端以为写成功了（AGENTS.md §P6 不要过早抽象）。

import (
	"os"
	"path/filepath"
	"strings"
)

// netatalk 的 xattr 名。出处：Samba source3/lib/adouble.h
//
//	#define NETATALK_META_XATTR "org.netatalk.Metadata"
//	#if defined(HAVE_ATTROPEN)   // Solaris/FreeBSD 有独立 xattr 命名空间
//	#define AFPINFO_EA_NETATALK NETATALK_META_XATTR
//	#else
//	#define AFPINFO_EA_NETATALK "user." NETATALK_META_XATTR
//	#endif
//
// 这里写不带 `user.` 的裸名：命名空间前缀由 oscap 的 native 适配器在 Linux 上
// 自动补 `user.`（macOS 不需要前缀），见 oscap/native/posix.go 的 encodeXattrName。
// 两边加起来正好等于 Samba 的行为。走 builtin 时没有命名空间概念，裸名直接入库。
const netatalkMetaXattr = "org.netatalk.Metadata"

// adoubleNamePrefix 是资源派生旁路文件的前缀（Samba ADOUBLE_NAME_PREFIX）。
const adoubleNamePrefix = "._"

// dotUnderscoreName 把 /path/to/file 变成 /path/to/._file。
//
// 用 filepath.Split 而不是字符串拼接：basename 可能含 '.'，
// 也可能整个路径就是根，直接拼会拼出错误的位置。
func dotUnderscoreName(host string) string {
	dir, base := filepath.Split(host)
	return dir + adoubleNamePrefix + base
}

// isDotUnderscoreName 判断某个文件名是否是资源派生旁路文件。
//
// 目录枚举要把它们藏起来：macOS 客户端自己会通过 AFP_Resource 流访问，
// 让 `._foo` 以普通文件身份出现在列表里会让 Finder 显示重影，
// 也会让 Time Machine 的 band 计数出错。
func isDotUnderscoreName(name string) bool {
	return len(name) > len(adoubleNamePrefix) &&
		strings.HasPrefix(name, adoubleNamePrefix)
}

// ---------------------------------------------------------------- AFP_AfpInfo

// readAfpInfo 从 netatalk metadata xattr 里读出 AfpInfo。
//
// 返回 ErrNotFound 表示这个对象从来没设置过 FinderInfo ——
// 调用方应当据此判断 AFP_AfpInfo 流「不存在」。
func (l *LocalFS) readAfpInfo(host string) (*AfpInfo, error) {
	blob, err := l.xattrAt(host, nil).Get(netatalkMetaXattr)
	if err != nil {
		return nil, err
	}
	fi, err := parseMetaXattr(blob)
	if err != nil {
		// 磁盘上的 blob 损坏。当成「没有元数据」而不是让整个 CREATE 失败：
		// 一个坏掉的 FinderInfo 不该导致文件本身访问不了。
		return NewAfpInfo(), nil
	}
	ai := NewAfpInfo()
	ai.FinderInfo = *fi
	return ai, nil
}

// writeAfpInfo 把 FinderInfo 写进 netatalk metadata xattr。
func (l *LocalFS) writeAfpInfo(host string, ai *AfpInfo) error {
	x := l.xattrAt(host, nil)
	// FinderInfo 全零 = 客户端要求删除这个流（Samba ai_empty_finderinfo）。
	// 直接删 xattr，而不是写一个全零 blob —— 后者会让 Netatalk 认为
	// 这个对象「有元数据但是空的」，与「没有元数据」在 Finder 里表现不同。
	if ai.EmptyFinderInfo() {
		err := x.Remove(netatalkMetaXattr)
		if err != nil && err != ErrNotFound {
			return err
		}
		return nil
	}
	return x.Set(netatalkMetaXattr, marshalMetaXattr(&ai.FinderInfo))
}

// ---------------------------------------------------------------- AFP_Resource

// openResourceFork 打开 ._<name> 旁路文件里的资源派生。
//
// 返回的 *os.File 已经定位好，但**偏移换算由 localHandle 负责** ——
// 客户端看到的 offset 0 对应文件里的 adRsrcOffData（82）。
func (l *LocalFS) openResourceFork(host string, write, create bool) (*os.File, int64, error) {
	adPath := dotUnderscoreName(host)

	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := openHostFile(adPath, flag|openNoFollow, l.cfg.FileMode)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, 0, mapError(err)
		}
		if !create {
			return nil, 0, ErrNotFound
		}
		// 新建：写入 82 字节的 AppleDouble 头，资源长度为 0。
		f, err = openHostFile(adPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|openNoFollow, l.cfg.FileMode)
		if err != nil {
			return nil, 0, mapError(err)
		}
		if _, err := f.WriteAt(marshalRsrcHeader(nil, 0), 0); err != nil {
			_ = f.Close()
			_ = os.Remove(adPath)
			return nil, 0, mapError(err)
		}
		return f, adRsrcOffData, nil
	}

	// 已存在：读头部拿到资源数据的真实偏移。
	hdr := make([]byte, adRsrcHeaderSize)
	n, err := f.ReadAt(hdr, 0)
	if err != nil && n < adRsrcHeaderSize {
		_ = f.Close()
		// 头都读不全，说明这不是个合法的 AppleDouble 文件。
		return nil, 0, ErrBadAppleDouble
	}
	dataOff, _, err := parseRsrcHeader(hdr[:n])
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, dataOff, nil
}

// updateResourceForkLen 把资源派生的当前长度回写到 AppleDouble 头的
// RFORK entry 里。Netatalk 和 macOS 都以头里的长度为准，
// 不更新的话对端会读到旧长度（表现为资源派生被截断或读出垃圾）。
func updateResourceForkLen(f *os.File, dataOff, dataLen int64) error {
	if dataLen < 0 {
		return ErrInvalidArg
	}
	hdr := make([]byte, adRsrcHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return mapError(err)
	}
	entries, err := parseADHeader(hdr)
	if err != nil {
		return err
	}
	// 找到 RFORK entry 在头里的字节位置，只改它的 len 字段。
	off := adHeaderLen
	for _, e := range entries {
		if e.eid == adEIDRFork {
			var b [4]byte
			putBE32(b[:], uint32(dataLen))
			if _, err := f.WriteAt(b[:], int64(off+8)); err != nil {
				return mapError(err)
			}
			return nil
		}
		off += adEntryLen
	}
	// 没有 RFORK entry：重写整个头。
	_ = dataOff
	if _, err := f.WriteAt(marshalRsrcHeader(nil, uint32(dataLen)), 0); err != nil {
		return mapError(err)
	}
	return nil
}

func putBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// ---------------------------------------------------------------- 枚举

// streamsOf 列出一个对象的所有流，供 FileStreamInformation 使用。
//
// 必须与 openStream 的可用范围**严格一致**：列出一个打不开的流，
// 或者能打开却不列出，都会让客户端行为错乱（前者报错，后者写得进读不到）。
// 目录上的差别见下面各段注释。
func (l *LocalFS) streamsOf(host string, a *Attr) []StreamInfo {
	isDir := a.FileAttributes&FileAttributeDirectory != 0

	out := make([]StreamInfo, 0, 4)
	if !isDir {
		out = append(out, StreamInfo{
			Name:  DefaultStreamName,
			Size:  a.Size,
			Alloc: a.Alloc,
		})
	}

	// AFP_AfpInfo：存在 xattr 就报告，长度恒为 60。**目录上同样报告** ——
	// .sparsebundle 是目录，macOS 会往它上面设 FinderInfo。
	if _, err := l.readAfpInfo(host); err == nil {
		out = append(out, StreamInfo{
			Name:  StreamName(StreamAFPInfo),
			Size:  AfpInfoSize,
			Alloc: allocSizeFallback(AfpInfoSize),
		})
	}

	// AFP_Resource：报告 ._ 文件里资源段的长度，而不是整个 ._ 文件的长度。
	// 目录**不报告**：目录没有资源派生（Samba fruit_streaminfo_rsrc()
	// 对目录直接返回空，vfs_fruit.c:4047），openStream 那边也是回 ErrNotFound。
	if !isDir {
		if size, ok := resourceForkSize(dotUnderscoreName(host)); ok {
			out = append(out, StreamInfo{
				Name:  StreamName(StreamAFPResource),
				Size:  size,
				Alloc: allocSizeFallback(size),
			})
		}
	}

	// 通用 named stream（user.DosStream.*）。文件和目录都有。
	out = append(out, l.dosStreamsOf(host)...)
	return out
}

// resourceForkSize 读出 ._ 文件里资源段的长度。
func resourceForkSize(adPath string) (int64, bool) {
	f, err := openHostFile(adPath, os.O_RDONLY, 0)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()

	hdr := make([]byte, adRsrcHeaderSize)
	n, err := f.ReadAt(hdr, 0)
	if err != nil && n < adHeaderLen {
		return 0, false
	}
	dataOff, dataLen, err := parseRsrcHeader(hdr[:n])
	if err != nil {
		return 0, false
	}
	// 头里的长度可能与实际文件大小不一致（被外部工具改过）。
	// 以实际大小为准取小值，避免上报一个读不出来的长度。
	fi, err := f.Stat()
	if err != nil {
		return 0, false
	}
	if actual := fi.Size() - dataOff; actual < dataLen {
		if actual < 0 {
			actual = 0
		}
		dataLen = actual
	}
	if dataLen == 0 {
		// 长度为零的资源派生等于没有，不要报告出去 ——
		// Finder 看到一个 0 字节的 AFP_Resource 会认为文件损坏。
		return 0, false
	}
	return dataLen, true
}
