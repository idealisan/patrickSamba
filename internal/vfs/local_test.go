package vfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestFS 建一个临时共享。
func newTestFS(t *testing.T, readOnly bool) *LocalFS {
	t.Helper()
	root := t.TempDir()
	fs, err := NewLocalFS(LocalConfig{
		Root:            root,
		ReadOnly:        readOnly,
		CaseInsensitive: true,
	})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func writeFile(t *testing.T, fs *LocalFS, rel, content string) {
	t.Helper()
	p := filepath.Join(fs.Root(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- Disposition

func TestOpenDispositions(t *testing.T) {
	// 覆盖 MS-SMB2 §2.2.13 的六种 CreateDisposition × 存在/不存在。
	cases := []struct {
		name       string
		disp       Disposition
		preCreate  bool
		wantAction Action
		wantErr    error
	}{
		{"Supersede/存在", Supersede, true, ActionSuperseded, nil},
		{"Supersede/不存在", Supersede, false, ActionCreated, nil},
		{"Open/存在", OpenExisting, true, ActionOpened, nil},
		{"Open/不存在", OpenExisting, false, 0, ErrNotFound},
		{"Create/存在", CreateNew, true, 0, ErrExist},
		{"Create/不存在", CreateNew, false, ActionCreated, nil},
		{"OpenIf/存在", OpenAlways, true, ActionOpened, nil},
		{"OpenIf/不存在", OpenAlways, false, ActionCreated, nil},
		{"Overwrite/存在", TruncateExisting, true, ActionOverwritten, nil},
		{"Overwrite/不存在", TruncateExisting, false, 0, ErrNotFound},
		{"OverwriteIf/存在", TruncateAlways, true, ActionOverwritten, nil},
		{"OverwriteIf/不存在", TruncateAlways, false, ActionCreated, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newTestFS(t, false)
			if c.preCreate {
				writeFile(t, fs, "f.txt", "old content")
			}
			h, action, err := fs.Open(&OpenRequest{
				Path:        "f.txt",
				Flags:       OpenRead | OpenWrite,
				Disposition: c.disp,
			})
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer h.Close()
			if action != c.wantAction {
				t.Errorf("action = %d, want %d", action, c.wantAction)
			}

			// 截断类 disposition 必须真的把内容清掉。
			a, err := h.Stat()
			if err != nil {
				t.Fatal(err)
			}
			truncating := c.disp == Supersede || c.disp == TruncateExisting || c.disp == TruncateAlways
			if truncating && a.Size != 0 {
				t.Errorf("%v 应当截断文件，size = %d", c.disp, a.Size)
			}
			if !truncating && c.preCreate && a.Size == 0 {
				t.Errorf("%v 不应截断文件", c.disp)
			}
		})
	}
}

func TestOpenDirectory(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "dir/inner.txt", "x")

	// 打开已存在的目录
	h, action, err := fs.Open(&OpenRequest{
		Path: "dir", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("打开目录: %v", err)
	}
	if action != ActionOpened {
		t.Errorf("action = %d, want Opened", action)
	}
	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeDirectory == 0 {
		t.Error("目录应带 FILE_ATTRIBUTE_DIRECTORY")
	}
	h.Close()

	// 创建目录
	h2, action, err := fs.Open(&OpenRequest{
		Path: "newdir", Flags: OpenDirectory, Disposition: CreateNew,
	})
	if err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	h2.Close()
	if action != ActionCreated {
		t.Errorf("action = %d, want Created", action)
	}
	if fi, err := os.Stat(filepath.Join(fs.Root(), "newdir")); err != nil || !fi.IsDir() {
		t.Error("newdir 应当被创建成目录")
	}

	// FILE_NON_DIRECTORY_FILE 遇到目录 → ErrIsDir
	if _, _, err := fs.Open(&OpenRequest{
		Path: "dir", Flags: OpenRead | OpenNonDirectory, Disposition: OpenExisting,
	}); !errors.Is(err, ErrIsDir) {
		t.Errorf("NonDirectory 打开目录应返回 ErrIsDir，得到 %v", err)
	}

	// FILE_DIRECTORY_FILE 遇到文件 → ErrNotDir
	if _, _, err := fs.Open(&OpenRequest{
		Path: "dir/inner.txt", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotDir) {
		t.Errorf("Directory 打开文件应返回 ErrNotDir，得到 %v", err)
	}
}

func TestOpenShareRoot(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{Path: "", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("打开共享根: %v", err)
	}
	defer h.Close()
	if _, err := h.ReadDir("*", false, 0); err != nil && err != io.EOF {
		t.Fatalf("枚举共享根: %v", err)
	}
}

// ---------------------------------------------------------------- 读写

func TestReadWrite(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{
		Path: "data.bin", Flags: OpenRead | OpenWrite, Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	payload := []byte("hello smb")
	n, err := h.WriteAt(payload, 0)
	if err != nil || n != len(payload) {
		t.Fatalf("WriteAt = %d, %v", n, err)
	}

	buf := make([]byte, len(payload))
	if n, err := h.ReadAt(buf, 0); err != nil || n != len(payload) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if string(buf) != string(payload) {
		t.Errorf("读回 %q，want %q", buf, payload)
	}

	// 读到文件尾应返回 io.EOF（Handle 契约）
	big := make([]byte, len(payload)+10)
	if _, err := h.ReadAt(big, 0); err != io.EOF {
		t.Errorf("跨越 EOF 的读应返回 io.EOF，得到 %v", err)
	}

	// 稀疏写：偏移之后写入，中间是空洞
	if _, err := h.WriteAt([]byte("tail"), 4096); err != nil {
		t.Fatal(err)
	}
	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != 4100 {
		t.Errorf("size = %d, want 4100", a.Size)
	}

	if err := h.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.Stat(); a.Size != 4 {
		t.Errorf("截断后 size = %d, want 4", a.Size)
	}

	if err := h.Sync(true); err != nil {
		t.Errorf("Sync(full): %v", err)
	}
}

// TestReadWriteBounds 是安全用例：越界/溢出的 offset 必须被拒绝而不是崩溃。
func TestReadWriteBounds(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{
		Path: "f", Flags: OpenRead | OpenWrite, Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	buf := make([]byte, 16)
	for _, off := range []int64{-1, -4096, maxInt64, maxInt64 - 8} {
		if _, err := h.ReadAt(buf, off); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("ReadAt(off=%d) = %v, want ErrInvalidArg", off, err)
		}
		if _, err := h.WriteAt(buf, off); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("WriteAt(off=%d) = %v, want ErrInvalidArg", off, err)
		}
	}
	if err := h.Truncate(-1); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("Truncate(-1) = %v, want ErrInvalidArg", err)
	}
}

func TestWriteWithoutWriteAccess(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "ro.txt", "content")
	h, _, err := fs.Open(&OpenRequest{
		Path: "ro.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, ErrPermission) {
		t.Errorf("只申请读权限的句柄写入应被拒，得到 %v", err)
	}
}

// ---------------------------------------------------------------- 只读共享

func TestReadOnlyShare(t *testing.T) {
	fs := newTestFS(t, true)
	writeFile(t, fs, "f.txt", "content")

	if !fs.ReadOnly() {
		t.Fatal("ReadOnly() 应为 true")
	}

	// 读可以
	h, _, err := fs.Open(&OpenRequest{Path: "f.txt", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("只读共享上的读打开失败: %v", err)
	}
	if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读共享写入应返回 ErrReadOnly，得到 %v", err)
	}
	h.Close()

	// 一切写意图都要在 Open 入口就被拒
	writes := []OpenRequest{
		{Path: "f.txt", Flags: OpenRead | OpenWrite, Disposition: OpenExisting},
		{Path: "new.txt", Disposition: CreateNew},
		{Path: "f.txt", Disposition: TruncateExisting},
		{Path: "f.txt", Flags: OpenDeleteOnClose, Disposition: OpenExisting},
	}
	for _, req := range writes {
		r := req
		if _, _, err := fs.Open(&r); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Open(%+v) = %v, want ErrReadOnly", r, err)
		}
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Remove", fs.Remove("f.txt")},
		{"Mkdir", fs.Mkdir("d", 0)},
		{"Rename", fs.Rename("f.txt", "g.txt", false)},
	} {
		if !errors.Is(tc.err, ErrReadOnly) {
			t.Errorf("%s 在只读共享上应返回 ErrReadOnly，得到 %v", tc.name, tc.err)
		}
	}
}

// ---------------------------------------------------------------- 路径安全

// TestPathTraversal 是**安全底线用例**（AGENTS.md §8）。
// 任何一条通过都意味着共享外的文件可被访问。
func TestPathTraversal(t *testing.T) {
	fs := newTestFS(t, false)

	// 在共享根**之外**放一个秘密文件，作为逃逸成功与否的探针。
	outside := filepath.Join(filepath.Dir(fs.Root()), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	vectors := []string{
		"../secret.txt",
		`..\secret.txt`,
		"../../etc/passwd",
		"a/../../secret.txt",
		"/etc/passwd",
		`\\server\share`,
		`C:\Windows\System32\config\SAM`,
		"..",
		"../",
		"dir//../../secret.txt",
		"./../secret.txt",
		"a/b/../../../secret.txt",
		strings.Repeat("a", MaxComponentLen+1),
		strings.Repeat("a/", MaxPathLen),
		"bad\x00name",
		"con",
		"AUX.txt",
		"na|me",
		"na*me",
	}
	for _, v := range vectors {
		if _, _, err := fs.Open(&OpenRequest{Path: v, Flags: OpenRead, Disposition: OpenExisting}); err == nil {
			t.Errorf("路径穿越向量 %q 竟然打开成功", v)
		}
		if _, err := fs.Stat(v); err == nil {
			t.Errorf("路径穿越向量 %q 的 Stat 竟然成功", v)
		}
		// 写路径同样不能逃逸
		if _, _, err := fs.Open(&OpenRequest{Path: v, Flags: OpenWrite, Disposition: OpenAlways}); err == nil {
			t.Errorf("路径穿越向量 %q 竟然创建成功", v)
		}
	}

	// 确认探针文件没被改动
	if b, err := os.ReadFile(outside); err != nil || string(b) != "TOP SECRET" {
		t.Fatalf("共享外的文件被改动了: %q %v", b, err)
	}
}

// TestColonPathCreatesBaseNotEscape："na:me" 是**合法**的
// 「文件 na + 流 me」语法（SplitStreamPath 在 Resolve 之前剥掉流名，
// macOS 客户端就是这么发扩展属性的）。
//
// 基础文件不存在时，创建性打开会按 Samba 语义建出基础文件再开流
// （open.c:6508 "We may be creating the basefile as part of creating
// the stream"）。曾经它进上面的穿越向量表 —— 那时能通过靠的是 F2 缺陷
// （基础文件不存在一律 NOT_FOUND），不是真的有防御。这里钉住真正的
// 安全断言：不逃逸。共享根里只允许出现文件 "na"，绝不能出现名为
// "na:me" 的条目。
func TestColonPathCreatesBaseNotEscape(t *testing.T) {
	fs := newTestFS(t, false)

	h, action, err := fs.Open(&OpenRequest{Path: "na:me", Flags: OpenWrite, Disposition: OpenAlways})
	if err != nil {
		t.Fatalf("创建 na:me（文件 na + 流 me）不应失败: %v", err)
	}
	if action != ActionCreated {
		t.Errorf("action = %d, 期望 ActionCreated", action)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(fs.Root(), "na")); err != nil {
		t.Errorf("基础文件 na 应被创建: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fs.Root(), "na:me")); err == nil {
		t.Errorf("共享根里出现了名为 na:me 的条目 —— 冒号没有被当成流分隔符")
	}
}

func TestSymlinkEscape(t *testing.T) {
	fs := newTestFS(t, false)
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 共享内放一个指向共享外的软链
	link := filepath.Join(fs.Root(), "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("此环境不支持创建符号链接: %v", err)
	}
	if _, _, err := fs.Open(&OpenRequest{Path: "escape", Flags: OpenRead, Disposition: OpenExisting}); !errors.Is(err, ErrPermission) {
		t.Errorf("指向共享外的软链应被拒（ErrPermission），得到 %v", err)
	}

	// 共享**内**的软链应当可以正常跟随
	writeFile(t, fs, "real.txt", "inside")
	if err := os.Symlink(filepath.Join(fs.Root(), "real.txt"), filepath.Join(fs.Root(), "inside-link")); err != nil {
		t.Fatal(err)
	}
	h, _, err := fs.Open(&OpenRequest{Path: "inside-link", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("共享内的软链应可打开: %v", err)
	}
	defer h.Close()
	buf := make([]byte, 6)
	if _, err := h.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(buf) != "inside" {
		t.Errorf("通过软链读到 %q，want %q", buf, "inside")
	}
}

func TestCaseInsensitiveLookup(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "Report.TXT", "data")

	for _, name := range []string{"Report.TXT", "report.txt", "REPORT.TXT", "rEpOrT.tXt"} {
		if _, err := fs.Stat(name); err != nil {
			t.Errorf("大小写不敏感查找 %q 失败: %v", name, err)
		}
	}

	// 关掉大小写不敏感之后只能精确匹配 —— 但这条只在**宿主也大小写敏感**时
	// 成立：折叠宿主（APFS/NTFS）上内核自己就把 "report.txt" 认成 "Report.TXT"，
	// 本层的 CaseInsensitive=false 拦不住它。按宿主能力走两条分支。
	strict, err := NewLocalFS(LocalConfig{Root: fs.Root(), CaseInsensitive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer strict.Close()
	_, statErr := strict.Stat("report.txt")
	if hostFoldsCase(t) {
		if statErr != nil {
			t.Errorf("宿主折叠大小写，严格模式也应命中同一个对象，得到 %v", statErr)
		}
	} else if !errors.Is(statErr, ErrNotFound) {
		t.Errorf("大小写敏感模式下 %q 应当找不到，得到 %v", "report.txt", statErr)
	}
}

// ---------------------------------------------------------------- 目录枚举

func TestReadDir(t *testing.T) {
	fs := newTestFS(t, false)
	for _, n := range []string{"a.txt", "b.txt", "c.log", ".hidden"} {
		writeFile(t, fs, n, n)
	}
	if err := os.Mkdir(filepath.Join(fs.Root(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	h, _, err := fs.Open(&OpenRequest{Path: "", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	names := readAllDir(t, h, "*", 0)
	// Windows 期待 "." 与 ".."（docs/protocol-notes.md §9）
	for _, want := range []string{".", "..", "a.txt", "b.txt", "c.log", ".hidden", "sub"} {
		if !contains(names, want) {
			t.Errorf("枚举结果缺少 %q: %v", want, names)
		}
	}

	// 通配符过滤
	txt := readAllDir(t, h, "*.txt", 0)
	for _, n := range txt {
		if !strings.HasSuffix(strings.ToLower(n), ".txt") {
			t.Errorf("*.txt 匹配到了 %q", n)
		}
	}
	if !contains(txt, "a.txt") || !contains(txt, "b.txt") {
		t.Errorf("*.txt 应匹配 a.txt/b.txt，得到 %v", txt)
	}

	// 点开头的文件应带 HIDDEN
	for _, e := range readAllEntries(t, h, ".hidden", 0) {
		if e.Name == ".hidden" && e.Attr.FileAttributes&FileAttributeHidden == 0 {
			t.Error(".hidden 应带 FILE_ATTRIBUTE_HIDDEN")
		}
	}

	// 分批拉取：max 生效且不重不漏
	if _, err := h.ReadDir("*", true, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	seen := map[string]int{}
	total := 0
	for i := 0; ; i++ {
		batch, err := h.ReadDir("*", i == 0, 2)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) > 2 {
			t.Fatalf("max=2 却返回了 %d 条", len(batch))
		}
		for _, e := range batch {
			seen[e.Name]++
			total++
		}
		if i > 100 {
			t.Fatal("分批枚举没有收敛")
		}
	}
	if total != len(names) {
		t.Errorf("分批枚举得到 %d 条，一次性枚举得到 %d 条", total, len(names))
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("%q 在分批枚举里出现了 %d 次", n, c)
		}
	}
}

func TestReadDirOnFileFails(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.txt", "x")
	h, _, err := fs.Open(&OpenRequest{Path: "f.txt", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadDir("*", false, 0); !errors.Is(err, ErrNotDir) {
		t.Errorf("对文件句柄 ReadDir 应返回 ErrNotDir，得到 %v", err)
	}
}

// TestReadDirDotDotAtRoot 确认共享根的 ".." 不会指向共享外。
func TestReadDirDotDotAtRoot(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{Path: "", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	rootAttr, err := fs.Stat("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range readAllEntries(t, h, "*", 0) {
		if e.Name == ".." {
			if e.Attr.FileID != rootAttr.FileID {
				t.Error("共享根的 \"..\" 必须指向自己，不能泄漏上级目录")
			}
		}
	}
}

func readAllEntries(t *testing.T, h Handle, pattern string, max int) []DirEntry {
	t.Helper()
	out, err := h.ReadDir(pattern, true, max)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return out
}

func readAllDir(t *testing.T, h Handle, pattern string, max int) []string {
	t.Helper()
	var names []string
	for _, e := range readAllEntries(t, h, pattern, max) {
		names = append(names, e.Name)
	}
	return names
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 路径级操作

func TestRemoveMkdirRename(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "a.txt", "a")
	writeFile(t, fs, "b.txt", "b")

	if err := fs.Mkdir("d", 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("d", 0); !errors.Is(err, ErrExist) {
		t.Errorf("重复 Mkdir 应返回 ErrExist，得到 %v", err)
	}

	if err := fs.Rename("a.txt", "d/moved.txt", false); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := fs.Stat("a.txt"); !errors.Is(err, ErrNotFound) {
		t.Error("源文件应当已不存在")
	}
	if _, err := fs.Stat("d/moved.txt"); err != nil {
		t.Errorf("目标文件应当存在: %v", err)
	}

	// replace=false 撞上已存在的目标
	if err := fs.Rename("b.txt", "d/moved.txt", false); !errors.Is(err, ErrExist) {
		t.Errorf("replace=false 覆盖应返回 ErrExist，得到 %v", err)
	}
	// replace=true 允许覆盖
	if err := fs.Rename("b.txt", "d/moved.txt", true); err != nil {
		t.Errorf("replace=true 应当允许覆盖: %v", err)
	}

	// 非空目录不能删
	if err := fs.Remove("d"); !errors.Is(err, ErrNotEmpty) && !errors.Is(err, ErrPermission) {
		t.Errorf("删除非空目录应返回 ErrNotEmpty，得到 %v", err)
	}
	if err := fs.Remove("d/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("d"); err != nil {
		t.Fatalf("删除空目录: %v", err)
	}
	if err := fs.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除不存在的对象应返回 ErrNotFound，得到 %v", err)
	}
}

// TestRenameCaseOnly 覆盖「只改大小写」这个容易被大小写不敏感查找吃掉的场景。
func TestRenameCaseOnly(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "readme.txt", "x")
	if err := fs.Rename("readme.txt", "README.TXT", false); err != nil {
		t.Fatalf("改大小写的重命名失败: %v", err)
	}
	entries, err := os.ReadDir(fs.Root())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !contains(got, "README.TXT") {
		t.Errorf("磁盘上应当是 README.TXT，实际 %v", got)
	}
}

func TestDeleteOnClose(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "tmp.txt", "x")

	h, _, err := fs.Open(&OpenRequest{
		Path: "tmp.txt", Flags: OpenRead | OpenDeleteOnClose, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat("tmp.txt"); !errors.Is(err, ErrNotFound) {
		t.Error("DELETE_ON_CLOSE 的文件应在 Close 后消失")
	}

	// 事后通过 DeleteOnCloser 设置（SET_INFO FileDispositionInformation）
	writeFile(t, fs, "tmp2.txt", "x")
	h2, _, err := fs.Open(&OpenRequest{Path: "tmp2.txt", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := h2.(DeleteOnCloser)
	if !ok {
		t.Fatal("localHandle 应当实现 DeleteOnCloser")
	}
	if err := d.SetDeleteOnClose(true); err != nil {
		t.Fatal(err)
	}
	h2.Close()
	if _, err := fs.Stat("tmp2.txt"); !errors.Is(err, ErrNotFound) {
		t.Error("SetDeleteOnClose 后 Close 应删除文件")
	}
}

func TestDoubleCloseIsSafe(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")
	h, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("重复 Close 应当是幂等的，得到 %v", err)
	}
}

// ---------------------------------------------------------------- 属性 / 卷

func TestStatFS(t *testing.T) {
	fs := newTestFS(t, false)
	info, err := fs.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	if info.BlockSize == 0 {
		t.Error("BlockSize 不能为 0（客户端会除零）")
	}
	if info.TotalBlocks == 0 {
		t.Error("TotalBlocks 不应为 0")
	}
	if info.VolumeSerial == 0 {
		t.Error("VolumeSerial 不能为 0")
	}
	if info.CaseSensitive {
		t.Error("应向客户端宣称大小写不敏感")
	}
	if info.MaxComponentLen == 0 {
		t.Error("MaxComponentLen 不应为 0")
	}

	// 卷序列号必须跨实例稳定
	fs2, err := NewLocalFS(LocalConfig{Root: fs.Root()})
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	info2, err := fs2.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	if info.VolumeSerial != info2.VolumeSerial {
		t.Error("同一共享根的卷序列号必须稳定，否则客户端缓存会失效")
	}
}

func TestAttrNeverZero(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "plain.txt", "x")
	a, err := fs.Stat("plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes == 0 {
		t.Error("FileAttributes 不能为 0（docs/protocol-notes.md §11）")
	}
	if a.NLink == 0 {
		t.Error("NLink 至少为 1")
	}
	if a.WriteTime.IsZero() {
		t.Error("WriteTime 不应为零值")
	}
}

func TestSetAttr(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Flags: OpenRead | OpenWrite, Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("0123456789"), 0); err != nil {
		t.Fatal(err)
	}

	// EndOfFile
	if err := h.SetAttr(&Attr{Size: 4}, AttrSize); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.Stat(); a.Size != 4 {
		t.Errorf("AttrSize 未生效，size = %d", a.Size)
	}

	// 时间
	want := timeFromFiletime(t, 133000000000000000)
	if err := h.SetAttr(&Attr{WriteTime: want}, AttrWriteTime); err != nil {
		t.Fatal(err)
	}
	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !a.WriteTime.Equal(want) {
		t.Errorf("WriteTime = %v, want %v", a.WriteTime, want)
	}

	// READONLY 属性位
	if err := h.SetAttr(&Attr{FileAttributes: FileAttributeReadonly}, AttrFileAttributes); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.Stat(); a.FileAttributes&FileAttributeReadonly == 0 {
		t.Error("READONLY 属性未生效")
	}
	if err := h.SetAttr(&Attr{FileAttributes: FileAttributeArchive}, AttrFileAttributes); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.Stat(); a.FileAttributes&FileAttributeReadonly != 0 {
		t.Error("取消 READONLY 未生效")
	}

	// 不支持的属性必须**静默忽略**而不是报错，否则 Explorer 复制会失败
	if err := h.SetAttr(&Attr{
		CreateTime:     want,
		ChangeTime:     want,
		FileAttributes: FileAttributeHidden | FileAttributeSystem,
	}, AttrCreateTime|AttrChangeTime|AttrFileAttributes); err != nil {
		t.Errorf("无法表达的属性应被忽略而不是报错: %v", err)
	}
}

func timeFromFiletime(t *testing.T, ft uint64) time.Time {
	t.Helper()
	return FiletimeToTime(ft)
}

func TestStreams(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.txt", "hello")
	streams, err := fs.Streams("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].Name != "::$DATA" {
		t.Errorf("普通文件应报告主数据流，得到 %+v", streams)
	}
	if streams[0].Size != 5 {
		t.Errorf("主数据流大小 = %d, want 5", streams[0].Size)
	}

	// 还没写过资源派生时，打开它应报「不存在」而不是打开主数据流 ——
	// 后者会让客户端把资源派生的内容写进文件本体，造成数据损坏。
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Stream: StreamAFPResource, Flags: OpenRead, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的资源派生应返回 ErrNotFound，得到 %v", err)
	}

	// 通用 ADS 现在**是支持的**（落 user.DosStream.* xattr，见 stream_xattr.go）。
	// 没写过的通用流打开时应报「不存在」，而不是 ErrNotSupported ——
	// 后者会让客户端以为整个共享不支持 ADS 而放弃使用。
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Stream: "SomeOtherStream", Flags: OpenRead, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的通用 ADS 应返回 ErrNotFound，得到 %v", err)
	}
}

func TestAttrOnlyHandle(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.txt", "hello")
	h, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Flags: OpenRead | OpenAttrOnly, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	a, err := h.Stat()
	if err != nil {
		t.Fatalf("属性句柄应能查属性: %v", err)
	}
	if a.Size != 5 {
		t.Errorf("size = %d, want 5", a.Size)
	}
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrNotSupported) {
		t.Errorf("属性句柄不应能读数据，得到 %v", err)
	}
}
