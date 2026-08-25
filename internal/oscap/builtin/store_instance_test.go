package builtin

// store_instance_test.go —— 默认旁路库文件名的 InstanceID 推导（OI-1）。
//
// 被钉死的契约：
//  1. 空 InstanceID ⇒ 历史文件名逐字节不变（测试与库直连场景零变化）；
//  2. 不同 InstanceID ⇒ 不同文件名，且不含任何文件名非法字符；
//  3. 同一 InstanceID ⇒ 同一文件名（重启复用元数据的前提）；
//  4. 默认落点仍在共享根**之外**；
//  5. 显式 MetadataPath 优先于实例区分（用户手写落点 = 用户负责唯一性）。

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// TestMetadataFileNameEmptyInstanceIsHistoricalName 用测试内**独立复算**的
// fnv64a 公式对照空实例的文件名 —— 改动推导逻辑时若不小心动了历史分支，
// 这里会先红。
func TestMetadataFileNameEmptyInstanceIsHistoricalName(t *testing.T) {
	root := filepath.Join("srv", "data")
	h := fnv.New64a()
	_, _ = h.Write([]byte(normalizePath(filepath.Clean(root))))
	want := fmt.Sprintf(".stupidsamba-oscap-%016x.db", h.Sum64())
	if got := metadataFileName(root, ""); got != want {
		t.Fatalf("空 InstanceID 必须回退历史文件名: got %q, want %q", got, want)
	}
}

func TestMetadataFileNameDistinguishesInstances(t *testing.T) {
	root := filepath.Join("srv", "data")
	base := metadataFileName(root, "")
	if !strings.Contains(base, ".stupidsamba-oscap-") {
		t.Fatalf("基准名形态不对: %q", base)
	}

	ids := []string{
		"127.0.0.1:4451",
		"127.0.0.1:4452",
		"192.168.7.9:4451",
		"[::1]:4451",
		"a:b", // 与下一个清洗后同标签，靠原始值哈希区分
		"a_b", //
		":::", // 全非法字符
		"host with space:1",
	}
	seen := map[string]string{}
	for _, id := range ids {
		n := metadataFileName(root, id)
		if n == base {
			t.Errorf("实例 %q 没有被编进文件名", id)
		}
		if prev, dup := seen[n]; dup {
			t.Errorf("实例 %q 与 %q 得到同一个文件名 %q", id, prev, n)
		}
		if strings.ContainsAny(n, ":/\\ ") {
			t.Errorf("文件名 %q 含非法字符（来自实例 %q）", n, id)
		}
		if again := metadataFileName(root, id); again != n {
			t.Errorf("实例 %q 推导不确定: %q vs %q", id, n, again)
		}
		seen[n] = id
	}
}

// TestDefaultMetadataPathWithInstanceStaysOutsideRoot：带实例的默认落点
// 与空实例落在**同一个目录**（兄弟位置），只是文件名不同 —— 运维按目录
// 巡检的习惯不被打破；且依旧不得落进共享根。
func TestDefaultMetadataPathWithInstanceStaysOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	pEmpty, err := defaultMetadataPath(root, "")
	if err != nil {
		t.Fatalf("空实例推导失败: %v", err)
	}
	pInst, err := defaultMetadataPath(root, "127.0.0.1:4455")
	if err != nil {
		t.Fatalf("带实例推导失败: %v", err)
	}
	if filepath.Dir(pInst) != filepath.Dir(pEmpty) {
		t.Errorf("带实例的默认落点换了目录: %q vs %q", pInst, pEmpty)
	}
	if insideRoot(root, pInst) {
		t.Errorf("默认库文件落在共享根里了: %s", pInst)
	}
}

func insideRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestResolveMetadataPathExplicitPathWinsOverInstance：用户手写的
// metadata_path 原样生效，实例区分不掺进来 —— 这是已拍板的契约，
// 「用户写同一个路径 = 用户自己负责唯一性」。
func TestResolveMetadataPathExplicitPathWinsOverInstance(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "custom.db")
	o := oscap.Options{
		Root:         dir,
		MetadataPath: explicit,
		InstanceID:   "127.0.0.1:4451",
	}
	got, err := resolveMetadataPath(o)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("显式路径被改写了: got %q, want %q", got, explicit)
	}

	// 配成目录时接受，但文件名必须带上实例片段。
	o.MetadataPath = dir
	got, err = resolveMetadataPath(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, metadataFileName(dir, "127.0.0.1:4451")); got != want {
		t.Fatalf("目录形态的落点与预期不符: got %q, want %q", got, want)
	}
	if !strings.Contains(filepath.Base(got), "127.0.0.1_4451") {
		t.Fatalf("目录形态下文件名丢了实例片段: %q", got)
	}
}
