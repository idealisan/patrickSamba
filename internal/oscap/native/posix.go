//go:build linux || darwin

package native

// posix.go —— linux 与 darwin 共用的底座：errno 映射、扩展属性名编解码、
// 句柄/路径二选一的取 fd 逻辑。
//
// 平台差异只有两处，放在 plat_linux.go / plat_darwin.go：
//   - 扩展属性的命名空间前缀（Linux 是 `user.`，macOS 没有）；
//   - 「属性不存在」的 errno（Linux 是 ENODATA，macOS 是 ENOATTR）。

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// mapPosixErr 把 errno 映射成 oscap 的 sentinel。
//
// 认不出来的 errno**原样返回**，不要笼统地塞成 ErrNotSupported ——
// 那会把「磁盘满」「配额超限」这类真问题伪装成能力缺失，
// 上层于是去 builtin 重试一遍，然后以同样的原因再失败一次，
// 而日志里只剩「不支持」这四个字。
func mapPosixErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errnoNoAttr):
		// 属性/流不存在。注意这与「文件系统不支持扩展属性」是两回事，
		// 后者是 ENOTSUP，见 oscap.ErrNotFound / ErrNotSupported 的注释。
		return oscap.ErrNotFound
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
		return oscap.ErrNotFound
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.ENOSYS):
		return oscap.ErrNotSupported
	case errors.Is(err, unix.EROFS):
		return oscap.ErrReadOnly
	case errors.Is(err, unix.EEXIST):
		return oscap.ErrExist
	case errors.Is(err, unix.ENAMETOOLONG):
		return oscap.ErrInvalidArg
	}
	return err
}

// errNoSpace 是「装不下」的 errno。
//
// 命名流超过 xattr 承载上限时用它而不是自造 sentinel：vfs 的 errmap 已经把
// ENOSPC 映射成 ErrNoSpace → STATUS_DISK_FULL，客户端看得懂「写不下」。
const errNoSpace = unix.ENOSPC

// --- 扩展属性名编解码 -------------------------------------------------------
//
// 契约（ports.go 的 Xattr 注释）：**双向对称** ——
// ListXattr 报出来的名字，客户端拿去 GetXattr 必须能取到。
//
// 因此这里的规则刻意做成**无条件加前缀 / 剥一层前缀**，而不是
// internal/vfs/xattr_unix.go 那种「已经带已知命名空间就原样透传」：
// 透传规则在名字本身叫 `user.x` 时会破对称 ——
// 磁盘上的 `user.user.x` 解码成 `user.x`，再编码回去却变成 `user.x`，
// 指向了另一个属性。无条件规则下 encode(decode(raw)) ≡ raw、
// decode(encode(n)) ≡ n 恒成立。
//
// 对真实的 SMB 侧名字（`com.apple.*`、`DosStream.*`）两种规则结果相同，
// 所以与 vfs 现有落盘格式完全兼容。

// maxXattrNameLen 是**完整**扩展属性名（含命名空间前缀）的字节上限。
//
// Linux 的 XATTR_NAME_MAX 是 255，macOS 的上限是 127；这里统一取严的 127？
// **不**——取 255。理由与 vfs/stream_xattr.go 相反方向的同一个考虑：
// 这个常量只用来**拒绝**过长的名字，取宽会让 macOS 上的调用如实拿到内核的
// ENAMETOOLONG（照样是错误，不会静默截断），取严则会在 Linux 上平白拒掉
// 一批本可写入的名字。真正危险的是**静默截断**，而截断只发生在我们自己
// 动手切字符串的时候 —— 本包一次都不切。
const maxXattrNameLen = 255

// reservedStreamPrefix 是命名流在扩展属性里占用的名字前缀（SMB 侧视角，
// 不含命名空间）。见 stream_posix.go。
//
// Xattr 能力必须把它**藏起来**：这块名字空间是命名流的落盘细节，
// 若从 ListXattr 里漏出去，客户端会把流内容当成一个普通扩展属性读走，
// 甚至覆盖掉 —— 那会直接损坏资源叉。
const reservedStreamPrefix = "DosStream."

