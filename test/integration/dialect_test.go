//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// TestMultiDialect 验证 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1 五种方言各自能完成
// 协商 + NTLMv2 认证 + 建树 + 创建/写入/读取/关闭 的完整文件操作。
//
// 重点覆盖正被并行改动的协商路径，以及 3.1.1 的 preauth 哈希链（客户端必须
// 与服务端累积顺序完全一致才能派生出正确密钥）。
func TestMultiDialect(t *testing.T) {
	h := startServer(t, harnessOptions{})
	for _, d := range []wire.Dialect{wire.SMB202, wire.SMB210, wire.SMB300, wire.SMB302, wire.SMB311} {
		d := d
		t.Run(d.String(), func(t *testing.T) {
			c := newRawClient(t, h.Addr)
			if err := c.dial([]wire.Dialect{d}, false); err != nil {
				t.Fatalf("dial %s: %v", d, err)
			}
			if c.dialect != d {
				t.Fatalf("协商方言 = %s, 期望 %s", c.dialect, d)
			}
			if err := c.treeConnect(testShare); err != nil {
				t.Fatalf("treeConnect: %v", err)
			}
			name := fmt.Sprintf("md-%s.txt", d.String())
			fid, err := c.create(name, wire.FileOpenIf, wire.FileNonDirectoryFile)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			data := []byte("hello " + d.String())
			if err := c.write(fid, 0, data); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := c.read(fid, 0, uint32(len(data)))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != string(data) {
				t.Fatalf("数据不匹配: got %q want %q", got, data)
			}
			if err := c.closeFile(fid); err != nil {
				t.Fatalf("close: %v", err)
			}
			c.close()
		})
	}
}
