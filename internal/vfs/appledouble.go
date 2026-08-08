package vfs

// appledouble.go —— AppleDouble v2 容器的编解码。
//
// # 为什么需要它
//
// macOS 的 `AFP_AfpInfo` / `AFP_Resource` 两个流不能直接按流名存成 xattr，
// 否则 Netatalk 与既有 Samba 共享读不出来 —— 而「macOS 已有的备份还能读」
// 是本项目阶段二的硬需求。所以要跟 Samba `vfs_fruit` 的落盘格式对齐。
//
// # 后端模式的选择（调研结论）
//
// `man vfs_fruit` 给了几种模式：
//
//	fruit:metadata = netatalk | stream        默认 netatalk
//	fruit:resource = file | xattr | stream    默认 file
//
// **本实现选择 Samba 的两个默认值：metadata=netatalk + resource=file。** 理由：
//
//  1. 默认值就是现网绝大多数 Samba 共享的实际格式，兼容面最大；
//  2. `xattr` 模式 man page 明确写了「requires a filesystem with large xattr
//     support ... boils down to Solaris and derived platforms and ZFS」，
//     Linux 上 ext4 单条 xattr 受限于一个 inode block（通常 4KiB），
//     放资源派生会直接失败；
//  3. `stream` 模式是把流下推给 VFS 栈里的下一个模块（Samba 靠
//     streams_xattr 之类实现），我们没有那层栈，等于要自己另发明一套落盘
//     格式，反而失去兼容性。
//
// 于是：
//
//	AFP_AfpInfo   → xattr  user.org.netatalk.Metadata   （402 字节 AppleDouble blob）
//	AFP_Resource  → 旁路文件 ._<basename>                （82 字节头 + 资源数据）
//
// # 常量出处（全部已核对，非猜测）
//
// Samba `source3/lib/adouble.h` / `source3/lib/adouble.c`：
//
//	AD_APPLEDOUBLE_MAGIC 0x00051607
//	AD_VERSION2          0x00020000
//	AD_HEADER_LEN        26   = magic(4) + version(4) + filler(16) + nentries(2)
//	AD_ENTRY_LEN         12   = eid(4) + off(4) + len(4)
//	ADEDLEN_FINDERI      32
//	ADEDLEN_COMMENT      200
//	ADEDLEN_FILEDATESI   16
//	AD_DATASZ_XATTR      402  （编译期 #error 断言过）
//	AD_DATASZ_DOT_UND    82   （编译期 #error 断言过）
//	AD_FILLER_TAG        "Netatalk        "
//
// **所有整数字段都是大端**（adouble.c 里用 RSIVAL/RIVAL，即 PUSH/PULL_BE_U32）。
//
// 一个易错细节：filler 那 16 字节，Samba 的 ad_pack() 只在 ADOUBLE_RSRC
// （即 `._` 文件）时写 "Netatalk        "，metadata xattr 的 filler 留全零。

import (
	"encoding/binary"
	"errors"
)

// AppleDouble 容器常量。出处见文件头注释。
const (
	adMagic   uint32 = 0x00051607 // AD_APPLEDOUBLE_MAGIC
	adVersion uint32 = 0x00020000 // AD_VERSION2

	adHeaderLen = 26 // AD_HEADER_LEN
	adEntryLen  = 12 // AD_ENTRY_LEN

	adOffMagic    = 0
	adOffVersion  = 4
	adOffFiller   = 8
	adOffNEntries = 24

	adFillerLen = 16
	// adFillerTag 是 AD_FILLER_TAG，**只用于 `._` 资源派生文件**。
	adFillerTag = "Netatalk        "

	// metadata xattr 的固定布局（AD_DATASZ_XATTR = 402）。
	adMetaNumEntries  = 8 // ADEID_NUM_XATTR
	adMetaSize        = 402
	adMetaOffFinderI  = adHeaderLen + adMetaNumEntries*adEntryLen // 122
	adMetaOffComment  = adMetaOffFinderI + FinderInfoSize         // 154
	adMetaOffDatesI   = adMetaOffComment + 200                    // 354
	adMetaOffAFPFileI = adMetaOffDatesI + 16                      // 370
	adMetaOffPrivDev  = adMetaOffAFPFileI + 4                     // 374
	adMetaOffPrivIno  = adMetaOffPrivDev + 8                      // 382
	adMetaOffPrivSyn  = adMetaOffPrivIno + 8                      // 390
	adMetaOffPrivID   = adMetaOffPrivSyn + 8                      // 398

	// `._` 资源派生文件的固定布局（AD_DATASZ_DOT_UND = 82）。
	adRsrcNumEntries = 2 // ADEID_NUM_DOT_UND
	adRsrcHeaderSize = 82
	adRsrcOffFinderI = adHeaderLen + adRsrcNumEntries*adEntryLen // 50
	adRsrcOffData    = adRsrcOffFinderI + FinderInfoSize         // 82
)

