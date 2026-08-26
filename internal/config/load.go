package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

// Load 从 path 读取 YAML 配置，填默认值并完成校验。
//
// 严格模式：**未知字段一律报错**。配置项拼错却被静默忽略是运维灾难
// （用户以为改了设置，实际服务在用默认值）。
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("未指定配置文件路径")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	return Parse(data)
}

// Parse 解析一段 YAML 字节，填默认值并校验。
func Parse(data []byte) (*Config, error) {
	c, err := Decode(data)
	if err != nil {
		return nil, err
	}
	ApplyDefaults(c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Decode 只做 YAML 反序列化，不填默认值、不校验。
//
// 单独导出是为了让测试能分别验证「解析」与「校验」两个阶段。
func Decode(data []byte) (*Config, error) {
	data, err := stripBOM(data)
	if err != nil {
		return nil, err
	}
	var c Config
	// yaml.Strict() 启用 DisallowUnknownField。
	// 重复键在 goccy/go-yaml v1.19 起默认就报错（需要 AllowDuplicateMapKey 才放行），
	// 正合我们的需要：配置里出现重复键一定是笔误。
	opts := []yaml.DecodeOption{
		yaml.Strict(),
	}
	if err := yaml.UnmarshalWithOptions(data, &c, opts...); err != nil {
		// goccy/go-yaml 的错误里带有行号与源码片段，格式化出来对用户友好得多。
		return nil, fmt.Errorf("解析配置文件失败:\n%s", yaml.FormatError(err, false, true))
	}
	return &c, nil
}

// stripBOM 剥掉文件开头的字节序标记（BOM），返回可直接交给 YAML 解析器的字节。
//
// 背景：Windows 上用记事本、PowerShell 重定向等编辑/生成配置是最常见的使用路径，
// 它们保存 UTF-8 时经常附加 BOM。YAML 规范不允许文档以 BOM 开头，
// goccy/go-yaml 会报 "[1:1] unexpected key name"，用户对着一个看不见的字符毫无办法
// （v0.5.0 Windows 包的真实用户报告）。UTF-8 BOM 直接剥掉；
// UTF-16 BOM 意味着整份文件都是宽字节编码，没法只剥三个字节了事，
// 给出人话报错让用户另存为 UTF-8。
func stripBOM(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return data[3:], nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}),
		bytes.HasPrefix(data, []byte{0x00, 0x00, 0xFE, 0xFF}):
		return nil, errors.New("配置文件是 UTF-32 编码: 请用编辑器把它另存为「UTF-8」后重试")
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return nil, errors.New("配置文件是 UTF-16 编码: 请用编辑器把它另存为「UTF-8」后重试")
	default:
		return data, nil
	}
}
