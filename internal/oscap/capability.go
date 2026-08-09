package oscap

// capability.go —— 能力枚举与适配器种类。
//
// 「能力」的划分粒度就是**降级的粒度**：矩阵按 Capability 逐项决定走 native
// 还是 builtin，所以一项能力必须是「要么整套能用、要么整套不能用」的最小单元。
// 例如 SparseFile 的四个方法绑在一起：只能查空洞却不能打洞的实现是没有意义的
// （Time Machine 要的就是打洞回收），这种半吊子必须整项判为不支持，
// 让 builtin 接管，而不是留一半 native 一半报错。

// Capability 是一项操作系统可选能力。
type Capability int

// 能力清单。新增能力时必须**同时**给出 native 与 builtin 两份实现
// （AGENTS.md §1.2「不许赊账」），并在 capabilityNames 里登记名字。
const (
	// CapXattr 扩展属性。macOS 的 com.apple.* 元数据、Netatalk 兼容的
	// FinderInfo blob 都落在这里。
	CapXattr Capability = iota

	// CapSparseFile 稀疏文件：打洞、预分配、已分配区间查询。
	CapSparseFile

	// CapNamedStream 命名流（alternate data stream）：
	// SMB 的 `file:AFP_Resource:$DATA` 一类。
	CapNamedStream

	// CapStableFileID 卷内稳定唯一 ID，对应 FileInternalInformation / QFid。
	// 要求同一对象**跨重命名**返回同一个值。
	CapStableFileID

	// CapCreationTime 真实创建时间（birth time），不是 POSIX 的 ctime。
	CapCreationTime

	// CapDOSAttributes DOS 属性位（FILE_ATTRIBUTE_HIDDEN / SYSTEM / ARCHIVE …）。
	CapDOSAttributes

	// capCount 是能力总数，必须永远排在最后。
	capCount
)

// capabilityNames 是能力的稳定字符串名。
//
// 这些名字会出现在日志、启动错误与 Matrix.String() 里，属于**对外可见的
// 稳定标识**（运维会照着它搜文档、写监控）。改名等于破坏兼容，别顺手改。
var capabilityNames = [capCount]string{
	CapXattr:         "xattr",
	CapSparseFile:    "sparse_file",
	CapNamedStream:   "named_stream",
	CapStableFileID:  "stable_file_id",
	CapCreationTime:  "creation_time",
	CapDOSAttributes: "dos_attributes",
}

func (c Capability) String() string {
	if c < 0 || c >= capCount {
		return "capability(?)"
	}
	return capabilityNames[c]
}

// Valid 判断是否为已定义的能力。
func (c Capability) Valid() bool { return c >= 0 && c < capCount }

// Capabilities 返回全部能力，按声明顺序。
//
// 每次返回新切片：调用方（含测试）经常就地排序或过滤，共享底层数组会串味。
func Capabilities() []Capability {
	out := make([]Capability, 0, capCount)
	for c := Capability(0); c < capCount; c++ {
		out = append(out, c)
	}
	return out
}

// Kind 是适配器种类。
type Kind uint8

const (
	// KindBuiltin 是本项目自实现的适配器（只用普通文件 + 套接字）。
	//
	// **零值刻意落在 builtin 这一侧**：忘记赋值的字段会退化成「哪儿都能跑」
	// 的那个实现，而不是退化成「可能不存在」的那个。默认值必须落在安全侧。
	KindBuiltin Kind = iota

	// KindNative 是借助 OS 可选能力的适配器。
	KindNative
)

func (k Kind) String() string {
	switch k {
	case KindNative:
		return "native"
	case KindBuiltin:
		return "builtin"
	default:
		return "kind(?)"
	}
}