// AppleDouble entry ID。Samba adouble.h 的 ADEID_*。
const (
	adEIDRFork      uint32 = 2
	adEIDComment    uint32 = 4
	adEIDFinderI    uint32 = 9
	adEIDFileDatesI uint32 = 8
	adEIDAFPFileI   uint32 = 14
)

// 私有 entry 的**磁盘上**真实 ID（Samba adouble.h 的 AD_DEV/AD_INO/AD_SYN/AD_ID）。
// 注意它们和 ADEID_PRIV* 的枚举值不同，落盘时要用这一组。
const (
	adEIDPrivDev uint32 = 0x80444556 // "\x80DEV"
	adEIDPrivIno uint32 = 0x80494E4F // "\x80INO"
	adEIDPrivSyn uint32 = 0x8053594E // "\x80SYN"
	adEIDPrivID  uint32 = 0x8053567E
)

// ErrBadAppleDouble 表示 AppleDouble 容器损坏。
var ErrBadAppleDouble = errors.New("vfs: malformed AppleDouble")

// adEntry 是一条 entry 描述符。
type adEntry struct {
	eid  uint32
	off  uint32
	elen uint32
}

// marshalMetaXattr 把 FinderInfo 打包成 402 字节的 netatalk metadata blob。
//
// entry 表与偏移完全照抄 Samba 的 entry_order_meta_xattr[]：
// 8 条 entry，其中只有 FINDERI / FILEDATESI / AFPFILEI 声明了非零长度，
// 其余（COMMENT 与 4 个私有 entry）长度为 0 但仍占据 entry 槽位与偏移。
func marshalMetaXattr(finder *[FinderInfoSize]byte) []byte {
	entries := []adEntry{
		{adEIDFinderI, adMetaOffFinderI, FinderInfoSize},
		{adEIDComment, adMetaOffComment, 0},
		{adEIDFileDatesI, adMetaOffDatesI, 16},
		{adEIDAFPFileI, adMetaOffAFPFileI, 4},
		{adEIDPrivDev, adMetaOffPrivDev, 0},
		{adEIDPrivIno, adMetaOffPrivIno, 0},
		{adEIDPrivSyn, adMetaOffPrivSyn, 0},
		{adEIDPrivID, adMetaOffPrivID, 0},
	}

	buf := make([]byte, adMetaSize)
	binary.BigEndian.PutUint32(buf[adOffMagic:], adMagic)
	binary.BigEndian.PutUint32(buf[adOffVersion:], adVersion)
	// filler 留零：ad_pack() 只在 ADOUBLE_RSRC 时写 "Netatalk        "。
	binary.BigEndian.PutUint16(buf[adOffNEntries:], uint16(len(entries)))

	off := adHeaderLen
	for _, e := range entries {
		binary.BigEndian.PutUint32(buf[off:], e.eid)
		binary.BigEndian.PutUint32(buf[off+4:], e.off)
		binary.BigEndian.PutUint32(buf[off+8:], e.elen)
		off += adEntryLen
	}

	copy(buf[adMetaOffFinderI:adMetaOffFinderI+FinderInfoSize], finder[:])
	return buf
}

