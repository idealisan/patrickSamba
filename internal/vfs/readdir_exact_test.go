package vfs

// readdir_exact_test.go —— QUERY_DIRECTORY 精确名字（无通配符）的快速路径。
//
// 背景：Time Machine 的 .sparsebundle/bands/ 有十万级条目，而 macOS 反复
// 做的是「某个 band 在不在」这种**带精确文件名**的 QUERY_DIRECTORY。
// localHandle.lookupExactLocked 为此加了一条不拍快照的直查路径
// （Samba dptr_ReadDirName() 同款优化）。
//
// 快速路径最容易出的三类 bug，这里逐一钉住：
//  1. 枚举状态：吐完唯一那条之后必须 NO_MORE_FILES，不能无限重复；
//  2. 语义漂移：大小写、"._" 旁路文件、"."/".."、路径穿越必须与
//     完整快照路径给出**完全一致**的结果；
//  3. 附带信息丢失：不拍快照就没有 dotUnder 集合，
//     AAPL readdir_attr 的资源派生大小不能因此变成 0。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readDirOnce 取一批，把 io.EOF 归一化成「空批次」。
func readDirOnce(t *testing.T, h Handle, pattern string, restart bool) []DirEntry {
	t.Helper()
	b, err := h.ReadDir(pattern, restart, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadDir(%q, restart=%v): %v", pattern, restart, err)
	}
	return b
}

func TestReadDirExactNameSingleEntryThenEOF(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "bands/0", "a")
	writeFile(t, fs, "bands/1", "bb")
	writeFile(t, fs, "bands/2", "ccc")

	h, _ := dirHandle(t, fs, "bands")

	got := readDirOnce(t, h, "1", false)
	if len(got) != 1 || got[0].Name != "1" {
		t.Fatalf("精确查 \"1\" 得到 %v，期望恰好一条 \"1\"", names(got))
	}
	if got[0].Attr.Size != 2 {
		t.Errorf("属性没填对：Size=%d，期望 2", got[0].Attr.Size)
	}

	// 第二次必须结束 —— 快速路径没有快照可继续，
	// 忘了这一步的话客户端会陷入死循环。
	if b, err := h.ReadDir("1", false, 0); !errors.Is(err, io.EOF) || len(b) != 0 {
		t.Fatalf("第二次 ReadDir = (%v, %v)，期望 (空, io.EOF)", names(b), err)
	}
	// 第三次也一样，不能因为状态被清掉又活过来。
	if b, err := h.ReadDir("1", false, 0); !errors.Is(err, io.EOF) || len(b) != 0 {
		t.Fatalf("第三次 ReadDir = (%v, %v)，期望 (空, io.EOF)", names(b), err)
	}
}

func TestReadDirExactNameMissFallsBackToScan(t *testing.T) {
	fs := newTestFS(t, false)
	// 宿主机（Linux/ext4）大小写敏感，但 SMB 的名字比较是大小写不敏感的。
	// 精确 stat 会 miss，必须退回完整扫描才能匹配上。
	writeFile(t, fs, "bands/BandFile", "x")

	h, _ := dirHandle(t, fs, "bands")
	got := readDirOnce(t, h, "bandfile", false)
	if len(got) != 1 || got[0].Name != "BandFile" {
		t.Fatalf("大小写不敏感查询得到 %v，期望 [BandFile]", names(got))
	}

	// 真的不存在时：一条都不返回（上层据此回 STATUS_NO_SUCH_FILE）。
	h2, _ := dirHandle(t, fs, "bands")
	if b := readDirOnce(t, h2, "nope", false); len(b) != 0 {
		t.Fatalf("查不存在的名字得到 %v，期望空", names(b))
	}
}

