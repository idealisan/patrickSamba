package command

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件钉的是 v0.5 读路径去双拷贝（perf-v050 报告 §4/§8 移交项）：
//
//	旧路径：handleRead 每次 make([]byte, Length) 把文件读进临时缓冲，
//	        再由 wire.ReadResponse.Append 整体二次拷进响应缓冲
//	        （heap profile 各占 34%/32%，是当时最大的剩余分配源）。
//	新路径：wire.ReserveReadResponse 把数据窗口开在响应缓冲里，
//	        ReadAt 一步写进最终位置。
//
// 两件事必须有机器证明：
//  1. 线上字节序列逐字节不变（下方 legacy 对照）；
//  2. 每次响应的分配次数下降（AllocsPerRun 对照）。

// legacyReadResponse 是旧路径的忠实复刻（临时缓冲 + Append 二次拷贝），
// 只用于本文件内的对照；产品代码里已无这条路径。
func legacyReadResponse(t *testing.T, open *Open, off uint64, length, minimumCount uint32) []byte {
	t.Helper()
	buf := make([]byte, length)
	n, rerr := open.Handle.ReadAt(buf, int64(off))
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		t.Fatalf("legacy ReadAt: %v", rerr)
	}
	if n == 0 && length != 0 {
		t.Fatal("legacy 路径此处应回 END_OF_FILE（用例设计错误）")
	}
	if minimumCount > 0 && uint32(n) < minimumCount {
		t.Fatal("legacy 路径此处应回 END_OF_FILE（用例设计错误）")
	}
	out, err := (&wire.ReadResponse{Data: buf[:n]}).Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("legacy Append: %v", err)
	}
	return out
}

// runNewRead 构造一条 READ 请求喂给新 handleRead，返回完整响应
// （64 字节头 + 报文体）；status 错误原样交还给调用方断言。
func runNewRead(t *testing.T, p *lockIOPair, open *Open, off uint64, length, minimumCount uint32) ([]byte, error) {
	t.Helper()
	req := &wire.ReadRequest{
		Offset:       off,
		Length:       length,
		MinimumCount: minimumCount,
		FileID:       compoundFID,
	}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码 READ Request: %v", err)
	}
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  p.tree,
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	if err := handleRead(ctx); err != nil {
		return nil, err
	}
	return ctx.Out, nil
}

// TestHandleReadBytesIdenticalToLegacy：满读、部分 EOF 读、零长度读、
// MinimumCount 边界 —— 新路径输出必须与旧路径逐字节一致；
// 失败的面（EOF / MINCOUNT 不足）钉「失败且不留半截响应」。
func TestHandleReadBytesIdenticalToLegacy(t *testing.T) {
	content := bytes.Repeat([]byte("samba-read-path"), 4096) // 60 KiB
	p := newLockIOPair(t, content)

	cases := []struct {
		name         string
		off          uint64
		length       uint32
		minimumCount uint32
		wantErr      bool
	}{
		{"整块读", 0, 4096, 0, false},
		{"单字节读", 5, 1, 0, false},
		{"大块读", 0, 1 << 16, 0, false},
		{"部分EOF", uint64(len(content)) - 10, 100, 0, false},
		{"零长度", 0, 0, 0, false},
		{"零长度mincount不足", 0, 0, 1, true},
		{"mincount恰好满足", 0, 1024, 1024, false},
		{"越界EOF", uint64(len(content)) + 8, 16, 0, true},
		{"mincount不足", 0, 1024, 2048, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runNewRead(t, p, p.a, tc.off, tc.length, tc.minimumCount)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应失败（END_OF_FILE 面），实际成功且 out=%d 字节", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("handleRead: %v", err)
			}
			want := legacyReadResponse(t, p.a, tc.off, tc.length, tc.minimumCount)
			if !bytes.Equal(got, want) {
				t.Fatalf("字节序列不一致：new=%d 字节 legacy=%d 字节\nnew   =% x\nlegacy=% x",
					len(got), len(want), got[:min(len(got), 96)], want[:min(len(want), 96)])
			}
		})
	}
}

// benchPair 是基准用的最小环境（testing.B 没有 TempDir，用 MkdirTemp；
// 目录由 b.Cleanup 回收，不属于工作树清理操作）。
type benchPair struct {
	tree *Tree
	open *Open
}

