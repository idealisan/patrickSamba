//go:build windows

package native

// stream_windows.go —— NTFS alternate data stream，命名流在 Windows 上的**原生**落点。
//
// # 落盘形态
//
// 与 POSIX 侧（stream_posix.go 把流塞进 xattr）完全不同：这里的流就是
// NTFS 真正的 ADS，路径写法 `文件:流名:$DATA`。好处是它与 Windows 上的
// 本地工具看到的是同一份东西（`dir /r`、`more < file:stream`），
// 也没有 xattr 那个 64 KiB 的承载上限 —— ADS 可以任意大。
//
// # 关于 kernel32
//
// 流枚举要用 FindFirstStreamW / FindNextStreamW，golang.org/x/sys/windows
// **没有**封装它们（GetFileSizeEx 同样没有），所以本文件自己声明。
// 用的是 AGENTS.md §1.2 白名单里的 `NewLazySystemDLL("kernel32.dll")` ——
// 走 System32 安全加载路径，名字是字面量。
// **不要**改成 NewLazyDLL / LoadLibrary：那是 DLL 劫持的经典入口，
// scripts/check-constraints.sh 的 C9 段会当场判红。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

var (
	modkernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procFindFirstStreamW = modkernel32.NewProc("FindFirstStreamW")
	procFindNextStreamW  = modkernel32.NewProc("FindNextStreamW")
	procGetFileSizeEx    = modkernel32.NewProc("GetFileSizeEx")
)

// findStreamInfoStandard 是 STREAM_INFO_LEVELS 的唯一取值
// （fileapi.h：`FindStreamInfoStandard = 0`）。
const findStreamInfoStandard = 0

// streamProcsAvailable 确认本文件用到的三个 kernel32 导出在本机真的存在。
//
// 必须先问一句再调：LazyProc.Call **找不到导出就 panic**（它内部是
// mustFind），而本包的第一条纪律是「做不到就如实说，绝不 panic」
// （native.go 包注释）。FindFirstStreamW/FindNextStreamW 是 Vista 才有的，
// 将来若在 Wine、ReactOS 或某个精简运行时上跑，缺失是完全可能的。
//
// **三个导出一起判，判定点在 newSet 而不是各调用点**：按 capability.go
// 的划分，一项能力必须是「要么整套能用、要么整套不能用」的最小单元 ——
// 能开流却枚举不出来的 winStreams 是半吊子，那种状态必须整项判为不支持、
// 让 builtin 接管，而不是留一半能用一半 panic/报错。
func streamProcsAvailable() error {
	for _, p := range []*windows.LazyProc{
		procFindFirstStreamW, procFindNextStreamW, procGetFileSizeEx,
	} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("oscap/native: kernel32 无导出 %s（%v）: %w",
				p.Name, err, oscap.ErrNotSupported)
		}
	}
	return nil
}

// win32FindStreamData 对应 WIN32_FIND_STREAM_DATA（fileapi.h）。
//
//	typedef struct _WIN32_FIND_STREAM_DATA {
//	  LARGE_INTEGER StreamSize;
//	  WCHAR         cStreamName[MAX_PATH + 36];
//	} WIN32_FIND_STREAM_DATA;
//
// 296 = MAX_PATH(260) + 36。这个 36 是给 `:` 与 `:$DATA` 一类的
// 流类型后缀留的余量，**不要**自作主张改小：FindNextStreamW 按结构体
// 固定大小写入，缓冲区短了就是一次栈溢出。
type win32FindStreamData struct {
	StreamSize  int64
	CStreamName [296]uint16
}

// maxWinStreamNameLen 是 NTFS 的流名上限（255 个字符）。
//
// 与 POSIX 侧那个 234 字节的预算来源不同（那边受 xattr 名总长约束），
// 所以刻意**不**统一成一个数，见 native.go 里 validateStreamName 的说明。
const maxWinStreamNameLen = 255

// winStreams 实现 oscap.NamedStream。
type winStreams struct {
	readOnly bool
}

var _ oscap.NamedStream = (*winStreams)(nil)

