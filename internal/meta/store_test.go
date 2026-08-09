package meta

// store_test.go —— 与平台/后端无关的纯逻辑测试，在**所有**平台上都跑。
//
// 这里的每个「能存能取」类断言都配了反向对照：只证明正例的测试是不可证伪的，
// 一个永远返回 true 的实现也能过。

import (
	"errors"
	"testing"
)

func TestRecordCodecRoundTrip(t *testing.T) {
	rec := Record{UID: 0xDEADBEEF, GID: 0x01020304, Mode: 0o7777, FileID: 0x1122334455667788}

	buf := encodeRecord(rec)
	if len(buf) != recordLen {
		t.Fatalf("编码长度 = %d, want %d", len(buf), recordLen)
	}
	if buf[0] != recordVersion {
		t.Errorf("首字节应是版本号 %d, got %d", recordVersion, buf[0])
	}
	// 显式断言小端（AGENTS.md §5：字节序必须写死，跨架构搬库不能漂移）
	if buf[1] != 0xEF || buf[2] != 0xBE || buf[3] != 0xAD || buf[4] != 0xDE {
		t.Errorf("UID 未按小端编码: % x", buf[1:5])
	}
	if buf[13] != 0x88 || buf[20] != 0x11 {
		t.Errorf("FileID 未按小端编码: % x", buf[13:21])
	}

	got, ok := decodeRecord(buf)
	if !ok || got != rec {
		t.Errorf("往返 = %+v, %v; want %+v, true", got, ok, rec)
	}
}

// TestRecordCodecRejectsGarbage 是 RoundTrip 的反向对照：
// 解码器必须**拒绝**它不认识的字节，而不是硬解出一个看似合理的记录。
func TestRecordCodecRejectsGarbage(t *testing.T) {
	buf := encodeRecord(Record{UID: 1, GID: 2, Mode: 0o644, FileID: 3})

	if _, ok := decodeRecord(nil); ok {
		t.Error("nil 不应被接受")
	}
	// 任何长度不足的截断都必须当作「没有记录」而不是 panic
	for n := 0; n < recordLen; n++ {
		if _, ok := decodeRecord(buf[:n]); ok {
			t.Errorf("长度 %d 的残缺记录不应被接受", n)
		}
	}
	// 版本号不认识 → 当作没有记录，绝不能按 v1 布局硬解
	bad := append([]byte(nil), buf...)
	bad[0] = recordVersion + 1
	if _, ok := decodeRecord(bad); ok {
		t.Error("未知版本号不应被接受")
	}
	// 也覆盖旧版 12 字节格式（internal/vfs 里那版）：长度不够，必然被拒
	if _, ok := decodeRecord(make([]byte, 12)); ok {
		t.Error("旧版 12 字节记录不应被当成有效记录")
	}
}

// TestRecordModeMasked：文件类型位必须被剥掉，只留权限位。
// 存了类型位就会和宿主文件系统的真实类型分叉。
func TestRecordModeMasked(t *testing.T) {
	// 0o100644 = S_IFREG | 0644
	got, ok := decodeRecord(encodeRecord(Record{Mode: 0o100644}))
	if !ok {
		t.Fatal("解码失败")
	}
	if got.Mode != 0o644 {
		t.Errorf("Mode = %o, want %o（类型位应被掩掉）", got.Mode, 0o644)
	}
	// 反向对照：掩码不能掩过头，setuid/setgid/sticky 必须原样保留
	got, _ = decodeRecord(encodeRecord(Record{Mode: 0o7777}))
	if got.Mode != 0o7777 {
		t.Errorf("Mode = %o, want %o（suid/sgid/sticky 不该被掩掉）", got.Mode, 0o7777)
	}
}

