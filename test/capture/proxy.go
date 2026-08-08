// Command captureproxy 是一个 SMB 帧转储 TCP 代理，用来采集**真实**的线上字节，
// 作为 wire 包 golden test 的基准数据（AGENTS.md §3「关键路径要有抓包比对」）。
//
//	真实第三方客户端  →  captureproxy  →  真实 smbd
//	(smbclient/gosmb2)   (记录每一帧)      (Samba 官方实现)
//
// 为什么不用 tcpdump/tshark：
//   - 需要 CAP_NET_RAW / root，容器与 CI 里都不一定有；
//   - 还要额外装包、还要处理 TCP 重组；
//   - 而 SMB over Direct TCP 本身就是「4 字节大端长度前缀 + 报文」的定长分帧，
//     在应用层做转发代理就能拿到干净的、已重组的完整帧，零权限要求。
//
// 用法：
//
//	go run ./test/capture -listen 127.0.0.1:4451 -upstream 127.0.0.1:4450 \
//	    -out internal/smb/wire/testdata/capture -scenario negotiate-smb311
//
// 然后让客户端去连 -listen 地址。每收到一帧就落一个文件：
//
//	<out>/<scenario>/<序号>-<c2s|s2c>-<命令名>.bin
//
// 文件内容是**去掉 4 字节长度前缀之后**的 SMB 报文本体，
// 也就是 wire.ParseHeader / wire.ParseXxx 直接接受的字节。
// 同目录下的 manifest.txt 是人可读的帧摘要，便于挑选与核对。
//
// 这是开发/测试工具，不属于产品运行时代码，不进主二进制的依赖图。
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	var (
		listen   = flag.String("listen", "127.0.0.1:4451", "代理监听地址")
		upstream = flag.String("upstream", "127.0.0.1:4450", "真实 smbd 地址")
		outDir   = flag.String("out", "internal/smb/wire/testdata/capture", "fixture 输出根目录")
		scenario = flag.String("scenario", "default", "场景名（作为子目录名）")
		idle     = flag.Duration("idle", 0, "空闲多久后自动退出（0 = 不自动退出）")
		quiet    = flag.Bool("quiet", false, "只写文件，不打印每帧摘要")
	)
	flag.Parse()

	d := filepath.Join(*outDir, *scenario)
	if err := os.MkdirAll(d, 0o755); err != nil {
		fatal("创建输出目录: %v", err)
	}
	// 每次采集都从干净目录开始，避免旧帧与新帧混在一起。
	if err := clearDir(d); err != nil {
		fatal("清理输出目录: %v", err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal("监听 %s: %v", *listen, err)
	}
	defer ln.Close()

	r := &recorder{dir: d, quiet: *quiet, start: time.Now()}
	fmt.Fprintf(os.Stderr, "captureproxy: %s → %s，场景 %q，输出 %s\n", *listen, *upstream, *scenario, d)

	idleTimer := newIdleWatch(*idle, func() {
		fmt.Fprintf(os.Stderr, "captureproxy: 空闲 %s，退出\n", *idle)
		ln.Close()
	})

	var wg sync.WaitGroup
	connID := 0
	for {
		c, err := ln.Accept()
		if err != nil {
			break // 监听已关闭
		}
		connID++
		idleTimer.kick()
		wg.Add(1)
		go func(id int, c net.Conn) {
			defer wg.Done()
			if err := handle(id, c, *upstream, r, idleTimer); err != nil {
				fmt.Fprintf(os.Stderr, "captureproxy: conn%d: %v\n", id, err)
			}
		}(connID, c)
	}
	wg.Wait()

	if err := r.writeManifest(); err != nil {
		fatal("写 manifest: %v", err)
	}
	fmt.Fprintf(os.Stderr, "captureproxy: 共记录 %d 帧\n", r.count())
}

// handle 把一条客户端连接桥接到上游 smbd，并双向转储每一帧。
func handle(id int, client net.Conn, upstream string, r *recorder, w *idleWatch) error {
	defer client.Close()

	server, err := net.DialTimeout("tcp", upstream, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接上游 %s: %w", upstream, err)
	}
	defer server.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(id, client, server, dirC2S, r, w) }()
	go func() { defer wg.Done(); pump(id, server, client, dirS2C, r, w) }()
	wg.Wait()
	return nil
}

const (
	dirC2S = "c2s"
	dirS2C = "s2c"
)

// maxFrame 是允许的单帧上限。Direct TCP 的长度字段只有 24 位，
// 这里再收紧一道，避免异常输入把内存吃光。
const maxFrame = 16 << 20

