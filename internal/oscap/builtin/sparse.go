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
	"sort"

	"github.com/finalappstore/stupidsamba/internal/oscap"
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

	if err := a.writeZeros(ref, off, end-off); err != nil {
		return err
	}

	// 先落盘数据、后记账。反过来的话，写零失败时库里会留下一条
	// 「这里是空洞」的假记录，而那段其实还是旧数据 —— 那就成了真正的少报。
	return a.addHole(ref, oscap.Range{Offset: off, Length: end - off})
}

// writeZeros 把 [off, off+n) 写成零。
func (a *adapter) writeZeros(ref oscap.Ref, off, n int64) error {
	f, done, err := a.openWrite(ref)
	if err != nil {
		return err
	}
	defer done()

	buf := make([]byte, min64(n, scanChunkSize))
	for n > 0 {
		chunk := min64(n, int64(len(buf)))
		if _, err := f.WriteAt(buf[:chunk], off); err != nil {
			return mapPathError(err)
		}
		off += chunk
		n -= chunk
	}
	// 打洞是回收语义，客户端（Time Machine）随后可能立刻查询已分配区间。
	// 不 sync 的话崩溃后会出现「库说是空洞、盘上还是旧数据」的不一致。
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
