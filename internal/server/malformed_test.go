package server

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// newFullTestConn 构造一条**装配完整**的连接：真实的 NTLM 认证后端、
// 一个落在 t.TempDir() 上的磁盘共享、IPC$ 管道共享，以及一个真实的 Server。
//
// 与 newTestConn 的区别：后者的 Settings 只有 Logger，Auth/Shares/srv 全是 nil，
// 拿它跑畸形报文只会撞上测试脚手架自己的 nil，掩盖真正的问题。
func newFullTestConn(t *testing.T) *Connection {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := auth.NewStaticStore(config.Auth{
		AllowGuest: true,
		Users:      []config.User{{Name: "alice", Password: "secret"}},
	}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造账户库失败: %v", err)
	}
	provider := auth.NewNTLMProvider(auth.Options{
		Store:          store,
		ServerName:     "TESTSRV",
		DomainName:     "WORKGROUP",
		AllowAnonymous: true,
	})

	fs, err := vfs.NewLocalFS(vfs.LocalConfig{
		Root:            t.TempDir(),
		CaseInsensitive: true,
		VolumeLabel:     "data",
	})
	if err != nil {
		t.Fatalf("构造 VFS 失败: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	settings := &command.Settings{
		ServerName:         "TESTSRV",
		Domain:             "WORKGROUP",
		StartTime:          time.Now(),
		MinDialect:         dialect.SMB202,
		MaxDialect:         dialect.SMB311,
		EncryptionEnabled:  true,
		AllowSMB1Negotiate: true,
		AllowGuest:         true,
		Auth:               provider,
		Shares: []*command.Share{
			{Name: "data", Type: wire.ShareTypeDisk, FS: fs, GuestOK: true, Browseable: true},
			{Name: command.IPCShareName, Type: wire.ShareTypePipe, GuestOK: true},
		},
		Logger: log,
	}

	srv, err := New(Options{Settings: settings, Logger: log})
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}

	return &Connection{
		srv:     srv,
		log:     log,
		state:   command.NewConn(settings, "test", "test"),
		credits: NewCredits(0),
	}
}

const captureRoot = "../smb/wire/testdata/capture"

// captureFrames 读取 internal/smb/wire/testdata/capture 下所有 c2s 报文，
// 作为变异测试的种子。这些是真实抓包（AGENTS.md §3：字节向量不要凭空编造）。
func captureFrames(t *testing.T) [][]byte {
	t.Helper()

	var out [][]byte
	err := filepath.Walk(captureRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(p) != ".bin" {
			return nil
		}
		// 只要客户端发给服务端的方向。
		if !strings.Contains(filepath.Base(p), "-c2s-") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if len(b) > 0 {
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历抓包目录失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没有找到任何 c2s 抓包，变异测试没有种子")
	}
	return out
}

// mutations 对一个种子报文生成一批畸形变体：
// 截断、长度字段撒谎、字段填 0xFF（超大 count / 越界 offset）、字段清零。
func mutations(seed []byte) [][]byte {
	var out [][]byte
	add := func(b []byte) {
		if len(b) > 0 {
			out = append(out, b)
		}
	}

	// 1) 各种长度的截断（含超短帧：1 字节、4 字节、63 字节 —— 头都不完整）。
	for _, n := range []int{1, 2, 3, 4, 8, 16, 31, 32, 63, 64, 65, 70, 100} {
		if n < len(seed) {
			add(append([]byte(nil), seed[:n]...))
		}
	}
	add(append([]byte(nil), seed[:len(seed)-1]...))

	// 2) 逐字节把关键区域（SMB2 头 + body 前 32 字节）翻成 0xFF / 0x00 / 0x80。
	//    覆盖 NextCommand、StructureSize、各种 offset/length 字段。
	limit := min(len(seed), 96)
	for i := range limit {
		for _, v := range []byte{0xFF, 0x00, 0x80} {
			if seed[i] == v {
				continue
			}
			m := append([]byte(nil), seed...)
			m[i] = v
			add(m)
		}
	}

	// 3) 32 位字段整体撒谎：把每个 4 字节对齐位置写成 0xFFFFFFFF /
	//    0x7FFFFFFF / 0x80000000，模拟"超大 count / 负数 offset"。
	for off := 0; off+4 <= min(len(seed), 160); off += 4 {
		for _, v := range [][]byte{
			{0xFF, 0xFF, 0xFF, 0xFF},
			{0xFF, 0xFF, 0xFF, 0x7F},
			{0x00, 0x00, 0x00, 0x80},
		} {
			m := append([]byte(nil), seed...)
			copy(m[off:], v)
			add(m)
		}
	}

	return out
}

// feedNoPanic 把一个畸形帧喂给连接，捕获 panic 并作为测试失败报出。
func feedNoPanic(t *testing.T, c *Connection, what string, frame []byte) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s 触发 panic: %v\n首 64 字节: % x\n%s",
				what, r, frame[:min(64, len(frame))], debug.Stack())
		}
	}()
	_, _ = c.handleFrame(frame)
}

// TestMalformedFramesNoPanic 把真实抓包的变异体喂给帧处理入口，
// 要求：**永远不 panic**。返回错误（断连）是可以接受的结果，
// 崩掉整个进程不是（AGENTS.md §5 编码规范：外部输入解析失败一律返回错误）。
func TestMalformedFramesNoPanic(t *testing.T) {
	seeds := captureFrames(t)
	c := newFullTestConn(t)

	n := 0
	for si, seed := range seeds {
		for mi, m := range mutations(seed) {
			feedNoPanic(t, c, fmt.Sprintf("协商前 种子 #%d 变体 #%d", si, mi), m)
			n++
		}
	}
	t.Logf("已喂入 %d 个畸形帧变体", n)
}

// TestMalformedFramesAfterSessionNoPanic 与上一个测试相同，但先让连接完成
// NEGOTIATE 并建立一个**已认证**会话 —— 认证后的代码路径（句柄解析、
// 读写、目录枚举）与认证前完全不同，是攻击面最大的部分。
func TestMalformedFramesAfterSessionNoPanic(t *testing.T) {
	seeds := captureFrames(t)

	c := newFullTestConn(t)
	establishSession(t, c, false)

	n := 0
	for si, seed := range seeds {
		for mi, m := range mutations(seed) {
			feedNoPanic(t, c, fmt.Sprintf("已认证 种子 #%d 变体 #%d", si, mi), m)
			n++
		}
	}
	t.Logf("已喂入 %d 个畸形帧变体（已认证会话）", n)
}

// FuzzHandleFrame 是上面两个表驱动测试的模糊版本，用抓包做语料。
// CI 只跑种子语料（go test 会执行 f.Add 的全部输入）；
// 需要真正模糊时用 `go test -fuzz=FuzzHandleFrame ./internal/server/`。
func FuzzHandleFrame(f *testing.F) {
	_ = filepath.Walk(captureRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(p) != ".bin" {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && len(b) > 0 {
			f.Add(b)
		}
		return nil
	})
	f.Add([]byte{0xFE, 'S', 'M', 'B'})
	f.Add([]byte{0xFF, 'S', 'M', 'B'})
	f.Add([]byte{0xFD, 'S', 'M', 'B'})

	f.Fuzz(func(t *testing.T, frame []byte) {
		if len(frame) == 0 {
			// 传输层保证不会有 0 长度帧（ErrEmptyFrame）。
			return
		}
		c := newFullTestConn(t)
		_, _ = c.handleFrame(frame)
	})
}
