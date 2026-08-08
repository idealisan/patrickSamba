// gosmb2client 用第三方 Go SMB2 客户端库对 stupidsamba 做验收测试。
//
// 这是 AGENTS.md §3 客户端矩阵的第 6 项。选它的价值在于：
// 它是与 Samba / Windows 完全独立的第三方协议栈实现，且能进 CI（无需 root、无需挂载）。
//
// 用法: go run . <host:port> <user> <pass> <share>
package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path"
	"time"

	"github.com/hirochachacha/go-smb2"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "用法: go run . <host:port> <user> <pass> <share>")
		os.Exit(2)
	}
	addr, user, pass, share := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	if err := run(addr, user, pass, share); err != nil {
		fmt.Fprintf(os.Stderr, "  失败: %v\n", err)
		os.Exit(1)
	}
}

func run(addr, user, pass, share string) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("拨号: %w", err)
	}
	defer conn.Close()

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{User: user, Password: pass},
	}
	s, err := d.Dial(conn)
	if err != nil {
		return fmt.Errorf("SMB 握手/认证: %w", err)
	}
	defer s.Logoff()

	fs, err := s.Mount(share)
	if err != nil {
		return fmt.Errorf("挂载 %s: %w", share, err)
	}
	defer fs.Umount()
	fmt.Println("  挂载 OK")

	// 1) 目录枚举
	ents, err := fs.ReadDir(".")
	if err != nil {
		return fmt.Errorf("ReadDir: %w", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	fmt.Printf("  ReadDir: %v\n", names)
	if !contains(names, "hello.txt") {
		return fmt.Errorf("目录列表里没有 hello.txt: %v", names)
	}

	// 2) 读文件
	body, err := fs.ReadFile("hello.txt")
	if err != nil {
		return fmt.Errorf("ReadFile: %w", err)
	}
	if !bytes.Contains(body, []byte("hello from stupidsamba")) {
		return fmt.Errorf("hello.txt 内容不匹配: %q", body)
	}
	fmt.Println("  ReadFile OK")

	// 3) 写 + 回读
	payload := bytes.Repeat([]byte("go-smb2 write test\n"), 500)
	if err := fs.WriteFile("gosmb2.txt", payload, 0o644); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	back, err := fs.ReadFile("gosmb2.txt")
	if err != nil {
		return fmt.Errorf("回读: %w", err)
	}
	if !bytes.Equal(back, payload) {
		return fmt.Errorf("回读内容不一致: 写入 %d 字节, 读回 %d 字节", len(payload), len(back))
	}
	fmt.Printf("  写入/回读 %d 字节 OK\n", len(payload))

	// 4) 大文件（跨多个 READ，验证分块与 credit）
	blob, err := fs.ReadFile("blob.bin")
	if err != nil {
		return fmt.Errorf("读 blob.bin: %w", err)
	}
	if len(blob) != 1<<20 {
		return fmt.Errorf("blob.bin 应为 1048576 字节, 实际 %d", len(blob))
	}
	fmt.Println("  1MiB 大文件读取 OK")

	// 5) Stat
	st, err := fs.Stat("hello.txt")
	if err != nil {
		return fmt.Errorf("Stat: %w", err)
	}
	if st.IsDir() || st.Size() == 0 {
		return fmt.Errorf("Stat 结果异常: isDir=%v size=%d", st.IsDir(), st.Size())
	}
	fmt.Printf("  Stat OK: size=%d mtime=%s\n", st.Size(), st.ModTime().Format(time.RFC3339))

	// 6) 子目录
	subs, err := fs.ReadDir("subdir")
	if err != nil {
		return fmt.Errorf("读子目录: %w", err)
	}
	subNames := make([]string, 0, len(subs))
	for _, e := range subs {
		subNames = append(subNames, e.Name())
	}
	if !contains(subNames, "nested.txt") {
		return fmt.Errorf("子目录里没有 nested.txt: %v", subNames)
	}
	fmt.Println("  子目录枚举 OK")

	// 7) 创建目录 / 重命名 / 删除
	if err := fs.Mkdir("gosmb2dir", 0o755); err != nil {
		return fmt.Errorf("Mkdir: %w", err)
	}
	if err := fs.Rename("gosmb2.txt", path.Join("gosmb2dir", "moved.txt")); err != nil {
		return fmt.Errorf("Rename: %w", err)
	}
	if _, err := fs.Stat("gosmb2dir/moved.txt"); err != nil {
		return fmt.Errorf("重命名后 Stat: %w", err)
	}
	if err := fs.Remove("gosmb2dir/moved.txt"); err != nil {
		return fmt.Errorf("Remove 文件: %w", err)
	}
	if err := fs.Remove("gosmb2dir"); err != nil {
		return fmt.Errorf("Remove 目录: %w", err)
	}
	fmt.Println("  Mkdir/Rename/Remove OK")

	// 8) 随机位置读写（验证 offset 处理）
	f, err := fs.Create("seek.bin")
	if err != nil {
		return fmt.Errorf("Create: %w", err)
	}
	if _, err := f.WriteAt([]byte("TAIL"), 4096); err != nil {
		f.Close()
		return fmt.Errorf("WriteAt: %w", err)
	}
	tail := make([]byte, 4)
	if _, err := f.ReadAt(tail, 4096); err != nil {
		f.Close()
		return fmt.Errorf("ReadAt: %w", err)
	}
	f.Close()
	if string(tail) != "TAIL" {
		return fmt.Errorf("稀疏偏移读写不一致: %q", tail)
	}
	if err := fs.Remove("seek.bin"); err != nil {
		return fmt.Errorf("清理 seek.bin: %w", err)
	}
	fmt.Println("  稀疏偏移读写 OK")

	// 9) 卷信息
	if stat, err := fs.Statfs("."); err == nil {
		fmt.Printf("  Statfs OK: total=%d free=%d blockSize=%d\n",
			stat.TotalBlockCount(), stat.FreeBlockCount(), stat.BlockSize())
	} else {
		fmt.Printf("  Statfs 未支持(可接受): %v\n", err)
	}

	return nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
