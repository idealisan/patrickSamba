package builtin

// sparse.go —— CapSparseFile 的 builtin 实现。
//
// # 做得到什么、做不到什么（先说清楚，免得读者以为它真的在省磁盘）
//
// 普通文件系统没有「释放已写区间」的可移植手段。所以本实现追求的是
// ports.go 明确许可的那件事：**可观测语义**一致 —— 打过洞的区间读回来是零、
// AllocatedRanges 不报它 —— 至于磁盘占用降没降，取决于宿主文件系统会不会
// 自己把整块的零折叠掉。**不假装省了空间，也不因为省不了就拒绝服务。**
//
// # 但也不能反过来「省不了就干脆多占」
//
// 无条件写零是最省事的实现，代价是一次**回收**操作会让文件变胖：在支持稀疏写
// 但没有 punch hole 的文件系统上（tmpfs、macOS 上的 APFS —— 探测判据就是
// 「能不能打洞」，见 oscap 的 probe），被打的区间本来就是洞，写零等于把它填实。
// 所以这里做两件尽量少占盘的事，都不需要任何可选能力：
//
//  1. 只写**含非零字节**的块，本来就是零的块一个字节都不碰（zeroNonZero）；
//  2. 打洞区间顶到 EOF 时，用「截断掉再还原长度」把块真的还回去（reclaimTail）。
//
// 第 2 条是唯一能真回收空间的情形；第 1 条只保证不再自伤，不承诺省盘。
//
// # 为什么记了账还要回读校验
//
// 只信 KV 里记的空洞会引出一个真实的数据风险：客户端先打洞、后把真实数据
// 写回同一段（写是走普通 IO 的，oscap 根本看不见），KV 却还记着「这是空洞」，
// 于是 AllocatedRanges 少报了一段真实数据。ports.go 警告过这个方向：
// 「少报会让客户端以为数据丢了」。
//
// 所以 AllocatedRanges 只把 KV 记录当作**扫描范围的上界**，逐块回读确认确实
// 还是零才算空洞 —— 被覆写过的部分自动回到「已分配」。代价被限制在
// 「客户端确实打过洞的那些区间 ∩ 查询窗口」之内，而不是整个文件。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// zeroBlockSize 是判定「这一块是不是空洞」的粒度。
//
// 取 4 KiB：与主流文件系统的分配粒度一致，也是 NTFS 报 allocated range 的量级。
// 更细没有意义（底层本来也按块分配），更粗会让被部分覆写的空洞整块回到已分配。
const zeroBlockSize = 4096

// scanChunkSize 是回读校验一次读多少。64 KiB 在系统调用开销与内存占用之间取平衡。
const scanChunkSize = 64 * 1024

// rangeRecordLen 是一条区间记录的字节数：offset(int64) + length(int64)。
// 编码显式小端（AGENTS.md §5：涉及字节布局的地方必须写明字节序）。
const rangeRecordLen = 16

// PunchHole 实现 oscap.SparseFile。
func (a *adapter) PunchHole(ref oscap.Ref, off, length int64) error {
	if off < 0 || length < 0 {
		return oscap.ErrInvalidArg
	}
	if a.st.readOnly {
		return oscap.ErrReadOnly
	}
	if length == 0 {
		return nil
	}
	if !addOK(off, length) {
		return oscap.ErrInvalidArg
	}

	size, err := a.fileSize(ref)
	if err != nil {
		return err
	}
	// 文件逻辑长度不变：越过 EOF 的部分不写、也不记账。
	end := min64(off+length, size)
	if end <= off {
		return nil
	}

	// 尾部区间先试「真回收」：截断掉再还原长度，块是真的还了回去。
	// 中间的洞没有等价手段 —— 普通文件系统没有「只释放这几个块、前后内容原地
	// 不动」的操作。失败不打紧：下面的写零路径会把长度顶回原样。
	if end >= size {
		if err := a.reclaimTail(ref, off, size); err == nil {
			return a.addHole(ref, oscap.Range{Offset: off, Length: end - off})
		}
	}

	if err := a.zeroNonZero(ref, off, end); err != nil {
		return err
	}

	// 先落盘数据、后记账。反过来的话，写零失败时库里会留下一条
	// 「这里是空洞」的假记录，而那段其实还是旧数据 —— 那就成了真正的少报。
	return a.addHole(ref, oscap.Range{Offset: off, Length: end - off})
}

