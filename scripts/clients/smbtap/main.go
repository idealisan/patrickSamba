// smbtap —— 验收测试用的 SMB 线级旁路探针。
//
// 它是一个透明 TCP 中继：客户端连 -listen，它把字节原样转发到 -target，
// 同时在转发路径上按 Direct TCP 传输层（4 字节长度前缀，**大端**）切帧，
// 只读每帧开头的 ProtocolId 与 SMB2 头里的 Command，把统计写进 -report。
//
// 为什么需要它：
//
//	"加密生效"过去是靠"smbclient 能读到文件内容"来判定的，而这个判定对
//	**明文旁路**完全无感 —— 服务端没加密、客户端也没加密，文件照样读得到，
//	用例照样绿。真实发生过：encryption_required: true 的服务被
//	`smbclient -m SMB2_10` 一句降级成全程明文，验收脚本一声不吭。
//	唯一可靠的判据是看网线上到底是什么字节，所以有了这个探针。
//
// 判据（MS-SMB2 §2.2.41 SMB2 TRANSFORM_HEADER）：
//
//	加密帧的 ProtocolId 是 0xFD 'S' 'M' 'B'，明文帧是 0xFE 'S' 'M' 'B'。
//	会话建立之前必然是明文（NEGOTIATE / SESSION_SETUP 本来就不能加密，
//	MS-SMB2 §3.3.4.1.4），会话建立之后若加密生效，TREE_CONNECT 及其后的
//	所有请求都必须是 TRANSFORM 帧。因此断言写成：
//	    明文帧里出现的 Command 只允许是 NEGOTIATE(0x00) 与 SESSION_SETUP(0x01)，
//	    出现任何第三种命令的明文帧 = 加密没生效。
//	这比"transform 帧数 > 0"严格：混合了几帧明文的实现同样会被抓出来。
//
// 用法：
//
//	smbtap -listen 127.0.0.1:14445 -target 127.0.0.1:4445 -report /tmp/tap.txt [-conns 1]
//
// 探针在收够 -conns 个连接且它们全部结束后写报告并退出（超时同样写报告）。
// 报告是 key=value 行，便于 shell 直接 grep。
//
// 注意：这是**验收脚本的一部分**，不是服务运行时依赖，不违反 AGENTS.md C3。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SMB 各协议的 ProtocolId（MS-SMB2 §2.2.1.1 / §2.2.41、MS-CIFS §2.2.3.1）。
var (
	protoSMB2      = []byte{0xFE, 'S', 'M', 'B'} // 明文 SMB2/3
	protoTransform = []byte{0xFD, 'S', 'M', 'B'} // SMB3 TRANSFORM_HEADER（加密）
	protoSMB1      = []byte{0xFF, 'S', 'M', 'B'} // SMB1
)

// SMB2 命令码（MS-SMB2 §2.2.1.2 Command）。会话建立之前只可能出现这两个。
const (
	cmdNegotiate    uint16 = 0x0000
	cmdSessionSetup uint16 = 0x0001
)

// smb2HeaderSize 是 SMB2 定长头的大小（MS-SMB2 §2.2.1.2）。
const smb2HeaderSize = 64

// maxCompound 是复合链的遍历上限，纯防御，避免恶意 NextCommand 造成死循环。
const maxCompound = 64

type dirStats struct {
	frames    int
	plain     int
	transform int
	smb1      int
	other     int
	// plainCmds 是明文 SMB2 帧里出现过的所有 Command（去重后升序）。
	plainCmds map[uint16]bool
}

func newDirStats() *dirStats { return &dirStats{plainCmds: map[uint16]bool{}} }

// observe 解析一个 Direct TCP payload（已剥掉 4 字节长度前缀）。
// 只读不改，任何异常都只记为 other，绝不 panic —— 探针挂掉会把验收失败的
// 原因指向错误的地方。
func (s *dirStats) observe(payload []byte) {
	s.frames++
	switch {
	case len(payload) >= 4 && bytes.Equal(payload[:4], protoTransform):
		s.transform++
	case len(payload) >= 4 && bytes.Equal(payload[:4], protoSMB2):
		s.plain++
		for _, c := range smb2Commands(payload) {
			s.plainCmds[c] = true
		}
	case len(payload) >= 4 && bytes.Equal(payload[:4], protoSMB1):
		s.smb1++
	default:
		s.other++
	}
}

// smb2Commands 走一遍复合链（MS-SMB2 §3.2.4.1.4 NextCommand），
// 返回这一帧里所有 SMB2 消息的 Command。
func smb2Commands(payload []byte) []uint16 {
	var cmds []uint16
	off := 0
	for i := 0; i < maxCompound; i++ {
		if off < 0 || off+smb2HeaderSize > len(payload) {
			break
		}
		h := payload[off:]
		if !bytes.Equal(h[:4], protoSMB2) {
			break
		}
		// Command 在头内偏移 12，2 字节**小端**（SMB2 报文体一律小端）。
		cmds = append(cmds, binary.LittleEndian.Uint16(h[12:14]))
		// NextCommand 在偏移 20，4 字节小端，是到下一个头的字节偏移。
		next := binary.LittleEndian.Uint32(h[20:24])
		if next < smb2HeaderSize {
			break // 0 表示链结束；小于一个头的值是畸形，同样停下
		}
		off += int(next)
	}
	return cmds
}

