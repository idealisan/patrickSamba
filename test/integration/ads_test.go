//go:build integration

package integration

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// TestADSReadWrite 验证备用数据流（ADS / 命名流）的读写：
// 在普通文件上创建一条命名流 "file:stream:$DATA"，独立写入并读回，
// 且不影响主数据流内容。覆盖正被并行改动的 CREATE 流名解析路径。
func TestADSReadWrite(t *testing.T) {
	h := startServer(t, harnessOptions{})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("treeConnect: %v", err)
	}

	base := "adsbase.txt"
	// 创建主文件并写入主数据流。
	bfid, err := c.create(base, wire.FileOpenIf, wire.FileNonDirectoryFile)
	if err != nil {
		t.Fatalf("create base: %v", err)
	}
	baseData := []byte("main-stream-content")
	if err := c.write(bfid, 0, baseData); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := c.closeFile(bfid); err != nil {
		t.Fatalf("close base: %v", err)
	}

	// 打开同一文件的命名流并写入。
	streamName := base + ":ads:$DATA"
	sfid, err := c.create(streamName, wire.FileOpenIf, wire.FileNonDirectoryFile)
	if err != nil {
		t.Fatalf("create stream %q: %v", streamName, err)
	}
	streamData := []byte("alternate-stream-data")
	if err := c.write(sfid, 0, streamData); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	gotStream, err := c.read(sfid, 0, uint32(len(streamData)))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(gotStream) != string(streamData) {
		t.Fatalf("流内容不匹配: got %q want %q", gotStream, streamData)
	}
	if err := c.closeFile(sfid); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// 重新打开主文件，确认主数据流未被流写入破坏。
	bfid2, err := c.create(base, wire.FileOpenIf, wire.FileNonDirectoryFile)
	if err != nil {
		t.Fatalf("reopen base: %v", err)
	}
	gotBase, err := c.read(bfid2, 0, uint32(len(baseData)))
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if string(gotBase) != string(baseData) {
		t.Fatalf("主数据流被破坏: got %q want %q", gotBase, baseData)
	}
	if err := c.closeFile(bfid2); err != nil {
		t.Fatalf("close base2: %v", err)
	}
}