// zeroNonZero 把 [off, end) 里**含非零字节**的块写成零，整块已经是零的部分一个字节都不动。
//
// 为什么要先读一遍：无条件写零会把「本来就是洞」的区间填实 —— 在支持稀疏写但
// 打不了洞的宿主上（tmpfs、macOS 的 APFS）那是净亏损，一次回收反而让文件多占盘。
// 读一遍花的是内存带宽，换来的是不再实体化，而且整段本来就是零时连写句柄都不用开、
// 随后的 fsync 也省了。
//
// 判零粒度与 AllocatedRanges 的回读校验一致（zeroBlockSize，按**文件绝对偏移**
// 对齐），这样「记账的洞」与「查出来的洞」是同一套边界。
func (a *adapter) zeroNonZero(ref oscap.Ref, off, end int64) error {
	rf, rdone, err := a.openRead(ref)
	if err != nil {
		return err
	}
	defer rdone()

	// 写句柄按需开：整段已经是零时一次都不用开。
	var (
		wf    *os.File
		wdone func()
	)
	defer func() {
		if wdone != nil {
			wdone()
		}
	}()
	ensureWrite := func() error {
		if wf != nil {
			return nil
		}
		f, done, err := a.openWrite(ref)
		if err != nil {
			return err
		}
		wf, wdone = f, done
		return nil
	}

	// 读缓冲与写缓冲**必须**是两块：写缓冲恒为全零，读缓冲会被填进文件真实内容。
	// 复用同一块的话，写零的时候写回去的正是刚读出来的旧数据 —— 打洞成了原地重写。
	rbuf := make([]byte, scanChunkSize)
	zbuf := make([]byte, scanChunkSize)
	wrote := false
	pos := off
	for pos < end {
		n := min64(end-pos, scanChunkSize)
		read, rerr := rf.ReadAt(rbuf[:n], pos)
		if read > 0 {
			for _, r := range nonZeroBlocks(rbuf[:read], pos) {
				if err := ensureWrite(); err != nil {
					return err
				}
				if err := writeZerosAt(wf, zbuf, r.Offset, r.Length); err != nil {
					return err
				}
				wrote = true
			}
			pos += int64(read)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return mapPathError(rerr)
			}
			// pos 已越过当前 EOF，剩下的字节根本不存在。这只可能发生在
			// reclaimTail 半途失败之后 —— 那种情况下必须写零把长度顶回 end，
			// 绝不能当成「已经是零」跳过，否则文件就真的短了。
			if pos < end {
				if err := ensureWrite(); err != nil {
					return err
				}
				if err := writeZerosAt(wf, zbuf, pos, end-pos); err != nil {
					return err
				}
				wrote = true
			}
			break
		}
	}
	if !wrote {
		return nil
	}
	// 打洞是回收语义，客户端（Time Machine）随后可能立刻查询已分配区间。
	// 不 sync 的话崩溃后会出现「库说是空洞、盘上还是旧数据」的不一致。
	if err := wf.Sync(); err != nil {
		return mapPathError(err)
	}
	return nil
}

// writeZerosAt 把 [off, off+n) 写成零，buf 必须是全零的复用缓冲。
func writeZerosAt(f *os.File, buf []byte, off, n int64) error {
	for n > 0 {
		chunk := min64(n, int64(len(buf)))
		if _, err := f.WriteAt(buf[:chunk], off); err != nil {
			return mapPathError(err)
		}
		off += chunk
		n -= chunk
	}
	return nil
}