func (s *dirStats) sortedPlainCmds() []uint16 {
	out := make([]uint16, 0, len(s.plainCmds))
	for c := range s.plainCmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// postAuthPlainCmds 返回**不该以明文出现**的命令：除 NEGOTIATE / SESSION_SETUP
// 之外的一切。加密生效时这个集合必须为空。
func (s *dirStats) postAuthPlainCmds() []uint16 {
	var out []uint16
	for _, c := range s.sortedPlainCmds() {
		if c != cmdNegotiate && c != cmdSessionSetup {
			out = append(out, c)
		}
	}
	return out
}

func joinCmds(cmds []uint16) string {
	parts := make([]string, len(cmds))
	for i, c := range cmds {
		parts[i] = "0x" + strconv.FormatUint(uint64(c), 16)
	}
	return strings.Join(parts, ",")
}

type tap struct {
	mu    sync.Mutex
	c2s   *dirStats
	s2c   *dirStats
	conns int
}

func main() {
	listenAddr := flag.String("listen", "", "监听地址，客户端连这里")
	targetAddr := flag.String("target", "", "真实服务端地址")
	report := flag.String("report", "", "报告文件路径")
	conns := flag.Int("conns", 1, "观测多少个连接后退出")
	timeout := flag.Duration("timeout", 60*time.Second, "整体超时")
	flag.Parse()

	if *listenAddr == "" || *targetAddr == "" || *report == "" {
		fmt.Fprintln(os.Stderr, "用法: smbtap -listen <addr> -target <addr> -report <file> [-conns N]")
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smbtap: 监听 %s 失败: %v\n", *listenAddr, err)
		os.Exit(1)
	}
	// 探针就绪的信号：调用方 grep 这一行再启动客户端，避免竞态。
	fmt.Println("smbtap ready", ln.Addr().String())
	os.Stdout.Sync()

	t := &tap{c2s: newDirStats(), s2c: newDirStats()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < *conns; i++ {
			cli, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				t.handle(cli, *targetAddr)
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(*timeout):
		fmt.Fprintln(os.Stderr, "smbtap: 超时，按已观测到的数据出报告")
	}
	ln.Close()

	if err := t.write(*report); err != nil {
		fmt.Fprintf(os.Stderr, "smbtap: 写报告失败: %v\n", err)
		os.Exit(1)
	}
}

func (t *tap) handle(cli net.Conn, target string) {
	defer cli.Close()
	srv, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smbtap: 连接 %s 失败: %v\n", target, err)
		return
	}
	defer srv.Close()

	t.mu.Lock()
	t.conns++
	t.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.pump(srv, cli, t.c2s); srv.Close(); cli.Close() }()
	go func() { defer wg.Done(); t.pump(cli, srv, t.s2c); srv.Close(); cli.Close() }()
	wg.Wait()
}

// pump 把 src 的字节原样写给 dst，顺带按 Direct TCP 帧边界喂给 sink。
//
// 之所以能这样切帧：Direct TCP 传输（MS-SMB2 §2.1）每条消息前有 4 字节头，
// 首字节为 0，后 3 字节是**大端**长度。转发必须逐帧同步进行，不能先攒后发，
// 否则会改变时序、把客户端超时逼出来。
func (t *tap) pump(dst io.Writer, src io.Reader, sink *dirStats) {
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			return
		}
		if _, err := dst.Write(hdr); err != nil {
			return
		}
		n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if n == 0 {
			continue // keep-alive 之类的空消息
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(src, buf); err != nil {
			return
		}
		if _, err := dst.Write(buf); err != nil {
			return
		}
		t.mu.Lock()
		sink.observe(buf)
		t.mu.Unlock()
	}
}

func (t *tap) write(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "conns=%d\n", t.conns)
	for _, d := range []struct {
		name string
		s    *dirStats
	}{{"c2s", t.c2s}, {"s2c", t.s2c}} {
		fmt.Fprintf(&b, "%s_frames=%d\n", d.name, d.s.frames)
		fmt.Fprintf(&b, "%s_plain=%d\n", d.name, d.s.plain)
		fmt.Fprintf(&b, "%s_transform=%d\n", d.name, d.s.transform)
		fmt.Fprintf(&b, "%s_smb1=%d\n", d.name, d.s.smb1)
		fmt.Fprintf(&b, "%s_other=%d\n", d.name, d.s.other)
		fmt.Fprintf(&b, "%s_plain_cmds=%s\n", d.name, joinCmds(d.s.sortedPlainCmds()))
		fmt.Fprintf(&b, "%s_plain_postauth_cmds=%s\n", d.name, joinCmds(d.s.postAuthPlainCmds()))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
