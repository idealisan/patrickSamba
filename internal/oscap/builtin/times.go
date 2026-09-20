package builtin

// times.go —— CapCreationTime 的 builtin 实现。
//
// # 为什么查不到时是 ErrNotSupported 而不是拿 mtime 顶上
//
// ports.go 写得很直白：冒充会让上层永远不知道该去问 builtin 要那个记下来的真值。
// 更实际的后果是 macOS —— 它会把「创建时间随每次写入一起变化」的文件当成
// 被替换过的新文件。所以这里只回答两种答案：库里存过就给真值，没存过就明说没有，
// 让 vfs 层去走它的合成逻辑。
//
// # 也不在读路径上「顺手记一笔」
//
// 一个诱人的写法是：首次查询时把当前 mtime 记成创建时间。别这么做 ——
// 那等于把冒充值变成了持久事实，而且读路径会突然产生写入（只读共享上直接违约）。
// 创建时间由 vfs 在真正创建对象时调 SetCreationTime 落下来
// （internal/vfs/local.go 的 stampCreationTime，挂在 openFile/openDir/Mkdir
// 的 created/superseded 分支上；矩阵把 CapCreationTime 交给 native 时跳过 ——
// 内核 birthtime 已是真值）。

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// timeRecordLen 是一条时间记录的字节数：sec(int64) + nsec(int32)，显式小端。
//
// 不存 UnixNano：那个表示范围只到 2262 年，且零值 time.Time 会溢出成一个
// 看似合法的垃圾数。sec+nsec 的组合覆盖全部合法 time.Time。
const timeRecordLen = 12

// CreationTime 实现 oscap.CreationTime。
func (a *adapter) CreationTime(ref oscap.Ref) (time.Time, error) {
	v, ok, err := a.st.get(bucketTimes, a.st.objKey(ref.Path))
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, oscap.ErrNotSupported
	}
	if len(v) != timeRecordLen {
		return time.Time{}, fmt.Errorf("%w: btime 记录长度 %d", errCorrupt, len(v))
	}
	sec := int64(binary.LittleEndian.Uint64(v[0:]))
	nsec := int64(int32(binary.LittleEndian.Uint32(v[8:])))
	return time.Unix(sec, nsec).UTC(), nil
}

// SetCreationTime 实现 oscap.CreationTime。
func (a *adapter) SetCreationTime(ref oscap.Ref, t time.Time) error {
	var rec [timeRecordLen]byte
	binary.LittleEndian.PutUint64(rec[0:], uint64(t.Unix()))
	binary.LittleEndian.PutUint32(rec[8:], uint32(int32(t.Nanosecond())))
	return a.st.put(bucketTimes, a.st.objKey(ref.Path), rec[:])
}
