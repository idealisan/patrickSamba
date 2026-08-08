package vfs

// time.go —— Windows FILETIME 与 Go time.Time 的双向转换。
//
// FILETIME（MS-DTYP §2.3.3）是自 **1601-01-01 00:00:00 UTC** 起的
// **100 纳秒**间隔数，64 位无符号。SMB2 报文里所有时间字段都是它
// （小端序，但字节序是 wire 层的事，本层只做数值转换）。
//
// 为什么不用 time.Time.UnixNano()：UnixNano 只在 1678~2262 年之间有效，
// 超出范围会静默溢出。FILETIME 的值域远大于此（客户端会发
// 0x7FFFFFFFFFFFFFFF 表示「永不」），必须用 Unix()+Nanosecond() 组合运算
// 并显式做边界钳制。

import "time"

const (
	// filetimeEpochDelta 是 1601-01-01 到 1970-01-01 之间的 100ns 间隔数。
	// 见 docs/protocol-notes.md §11：FILETIME = unixNanos/100 + 116444736000000000。
	filetimeEpochDelta = 116444736000000000

	// filetimeEpochSecs 是同一间隔的秒数（116444736000000000 / 1e7）。
	filetimeEpochSecs = 11644473600

	// hundredNSPerSecond 是每秒的 100ns 间隔数。
	hundredNSPerSecond = 10000000

	// maxFiletime 是本实现允许的最大 FILETIME。
	// Windows 用 0x7FFFFFFFFFFFFFFF 表示「无穷远 / 永不过期」，
	// 超过它的输入一律钳到这里，避免后续换算溢出。
	maxFiletime uint64 = 0x7FFFFFFFFFFFFFFF
)

// FiletimeUnspecified 表示「未指定」。
//
//   - 在 SET_INFO（FileBasicInformation，MS-FSCC §2.4.7）中表示**不修改**该时间；
//   - 在查询响应中表示该时间未知。
const FiletimeUnspecified uint64 = 0

// FiletimeNoChange（-1）在 SET_INFO 中表示**不修改**该时间，
// 并额外要求在该句柄的生命周期内**停止自动更新**这个时间戳
// （MS-FSCC §2.4.7 FileBasicInformation）。
const FiletimeNoChange uint64 = 0xFFFFFFFFFFFFFFFF

// FiletimeIsSet 判断一个来自客户端的 FILETIME 字段是否要求真正修改时间。
// 0 与 -1 都表示「别动」。
func FiletimeIsSet(ft uint64) bool {
	return ft != FiletimeUnspecified && ft != FiletimeNoChange
}

// FiletimeToTime 把 FILETIME 转成 UTC 的 time.Time。
//
// ft == 0 返回**零值** time.Time（IsZero() 为真），表示「未指定」；
// 调用方应当自行判断而不是把它当成 1601 年。
func FiletimeToTime(ft uint64) time.Time {
	if ft == FiletimeUnspecified {
		return time.Time{}
	}
	if ft > maxFiletime {
		ft = maxFiletime
	}
	secs := int64(ft/hundredNSPerSecond) - filetimeEpochSecs
	nsec := int64(ft%hundredNSPerSecond) * 100
	return time.Unix(secs, nsec).UTC()
}

// TimeToFiletime 把 time.Time 转成 FILETIME。
//
//   - 零值 time.Time 返回 0（「未指定」），与 FiletimeToTime 互逆；
//   - 早于 1601-01-01 的时间钳到 0（FILETIME 无法表示）；
//   - 超出 FILETIME 值域的时间钳到 maxFiletime。
//
// 注意精度：FILETIME 的粒度是 100ns，Go 的 time.Time 是 1ns，
// 转换会**向下取整**丢掉最低两位十进制纳秒。这是规范决定的，不是 bug。
func TimeToFiletime(t time.Time) uint64 {
	if t.IsZero() {
		return FiletimeUnspecified
	}
	secs := t.Unix()
	if secs < -filetimeEpochSecs {
		// 早于 1601 年，FILETIME 无法表示。
		return FiletimeUnspecified
	}
	total := secs + filetimeEpochSecs
	// 溢出检查：total 秒换算成 100ns 后不得超过 maxFiletime。
	if uint64(total) > maxFiletime/hundredNSPerSecond {
		return maxFiletime
	}
	ft := uint64(total)*hundredNSPerSecond + uint64(t.Nanosecond()/100)
	if ft > maxFiletime {
		return maxFiletime
	}
	return ft
}
