package oscap

// stub_test.go —— 测试用的能力实现桩。
//
// 顺带钉住一条**设计属性**：本桩用**一个**类型同时实现了全部六项能力。
// 这只有在六个接口的方法名两两不重名时才可能（见 ports.go 的命名约定）。
// 如果将来有人把某两个方法都改名成 Get/Set，这个文件会直接编译不过 ——
// 比在文档里写一句"请注意不要重名"可靠得多。

import "time"

// stub 是一套可辨识的能力实现：id 用来断言 Provider 到底挑中了哪一侧。
type stub struct{ id string }

var (
	_ Xattr         = stub{}
	_ SparseFile    = stub{}
	_ NamedStream   = stub{}
	_ StableFileID  = stub{}
	_ CreationTime  = stub{}
	_ DOSAttributes = stub{}
)

func (s stub) GetXattr(Ref, string) ([]byte, error)  { return nil, ErrNotSupported }
func (s stub) SetXattr(Ref, string, []byte) error    { return ErrNotSupported }
func (s stub) RemoveXattr(Ref, string) error         { return ErrNotSupported }
func (s stub) ListXattr(Ref) ([]string, error)       { return nil, ErrNotSupported }
func (s stub) PunchHole(Ref, int64, int64) error     { return ErrNotSupported }
func (s stub) Preallocate(Ref, int64, int64) error   { return ErrNotSupported }
func (s stub) SetSparse(Ref, bool) error             { return ErrNotSupported }
func (s stub) ListStreams(Ref) ([]StreamInfo, error) { return nil, ErrNotSupported }
func (s stub) RemoveStream(Ref, string) error        { return ErrNotSupported }
func (s stub) FileID(Ref) (uint64, error)            { return 0, ErrNotSupported }
func (s stub) CreationTime(Ref) (time.Time, error)   { return time.Time{}, ErrNotSupported }
func (s stub) SetCreationTime(Ref, time.Time) error  { return ErrNotSupported }
func (s stub) DOSAttributes(Ref) (uint32, error)     { return 0, ErrNotSupported }
func (s stub) SetDOSAttributes(Ref, uint32) error    { return ErrNotSupported }
func (s stub) AllocatedRanges(Ref, int64, int64) ([]Range, error) {
	return nil, ErrNotSupported
}

func (s stub) OpenStream(Ref, string, StreamFlags) (StreamHandle, error) {
	return nil, ErrNotSupported
}

// fullSet 返回一套六项齐全、都标着同一个 id 的实现。
func fullSet(id string) Set {
	s := stub{id: id}
	return Set{Xattr: s, Sparse: s, Streams: s, IDs: s, Times: s, DOS: s}
}

// stubID 取出某个能力实现的 id，用来断言"挑中的是哪一侧"。
// 传进来的不是 stub（或为 nil）时返回空串，让断言自然失败而不是 panic。
func stubID(v any) string {
	if s, ok := v.(stub); ok {
		return s.id
	}
	return ""
}

// countingProbe 是可控探测器：只对 native 集合里的能力返回 true，并记调用次数。
type countingProbe struct {
	native map[Capability]bool
	calls  int
}

func (p *countingProbe) probe(c Capability, _ Options) bool {
	p.calls++
	return p.native[c]
}

// allCaps 是"全部能力都原生支持"的探测集合。
func allCaps() map[Capability]bool {
	m := make(map[Capability]bool, capCount)
	for _, c := range Capabilities() {
		m[c] = true
	}
	return m
}
