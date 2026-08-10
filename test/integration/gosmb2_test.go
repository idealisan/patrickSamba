//go:build integration

// go-smb2 是第三类必须通过的第三方客户端（AGENTS.md §3）：
// 纯 Go 实现的 SMB2/3 客户端库，可在 CI 内直接跑端到端集成测试，
// 不依赖任何外部二进制（与 acceptance.sh 里的 smbclient / impacket 互补）。
//
// 注意依赖选择（AGENTS.md §4）：只用 MIT 许可的
// github.com/hirochachacha/go-smb2，绝不能用 AGPL-3.0 的
// macos-fuse-t/go-smb2。go.mod 里已固定为 hirochachacha 版本。
//
// 本测试复用 harness_test.go 的 startServer（进程内启动服务端，不 exec
// cmd/stupidsamba），用 go-smb2 做 ls / get / put / mkdir / rm / rename。
//
// 跑法：
//
//	CGO_ENABLED=0 go test -tags integration ./test/integration/ -run TestGoSMB2 -v
package integration

import (
	"net"
	"testing"

	"github.com/hirochachacha/go-smb2"
)

// connectGoSMB2 用 go-smb2 客户端连上测试服务端并返回已挂载的 share。
//
// 认证走 NTLMv2（hirochachacha/go-smb2 仅支持 NTLMv2，不支持 NTLMv1），
// 凭据来自 harness_test.go 里的测试账户常量（AGENTS.md C8：不读系统用户库）。
func connectGoSMB2(t *testing.T, addr string) *smb2.Share {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	initiator := &smb2.NTLMInitiator{
		User:        testUser,
		Password:    testPass,
		Domain:      testDomain,
		Workstation: "GO-SMB2-CI",
	}

	d := &smb2.Dialer{Initiator: initiator}
	session, err := d.Dial(conn)
	if err != nil {
		t.Fatalf("go-smb2 Dial(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = session.Logoff() })

	fs, err := session.Mount(testShare)
	if err != nil {
		t.Fatalf("go-smb2 Mount(%q): %v", testShare, err)
	}
	t.Cleanup(func() { _ = fs.Umount() })
	return fs
}

// TestGoSMB2FileOps 覆盖 §3 要求的六种基本文件操作：
// put / get / ls / mkdir / rm / rename。
func TestGoSMB2FileOps(t *testing.T) {
	h := startServer(t, harnessOptions{})
	fs := connectGoSMB2(t, h.Addr)

	const content = "hello from go-smb2 client\n"
	const fileName = "hello.txt"
	const renamedName = "renamed.txt"
	const dirName = "subdir"

	// --- put ---
	if err := fs.WriteFile(fileName, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fileName, err)
	}

	// --- get ---
	got, err := fs.ReadFile(fileName)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", fileName, err)
	}
	if string(got) != content {
		t.Fatalf("get 内容不符: got %q want %q", got, content)
	}

	// --- ls ---
	infos, err := fs.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var foundFile bool
	for _, fi := range infos {
		if fi.Name() == fileName {
			foundFile = true
			if fi.Size() != int64(len(content)) {
				t.Fatalf("ls 大小不符: got %d want %d", fi.Size(), len(content))
			}
		}
	}
	if !foundFile {
		t.Fatalf("ls 未列出刚 put 的 %q", fileName)
	}

	// --- mkdir ---
	if err := fs.Mkdir(dirName, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", dirName, err)
	}
	if fi, err := fs.Stat(dirName); err != nil {
		t.Fatalf("Stat(%q): %v", dirName, err)
	} else if !fi.IsDir() {
		t.Fatalf("Stat(%q) 应是目录", dirName)
	}

	// 在子目录里再 put 一个文件，验证路径语义。
	if err := fs.WriteFile(dirName+"/inner.txt", []byte("inner"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", dirName+"/inner.txt", err)
	}

	// --- rename ---
	if err := fs.Rename(fileName, renamedName); err != nil {
		t.Fatalf("Rename(%q->%q): %v", fileName, renamedName, err)
	}
	if _, err := fs.ReadFile(fileName); err == nil {
		t.Fatalf("rename 后旧名 %q 仍能读到，重命名未生效", fileName)
	}
	if got, err := fs.ReadFile(renamedName); err != nil || string(got) != content {
		t.Fatalf("rename 后读新名 %q: got %q err %v", renamedName, got, err)
	}

	// --- rm ---
	if err := fs.Remove(renamedName); err != nil {
		t.Fatalf("Remove(%q): %v", renamedName, err)
	}
	if _, err := fs.ReadFile(renamedName); err == nil {
		t.Fatalf("rm 后 %q 仍能读到", renamedName)
	}
	// 非空目录需先删内容（SMB DELETE 对非空目录返回错误，与 POSIX 不同）。
	if err := fs.Remove(dirName + "/inner.txt"); err != nil {
		t.Fatalf("Remove(%q): %v", dirName+"/inner.txt", err)
	}
	if err := fs.Remove(dirName); err != nil {
		t.Fatalf("Remove(%q): %v", dirName, err)
	}
	if _, err := fs.Stat(dirName); err == nil {
		t.Fatalf("rm 后 %q 仍存在", dirName)
	}
}
