// Package native 是 oscap 能力 port 的**原生适配器**（AGENTS.md §1.2 C9 / §5 P7）。
//
// # 它做什么
//
// 借助宿主操作系统**既有**的能力，把 internal/oscap 定义的六项语义做出来：
// Linux 的 xattr / fallocate(PUNCH_HOLE) / statx(BTIME)、macOS 的
// st_birthtimespec、Windows 的 NTFS ADS 与 FSCTL_SET_ZERO_DATA。
// 快、省，而且**与宿主上的本地工具看到的是同一份东西** ——
// `getfattr`、`ls -l@`、`du --apparent-size` 与 SMB 客户端看到的一致。
//
// 与之相对，internal/oscap/builtin 只用「普通文件 + 旁路存储」把同一份语义
// 做出来，慢但哪儿都能跑。选哪一侧由 oscap 的**能力矩阵逐项**决定，
// 本包不关心，也**不做整体二选一的分支**。
//
// # 三条实现纪律
//
//  1. **做不到就如实说**：任何一个方法在当前宿主上做不到，一律返回
//     oscap.ErrNotSupported。绝不 panic，绝不「假装成功」——
//     「成功回显 ≠ 事情真的发生了」是本项目反复栽过的坑。
//  2. **做不到的整项留 nil**：一项能力若在本平台没有原生实现（例如 POSIX 上
//     的 DOS 属性位），就让 Set 里对应字段保持 nil，让矩阵退到 builtin。
//     **不要**塞一个所有方法都返回 ErrNotSupported 的空壳 —— 那会让
//     provider 误以为 native 侧提供了这项能力（provider.Set.has 只判 nil），
//     于是 auto 模式下 builtin 永远接不到手，运行期才炸。
//  3. **与 probe 保持一致**：internal/oscap/probe_*.go 里每一项的判定，
//     对应的就是本包**到底有没有做**这一项。补齐一项实现时必须回去同步放开
//     探测（那份耦合是刻意的，见 probe.go 的说明）；反过来，探测报 true
//     而这里留 nil，会让 filesystem_mode: native「启动通过、运行时降级」，
//     正是 §1.2 要禁止的静默失效。
//
// # 路径与安全边界
//
// oscap.Ref.Path 是**已经过 vfs 层根目录约束校验**的宿主机路径
// （见 oscap 包注释「路径契约」），本包不重复做路径穿越校验。
// 但**由客户端控制、会被拼进宿主机名字空间的字符串**（命名流名字、
// 扩展属性名）仍然是不可信输入，本包自己校验 —— 它们会变成 xattr 名
// 或 `file:stream:$DATA` 路径，放行分隔符等于把写入引到别处去（AGENTS.md §8）。
package native

import (
	"fmt"
	"math"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// New 构造本平台的原生能力集合，是本包**唯一**的导出入口。
//
// 签名与 oscap.Factory 一致，装配层直接把它传给 oscap.Open。
//
// 返回错误的条件很窄：**只有真正的构造失败**（目前各平台都不需要在构造期
// 分配资源，所以实际只会因 Options 非法而失败）。某一项能力在本宿主上不可用
// **不是**错误 —— 那会让整个 native 侧被判定为不可用（provider.New 会
// 整体退到 builtin），把「一项降级」
// 放大成「全部降级」，与逐项矩阵的设计相悖。
func New(o oscap.Options) (oscap.Set, error) {
	if err := o.Validate(); err != nil {
		return oscap.Set{}, err
	}
	return newSet(o)
}

// checkWindow 校验一个 [off, off+length) 区间参数。
//
// 溢出必须在这里就拦住：off+length 溢出成负数后，下游的
// fallocate / DeviceIoControl 拿到的是一个完全不同的区间，
// 那会变成一次**越界写**（AGENTS.md §8「任何来自网络的 offset/length
// 字段在使用前必须校验边界，防止越界读与整数溢出」）。
func checkWindow(off, length int64) error {
	if off < 0 {
		return fmt.Errorf("oscap/native: offset %d 为负: %w", off, oscap.ErrInvalidArg)
	}
	if length < 0 {
		return fmt.Errorf("oscap/native: length %d 为负: %w", length, oscap.ErrInvalidArg)
	}
	if off > math.MaxInt64-length {
		return fmt.Errorf("oscap/native: offset %d + length %d 溢出: %w",
			off, length, oscap.ErrInvalidArg)
	}
	return nil
}

// validateStreamName 校验命名流名字里的字符。
//
// 这是一处**安全边界**（AGENTS.md §8）：流名由客户端控制，会被直接拼进
// 宿主机的名字空间 —— POSIX 上变成 xattr 名，Windows 上变成
// `文件:流:$DATA` 这样的路径。放行分隔符等于让客户端把写入引到别的对象上去。
//
// **长度上限由各平台自己加**：POSIX 上受 xattr 名总长约束（234 字节），
// NTFS 上是 255 个字符，两者预算来源不同，不该硬凑成一个数。
func validateStreamName(name string) error {
	if name == "" {
		// 空串是主数据流，走普通文件 IO，不归本能力管（ports.go 明文规定）。
		return fmt.Errorf("oscap/native: 命名流名为空: %w", oscap.ErrInvalidArg)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("oscap/native: 命名流名 %q 非法: %w", name, oscap.ErrInvalidArg)
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c < 0x20:
			// 控制字符（含 NUL）。NUL 尤其致命：它会被内核当成名字结束，
			// 于是 "a\x00b" 与 "a" 落到同一个位置。
			return fmt.Errorf("oscap/native: 命名流名含控制字符 0x%02x: %w",
				c, oscap.ErrInvalidArg)
		case c == '/', c == '\\', c == ':':
			// 裸冒号在 SMB 线格式里是流分隔符，**合法请求里根本不会出现**：
			// macOS 客户端把非法字符映射到私用区（`:` → U+F022，
			// 不是 U+F03A），见 vfs/stream_xattr.go 里那张映射表。
			// 所以拒绝裸冒号不会误伤真实客户端。
			return fmt.Errorf("oscap/native: 命名流名含分隔符 %q: %w",
				string(c), oscap.ErrInvalidArg)
		}
	}
	return nil
}

// wholeWindow 把 [off, end) 整段报成「已分配」。
//
// 这是 AllocatedRanges 在**探测不可用**时的降级答案，出处见 ports.go：
// 多报已分配是安全的（客户端最多多读一遍零），少报会让客户端以为数据丢了。
// 所以这里绝不能改成返回错误或空列表。
func wholeWindow(off, end int64) []oscap.Range {
	if end <= off {
		return nil
	}
	return []oscap.Range{{Offset: off, Length: end - off}}
}