// parseMetaXattr 从 netatalk metadata blob 里取出 FinderInfo。
//
// 不假设 entry 的顺序与偏移 —— 真实磁盘上的 blob 可能由不同版本的
// Netatalk/Samba 写入。按 entry 表查找 FINDERI，并**逐项校验边界**
// 再切片（AGENTS.md §5：先校验长度再切片，禁止裸切片 panic）。
func parseMetaXattr(buf []byte) (*[FinderInfoSize]byte, error) {
	entries, err := parseADHeader(buf)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.eid != adEIDFinderI {
			continue
		}
		if e.elen < FinderInfoSize {
			// 有些实现会写 len=0 的占位 entry，视为「没有 FinderInfo」。
			break
		}
		end := uint64(e.off) + FinderInfoSize
		if uint64(e.off) > uint64(len(buf)) || end > uint64(len(buf)) {
			return nil, ErrBadAppleDouble
		}
		var fi [FinderInfoSize]byte
		copy(fi[:], buf[e.off:end])
		return &fi, nil
	}
	// 容器合法但没有 FinderInfo：返回全零，等价于「没设置过」。
	return &[FinderInfoSize]byte{}, nil
}

// parseADHeader 校验 AppleDouble 头并返回 entry 表。
func parseADHeader(buf []byte) ([]adEntry, error) {
	if len(buf) < adHeaderLen {
		return nil, ErrBadAppleDouble
	}
	if binary.BigEndian.Uint32(buf[adOffMagic:]) != adMagic ||
		binary.BigEndian.Uint32(buf[adOffVersion:]) != adVersion {
		return nil, ErrBadAppleDouble
	}
	n := int(binary.BigEndian.Uint16(buf[adOffNEntries:]))
	// entry 表必须完整落在 buf 内，否则是损坏的容器。
	// 先算需要多少字节再比较，避免 n 很大时 n*adEntryLen 溢出。
	if n < 0 || n > (len(buf)-adHeaderLen)/adEntryLen {
		return nil, ErrBadAppleDouble
	}
	entries := make([]adEntry, 0, n)
	off := adHeaderLen
	for i := 0; i < n; i++ {
		entries = append(entries, adEntry{
			eid:  binary.BigEndian.Uint32(buf[off:]),
			off:  binary.BigEndian.Uint32(buf[off+4:]),
			elen: binary.BigEndian.Uint32(buf[off+8:]),
		})
		off += adEntryLen
	}
	return entries, nil
}

// marshalRsrcHeader 生成 `._<name>` 文件的 82 字节头。
//
// 布局照抄 entry_order_dot_und[]：2 条 entry（FINDERI 在 50，RFORK 在 82），
// rsrcLen 是资源派生的当前长度。**filler 要写 "Netatalk        "** ——
// 这是 metadata xattr 与 `._` 文件唯一的头部差异。
func marshalRsrcHeader(finder *[FinderInfoSize]byte, rsrcLen uint32) []byte {
	buf := make([]byte, adRsrcHeaderSize)
	binary.BigEndian.PutUint32(buf[adOffMagic:], adMagic)
	binary.BigEndian.PutUint32(buf[adOffVersion:], adVersion)
	copy(buf[adOffFiller:adOffFiller+adFillerLen], adFillerTag)
	binary.BigEndian.PutUint16(buf[adOffNEntries:], adRsrcNumEntries)

	off := adHeaderLen
	for _, e := range []adEntry{
		{adEIDFinderI, adRsrcOffFinderI, FinderInfoSize},
		{adEIDRFork, adRsrcOffData, rsrcLen},
	} {
		binary.BigEndian.PutUint32(buf[off:], e.eid)
		binary.BigEndian.PutUint32(buf[off+4:], e.off)
		binary.BigEndian.PutUint32(buf[off+8:], e.elen)
		off += adEntryLen
	}
	if finder != nil {
		copy(buf[adRsrcOffFinderI:adRsrcOffFinderI+FinderInfoSize], finder[:])
	}
	return buf
}

// parseRsrcHeader 从 `._` 文件头里取出资源派生的偏移与长度。
//
// 不假设偏移恒为 82：Mac OS X 自己写的 `._` 文件 entry 布局与 Netatalk
// 并不总是一致（Samba 的 ad_convert() 就是在处理这类差异）。
func parseRsrcHeader(buf []byte) (dataOff, dataLen int64, err error) {
	entries, err := parseADHeader(buf)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.eid == adEIDRFork {
			return int64(e.off), int64(e.elen), nil
		}
	}
	// 没有 RFORK entry：资源派生为空。
	return adRsrcOffData, 0, nil
}