// pump 从 src 逐帧读取、记录、原样写给 dst。
//
// 任何一侧出错或读完都关掉另一侧的写方向，让对端的 pump 也能收尾。
func pump(id int, src, dst net.Conn, dir string, r *recorder, w *idleWatch) {
	defer closeWrite(dst)

	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				fmt.Fprintf(os.Stderr, "captureproxy: conn%d %s 读长度前缀: %v\n", id, dir, err)
			}
			return
		}
		// MS-SMB2 §2.1 Direct TCP：4 字节，首字节为 0，后 3 字节是**大端**长度。
		// 注意与 SMB2 报文体的小端相反 —— 这是最容易搞混的地方。
		if hdr[0] != 0 {
			fmt.Fprintf(os.Stderr, "captureproxy: conn%d %s 非 Direct TCP 前缀 %02X\n", id, dir, hdr[0])
			return
		}
		n := int(binary.BigEndian.Uint32(hdr) & 0x00FFFFFF)
		if n == 0 || n > maxFrame {
			fmt.Fprintf(os.Stderr, "captureproxy: conn%d %s 帧长 %d 非法\n", id, dir, n)
			return
		}

		msg := make([]byte, n)
		if _, err := io.ReadFull(src, msg); err != nil {
			fmt.Fprintf(os.Stderr, "captureproxy: conn%d %s 读报文体: %v\n", id, dir, err)
			return
		}

		w.kick()
		r.record(id, dir, msg)

		if _, err := dst.Write(hdr); err != nil {
			return
		}
		if _, err := dst.Write(msg); err != nil {
			return
		}
	}
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
		return
	}
	_ = c.Close()
}

// ---------------------------------------------------------------------------
// 记录
// ---------------------------------------------------------------------------

type frameInfo struct {
	seq     int
	conn    int
	dir     string
	file    string
	summary string
}

type recorder struct {
	dir    string
	quiet  bool
	start  time.Time
	mu     sync.Mutex
	seq    int
	frames []frameInfo
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *recorder) record(conn int, dir string, msg []byte) {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	elapsed := time.Since(r.start)
	r.mu.Unlock()

	name, sum := describe(msg)
	file := fmt.Sprintf("%03d-%s-%s.bin", seq, dir, name)

	if err := os.WriteFile(filepath.Join(r.dir, file), msg, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "captureproxy: 写 %s: %v\n", file, err)
		return
	}

	line := fmt.Sprintf("%03d %s conn%d %6dB %-9s %s", seq, dir, conn, len(msg), name, sum)
	if !r.quiet {
		fmt.Fprintf(os.Stderr, "  [%7.3fs] %s\n", elapsed.Seconds(), line)
	}

	r.mu.Lock()
	r.frames = append(r.frames, frameInfo{seq: seq, conn: conn, dir: dir, file: file, summary: line})
	r.mu.Unlock()
}

func (r *recorder) writeManifest() error {
	r.mu.Lock()
	frames := append([]frameInfo(nil), r.frames...)
	r.mu.Unlock()

	sort.Slice(frames, func(i, j int) bool { return frames[i].seq < frames[j].seq })

	var b strings.Builder
	b.WriteString("# captureproxy 帧摘要\n")
	b.WriteString("# 每个 .bin 是去掉 4 字节 Direct TCP 长度前缀之后的 SMB 报文本体。\n")
	b.WriteString("# 列：序号 方向 连接 长度 命令 摘要\n")
	for _, f := range frames {
		b.WriteString(f.summary)
		b.WriteByte('\n')
	}
	return os.WriteFile(filepath.Join(r.dir, "manifest.txt"), []byte(b.String()), 0o644)
}

// ---------------------------------------------------------------------------
// 帧摘要
//
// 这里**故意不 import internal/smb/wire**：采集工具必须能在 wire 有 bug
// 甚至编译不过的时候照常工作，否则「用采集数据验证 wire」就成了循环论证。
// 所以下面重新用最朴素的方式读几个字段。
// ---------------------------------------------------------------------------

// SMB2 头字段偏移（MS-SMB2 §2.2.1.2）。
const (
	offStructureSize = 4
	offCommand       = 12
	offCredits       = 14
	offFlags         = 16
	offNextCommand   = 20
	offMessageID     = 24
	offTreeID        = 36
	offSessionID     = 40
	smb2HeaderSize   = 64
)

var commandNames = [...]string{
	"NEGOTIATE", "SESSION_SETUP", "LOGOFF", "TREE_CONNECT", "TREE_DISCONNECT",
	"CREATE", "CLOSE", "FLUSH", "READ", "WRITE", "LOCK", "IOCTL", "CANCEL",
	"ECHO", "QUERY_DIRECTORY", "CHANGE_NOTIFY", "QUERY_INFO", "SET_INFO",
	"OPLOCK_BREAK",
}

