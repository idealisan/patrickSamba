package vfs

// redirect_test.go —— 「链接逃逸」两道闸门的覆盖测试。
//
// # 起因：一个存活下来的变异体
//
// 给链接判据换平台钩子时，我用 `go test -overlay` 注入了一个把
// hostIsRedirect 恒置 false 的变异体，结果 **internal/vfs 全量测试依然全绿**。
// 也就是说这两道闸门当时**一行测试都没真正覆盖到**。
//
// 原有的 TestSymlinkEscape 用的是末级软链，走的是 EvalFinal（local.go:212）
// 那道闸门；而 resolveComponents 的逐级检查当时没有任何用例。
//
// # 两道闸门的分工
//
//	resolveComponents → checkSymlink   逐级下降，管中间分量
//	EvalFinal                          只看最后一跳，管 open 前的 TOCTOU
//
// # 一个必须说清楚的事实：中间分量在 POSIX 上本来就有第二重兜底
//
// 把 hostIsRedirect 关掉之后，中间分量的软链**并不会**造成 POSIX 上的逃逸：
// Lstat 一个指向目录的软链，IsDir() 是 false（实测确认），于是落进
// resolveComponents 的 `!last && !fi.IsDir()` 分支，以 ErrNotDir 被拒。
// 拒是拒了，但**理由是错的**，而且是巧合而非设计。
//
// 所以下面这几条中间分量用例，钉的是「以正确的理由（ErrPermission）拒绝」
// 以及「这道闸门确实接线了」，**不是**「否则就会逃逸」。别把它们的作用说大。
//
// # 真正的 Windows 逃逸在末级，不在中间
//
// junction 因为带 name surrogate 位，Go 连 ModeDir 都不给设
// （`src/os/types_windows.go:193`），所以中间分量的 junction 同样会撞上
// 那条 ErrNotDir 兜底。**末级**才是洞：
//
//	resolveComponents  last=true，ErrNotDir 那条分支被 !last 短路，放行
//	EvalFinal          junction 不带 ModeSymlink → 原样返回，不做包含性校验
//	os.OpenFile        真实路径遍历时跟随 junction → 拿到共享外的目录句柄
//
// 也就是说 **EvalFinal 才是这次必须修的那一道**，只改 resolveComponents
// 是修不好的。两处都换成平台钩子的原因即在此。
//
// 这条链路本身在 Linux 上验不了（造不出 junction），
// 只有判定规则在 winreparse_test.go 里有表驱动测试。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSymlinkEscapeViaIntermediateComponent 覆盖 resolveComponents 那道闸门。
//
// 布局：
//
//	<share>/gate        → 软链，指向共享外的目录
//	<outside>/secret.txt
//
// 客户端请求 "gate/secret.txt"：末级 secret.txt **本身不是软链**，
// 所以 EvalFinal 什么也发现不了；唯一能拦住它的是逐级下降时对 gate 的检查。
func TestSymlinkEscapeViaIntermediateComponent(t *testing.T) {
	fs := newTestFS(t, false)

	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := filepath.Join(fs.Root(), "gate")
	if err := os.Symlink(outsideDir, gate); err != nil {
		t.Skipf("此环境不支持创建符号链接: %v", err)
	}

	// 先确认前提成立：末级确实不是软链，因此 EvalFinal 这道闸门在本用例里
	// 是"沉默"的。前提不成立的话本用例就验不到想验的东西，必须立刻发现。
	if fi, err := os.Lstat(filepath.Join(gate, "secret.txt")); err != nil {
		t.Fatalf("准备阶段：%v", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("准备阶段：末级不应是软链，否则本用例验的是 EvalFinal 而非逐级检查")
	}

	_, _, err := fs.Open(&OpenRequest{
		Path: "gate/secret.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if !errors.Is(err, ErrPermission) {
		t.Errorf("经由中间分量软链访问共享外文件应被拒（ErrPermission），得到 %v", err)
	}
}

// TestSymlinkIntermediateInsideStillWorks 是上一条的反面对照。
//
// 只验"拒绝"不验"放行"是不够的：把 hostIsRedirect 写成恒 true 也能让
// 上一条通过，但那会让共享内的软链目录整个不可用。
func TestSymlinkIntermediateInsideStillWorks(t *testing.T) {
	fs := newTestFS(t, false)

	realDir := filepath.Join(fs.Root(), "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "ok.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(fs.Root(), "dirlink")); err != nil {
		t.Skipf("此环境不支持创建符号链接: %v", err)
	}

	h, _, err := fs.Open(&OpenRequest{
		Path: "dirlink/ok.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("经由共享内软链目录访问应当成功: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 6)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "inside" {
		t.Errorf("读到 %q，want %q", buf, "inside")
	}
}

// TestSymlinkEscapeDeepIntermediate 把软链埋得更深一层，确认逐级下降
// 不是只检查第一级。
func TestSymlinkEscapeDeepIntermediate(t *testing.T) {
	fs := newTestFS(t, false)

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	sub := filepath.Join(fs.Root(), "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(sub, "gate")); err != nil {
		t.Skipf("此环境不支持创建符号链接: %v", err)
	}

	_, _, err := fs.Open(&OpenRequest{
		Path: "a/b/gate/secret.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if !errors.Is(err, ErrPermission) {
		t.Errorf("深层中间分量软链逃逸应被拒，得到 %v", err)
	}
}
