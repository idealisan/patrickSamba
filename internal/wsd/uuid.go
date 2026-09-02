package wsd

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
)

// ---------------------------------------------------------------------------
// UUID：设备标识与消息标识
//
// 两者用途不同，生成方式也不同，别混用：
//
//	设备标识   **稳定**（重启不变）—— Windows 靠它判断"是不是同一台机器"。
//	           每次启动换一个的话，网络列表里会攒下一堆同名的僵尸条目。
//	消息标识   **随机**（每条不同）—— 客户端按它去重，重复会被丢弃。
// ---------------------------------------------------------------------------

// nsStupidSamba 是本项目派生设备 UUID 用的命名空间（一个固定的 UUIDv4 字面量）。
//
// 刻意**不用**标准的 DNS / URL 命名空间：那两个是公共的，别人也可能用
// (命名空间, "MYHOST") 派生出同样的 UUID，撞了会让两台不同机器在 Windows
// 网络里被当成同一台。
var nsStupidSamba = [16]byte{
	0x5f, 0x8a, 0x1c, 0x62, 0x9d, 0x34, 0x4b, 0x0e,
	0xa7, 0x51, 0x2c, 0xd9, 0x83, 0xf0, 0x6b, 0x15,
}

// DeriveUUID 按名字派生一个**稳定**的 UUIDv5（RFC 4122 §4.3）。
//
// 输入通常是「服务器名 + 主机名」：同一台机器、同一个配置，每次启动都得到
// 同一个值，于是 Windows 网络列表里的条目能跨重启保持是同一台。
// 这样既拿到了稳定性，又**不需要落盘任何状态文件** —— 无状态是这个项目的
// 一贯取向（AGENTS.md C9：OS 只当文件系统用，不额外依赖）。
func DeriveUUID(name string) string {
	h := sha1.New()
	h.Write(nsStupidSamba[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var b [16]byte
	copy(b[:], sum[:16])
	// 置版本位（0101 = v5）与变体位（RFC 4122 §4.1.1）。
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// newMessageID 生成一个随机 UUIDv4，供响应的 wsa:MessageID 使用。
//
// 不能复用设备的稳定 UUID，也不能复用请求的 MessageID —— 见文件头注释。
func newMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成消息 UUID 失败: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // 版本 4
	b[8] = (b[8] & 0x3f) | 0x80 // 变体 RFC 4122
	return formatUUID(b), nil
}

// formatUUID 按 8-4-4-4-12 排版成小写十六进制。
func formatUUID(b [16]byte) string {
	hexed := hex.EncodeToString(b[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" +
		hexed[16:20] + "-" + hexed[20:32]
}
