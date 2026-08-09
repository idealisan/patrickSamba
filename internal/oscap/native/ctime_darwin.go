//go:build darwin

package native

// ctime_darwin.go —— oscap.CreationTime 的原生实现：st_birthtimespec。
//
// darwin 的 stat(2) 直接带创建时间，不需要 Linux 那套 statx。
// x/sys/unix 把它命名为 Stat_t.Btim（ztypes_darwin_arm64.go），
// **不是** C 头文件里的 st_birthtimespec —— 写错名字只会在交叉编译时才炸。

import (
	"encoding/binary"
	"time"

	"golang.org/x/sys/unix"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// darwinTimes 实现 oscap.CreationTime。
type darwinTimes struct {
	readOnly bool
}

var _ oscap.CreationTime = (*darwinTimes)(nil)

func (t *darwinTimes) CreationTime(ref oscap.Ref) (time.Time, error) {
	var st unix.Stat_t
	var err error
	if fd, ok := refFD(ref); ok {
		err = unix.Fstat(fd, &st)
	} else {
		err = unix.Lstat(ref.Path, &st)
	}
	if err != nil {
		return time.Time{}, mapPosixErr(err)
	}
	if st.Btim.Sec <= 0 {
		// 挂上来的 FAT/exFAT 不记录创建时间，stat 照样成功但值是 0。
		// 如实报不支持，让上层去问 builtin 要那个记下来的真值 ——
		// 报一个 1970 年会让 Finder 显示一个明显错误的日期。
		return time.Time{}, oscap.ErrNotSupported
	}
	return time.Unix(st.Btim.Sec, int64(st.Btim.Nsec)), nil
}

// crtimeAttrBufLen 是 setattrlist 的属性缓冲长度：一个 struct timespec。
//
// darwin/amd64 与 darwin/arm64 的 timespec 都是 {int64 sec; int64 nsec}，
// 共 16 字节，两个字段都是小端。这里手工编码而不是 unsafe 地把
// unix.Timespec 拍进缓冲，是为了不给这段**无法在本项目 CI 上验证**的代码
// 再叠一层未定义行为的风险。
const crtimeAttrBufLen = 16

// SetCreationTime 用 setattrlist(ATTR_CMN_CRTIME) 设置创建时间。
//
// macOS 是三家里唯一一个 POSIX 侧真能写创建时间的平台（Linux 内核压根没有
// 这个接口，见 ctime_linux.go）。
//
// TODO: 待在真实 macOS 上验证。本项目 CI 只做交叉编译，跑不到这条路径。
// 依据是 setattrlist(2) 手册：attrBuf **不含** getattrlist 输出时那个前置的
// u_int32_t 长度字段，直接就是属性数据本身；ATTR_CMN_CRTIME 对应
// 一个 struct timespec。若实测有出入，改这里而不是让调用方绕开。
func (t *darwinTimes) SetCreationTime(ref oscap.Ref, ts time.Time) error {
	if t.readOnly {
		return oscap.ErrReadOnly
	}
	spec := unix.NsecToTimespec(ts.UnixNano())

	buf := make([]byte, crtimeAttrBufLen)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(spec.Sec))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(spec.Nsec))

	list := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_CRTIME,
	}
	// FSOPT_NOFOLLOW：与本包其余操作的 lstat/O_NOFOLLOW 语义一致 ——
	// 最后一跳若是符号链接，不能把写引到共享外面去（AGENTS.md §8）。
	//
	// setattrlist 只有路径版，没有 fd 版（fsetattrlist 是 macOS 10.13+
	// 才有的私有接口，x/sys/unix 没导出），所以这里不复用 ref.Handle。
	return mapPosixErr(unix.Setattrlist(ref.Path, &list, buf, unix.FSOPT_NOFOLLOW))
}
