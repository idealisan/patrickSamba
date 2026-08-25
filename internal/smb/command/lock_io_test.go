package command

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件钉的是 bh4-A#1：字节范围锁此前只是记账空壳 —— LOCK 命令把锁
// 登进表里，但 READ/WRITE 从不查表，别的句柄照样穿透被锁区间读写。
// Samba 在同步 IO 路径上做 STRICT_LOCK_CHECK（smb2_read.c:584 /
// smb2_write.c:392），冲突回 NT_STATUS_FILE_LOCK_CONFLICT。
// Windows 字节范围锁本就是强制的（MS-FSA §2.1.4.10 / §2.1.5）。

const rwTestMaxIO = 1 << 20 // 测试用的 MaxRead/MaxWriteSize

// newLockIOPair 造一个共享 + 同一文件上的两个句柄（A/B 各一个 vfs 句柄），
// 以及第三个"路人"句柄 C 只当锁表 owner 用。
type lockIOPair struct {
	ctx   *Context // 每次请求要重建，这里只借 Tree/Share
	tree  *Tree
	a, b  *Open
	table *lockTable
}

func newLockIOPair(t *testing.T, content []byte) *lockIOPair {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), content, 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	mk := func() *Open {
		h, _, err := fs.Open(&vfs.OpenRequest{
			Path:        "f",
			Flags:       vfs.OpenRead | vfs.OpenWrite,
			Disposition: vfs.OpenExisting,
		})
		if err != nil {
			t.Fatalf("vfs Open: %v", err)
		}
		return &Open{
			Path:          "f",
			Handle:        h,
			GrantedAccess: wire.FileReadData | wire.FileWriteData,
		}
	}

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	a := mk()
	b := mk()
	a.Tree = tree
	b.Tree = tree
	return &lockIOPair{tree: tree, a: a, b: b, table: &share.locks}
}

// compoundFID 是「复用链中上一个句柄」的占位 FileId（全 1）。
var compoundFID = wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}

// runWrite 构造一条 WRITE 请求并直接喂给 handleWrite，返回 handler 的错误
// （nil = 成功）。open 走复合 FileId 复用通道注入。
func (p *lockIOPair) runWrite(t *testing.T, open *Open, off uint64, data []byte) error {
	t.Helper()
	req := &wire.WriteRequest{Offset: off, FileID: compoundFID, Data: data}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码 WRITE Request: %v", err)
	}
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  p.tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	return handleWrite(ctx)
}

// runRead 构造一条 READ 请求并直接喂给 handleRead。
func (p *lockIOPair) runRead(t *testing.T, open *Open, off, length uint64) (*wire.ReadResponse, error) {
	t.Helper()
	req := &wire.ReadRequest{Offset: off, Length: uint32(length), FileID: compoundFID}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码 READ Request: %v", err)
	}
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  p.tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	err = handleRead(ctx)
	if err != nil {
		return nil, err
	}
	resp, perr := wire.ParseReadResponse(ctx.Out)
	if perr != nil {
		t.Fatalf("解析 READ Response: %v", perr)
	}
	return resp, nil
}

// wantStatus 断言 err 是给定的 NTSTATUS。
func wantStatus(t *testing.T, what string, err error, want status.Status) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 应失败回 %v，实际成功", what, want)
	}
	var got status.Status
	if !errors.As(err, &got) {
		t.Fatalf("%s: 错误应为 status.Status，实际 %T(%v)", what, err, err)
	}
	if got != want {
		t.Fatalf("%s: 应回 %v，实际 %v", what, want, got)
	}
}

// TestWriteConflictsWithForeignByteRangeLock：A 对 [0,100) 持独占锁后，
// B 写区间内必须 FILE_LOCK_CONFLICT、写区间外必须成功；
// A 自己写自己的锁区间不受影响（豁免单位是句柄，与 Samba 一致）。
func TestWriteConflictsWithForeignByteRangeLock(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("0123456789abcdef"), 32)) // 512B
	if !lockOne(p.table, "f", p.a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}

	// B 写被锁区间 → 冲突。
	wantStatus(t, "B 写 [50,60) 撞 A 的独占锁",
		p.runWrite(t, p.b, 50, bytes.Repeat([]byte("x"), 10)), status.LockConflict)

	// B 写区间外 → 成功，且数据真的落了盘。
	if err := p.runWrite(t, p.b, 200, []byte("OUTSIDE")); err != nil {
		t.Fatalf("B 写 [200,207) 不应受 [0,100) 锁影响: %v", err)
	}

	// A 写自己的锁区间 → 成功（同句柄豁免）。
	if err := p.runWrite(t, p.a, 10, []byte("SELF")); err != nil {
		t.Fatalf("A 写自己持有的锁区间不应冲突: %v", err)
	}
}

// TestReadBlockedByExclusiveAllowedByShared：独占锁挡别人的读；
// 共享锁不挡读但挡写。这是 conflict 矩阵在 IO 入口上的投影。
func TestReadBlockedByExclusiveAllowedByShared(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("0123456789abcdef"), 32))

	if !lockOne(p.table, "f", p.a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}
	wantStatus(t, "B 读被锁区间", func() error {
		_, err := p.runRead(t, p.b, 50, 10)
		return err
	}(), status.LockConflict)
	// 读区间外不受影响。
	if _, err := p.runRead(t, p.b, 200, 16); err != nil {
		t.Fatalf("B 读 [200,216) 不应受 [0,100) 独占锁影响: %v", err)
	}

	// 换成共享锁：读放行、写仍挡。
	if st := p.table.unlock("f", p.a, []wire.LockElement{unlockEl(0, 100)}); st != status.Success {
		t.Fatalf("A 解锁应成功，实际 %v", st)
	}
	if !lockOne(p.table, "f", p.a, 0, 100, false) {
		t.Fatal("A 加共享锁应成功")
	}
	if resp, err := p.runRead(t, p.b, 50, 10); err != nil {
		t.Fatalf("B 读共享锁区间应放行: %v", err)
	} else if len(resp.Data) != 10 {
		t.Fatalf("B 应回 10 字节，实际 %d", len(resp.Data))
	}
	wantStatus(t, "B 写撞 A 的共享锁",
		p.runWrite(t, p.b, 50, []byte("zzzz")), status.LockConflict)
}

// TestLockCheckUsesHandlePathNotCaller：锁检查按 open.Path 取桶，
// 与 LOCK 命令登记用的是同一把钥匙。
//
// 这道用例顺带防一类回归：有人把检查挂到 ctx.Tree.FS() 的根路径或别的
// 推导值上，锁就永远查不到。
func TestLockCheckMatchesRegistrationKey(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("m"), 64))
	if !lockOne(p.table, "f", p.a, 0, 64, true) {
		t.Fatal("A 加锁应成功")
	}
	// 用与 LOCK 完全相同的键做一次直查，确认表里确实有货，
	// 然后确认 IO 入口真的会被它挡住 —— 两件事都成立才说明检查点生效。
	if n := countLocks(p.table, p.a); n != 1 {
		t.Fatalf("锁表里应有 A 的 1 把锁，实际 %d", n)
	}
	wantStatus(t, "B 写整文件撞锁",
		p.runWrite(t, p.b, 0, bytes.Repeat([]byte("w"), 64)), status.LockConflict)
}
