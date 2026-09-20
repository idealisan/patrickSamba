package vfs

// casehost_test.go —— 探测「宿主文件系统折不折叠大小写」。
//
// 有一批用例的前提是「宿主大小写敏感」：它们要造两个仅大小写不同的名字、
// 或者要精确 stat 落空才能走到大小写回退。APFS / NTFS 默认折叠大小写，
// 在这些宿主上原样断言会把「宿主特性」报成「产品缺陷」。
//
// 处置是**按宿主能力走另一条断言分支**，而不是跳过：跳过的用例等于不存在
// （见 memory/feedback_falsifiable_assertions.md），而折叠宿主上的行为本身
// 也值得钉住。开关就是本文件这一个函数。

import (
	"os"
	"path/filepath"
	"testing"
)

// hostFoldsCase 探测临时目录所在文件系统是否把大小写视为等价。
//
// 做法：建一个小写名文件，再看大写名能否 Lstat 到。全程在 t.TempDir() 里，
// 不碰任何共享状态。
func hostFoldsCase(tb testing.TB) bool {
	tb.Helper()
	dir := tb.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe-lower"), nil, 0o644); err != nil {
		tb.Fatalf("造探测文件失败: %v", err)
	}
	_, err := os.Lstat(filepath.Join(dir, "PROBE-LOWER"))
	return err == nil
}
