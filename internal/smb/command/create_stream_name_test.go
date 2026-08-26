package command

// create_stream_name_test.go —— splitCreateName 的流名语法口径（bh5 F11）。
//
// Samba 对照：
//   - check_path_syntax（bh5 报告引证 smbd/smb2_reply.c:93-108）：':' 之后
//     必须还有字符，"f:" 这类残缺输入报 OBJECT_NAME_INVALID；
//   - vfs_streams_xattr_get_name()（vfs_streams_xattr.c:527-539）：类型后缀
//     只认 ":$DATA"（strcasecmp），其余拒绝。
//
// VFS 层的 SplitStreamPath 是同一份语法的另一套解析器（internal/vfs/stream.go），
// 两层的类型后缀口径必须一致：都只认 $DATA。差异历史见 bh5 报告 F11。

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
)

func TestSplitCreateNameTrailingColon(t *testing.T) {
	for _, name := range []string{"f:", "dir/f:", "a/b/c:"} {
		path, stream, err := splitCreateName(name)
		if err != status.ObjectNameInvalid {
			t.Errorf("splitCreateName(%q) = (%q,%q,%v), 期望 ObjectNameInvalid", name, path, stream, err)
		}
	}
}

func TestSplitCreateNameStreamSyntax(t *testing.T) {
	cases := []struct {
		in           string
		path, stream string
		wantErr      error
	}{
		// 主数据流的显式写法。
		{in: "f::$DATA", path: "f"},
		{in: "f::$data", path: "f"}, // 类型后缀大小写不敏感
		// 命名流：类型可省略。
		{in: "f:s:$DATA", path: "f", stream: "s"},
		{in: "f:s:$data", path: "f", stream: "s"},
		{in: "f:s", path: "f", stream: "s"},
		{in: "dir/f:s:$DATA", path: "dir/f", stream: "s"},
		// 共享根上的流。
		{in: ":AFP_AfpInfo:$DATA", path: "", stream: "AFP_AfpInfo"},
		// 非 $DATA 类型一律不支持（与 VFS 层收窄后的口径一致）。
		{in: "f:s:$INDEX_ALLOCATION", wantErr: status.NotSupported},
		{in: "f::$INDEX_ALLOCATION", wantErr: status.NotSupported},
		{in: "f:s:$BOGUS", wantErr: status.NotSupported},
		// 残缺的尾冒号段（":$" 后没有类型名）同样不是 $DATA。
		{in: "f::", wantErr: status.NotSupported},
		{in: "f:s:", wantErr: status.NotSupported},
	}
	for _, c := range cases {
		path, stream, err := splitCreateName(c.in)
		if c.wantErr != nil {
			if err != c.wantErr {
				t.Errorf("splitCreateName(%q) err = %v, 期望 %v", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCreateName(%q) 意外报错: %v", c.in, err)
			continue
		}
		if path != c.path || stream != c.stream {
			t.Errorf("splitCreateName(%q) = (%q,%q), 期望 (%q,%q)", c.in, path, stream, c.path, c.stream)
		}
	}
}