// reclaimTail 处理「打洞区间顶到 EOF」这个特例：截断掉再还原长度，把块**真的**还回去。
//
// 为什么只有这种情形能真回收：中间挖洞需要「只释放这几个块、前后内容原地不动」，
// 普通文件系统没有这种操作；而尾部截断是 POSIX 基本调用，ftruncate(off) 释放
// [off, EOF) 是货真价实的。还原长度时，支持稀疏的文件系统会把它变成洞 —— 正是
// 我们要的；不支持的会重新分配，那种宿主上本来就没洞可省，与写零等价，不亏。
//
// 必须写明的代价：截断期间文件会**短暂变短**，并发读者可能读到变短的文件。它
// 不会读到别人的数据（那个区间本来就要被清零），只是短一瞬；严格加锁那道闸在
// 命令层（ioctl.go 的 checkIO），这里不重复拿锁。
//
// 失败时不承诺长度已还原 —— 由调用方回落到写零路径，写零本身会把长度顶回去。
func (a *adapter) reclaimTail(ref oscap.Ref, off, size int64) error {
	f, done, err := a.openWrite(ref)
	if err != nil {
		return err
	}
	defer done()

	if err := f.Truncate(off); err != nil {
		return mapPathError(err)
	}
	// 还原长度时不能无脑写 size：截断期间可能有别的写入者把文件又写长了，
	// 一刀切回去会把并发写入的尾巴剪掉。取两者较大值。
	target := size
	if fi, serr := f.Stat(); serr == nil && fi.Size() > target {
		target = fi.Size()
	}
	if err := f.Truncate(target); err != nil {
		// 个别文件系统不能用 ftruncate 扩展（Samba 也踩过：Linux 上 fat 不行，
		// 见 vfs_default.c 的 vfswrap_ftruncate）。退一步在末尾写一个零字节把
		// 长度顶回去：中间那段读回来照样是零（POSIX 保证），语义不受影响。
		if _, err := f.WriteAt([]byte{0}, target-1); err != nil {
			return mapPathError(err)
		}
	}
	// 与写零路径同理：先落盘、后记账。
	if err := f.Sync(); err != nil {
		return mapPathError(err)
	}
	return nil
}

// Preallocate 实现 oscap.SparseFile。
//
// **诚实说明：在普通文件系统上这是一个尽力而为的提示，不是真正的空间预留。**
// [off,off+len) 落在 EOF 之内时那些块本来就已分配，无事可做；越过 EOF 的部分
// 无法在「不改变逻辑长度」的前提下预留 —— 那正是 fallocate(KEEP_SIZE) 这类
// **可选**能力才提供的东西，而本适配器的前提就是没有它。
//
// 为什么返回 nil 而不是 ErrNotSupported：Preallocate 对应 SMB 的 AllocationSize，
// 是一个 hint；报错会让上层把整个 create 判失败，而可观测契约（逻辑长度不变、
// 后续写入照常成功）在这里是完全成立的。参数非法与只读依旧如实报错。
func (a *adapter) Preallocate(ref oscap.Ref, off, length int64) error {
	if off < 0 || length < 0 || !addOK(off, length) {
		return oscap.ErrInvalidArg
	}
	if a.st.readOnly {
		return oscap.ErrReadOnly
	}
	if length == 0 {
		return nil
	}
	// 触一次 stat：对象不存在要如实报 ErrNotFound，不能假装预留成功。
	if _, err := a.fileSize(ref); err != nil {
		return err
	}
	return nil
}

// SetSparse 实现 oscap.SparseFile。
//
// 不对称是刻意的，理由见 ports.go：v=false 做不到，而谎称成功会让客户端
// 看到自相矛盾的视图。
func (a *adapter) SetSparse(ref oscap.Ref, v bool) error {
	if a.st.readOnly {
		return oscap.ErrReadOnly
	}
	if !v {
		return oscap.ErrNotSupported
	}
	return nil
}