// TestStaleFor 覆盖 FileID 印章：路径被复用时必须判为陈旧，
// 否则新文件会顶着上一个文件的属主。
func TestStaleFor(t *testing.T) {
	cases := []struct {
		name     string
		stored   uint64
		observed uint64
		want     bool
	}{
		{"印章一致 → 有效", 42, 42, false},
		{"印章不一致 → 陈旧（路径被复用）", 42, 43, true},
		{"记录里没印章 → 跳过校验", 0, 43, false},
		{"观察不到印章 → 跳过校验", 42, 0, false},
		{"两边都没有 → 跳过校验", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Record{FileID: c.stored}).StaleFor(c.observed); got != c.want {
				t.Errorf("StaleFor(%d) with stored=%d = %v, want %v",
					c.observed, c.stored, got, c.want)
			}
		})
	}
}

func TestNormKey(t *testing.T) {
	cases := map[string]string{
		"":            "/", // 共享根
		"/":           "/",
		"a":           "/a",
		"a/b.txt":     "/a/b.txt",
		"A/B.TXT":     "/a/b.txt", // 折叠：NTFS 大小写不敏感
		`a\b`:         "/a/b",     // Windows 分隔符
		`A\B\C`:       "/a/b/c",
		"/a//b/":      "/a/b", // 冗余分隔符收敛
		"./a":         "/a",
		"a/.":         "/a",
		"a/./b":       "/a/b",
		"深/目录/文件.txt": "/深/目录/文件.txt",
	}
	for in, want := range cases {
		got, err := normKey(in)
		if err != nil {
			t.Errorf("normKey(%q) 意外报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormKeyRejectsTraversal 是 TestNormKey 的反向对照，也是安全断言：
// ".." 一旦被当成普通分量参与前缀计算，Delete/Rename 就会作用到错误的子树上。
func TestNormKeyRejectsTraversal(t *testing.T) {
	bad := []string{
		"..",
		"../a",
		"a/../b",
		"a/..",
		"/../etc",
		`a\..\b`, // 反斜杠形式同样要拦
		"a\x00b", // NUL 截断
	}
	for _, in := range bad {
		if got, err := normKey(in); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("normKey(%q) = %q, %v; want ErrInvalidKey", in, got, err)
		}
	}
}

// TestNormKeyDistinguishesSiblings：规范化不能把不同对象折到同一个键上，
// 尤其是「前缀相近」的兄弟 —— 前缀扫描全靠这个区分度。
func TestNormKeyDistinguishesSiblings(t *testing.T) {
	pairs := [][2]string{
		{"a", "ab"},
		{"a/b", "ab"},
		{"d", "dd"},
		{"old", "older"},
	}
	for _, p := range pairs {
		k0, _ := normKey(p[0])
		k1, _ := normKey(p[1])
		if k0 == k1 {
			t.Errorf("%q 与 %q 折到了同一个键 %q", p[0], p[1], k0)
		}
		// 更关键的是：k0 的子项前缀不能误伤 k1
		if pre := childPrefix(k0); len(k1) >= len(pre) && k1[:len(pre)] == pre {
			t.Errorf("%q 的子项前缀 %q 会误伤兄弟 %q", p[0], pre, k1)
		}
	}
}

func TestChildPrefix(t *testing.T) {
	// 根是唯一特例：已经是 "/"，再拼一个就成了 "//"
	if got := childPrefix("/"); got != "/" {
		t.Errorf("childPrefix(\"/\") = %q, want \"/\"", got)
	}
	if got := childPrefix("/a"); got != "/a/" {
		t.Errorf("childPrefix(\"/a\") = %q, want \"/a/\"", got)
	}
}

func TestRelFromKey(t *testing.T) {
	cases := map[string]string{"/": "", "/a": "a", "/a/b": "a/b"}
	for in, want := range cases {
		if got := relFromKey(in); got != want {
			t.Errorf("relFromKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFold(t *testing.T) {
	if Fold("Foo.TXT") != "foo.txt" {
		t.Errorf("Fold 没有折叠大小写: %q", Fold("Foo.TXT"))
	}
	// 反向对照：折叠不能把不同的名字折成一个
	if Fold("a") == Fold("b") {
		t.Error("Fold 把不同名字折到了一起")
	}
}