// describe 返回用于文件名的命令名，以及一行人可读摘要。
func describe(msg []byte) (name, summary string) {
	if len(msg) < 4 {
		return "SHORT", fmt.Sprintf("过短(%d 字节)", len(msg))
	}
	switch {
	case msg[0] == 0xFD && string(msg[1:4]) == "SMB":
		return "TRANSFORM", "SMB3 加密报文（TRANSFORM_HEADER）"
	case msg[0] == 0xFF && string(msg[1:4]) == "SMB":
		return "SMB1", fmt.Sprintf("SMB1 报文 command=0x%02X", msg[4])
	case msg[0] != 0xFE || string(msg[1:4]) != "SMB":
		return "UNKNOWN", fmt.Sprintf("非 SMB 报文，前 4 字节 % X", msg[:4])
	}
	if len(msg) < smb2HeaderSize {
		return "SHORT", fmt.Sprintf("SMB2 头不完整(%d 字节)", len(msg))
	}

	cmd := binary.LittleEndian.Uint16(msg[offCommand:])
	name = commandName(cmd)
	flags := binary.LittleEndian.Uint32(msg[offFlags:])
	status := binary.LittleEndian.Uint32(msg[8:])

	var sb strings.Builder
	fmt.Fprintf(&sb, "mid=%d", binary.LittleEndian.Uint64(msg[offMessageID:]))
	fmt.Fprintf(&sb, " credits=%d", binary.LittleEndian.Uint16(msg[offCredits:]))
	if sid := binary.LittleEndian.Uint64(msg[offSessionID:]); sid != 0 {
		fmt.Fprintf(&sb, " sid=%#x", sid)
	}
	if flags&0x00000002 == 0 { // 非 ASYNC 时 0x24 处才是 TreeId
		if tid := binary.LittleEndian.Uint32(msg[offTreeID:]); tid != 0 {
			fmt.Fprintf(&sb, " tid=%#x", tid)
		}
	}
	if flags&0x00000001 != 0 { // SERVER_TO_REDIR：0x08 处才是 Status
		fmt.Fprintf(&sb, " status=%#08x", status)
	}
	if flags&0x00000008 != 0 {
		sb.WriteString(" SIGNED")
	}
	if flags&0x00000004 != 0 {
		sb.WriteString(" RELATED")
	}
	if ss := binary.LittleEndian.Uint16(msg[offStructureSize:]); ss != smb2HeaderSize {
		fmt.Fprintf(&sb, " !!头StructureSize=%d", ss)
	}

	// 复合链：把后续命令也列进文件名，否则同名文件会互相覆盖语义。
	if next := binary.LittleEndian.Uint32(msg[offNextCommand:]); next != 0 {
		chain := chainNames(msg)
		if len(chain) > 1 {
			name = strings.Join(chain, "+")
		}
		fmt.Fprintf(&sb, " compound(%d)", len(chain))
	}
	return name, sb.String()
}

// chainNames 走一遍复合链，返回其中每条消息的命令名。
func chainNames(msg []byte) []string {
	var out []string
	off := 0
	for i := 0; i < 16; i++ { // 防御畸形/循环链
		if off+smb2HeaderSize > len(msg) {
			break
		}
		out = append(out, commandName(binary.LittleEndian.Uint16(msg[off+offCommand:])))
		next := int(binary.LittleEndian.Uint32(msg[off+offNextCommand:]))
		if next == 0 || next < smb2HeaderSize {
			break
		}
		off += next
	}
	return out
}

func commandName(cmd uint16) string {
	if int(cmd) < len(commandNames) {
		return commandNames[cmd]
	}
	return fmt.Sprintf("CMD%02X", cmd)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// idleWatch 在指定时长内没有任何帧时触发回调，让采集脚本不必猜要 sleep 多久。
type idleWatch struct {
	d  time.Duration
	mu sync.Mutex
	t  *time.Timer
}

func newIdleWatch(d time.Duration, fn func()) *idleWatch {
	w := &idleWatch{d: d}
	if d > 0 {
		w.t = time.AfterFunc(d, fn)
	}
	return w
}

func (w *idleWatch) kick() {
	if w == nil || w.t == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.t.Reset(w.d)
}

func clearDir(d string) error {
	ents, err := os.ReadDir(d)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(d, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "captureproxy: "+format+"\n", args...)
	os.Exit(1)
}