func newBenchPair(b *testing.B, size int) *benchPair {
	b.Helper()
	root, err := os.MkdirTemp("", "cmdio-bench-")
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.WriteFile(filepath.Join(root, "f"), bytes.Repeat([]byte("z"), size), 0o644); err != nil {
		b.Fatalf("准备文件: %v", err)
	}
	fs, ferr := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if ferr != nil {
		b.Fatalf("NewLocalFS: %v", ferr)
	}
	b.Cleanup(func() { _ = fs.Close() })
	h, _, oerr := fs.Open(&vfs.OpenRequest{
		Path:        "f",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenExisting,
	})
	if oerr != nil {
		b.Fatalf("vfs Open: %v", oerr)
	}
	tree := &Tree{ID: 1, Share: &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}}
	open := &Open{Path: "f", Tree: tree, Handle: h,
		GrantedAccess: wire.FileReadData | wire.FileWriteData}
	return &benchPair{tree: tree, open: open}
}

func benchReadCtx(bp *benchPair, length uint32) *Context {
	req := &wire.ReadRequest{Offset: 0, Length: length, FileID: compoundFID}
	msg, _ := req.Append(make([]byte, wire.HeaderSize))
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  bp.tree,
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = bp.open
	return ctx
}

// sinkOut 防止编译器把基准工作优化掉。
var sinkOut []byte

// BenchmarkCommandHandleReadOldPath：旧路径（临时缓冲 + Append 二次拷贝）。
// 产品代码里已无该路径，这里是本文件内的对照实现；脚手架（构造请求与
// Context、解析请求）与新路径基准完全一致，差值只来自组装段。
//
//	go test ./internal/smb/command/ -bench BenchmarkCommandHandleRead -run '^$' -benchmem
func BenchmarkCommandHandleReadOldPath(b *testing.B) {
	bp := newBenchPair(b, 1<<20)
	const length = 1 << 20
	b.SetBytes(length)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := benchReadCtx(bp, length)
		if _, err := wire.ParseReadRequest(ctx.Msg); err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, length) // 旧路径第一步：临时缓冲
		n, rerr := bp.open.Handle.ReadAt(buf, 0)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			b.Fatal(rerr)
		}
		out, aerr := (&wire.ReadResponse{Data: buf[:n]}).Append(ctx.Out)
		if aerr != nil {
			b.Fatal(aerr)
		}
		sinkOut = out
	}
}

// BenchmarkCommandHandleReadNewPath：新路径（Reserve + ReadAt 直写 + Commit）。
func BenchmarkCommandHandleReadNewPath(b *testing.B) {
	bp := newBenchPair(b, 1<<20)
	const length = 1 << 20
	b.SetBytes(length)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := benchReadCtx(bp, length)
		if err := handleRead(ctx); err != nil {
			b.Fatal(err)
		}
		sinkOut = ctx.Out
	}
}

// TestHandleReadFewerAllocationsThanLegacy：AllocsPerRun 硬断言 ——
// 同一脚手架（构造请求与 Context、解析请求）下，「旧响应组装」与
// 「新响应组装」的每次分配数必须严格下降。两侧唯一差异就是
// handleRead 的读入+组装段，差值即优化收益。
func TestHandleReadFewerAllocationsThanLegacy(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("x"), 1<<20))
	const length = 1 << 20

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	// 预热：让懒初始化落在计数窗口之外。
	if _, err := runNewRead(t, p, p.a, 0, length, 0); err != nil {
		t.Fatalf("预热: %v", err)
	}

	bp := &benchPair{tree: p.tree, open: p.a}

	newAllocs := testing.AllocsPerRun(20, func() {
		ctx := benchReadCtx(bp, length)
		if err := handleRead(ctx); err != nil {
			t.Fatal(err)
		}
		sinkOut = ctx.Out
	})

	legacyAllocs := testing.AllocsPerRun(20, func() {
		ctx := benchReadCtx(bp, length)
		// 新路径含请求解析；对照侧支付同样的解析成本。
		if _, err := wire.ParseReadRequest(ctx.Msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, length) // 旧路径第一步：临时缓冲
		n, rerr := bp.open.Handle.ReadAt(buf, 0)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			t.Fatal(rerr)
		}
		out, aerr := (&wire.ReadResponse{Data: buf[:n]}).Append(ctx.Out)
		if aerr != nil {
			t.Fatal(aerr)
		}
		sinkOut = out
	})

	t.Logf("allocs/op：旧组装=%.1f 新组装=%.1f", legacyAllocs, newAllocs)
	if newAllocs >= legacyAllocs {
		t.Fatalf("新路径 allocs/op=%v 未低于旧路径 %v", newAllocs, legacyAllocs)
	}
}