// AllocatedRanges 实现 oscap.SparseFile。
func (a *adapter) AllocatedRanges(ref oscap.Ref, off, length int64) ([]oscap.Range, error) {
	if off < 0 || length < 0 || !addOK(off, length) {
		return nil, oscap.ErrInvalidArg
	}
	if length == 0 {
		return nil, nil
	}

	size, err := a.fileSize(ref)
	if err != nil {
		return nil, err
	}
	end := min64(off+length, size)
	if end <= off {
		return nil, nil
	}
	window := oscap.Range{Offset: off, Length: end - off}

	holes, err := a.loadHoles(ref)
	if err != nil {
		// 记录读不出来时降级为「整个窗口都已分配」（ports.go 指定的安全侧）：
		// 多报已分配最多让客户端多读一遍零，少报会让它以为数据没了。
		return []oscap.Range{window}, nil
	}
	holes = clipRanges(holes, window)
	if len(holes) == 0 {
		return []oscap.Range{window}, nil
	}

	verified, err := a.verifyHoles(ref, holes)
	if err != nil {
		return []oscap.Range{window}, nil
	}
	return subtractRanges(window, verified), nil
}

// verifyHoles 逐块回读，只保留「现在确实还是零」的部分。
//
// 读失败一律当作「这段是已分配」丢弃该记录 —— 同样是往安全侧倒。
func (a *adapter) verifyHoles(ref oscap.Ref, holes []oscap.Range) ([]oscap.Range, error) {
	f, done, err := a.openRead(ref)
	if err != nil {
		return nil, err
	}
	defer done()

	buf := make([]byte, scanChunkSize)
	var out []oscap.Range
	for _, h := range holes {
		pos := h.Offset
		limit := h.Offset + h.Length
		for pos < limit {
			n := min64(limit-pos, scanChunkSize)
			read, err := f.ReadAt(buf[:n], pos)
			if read > 0 {
				out = appendZeroBlocks(out, buf[:read], pos)
				pos += int64(read)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					// 窗口已裁剪到 EOF 之内，正常不该短读；真发生了就停下，
					// 剩下的部分按「已分配」处理（安全侧）。
					break
				}
				return nil, mapPathError(err)
			}
		}
	}
	return normalizeRanges(out), nil
}

// appendZeroBlocks 把 data 里全零的块（按 zeroBlockSize 切）作为空洞追加进 out。
//
// 块的边界对齐到**文件绝对偏移**而不是 buf 起点，否则相邻两次读的分块会错位，
// 同一个物理块可能一半被判空洞一半被判已分配。
func appendZeroBlocks(out []oscap.Range, data []byte, base int64) []oscap.Range {
	for i := 0; i < len(data); {
		// 本块在文件里的边界。
		blockEnd := (base + int64(i)) / zeroBlockSize * zeroBlockSize
		blockEnd += zeroBlockSize
		n := int(min64(blockEnd-(base+int64(i)), int64(len(data)-i)))
		if allZero(data[i : i+n]) {
			out = append(out, oscap.Range{Offset: base + int64(i), Length: int64(n)})
		}
		i += n
	}
	return out
}

