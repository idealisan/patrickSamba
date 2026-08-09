package config

// quota_warn_test.go —— quota_bytes 与共享现有用量之间的启动自检。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findWarning 返回第一条包含 substr 的告警。
func findWarning(ws []string, substr string) (string, bool) {
	for _, w := range ws {
		if strings.Contains(w, substr) {
			return w, true
		}
	}
	return "", false
}

func TestQuotaUsageWarnings(t *testing.T) {
	root := t.TempDir()

	full := filepath.Join(root, "full")
	if err := os.Mkdir(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "data.bin"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Shares: []Share{
		{Name: "toosmall", Path: full, QuotaBytes: 1 << 20},                    // 1 MiB < 已用 4 MiB
		{Name: "roomy", Path: full, QuotaBytes: 64 << 20},                      // 64 MiB > 已用 4 MiB
		{Name: "fresh", Path: empty, QuotaBytes: 1 << 20},                      // 空目录
		{Name: "nolimit", Path: full},                                          // 没配额
		{Name: "gone", Path: filepath.Join(root, "nope"), QuotaBytes: 1 << 20}, // 目录不存在
	}}

	ws := quotaUsageWarnings(cfg)

	if _, ok := findWarning(ws, `"toosmall"`); !ok {
		t.Errorf("配额小于现有用量时应告警，实际告警: %v", ws)
	}
	// 反向对照：配额充足 / 空目录 / 没配额 / 目录不存在，一条都不该报。
	// 少了这几条，一个「无脑对每个共享都告警」的实现也能通过上面那条断言。
	for _, name := range []string{`"roomy"`, `"fresh"`, `"nolimit"`, `"gone"`} {
		if got, ok := findWarning(ws, name); ok {
			t.Errorf("不该对 %s 告警，却报了: %s", name, got)
		}
	}
}

// TestQuotaUsageWarningIsActionable：告警必须是人话 ——
// 说清楚是哪个共享、什么问题、怎么改。
func TestQuotaUsageWarningIsActionable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.bin"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Shares: []Share{{Name: "backup", Path: dir, QuotaBytes: 1 << 20}}}

	ws := quotaUsageWarnings(cfg)
	if len(ws) != 1 {
		t.Fatalf("应有 1 条告警，得到 %d 条: %v", len(ws), ws)
	}
	msg := ws[0]
	for _, want := range []string{
		"shares[0]", // 哪个共享
		"backup",
		"1.0 MiB",           // 配额（人话单位，不是裸字节数）
		"Time Machine",      // 后果
		"请把 quota_bytes 调到", // 怎么改
		dir,                 // 改哪里
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("告警缺少 %q: %s", want, msg)
		}
	}
}

// TestQuotaUsageWarningsWiredIntoWarnings 确认这条检查真的接进了 Warnings()，
// 而不是只有 quotaUsageWarnings 自己是对的（main.go 只调 Warnings）。
func TestQuotaUsageWarningsWiredIntoWarnings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.bin"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Listen: Listen{Port: 4453},
		Shares: []Share{{Name: "backup", Path: dir, QuotaBytes: 1 << 20}},
	}
	if _, ok := findWarning(Warnings(cfg), "不大于该目录现有用量"); !ok {
		t.Error("Warnings() 里没有这条告警，说明没接上")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1 << 10, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{2 << 30, "2.0 GiB"},
		{2199023255552, "2.0 TiB"}, // configs/example.yaml 里的 quota_bytes
		{1 << 50, "1.0 PiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