// streamPath 拼出 `文件:流名` 这条路径，并做安全校验。
//
// 这是一处安全边界：流名由客户端控制，直接进宿主机名字空间。
// 放行 `\` 或 `:` 等于让客户端把写入引到**别的文件**上去
// （`a.txt:..\..\b.txt` 之类），validateStreamName 已经把这几个字符挡掉。
//
// 不拼 `:$DATA` 后缀：CreateFile 默认就按 $DATA 类型解析，
// 显式带上反而会让某些重定向器把它当成流名的一部分。
func streamPath(base, name string) (string, error) {
	if err := validateStreamName(name); err != nil {
		return "", err
	}
	if len(name) > maxWinStreamNameLen {
		// 不截断，如实拒绝：截断会让两个不同的流名落到同一个 ADS，
		// 后写的把先写的悄悄覆盖掉。
		return "", fmt.Errorf("oscap/native: 命名流名过长（%d > %d）: %w",
			len(name), maxWinStreamNameLen, oscap.ErrInvalidArg)
	}
	return base + ":" + name, nil
}

// mapStreamErr 在 mapWinErr 之上补两个**只有在流路径上才成立**的判定。
//
// 在一条带冒号的路径上，ERROR_INVALID_NAME / ERROR_INVALID_PARAMETER
// 的真实含义是「这个卷不支持 ADS」（FAT32/exFAT 外置盘、部分网络重定向器），
// 不是「你参数写错了」。这个区分很要紧：报 ErrNotSupported 上层才会退到
// builtin 的旁路存储，报 ErrInvalidArg 则会被当成客户端的错误直接回绝。
//
// 只在本文件用，不并进 mapWinErr —— 普通路径上 ERROR_INVALID_PARAMETER
// 确实就是参数非法，混为一谈会把真 bug 说成"环境不支持"。
func mapStreamErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_INVALID_NAME) ||
		errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return oscap.ErrNotSupported
	}
	return mapWinErr(err)
}

// parseStreamName 把 FindStreamData 报出来的 `:名字:$DATA` 还原成裸流名。
//
// 第二个返回值为 false 表示这条不该出现在结果里：主数据流（`::$DATA`）
// 按 ports.go 的规定不列，非 $DATA 类型（`:名字:$INDEX_ALLOCATION`，
// 目录的索引流）也不列 —— 它们不是客户端能当文件读写的数据流。
func parseStreamName(raw string) (string, bool) {
	if !strings.HasPrefix(raw, ":") {
		return "", false
	}
	rest := raw[1:]
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		// 没有类型后缀。理论上 NTFS 总会带，宽容处理。
		if rest == "" {
			return "", false
		}
		return rest, true
	}
	if !strings.EqualFold(rest[i:], ":$DATA") {
		return "", false
	}
	name := rest[:i]
	if name == "" {
		return "", false // 主数据流
	}
	return name, true
}

func (s *winStreams) ListStreams(ref oscap.Ref) ([]oscap.StreamInfo, error) {
	p, err := windows.UTF16PtrFromString(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("oscap/native: 路径 %q 非法: %w", ref.Path, oscap.ErrInvalidArg)
	}
	var data win32FindStreamData
	r, _, e := procFindFirstStreamW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(findStreamInfoStandard),
		uintptr(unsafe.Pointer(&data)),
		0,
	)
	h := windows.Handle(r)
	if h == windows.InvalidHandle {
		if errors.Is(e, windows.ERROR_HANDLE_EOF) {
			// 文档规定：一个流都没有时也是这个错。空结果是合法答案。
			return nil, nil
		}
		return nil, mapStreamErr(e)
	}
	defer func() { _ = windows.FindClose(h) }()

	var out []oscap.StreamInfo
	for {
		if name, ok := parseStreamName(windows.UTF16ToString(data.CStreamName[:])); ok {
			out = append(out, oscap.StreamInfo{
				Name: name,
				Size: data.StreamSize,
				// NTFS 不通过这个结构体报流的分配长度。取 Size 作为下限
				// 而不是自己按簇大小猜一个：猜错会让客户端算出负的剩余空间。
				Alloc: data.StreamSize,
			})
		}
		r, _, e := procFindNextStreamW.Call(uintptr(h), uintptr(unsafe.Pointer(&data)))
		if r == 0 {
			if errors.Is(e, windows.ERROR_HANDLE_EOF) {
				return out, nil
			}
			return nil, mapStreamErr(e)
		}
	}
}

