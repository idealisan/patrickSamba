// Command stupidsamba 是一个纯 Go 实现的 SMB/CIFS 文件共享服务器。
//
// 约束见 AGENTS.md：禁止 CGO、禁止外部动态库、禁止依赖外部进程。
package main

import (
	"flag"
	"fmt"
	"os"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "", "配置文件路径 (YAML)")
		showVersion = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("stupidsamba %s\n", version)
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "stupidsamba: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	// TODO(cmd): 加载配置 -> 构建 shares -> 启动 SMB server -> 启动 mDNS -> 等待信号优雅退出
	if configPath == "" {
		return fmt.Errorf("必须通过 -config 指定配置文件")
	}
	return fmt.Errorf("尚未实现：服务启动装配")
}