// nonZeroBlocks 与 appendZeroBlocks 互为补集：按**同一套**块边界切分（zeroBlockSize、
// 对齐到文件绝对偏移），返回含非零字节的那些块。
//
// 用同一套边界不是巧合：写零时按 4 KiB 判、查洞时按另一套判，会出现「记了账但
// 查出来不是洞」这种自己跟自己对不上的结果。
func nonZeroBlocks(data []byte, base int64) []oscap.Range {
	var out []oscap.Range
	for i := 0; i < len(data); {
		// 本块在文件里的边界（与 appendZeroBlocks 同款算法）。
		blockEnd := (base + int64(i)) / zeroBlockSize * zeroBlockSize
		blockEnd += zeroBlockSize
		n := int(min64(blockEnd-(base+int64(i)), int64(len(data)-i)))
		if !allZero(data[i : i+n]) {
			out = append(out, oscap.Range{Offset: base + int64(i), Length: int64(n)})
		}
		i += n
	}
	// 相邻的非零块合并成一次写：本来就是为了少写，别把省下的又赔进 syscall 次数。
	return normalizeRanges(out)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------ 空洞记录的读写

func (a *adapter) loadHoles(ref oscap.Ref) ([]oscap.Range, error) {
	v, ok, err := a.st.get(bucketHoles, a.st.objKey(ref.Path))
	if err != nil || !ok {
		return nil, err
	}
	if len(v)%rangeRecordLen != 0 {
		return nil, fmt.Errorf("%w: holes 记录长度 %d", errCorrupt, len(v))
	}
	out := make([]oscap.Range, 0, len(v)/rangeRecordLen)
	for i := 0; i+rangeRecordLen <= len(v); i += rangeRecordLen {
		out = append(out, oscap.Range{
			Offset: int64(binary.LittleEndian.Uint64(v[i:])),
			Length: int64(binary.LittleEndian.Uint64(v[i+8:])),
		})
	}
	return normalizeRanges(out), nil
}

func (a *adapter) addHole(ref oscap.Ref, r oscap.Range) error {
	cur, err := a.loadHoles(ref)
	if err != nil {
		// 旧记录损坏时不因此丢掉这次打洞：从新的一条重建。
		cur = nil
	}
	merged := normalizeRanges(append(cur, r))
	buf := make([]byte, 0, len(merged)*rangeRecordLen)
	var rec [rangeRecordLen]byte
	for _, m := range merged {
		binary.LittleEndian.PutUint64(rec[0:], uint64(m.Offset))
		binary.LittleEndian.PutUint64(rec[8:], uint64(m.Length))
		buf = append(buf, rec[:]...)
	}
	return a.st.put(bucketHoles, a.st.objKey(ref.Path), buf)
}

// ------------------------------------------------------------ 区间集合运算

// normalizeRanges 排序、丢弃空区间、合并相邻或重叠的区间。
func normalizeRanges(in []oscap.Range) []oscap.Range {
	if len(in) == 0 {
		return nil
	}
	rs := make([]oscap.Range, 0, len(in))
	for _, r := range in {
		if r.Length > 0 {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Offset < rs[j].Offset })
	out := []oscap.Range{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Offset <= last.Offset+last.Length {
			if e := r.Offset + r.Length; e > last.Offset+last.Length {
				last.Length = e - last.Offset
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// clipRanges 把区间集裁剪到窗口内。输入需已规范化或将被规范化。
func clipRanges(in []oscap.Range, w oscap.Range) []oscap.Range {
	wEnd := w.Offset + w.Length
	var out []oscap.Range
	for _, r := range in {
		s := max64(r.Offset, w.Offset)
		e := min64(r.Offset+r.Length, wEnd)
		if e > s {
			out = append(out, oscap.Range{Offset: s, Length: e - s})
		}
	}
	return normalizeRanges(out)
}

// subtractRanges 返回 w 减去 holes 之后的部分（holes 必须已规范化且在 w 内）。
func subtractRanges(w oscap.Range, holes []oscap.Range) []oscap.Range {
	var out []oscap.Range
	cur := w.Offset
	wEnd := w.Offset + w.Length
	for _, h := range holes {
		if h.Offset > cur {
			out = append(out, oscap.Range{Offset: cur, Length: h.Offset - cur})
		}
		if e := h.Offset + h.Length; e > cur {
			cur = e
		}
	}
	if cur < wEnd {
		out = append(out, oscap.Range{Offset: cur, Length: wEnd - cur})
	}
	return out
}

// addOK 判断 off+length 会不会溢出 int64。
//
// 这类校验不是形式主义：offset/length 直接来自网络报文（AGENTS.md §8
// 要求任何来自网络的 offset/length 在使用前校验边界），溢出后的负数会让
// 后面所有比较全部反向。
func addOK(off, length int64) bool { return off+length >= off }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
