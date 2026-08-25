package oscap

// instanceid_test.go —— InstanceID 清洗/编码规则的 golden test。
//
// 这份规则是两处旁路存储（oscap/builtin 与 Windows 元数据旁路）共用的
// 单一真源，任何一侧的默认落点都依赖它的两个硬保证：
// 确定性（同输入同输出 ⇒ 同一实例重启后复用同一份元数据）
// 与可区分性（不同原始值必得不同后缀 ⇒ 不同实例不抢同一个 bbolt 文件锁）。

import (
	"strings"
	"testing"
)

func TestSanitizeInstanceID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		// 监听端点最常见的形态：冒号折叠成下划线，点保留。
		{"127.0.0.1:4451", "127.0.0.1_4451"},
		// IPv6 端点：方括号与成串冒号折叠合并成一个下划线，首尾的下划线去掉。
		{"[::1]:4451", "1_4451"},
		// 路径分隔符与空格（文件名安全的底线）。
		{"a/b", "a_b"},
		{`a\b`, "a_b"},
		{"a b", "a_b"},
		// 合法字符原样保留。
		{"host-01.example.com", "host-01.example.com"},
		{"UPPER_9.x-y", "UPPER_9.x-y"},
		// 全非法字符：标签清空（唯一性由 InstanceIDSuffix 的哈希兜底）。
		{":::", ""},
	}
	for _, c := range cases {
		if got := SanitizeInstanceID(c.in); got != c.want {
			t.Errorf("SanitizeInstanceID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeInstanceIDCollapsesAndTrims(t *testing.T) {
	// 连续非法字符只留一个 '_'，且首尾不留 —— 否则同样的端点换个写法
	// 就得到难看且不一致的标签。
	if got, want := SanitizeInstanceID("///a///b///"), "a_b"; got != want {
		t.Errorf("SanitizeInstanceID 折叠/去首尾失败: got %q, want %q", got, want)
	}
}

func TestSanitizeInstanceIDTruncatesToTagLen(t *testing.T) {
	long := strings.Repeat("a", maxInstanceTagLen*3)
	got := SanitizeInstanceID(long)
	if len(got) != maxInstanceTagLen {
		t.Errorf("截断后长度 = %d, want %d", len(got), maxInstanceTagLen)
	}
	// 非 ASCII 输入也必须安全：全部被折叠，不会按字节切碎出半个字符。
	if got := SanitizeInstanceID(strings.Repeat("汉", 100)); got != "" {
		t.Errorf("非 ASCII 输入应整体折叠为空标签，got %q", got)
	}
	// 截断边界上的确定性。
	mid := strings.Repeat("a", maxInstanceTagLen) + ":4451"
	if got := SanitizeInstanceID(mid); got != strings.Repeat("a", maxInstanceTagLen) {
		t.Errorf("超长端点的标签应为前 %d 字节，got 长度 %d", maxInstanceTagLen, len(got))
	}
}

func TestInstanceIDSuffixDeterministic(t *testing.T) {
	a1, ok := InstanceIDSuffix("127.0.0.1:4451")
	if !ok {
		t.Fatal("非空 InstanceID 必须产出后缀")
	}
	a2, _ := InstanceIDSuffix("127.0.0.1:4451")
	if a1 != a2 {
		t.Fatalf("同一实例两次推导不一致: %q vs %q（重启后元数据会丢）", a1, a2)
	}
	if !strings.Contains(a1, "127.0.0.1_4451") {
		t.Errorf("后缀应包含清洗后的可读标签，got %q", a1)
	}
}

func TestInstanceIDSuffixDistinguishesRawValues(t *testing.T) {
	// 清洗会把信息折叠掉："a:b" 与 "a_b" 的标签相同。
	// 唯一性由取自**原始值**的哈希兜底 —— 这一条不成立的话，
	// 「不同实例必得不同路径」就是假的。
	s1, ok1 := InstanceIDSuffix("a:b")
	s2, ok2 := InstanceIDSuffix("a_b")
	if !ok1 || !ok2 {
		t.Fatalf("两个非空输入都必须有后缀: %v %v", ok1, ok2)
	}
	if s1 == s2 {
		t.Fatalf("原始值不同的两个 InstanceID 得到了同一个后缀 %q", s1)
	}
	// 全非法字符的实例也要能与其他实例区分。
	s3, ok := InstanceIDSuffix(":::")
	if !ok || s3 == "" {
		t.Fatalf("全非法字符的实例没有产出后缀: (%q, %v)", s3, ok)
	}
	if strings.ContainsAny(s3, ":/\\ ") {
		t.Errorf("后缀 %q 含文件名非法字符", s3)
	}
	s4, _ := InstanceIDSuffix(",,,")
	if s3 == s4 {
		t.Fatalf("两个不同的全非法字符实例得到了同一个后缀: %q", s3)
	}
}

func TestInstanceIDSuffixEmptyIsFalse(t *testing.T) {
	if s, ok := InstanceIDSuffix(""); ok || s != "" {
		t.Errorf("空 InstanceID 应返回 (\"\", false)，got (%q, %v)", s, ok)
	}
}
