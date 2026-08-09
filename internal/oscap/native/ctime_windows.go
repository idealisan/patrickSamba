//go:build windows

package native

// ctime_windows.go —— Windows 上的真实创建时间：可读**也可写**。
//
// Windows 是三个平台里唯一两个方向都原生成立的：文件时间三元组
// （Creation / LastAccess / LastWrite）从 FAT 时代就在，SetFileTime 直接改。
// 对照：Linux 只能读（statx BTIME，见 ctime_linux.go 里那段长注释），
// macOS 读 st_birthtimespec、写 setattrlist(ATTR_CMN_CRTIME)。

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// winTimes 用 Win32 文件时间实现 oscap.CreationTime。
type winTimes struct {
	readOnly bool
}

var _ oscap.CreationTime = (*winTimes)(nil)

// CreationTime 读取创建时间。
func (t *winTimes) CreationTime(ref oscap.Ref) (time.Time, error) {
	h, cleanup, err := winHandle(ref, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return time.Time{}, err
	}
	defer cleanup()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return time.Time{}, mapWinErr(err)
	}
	ft := info.CreationTime
	if ft.LowDateTime == 0 && ft.HighDateTime == 0 {
		// 零值 FILETIME 表示"这个卷不记录创建时间"（某些网络重定向器
		// 与部分 FAT 变体如此）。不要把它换算成 1601-01-01 交上去 ——
		// 那是个看起来很真的假值，上层再也不会去问 builtin 要真值。
		return time.Time{}, fmt.Errorf("oscap/native: %q 无创建时间记录: %w",
			ref.Path, oscap.ErrNotSupported)
	}
	return time.Unix(0, ft.Nanoseconds()), nil
}

// SetCreationTime 设置创建时间。
//
// 三个 FILETIME 参数传 nil 表示"这一项不动"，所以这里只改创建时间，
// 访问时间与修改时间原样保留 —— 上层若要一并改，会分别调用 vfs 的
// Chtimes，不该被本方法顺手改掉。
func (t *winTimes) SetCreationTime(ref oscap.Ref, ts time.Time) error {
	if t.readOnly {
		return fmt.Errorf("oscap/native: 只读挂载: %w", oscap.ErrReadOnly)
	}
	h, cleanup, err := winHandle(ref, windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer cleanup()

	ft := windows.NsecToFiletime(ts.UnixNano())
	if err := windows.SetFileTime(h, &ft, nil, nil); err != nil {
		return mapWinErr(err)
	}
	return nil
}
