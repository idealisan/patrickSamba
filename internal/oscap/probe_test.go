package oscap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProbeNativeNeverPanicsAndLeavesNoTrace 是探测的两条硬保证。
//
// 「不留痕迹」这一条是可证伪的：探测会在共享根下建临时文件
// （probeFilePrefix），漏删的话客户端会在共享里看到一堆垃圾。
// 这里在探测前后对目录做一次逐项比对，比"看起来没问题"可靠。
func TestProbeNativeNeverPanicsAndLeavesNoTrace(t *testing.T) {
	root := t.TempDir()
	// 放一个既有文件，确认探测不会把它动掉。
	victim := filepath.Join(root, "keepme")
	if err := os.WriteFile(victim, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range Capabilities() {
		// 只要求不 panic；结果取决于构建机的文件系统，不做取值断言
		// （拿环境当断言等于把测试交给运气）。
		_ = ProbeNative(c, Options{Root: root})
	}

	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "keepme" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("探测留下了垃圾文件: %v（只应剩 keepme）", names)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "hello" {
		t.Errorf("探测改动了既有文件: %q, err=%v", got, err)
	}
}

// TestProbeNativeRejectsBadInput 是负向对照。
func TestProbeNativeRejectsBadInput(t *testing.T) {
	root := t.TempDir()

	if ProbeNative(Capability(-1), Options{Root: root}) {
		t.Error("非法 Capability 应返回 false")
	}
	if ProbeNative(capCount, Options{Root: root}) {
		t.Error("越界 Capability 应返回 false")
	}
	if ProbeNative(CapXattr, Options{}) {
		t.Error("Root 为空应返回 false")
	}
	// 不存在的目录：探测必须如实报 false，而不是猜一个乐观答案。
	missing := filepath.Join(root, "no-such-dir")
	for _, c := range Capabilities() {
		if ProbeNative(c, Options{Root: missing}) {
			t.Errorf("不存在的 Root 上 %s 却报支持", c)
		}
	}
}

// TestProbeIsSelfConsistent 用「探测说支持，那就必须真的能用」做交叉验证。
//
// 这条比断言具体取值有价值得多：它在**任何**文件系统上都成立，
// 而且能抓到最危险的那类错误 —— 假阳性（探测报 true、adapter 运行时才炸，
// 在 filesystem_mode: native 下表现为启动通过、干活时失败）。
func TestProbeIsSelfConsistent(t *testing.T) {
	root := t.TempDir()

	if ProbeNative(CapStableFileID, Options{Root: root}) {
		// 说支持稳定 ID，那至少 stat 得出来，且两次一致。
		a, err1 := fileIDForTest(root)
		b, err2 := fileIDForTest(root)
		if err1 != nil || err2 != nil {
			t.Errorf("探测报支持 stable_file_id，实际取不到: %v / %v", err1, err2)
		} else if a != b {
			t.Errorf("stable_file_id 两次取值不同: %d != %d", a, b)
		} else if a == 0 {
			t.Error("stable_file_id 取到 0 —— 那是「未知」的哨兵，不能当 ID 用")
		}
	}

	// POSIX 上命名流承载在扩展属性之上，两项探测结果必须一致
	// （见 ports.go 的 NamedStream 注释与 probe_linux.go 的实现）。
	// 这条耦合一旦被谁改断，native 命名流会在没有 xattr 的机器上炸。
	if runtimeIsPOSIX() {
		x := ProbeNative(CapXattr, Options{Root: root})
		s := ProbeNative(CapNamedStream, Options{Root: root})
		if x != s {
			t.Errorf("POSIX 上 named_stream 应与 xattr 同进退，实际 xattr=%v named_stream=%v", x, s)
		}
	}
}

// TestProbeFilePrefixIsIdentifiable：残留文件必须一眼看得出是谁留下的。
//
// 进程在探测中途被杀是会发生的（本项目的开发容器就经常崩）。
// 一个叫 tmp123456 的残留文件谁也不知道该不该删。
func TestProbeFilePrefixIsIdentifiable(t *testing.T) {
	if !strings.HasPrefix(probeFilePrefix, ".") {
		t.Errorf("探测临时文件前缀 %q 应以点开头（POSIX 下默认隐藏、SMB 侧映射为 HIDDEN）", probeFilePrefix)
	}
	if !strings.Contains(probeFilePrefix, "stupidsamba") {
		t.Errorf("探测临时文件前缀 %q 应带项目名，残留时才认得出来", probeFilePrefix)
	}
}

// TestWithProbeFileCleansUpOnFalse 确认返回 false 的那条路径也会清理。
func TestWithProbeFileCleansUpOnFalse(t *testing.T) {
	root := t.TempDir()
	var seen string

	if withProbeFile(root, func(f *os.File) bool {
		seen = f.Name()
		return false
	}) {
		t.Fatal("fn 返回 false，withProbeFile 却返回 true")
	}
	if seen == "" {
		t.Fatal("fn 没有被调用")
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("探测文件 %s 没被删掉: err=%v", seen, err)
	}
}

// TestWithProbeFileUnwritableRoot：建不出临时文件时必须返回 false，
// 而不是猜一个乐观答案。
func TestWithProbeFileUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "definitely-not-here")
	called := false
	if withProbeFile(root, func(*os.File) bool { called = true; return true }) {
		t.Error("根目录不可写时应返回 false")
	}
	if called {
		t.Error("临时文件都没建成，不该调用 fn")
	}
}