func TestReadDirExactNameSameAsWildcardScan(t *testing.T) {
	// 快速路径与完整扫描必须给出一致的结果集，逐个名字对拍。
	fs := newTestFS(t, false)
	writeFile(t, fs, "d/plain", "1")
	writeFile(t, fs, "d/UPPER", "22")
	writeFile(t, fs, "d/名字", "333")
	writeFile(t, fs, "d/with space", "4444")
	if err := os.Mkdir(filepath.Join(fs.Root(), "d", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// AppleDouble 旁路文件：完整扫描会把它过滤掉（它是 AFP_Resource 的
	// 载体，不是独立条目），快速路径也必须一视同仁。
	writeFile(t, fs, "d/._plain", "rsrc")

	// 完整扫描的基准结果。
	hAll, _ := dirHandle(t, fs, "d")
	want := map[string]bool{}
	for _, e := range readDirOnce(t, hAll, "*", false) {
		want[e.Name] = true
	}
	for _, n := range []string{".", "..", "plain", "UPPER", "名字", "with space", "sub"} {
		if !want[n] {
			t.Fatalf("基准扫描里缺 %q，测试前提不成立：%v", n, want)
		}
	}
	if want["._plain"] {
		t.Fatal("基准扫描不应列出 ._plain")
	}

	for _, name := range []string{
		".", "..", "plain", "UPPER", "名字", "with space", "sub",
		"._plain", "nosuch", "PLAIN", "upper",
	} {
		h, _ := dirHandle(t, fs, "d")
		got := readDirOnce(t, h, name, false)

		// 期望值直接从基准集合推导：大小写不敏感地找一遍。
		wantN := ""
		for n := range want {
			if MatchDOS(name, n) {
				wantN = n
			}
		}
		switch {
		case wantN == "" && len(got) != 0:
			t.Errorf("精确查 %q 得到 %v，期望空", name, names(got))
		case wantN != "" && (len(got) != 1 || got[0].Name != wantN):
			t.Errorf("精确查 %q 得到 %v，期望 [%s]", name, names(got), wantN)
		}
	}
}

// TestReadDirExactNameNoTraversal：快速路径拿的是客户端给的模式串，
// 必须挡住 "../x" 这类名字，否则精确查询就成了目录穿越原语。
func TestReadDirExactNameNoTraversal(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "secret", "s")
	writeFile(t, fs, "d/inside", "i")

	h, _ := dirHandle(t, fs, "d")
	for _, bad := range []string{"../secret", "..\\secret", "/secret", "d/inside"} {
		got := readDirOnce(t, h, bad, true)
		if len(got) != 0 {
			t.Errorf("模式 %q 返回了 %v，必须一条都不返回", bad, names(got))
		}
	}
}

// TestReadDirExactThenRestartWildcard：先精确查、再 RESTART_SCANS 换成 "*"，
// 必须能拿到完整目录 —— 快速路径不能把句柄的枚举状态钉死。
func TestReadDirExactThenRestartWildcard(t *testing.T) {
	fs := newTestFS(t, false)
	for i := 0; i < 10; i++ {
		writeFile(t, fs, fmt.Sprintf("bands/%x", i), "x")
	}

	h, _ := dirHandle(t, fs, "bands")
	if got := readDirOnce(t, h, "3", false); len(got) != 1 {
		t.Fatalf("精确查得到 %v", names(got))
	}

	total := 0
	first := true
	for {
		batch, err := h.ReadDir("*", first, 4)
		first = false
		total += len(batch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
	}
	if total != 12 { // 10 个 band + "." + ".."
		t.Fatalf("restart 后全量枚举到 %d 条，期望 12", total)
	}
}

// TestReadDirExactKeepsResourceForkInfo：快速路径不拍快照，
// dotUnder 集合是空的。AppleInfoAt 必须据此**回退到探测**，
// 而不是把「集合里没有」当成「没有资源派生」——
// 否则 Finder 会看到图标与资源派生凭空消失。
func TestReadDirExactKeepsResourceForkInfo(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "d/doc", "data")

	hr, _ := openStreamH(t, fs, "d/doc", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := hr.WriteAt(make([]byte, 512), 0); err != nil {
		t.Fatal(err)
	}
	if err := hr.Close(); err != nil {
		t.Fatal(err)
	}

	h, dm := dirHandle(t, fs, "d")
	if got := readDirOnce(t, h, "doc", false); len(got) != 1 {
		t.Fatalf("精确查 doc 得到 %v", names(got))
	}
	_, rsrc, err := dm.AppleInfoAt("doc")
	if err != nil {
		t.Fatalf("AppleInfoAt: %v", err)
	}
	if rsrc != 512 {
		t.Errorf("走快速路径后资源派生大小 = %d，期望 512", rsrc)
	}
}

func names(es []DirEntry) []string {
	out := make([]string, len(es))
	for i := range es {
		out[i] = es[i].Name
	}
	return out
}

// BenchmarkReadDirExactName 量的就是被优化掉的那个开销：
// 在 5 万条目的 bands 目录上查单个名字。
//
// 优化前（每次都 readdirnames + sort 五万条）实测约 19 ms/次，
// 优化后是一次 lstat。
func BenchmarkReadDirExactName(b *testing.B) {
	if testing.Short() {
		b.Skip("建 5 万个文件较慢，-short 下跳过")
	}
	root := buildBandDir(b, 50_000)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, _, err := fs.Open(&OpenRequest{
			Path: "bands", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
		})
		if err != nil {
			b.Fatal(err)
		}
		got, err := h.ReadDir(fmt.Sprintf("%x", i%50_000), false, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			b.Fatal(err)
		}
		if len(got) != 1 {
			b.Fatalf("查到 %d 条，期望 1", len(got))
		}
		_ = h.Close()
	}
}