// encodeXattrName 把 SMB 侧名字映射成宿主机扩展属性名。
func encodeXattrName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("oscap/native: 扩展属性名为空: %w", oscap.ErrInvalidArg)
	}
	if strings.ContainsRune(name, 0) {
		// NUL 会被内核当成名字结束，也会破坏 listxattr 的 NUL 分隔格式：
		// "a\x00b" 写进去、列出来变成 "a"，两个名字就此撞车。
		return "", fmt.Errorf("oscap/native: 扩展属性名含 NUL: %w", oscap.ErrInvalidArg)
	}
	if strings.HasPrefix(name, reservedStreamPrefix) {
		return "", fmt.Errorf(
			"oscap/native: 扩展属性名 %q 落在命名流保留前缀 %q 内: %w",
			name, reservedStreamPrefix, oscap.ErrInvalidArg)
	}
	full := xattrNamespace + name
	if len(full) > maxXattrNameLen {
		// 不截断，如实拒绝：截断会让两个不同的名字映射到同一个属性，
		// 后写的悄悄覆盖先写的。
		return "", fmt.Errorf("oscap/native: 扩展属性名过长（%d > %d 字节）: %w",
			len(full), maxXattrNameLen, oscap.ErrInvalidArg)
	}
	return full, nil
}

// decodeXattrName 是 encodeXattrName 的逆操作。
// 第二个返回值为 false 表示这个属性不属于我们暴露给 SMB 侧的名字空间
// （别的命名空间，或者命名流的落盘属性），调用方应当跳过它。
func decodeXattrName(raw string) (string, bool) {
	if !strings.HasPrefix(raw, xattrNamespace) {
		return "", false
	}
	name := raw[len(xattrNamespace):]
	if name == "" || strings.HasPrefix(name, reservedStreamPrefix) {
		return "", false
	}
	return name, true
}

// --- fd / 路径二选一 --------------------------------------------------------

// refFD 返回可用的 fd，第二个返回值表示是否拿到。
//
// Ref.Handle 为 nil 是**常态而不是异常**（SMB 的 OpenAttrOnly 句柄背后
// 就没有 *os.File，见 oscap.Ref 的注释），所以每一处调用都必须备好路径版分支。
func refFD(ref oscap.Ref) (int, bool) {
	if ref.Handle == nil {
		return -1, false
	}
	return int(ref.Handle.Fd()), true
}

// openForWrite 取一个可写 fd。
//
// 优先复用调用方的句柄；没有句柄时自己按路径打开，返回的 cleanup
// 负责关掉我们自己开的那个（复用调用方句柄时是空操作 —— 关掉别人的 fd
// 会让上层的后续读写莫名其妙地失败，这是最难查的一类 bug）。
//
// 一律带 O_NOFOLLOW：与 internal/vfs 打开最后一跳时的做法保持一致
// （vfs/path.go 的 TOCTOU 说明）。vfs 已经校验过路径不越界，但校验与打开
// 之间存在时间窗，最后一跳若是符号链接就能把写引到共享外面去。
func openForWrite(ref oscap.Ref) (fd int, cleanup func(), err error) {
	if fd, ok := refFD(ref); ok {
		return fd, func() {}, nil
	}
	f, err := os.OpenFile(ref.Path, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		// 直接把 *os.PathError 交给 mapPosixErr：errors.Is 会沿着 Unwrap
		// 链找到底层 errno，而认不出来的错误保留外壳（带着文件名）更好排查。
		return -1, nil, mapPosixErr(err)
	}
	return int(f.Fd()), func() { _ = f.Close() }, nil
}

// openForRead 取一个只读 fd，语义同 openForWrite。
//
// 只读共享上也能用，所以稀疏区间查询这类纯读操作必须走它而不是 openForWrite。
func openForRead(ref oscap.Ref) (fd int, cleanup func(), err error) {
	// 刻意**不复用** ref.Handle：本函数的调用方（AllocatedRanges）要用
	// lseek(SEEK_DATA/SEEK_HOLE) 探测空洞，而 lseek 会移动 fd 的文件偏移。
	// 在别人的句柄上动偏移是一种隔空作用的副作用：vfs 现在全走 pread/pwrite
	// 所以看不出问题，哪天有人加了一次 Read(2) 就会读到错误的位置，
	// 且现场与这里隔着好几层。自己开一个 fd 的代价是一次 open，不值得省。
	f, err := os.OpenFile(ref.Path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, nil, mapPosixErr(err)
	}
	return int(f.Fd()), func() { _ = f.Close() }, nil
}
