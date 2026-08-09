// 冒烟客户端 3/3：hirochachacha/go-smb2（纯 Go 第三方客户端库，
// AGENTS.md §3 矩阵第 6 项）。
//
// 用法: go run . <host> <port> <user> <pass> <share> <workdir> <前缀>
// 退出码: 0 成功 / 1 失败
//
// 选它的价值：与 Samba、与 impacket 都完全独立的第三方协议栈，且无需 root、
// 无需挂载，是三家里唯一能无条件进 CI 的（mount.cifs 在容器里根本跑不了）。
//
// 本程序只负责**发操作**并对目录枚举做断言。文件内容与磁盘副作用的判据一律由
// test/e2e/smoke.sh 在服务端磁盘上独立核对 —— 客户端说成功不算数。
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hirochachacha/go-smb2"
)

func main() {
	if len(os.Args) < 8 {
		fmt.Fprintln(os.Stderr, "用法: go run . <host> <port> <user> <pass> <share> <workdir> <前缀>")
		os.Exit(2)
	}
	host, port := os.Args[1], os.Args[2]
	user, pass, share := os.Args[3], os.Args[4], os.Args[5]
	work, p := os.Args[6], os.Args[7]

	if err := run(net.JoinHostPort(host, port), user, pass, share, work, p); err != nil {
		fmt.Fprintf(os.Stderr, "  [gosmb2] 失败: %v\n", err)
		os.Exit(1)
	}
}

func run(addr, user, pass, share, work, p string) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("拨号: %w", err)
	}
	defer conn.Close()

	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: pass}}
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
	fmt.Println("  [gosmb2] 挂载 OK")

	// --- 1. 目录枚举。磁盘看不出「客户端有没有看见」，只能在客户端断言。
	ents, err := fs.ReadDir(".")
	if err != nil {
		return fmt.Errorf("ReadDir: %w", err)
	}
	seen := make(map[string]bool, len(ents))
	for _, e := range ents {
		seen[e.Name()] = true
	}
	for _, want := range []string{
		"seed-" + p + ".bin", "mov-" + p + ".bin", "del-" + p + ".bin",
		"rmd-" + p, "subdir",
	} {
		if !seen[want] {
			return fmt.Errorf("目录列表里没有 %s: %v", want, keys(seen))
		}
	}
	// 反向断言：不存在的名字绝不能出现（防「把请求里的名字原样回显」的实现）。
	if seen["nosuch-"+p+".bin"] {
		return fmt.Errorf("目录列表里出现了根本不存在的 nosuch-%s.bin", p)
	}

	// --- 2. 下载。落到 workdir，由驱动做整字节比对。
	body, err := fs.ReadFile("seed-" + p + ".bin")
	if err != nil {
		return fmt.Errorf("读 seed: %w", err)
	}
	if err := os.WriteFile(filepath.Join(work, "got-"+p+".bin"), body, 0o644); err != nil {
		return fmt.Errorf("落盘 got: %w", err)
	}

	// --- 3. 上传。上传出来的 up-*.bin 之后不再被任何操作碰，避免自毁证据。
	payload, err := os.ReadFile(filepath.Join(work, "up-"+p+".bin"))
	if err != nil {
		return fmt.Errorf("读上传载荷: %w", err)
	}
	if err := fs.WriteFile("up-"+p+".bin", payload, 0o644); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}

	// --- 4. 建目录。
	if err := fs.Mkdir("dir-"+p, 0o755); err != nil {
		return fmt.Errorf("Mkdir: %w", err)
	}

	// --- 5. 重命名（跨目录）。mov-*.bin 由驱动预先放在共享目录里。
	if err := fs.Rename("mov-"+p+".bin", "dir-"+p+"/moved-"+p+".bin"); err != nil {
		return fmt.Errorf("Rename: %w", err)
	}

	// --- 6. 删文件（对象由驱动预先放好）。
	if err := fs.Remove("del-" + p + ".bin"); err != nil {
		return fmt.Errorf("Remove 文件: %w", err)
	}

	// --- 7. 删目录（对象由驱动预先放好，是空目录）。
	if err := fs.Remove("rmd-" + p); err != nil {
		return fmt.Errorf("Remove 目录: %w", err)
	}

	fmt.Println("  [gosmb2] 七个操作全部发送完毕")
	return nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
