//go:build windows

package native

// fileid_windows.go —— Windows 上的稳定 FileID：NTFS file reference number。

import (
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// winIDs 用 GetFileInformationByHandle 的 FileIndex 作为稳定 ID。
//
// 无状态，故用值类型。
type winIDs struct{}

var _ oscap.StableFileID = winIDs{}

// FileID 返回 NTFS 的 file reference number。
//
// 它满足 ports.go 的两条硬要求：卷内唯一，且**跨重命名不变**
// （NTFS 的 MFT 记录号不随目录项变化，与 POSIX 的 inode 同性质）。
//
// 两个已知边界，都如实报 ErrNotSupported 而不是给个「差不多的」值：
//
//   - FAT/exFAT 上该索引恒为 0，所有对象会撞成同一个 ID。
//     probe_windows.go 的 probeFileIndex 判的就是这一条。
//   - ReFS 的真 ID 是 128 位，这里拿到的 64 位只是它的低半部分，
//     微软文档明说**不保证唯一**。我们无法从这个 64 位值判断自己是不是
//     在 ReFS 上，所以退而求其次：不去猜，把「可能撞」的风险留给
//     filesystem_mode 的使用者，并在此明确记录。
//     真要在 ReFS 上做对，得走 GetFileInformationByHandleEx(FileIdInfo)
//     拿 128 位再由 port 层扩宽返回值 —— 那是 port 的接口变更，
//     不是本适配器能单方面决定的（uint64 折叠成 64 位一样会撞）。
func (winIDs) FileID(ref oscap.Ref) (uint64, error) {
	h, cleanup, err := winHandle(ref, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, mapWinErr(err)
	}
	id := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if id == 0 {
		return 0, fmt.Errorf("oscap/native: %q 所在卷不提供文件索引: %w",
			ref.Path, oscap.ErrNotSupported)
	}
	return id, nil
}
