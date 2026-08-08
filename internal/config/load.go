package config

import (
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
