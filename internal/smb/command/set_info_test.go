package command

import (
	"strings"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// renameTarget 是 SET_INFO 的 FileRenameInformation / FileLinkInformation
// 唯一的路径入口 —— 客户端送来的**原始线上字节**在这里被翻译成共享内相对路径。
// 它是 AGENTS.md §8 要求的那道防穿越屏障。
//
// 为什么必须用单元测试而不是 smbclient 验证（实测踩过）：
// smbclient 会在**客户端侧**就把 `..\x` 规范化成 `\x` 再发出去，
// 服务端根本收不到 `..`。也就是说拿 smbclient 跑一遍「看起来没逃逸」
// 完全证明不了服务端有防御 —— 那只证明了 smbclient 很规矩。
// 真正的攻击者会直接构造报文。所以这道防线只能在这一层钉死。
func TestRenameTargetRejectsTraversal(t *testing.T) {
	// 这些必须**全部被拒**，一条都不能放过。
	evil := []string{
		`..\escaped.txt`,           // 最直接的父目录
		`..\..\..\tmp\owned`,       // 多级上跳
		`dir\..\..\escaped.txt`,    // 先进再跳，净效果越界
		`.\..\escaped.txt`,         // 混入 "." 干扰
		`..`,                       // 光秃秃的父目录
		`dir\..\..`,                // 净效果是根的父目录
		"a\\\x00b",                 // NUL 截断攻击
		"bad\x01name",              // 其它控制字符
		`a\\b`,                     // 中间空分量（畸形）
		`stream.txt:AFP_Resource`,  // 带流名的改名，我们不支持
		`C:\Windows\System32\evil`, // Windows 绝对路径（含 ':' ）
	}
	for _, name := range evil {
		got, err := renameTarget(name)
		if err == nil {
			t.Errorf("renameTarget(%q) 应被拒，实际返回 %q —— 这是一个路径穿越漏洞",
				name, got)
		}
	}
}

// TestRenameTargetAcceptsLegitimate：正常目标必须能通过，
// 否则「安全」就变成了「什么都干不了」。
func TestRenameTargetAcceptsLegitimate(t *testing.T) {
	cases := []struct{ in, want string }{
		{`new.txt`, "new.txt"},
		{`\new.txt`, "new.txt"},        // 真实客户端会带前导反斜杠
		{`dir\new.txt`, "dir/new.txt"}, // 反斜杠转斜杠
		{`dir\sub\new.txt`, "dir/sub/new.txt"},
		{`dir\.\new.txt`, "dir/new.txt"},      // "." 被吃掉
		{`dir\sub\..\new.txt`, "dir/new.txt"}, // 上跳但未越界，合法
		{`dir\`, "dir"},                       // 结尾多余分隔符要容忍
	}
	for _, c := range cases {
		got, err := renameTarget(c.in)
		if err != nil {
			t.Errorf("renameTarget(%q) 意外失败: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("renameTarget(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestRenameTargetStreamRejectedNotSilentlyStripped：带流名的目标必须**报错**，
// 不能把流名悄悄剥掉。
//
// 剥掉的后果是把「重命名 t.txt 的资源派生」变成「重命名 t.txt 本身」——
// 客户端以为动的是流，实际动的是主文件，属于静默数据破坏。
func TestRenameTargetStreamRejectedNotSilentlyStripped(t *testing.T) {
	got, err := renameTarget(`t.txt:AFP_Resource`)
	if err == nil {
		t.Fatalf("带流名的改名应被拒，实际返回 %q", got)
	}
	if !strings.Contains(err.Error(), vfs.ErrNotSupported.Error()) {
		t.Errorf("err = %v, 期望包含 %v（好让上层映射成 STATUS_NOT_SUPPORTED）",
			err, vfs.ErrNotSupported)
	}
}
