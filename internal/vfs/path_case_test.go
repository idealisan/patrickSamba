package vfs

// path_case_test.go —— 大小写不敏感回退查找的**语义**回归。
//
// 性能侧在 path_perf_test.go，这里只管「折叠出来的名字对不对」。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustResolver 造一个 Resolver，并返回**它实际使用的根**。
//
// 不能直接返回 t.TempDir() 的原值：NewResolver 会对 root 做 EvalSymlinks，
// 而 macOS 上 /var 是指向 /private/var 的软链，两者字面不等 —— 拿原值比对
// 会把「路径解析正确」误判成失败。
func mustResolver(t *testing.T, caseInsensitive bool) (*Resolver, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewResolver(root, caseInsensitive)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r, r.root
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("建 %s: %v", path, err)
	}
}

// TestResolveParentPrefersExactCase 是精确匹配优先的**语义**证据。
//
// 目录里同时存在 "a.txt" 与 "A.TXT" 时，客户端指名哪个就得到哪个。
// 旧实现无条件调 lookupCaseInsensitive，返回的是 readdir 顺序里第一个
// EqualFold 命中 —— 也就是说结果取决于目录项的物理顺序，可能把客户端
// 明确指名的 "A.TXT" 折叠成 "a.txt"，操作打到错误的文件上。
func TestResolveParentPrefersExactCase(t *testing.T) {
	r, root := mustResolver(t, true)
	touch(t, filepath.Join(root, "a.txt"))
	touch(t, filepath.Join(root, "A.TXT"))

	for _, want := range []string{"a.txt", "A.TXT"} {
		dir, name, err := r.ResolveParent(want)
		if err != nil {
			t.Fatalf("ResolveParent(%q): %v", want, err)
		}
		if dir != root {
			t.Errorf("ResolveParent(%q) dir = %q, want %q", want, dir, root)
		}
		if name != want {
			t.Errorf("ResolveParent(%q) name = %q，精确存在的名字被折叠成了别的对象", want, name)
		}
	}
}

// TestResolveParentFoldsOnMiss 是**反向对照**：精确匹配 miss 时回退必须仍然生效。
//
// 没有这一条，上面那个测试可以靠「把回退整个删掉」作弊通过。
func TestResolveParentFoldsOnMiss(t *testing.T) {
	r, root := mustResolver(t, true)
	touch(t, filepath.Join(root, "report.txt"))

	_, name, err := r.ResolveParent("REPORT.TXT")
	if err != nil {
		t.Fatalf("ResolveParent: %v", err)
	}
	// 两条分支都必须落在磁盘上那个对象上：
	//   大小写敏感宿主 —— 精确 stat 落空，回退折叠，拿回磁盘写法 "report.txt"；
	//   折叠宿主（APFS/NTFS）—— 精确 stat 直接命中，拿回客户端请求的写法。
	// 后者是宿主特性不是缺陷，但仍要求它与磁盘上的名字是同一个（忽略大小写）。
	if !strings.EqualFold(name, "report.txt") {
		t.Fatalf("name = %q，与磁盘上的 %q 不是同一个名字", name, "report.txt")
	}
	if !hostFoldsCase(t) && name != "report.txt" {
		t.Errorf("name = %q，want %q —— 大小写回退没折叠回磁盘写法", name, "report.txt")
	}
}

// TestResolveParentMissingNameKeptVerbatim：名字在任何大小写下都不存在时，
// 必须原样返回，供调用方拿去创建。
func TestResolveParentMissingNameKeptVerbatim(t *testing.T) {
	r, root := mustResolver(t, true)
	touch(t, filepath.Join(root, "other.txt"))

	dir, name, err := r.ResolveParent("NewBand-01")
	if err != nil {
		t.Fatalf("ResolveParent: %v", err)
	}
	if dir != root || name != "NewBand-01" {
		t.Errorf("(%q, %q), want (%q, %q)", dir, name, root, "NewBand-01")
	}
}

// TestResolveParentCaseSensitiveResolver：关掉 caseInsensitive 时不折叠。
func TestResolveParentCaseSensitiveResolver(t *testing.T) {
	r, root := mustResolver(t, false)
	touch(t, filepath.Join(root, "report.txt"))

	_, name, err := r.ResolveParent("REPORT.TXT")
	if err != nil {
		t.Fatalf("ResolveParent: %v", err)
	}
	if name != "REPORT.TXT" {
		t.Errorf("name = %q，大小写敏感的 Resolver 不该折叠", name)
	}
}

// TestResolveExactCasePreferred 覆盖 resolveComponents 那条路径（Open/Create 用）。
func TestResolveExactCasePreferred(t *testing.T) {
	r, root := mustResolver(t, true)
	if err := os.Mkdir(filepath.Join(root, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(root, "Sub", "b.dat"))
	touch(t, filepath.Join(root, "Sub", "B.DAT"))

	for _, want := range []string{"b.dat", "B.DAT"} {
		host, err := r.Resolve("Sub/" + want)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", want, err)
		}
		if filepath.Base(host) != want {
			t.Errorf("Resolve(Sub/%s) = %q，精确存在的名字被折叠了", want, host)
		}
	}
}
