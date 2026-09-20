package builtin

// helper_test.go —— 测试脚手架。
//
// 一条纪律：**旁路库不落在共享根里**。测试用两个独立的 TempDir，
// 一个当共享根、一个放库文件 —— 这既贴合生产配置，也让「列目录时会不会
// 看见库文件」这类问题在测试里就暴露出来。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

type env struct {
	t    *testing.T
	root string
	meta string
	set  oscap.Set
}

// newEnv 建一套可读写的 builtin 实现。
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:    t,
		root: t.TempDir(),
		meta: filepath.Join(t.TempDir(), "oscap.db"),
	}
	e.open(false)
	return e
}

func (e *env) open(readOnly bool) {
	e.t.Helper()
	set, err := New(oscap.Options{Root: e.root, MetadataPath: e.meta, ReadOnly: readOnly})
	if err != nil {
		e.t.Fatalf("New(readOnly=%v) 失败: %v", readOnly, err)
	}
	e.set = set
	e.t.Cleanup(func() {
		if set.Close != nil {
			_ = set.Close()
		}
	})
}

// reopen 关掉当前实现再按新的只读标志重开，共用同一个库文件。
//
// 只读路径必须这样测：直接建一个只读实现会得到空库，
// 那样「读得到旧数据、写一律被拒」这条真正要验的性质根本走不到。
func (e *env) reopen(readOnly bool) {
	e.t.Helper()
	if e.set.Close != nil {
		if err := e.set.Close(); err != nil {
			e.t.Fatalf("关闭旁路存储失败: %v", err)
		}
	}
	e.open(readOnly)
}

// file 在共享根下建一个文件并返回它的 Ref。
func (e *env) file(name string, content []byte) oscap.Ref {
	e.t.Helper()
	p := filepath.Join(e.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		e.t.Fatalf("写文件失败: %v", err)
	}
	return oscap.Ref{Path: p}
}

func (e *env) read(ref oscap.Ref) []byte {
	e.t.Helper()
	b, err := os.ReadFile(ref.Path)
	if err != nil {
		e.t.Fatalf("读文件失败: %v", err)
	}
	return b
}

// mkfile 在 root 下建一个 2 块大小的测试文件，供不走 env 的用例使用。
func mkfile(t *testing.T, root, name string) oscap.Ref {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, bytesRepeat(0xAA, 2*zeroBlockSize), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return oscap.Ref{Path: p}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