func (s *winStreams) OpenStream(ref oscap.Ref, name string, flags oscap.StreamFlags) (oscap.StreamHandle, error) {
	full, err := streamPath(ref.Path, name)
	if err != nil {
		return nil, err
	}
	wantWrite := flags&(oscap.StreamWrite|oscap.StreamCreate|oscap.StreamTruncate) != 0
	if wantWrite && s.readOnly {
		return nil, oscap.ErrReadOnly
	}

	var of int
	switch {
	case wantWrite && flags&oscap.StreamRead != 0:
		of = os.O_RDWR
	case wantWrite:
		of = os.O_WRONLY
	default:
		of = os.O_RDONLY
	}
	if flags&oscap.StreamCreate != 0 {
		of |= os.O_CREATE

		// 先确认宿主对象自己在。
		//
		// 少了这一步，对一个**不存在**的文件请求 `x.txt:s` 会让
		// CreateFile 顺手把 x.txt 也建出来（一个零长度的幽灵文件）。
		// POSIX 侧不会这样（xattr 写不到不存在的 inode 上），
		// 两边行为不一致的能力会让上层写出只在一个平台成立的逻辑。
		if _, err := os.Lstat(ref.Path); err != nil {
			return nil, mapWinErr(err)
		}
	}
	if flags&oscap.StreamTruncate != 0 {
		of |= os.O_TRUNC
	}

	f, err := os.OpenFile(full, of, 0o666)
	if err != nil {
		return nil, mapStreamErr(err)
	}
	return &winStreamHandle{f: f, write: wantWrite}, nil
}

func (s *winStreams) RemoveStream(ref oscap.Ref, name string) error {
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	full, err := streamPath(ref.Path, name)
	if err != nil {
		return err
	}
	// DeleteFileW 作用在流路径上时只删这个流，宿主文件不动。
	if err := os.Remove(full); err != nil {
		return mapStreamErr(err)
	}
	return nil
}

// winStreamHandle 是一个已打开的 ADS 句柄。
//
// ADS 就是普通的数据流，所以直接复用 *os.File 的 pread/pwrite ——
// 不像 POSIX 侧那样每次都要把整条流读出来改完再写回去。
type winStreamHandle struct {
	mu sync.Mutex
	f  *os.File
	// write 记录打开时有没有要写。没要就拒绝写操作 ——
	// 底层句柄本来也没有写权，但先在这里挡住能给出**语义正确**的
	// ErrReadOnly，而不是一个 ERROR_ACCESS_DENIED。
	write  bool
	closed bool
}

var _ oscap.StreamHandle = (*winStreamHandle)(nil)

func (h *winStreamHandle) begin() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return oscap.ErrClosed
	}
	return nil
}

func (h *winStreamHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("oscap/native: 读偏移 %d 为负: %w", off, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()

	n, err := h.f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		// io.EOF 原样透传：它是 io.ReaderAt 契约的一部分，
		// 被映射成别的错误会让调用方分不清"读到头了"和"出事了"。
		return n, mapWinErr(err)
	}
	return n, err
}

func (h *winStreamHandle) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("oscap/native: 写偏移 %d 为负: %w", off, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()
	if !h.write {
		return 0, oscap.ErrReadOnly
	}

	n, err := h.f.WriteAt(p, off)
	if err != nil {
		return n, mapWinErr(err)
	}
	return n, nil
}

func (h *winStreamHandle) Truncate(size int64) error {
	if size < 0 {
		return fmt.Errorf("oscap/native: 截断长度 %d 为负: %w", size, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return err
	}
	defer h.mu.Unlock()
	if !h.write {
		return oscap.ErrReadOnly
	}
	if err := h.f.Truncate(size); err != nil {
		return mapWinErr(err)
	}
	return nil
}

// Size 返回流的当前长度。
//
// 用 GetFileSizeEx 而不是 Seek(0, io.SeekEnd)：后者会**移动文件指针**，
// 而这个句柄可能同时有别的操作在用。查长度是个只读动作，不该有副作用。
func (h *winStreamHandle) Size() (int64, error) {
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()

	var size int64
	r, _, e := procGetFileSizeEx.Call(h.f.Fd(), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return 0, mapWinErr(e)
	}
	return size, nil
}

// Close 关闭底层句柄。重复关闭幂等返回 nil ——
// defer Close() 与显式 Close() 并存是常见写法，为此报错纯属添乱。
func (h *winStreamHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if err := h.f.Close(); err != nil {
		return mapWinErr(err)
	}
	return nil
}
