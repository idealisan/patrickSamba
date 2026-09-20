package command

import (
	"fmt"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

// create_context.go —— create context 注册表。
//
// CREATE 是唯一一条「在一个命令里塞进任意多个子协议」的命令：AlSi 要预留
// 磁盘空间、MxAc 要回报最大访问、AAPL 要协商 Apple 扩展、DHnQ/DH2Q 要申请
// 持久句柄、RqLs 要申请 lease……全写进 createFile 只会让那个函数无限膨胀，
// 而且多个 agent 会在同一段 if 链上互相覆盖。
//
// 这里把每一种 create context 抽成一个注册项（AGENTS.md §5 P2 策略模式，
// 与 dispatch.go 的命令分发表同构）：**新增一种 create context 只需要在
// 自己的文件里 init() 调 registerCreateContext，不改 create.go。**
//
// # 四个阶段
//
// 阶段的划分不是设计出来的，是 createFile 里四个语义不同的时点决定的：
//
//	Parse       打开文件**之前**        校验载荷、做协商。此时还没有句柄要回收，
//	                                    返回错误可以直接让整个 CREATE 失败。
//	Opened      fs.Open 之后、Stat 之前  需要真实句柄、且结果必须反映进响应属性
//	                                    的（AlSi 预留完才 Stat，AllocationSize
//	                                    才是预留后的值）。
//	Registered  Session.AddOpen 之后     需要一个完整的 Open（durable / lease）。
//	Respond     编码响应时               产出响应 context。
//
// # 两条不可动摇的规矩
//
//  1. **不认识的 create context 必须静默忽略**（MS-SMB2 §3.3.5.9）。
//     本注册表天然满足：没注册过的名字压根匹配不到 handler，
//     既不报错也不进响应链。
//  2. **响应链的次序由 spec.Order 固定，不跟随请求顺序。**
//     规范没有规定次序（客户端按名字查找），固定它纯粹是为了让 golden test
//     可比 —— 见 create_context_test.go。

// createContextHandler 处理一种（或一组相关的）create context。
//
// 注册表在每次 CREATE 里为「请求中确实出现了的」名字实例化一个 handler，
// 四个阶段依次回调，中间状态由 handler 自己保存（所以它是每请求一个实例，
// 不是单例）。绝大多数 handler 只关心其中一两个阶段，内嵌
// createContextBase 即可只覆盖需要的那些。
type createContextHandler interface {
	// Parse 在打开文件之前调用。请求链里每出现一个本 handler 认领的名字
	// 就调用一次，name 是该名字，data 是它的载荷（可能为空）。
	//
	// 返回非 nil 会让**整个 CREATE 失败**，因此只用于规范要求硬拒的畸形
	// 载荷（如 MxAc 长度非法）。不确定的一律放行。
	Parse(ctx *Context, name string, data []byte) error

	// Opened 在 fs.Open 成功之后、Stat 之前调用。
	Opened(ctx *Context, h vfs.Handle, action vfs.Action) error

	// Registered 在句柄登记进会话句柄表之后调用。
	Registered(ctx *Context, open *Open) error

	// Respond 把要回给客户端的响应 context 追加进 resp.Contexts。
	//
	// 它拿到的是**完整**的 *wire.CreateResponse：lease 模块借此覆写
	// resp.OplockLevel（授予 lease 时写 SMB2_OPLOCK_LEVEL_LEASE=0xFF，
	// 授予 oplock 时写 II/Exclusive/Batch），而不仅是追加 context。
	// 响应名字由 handler 自己决定 —— 它未必等于请求名字（重连用 DHnC/DH2C
	// 请求，但响应回的是 DHnQ，见 wire/durable_context.go 的说明）。
	//
	// 返回非 nil 会让**整个 CREATE 失败**（例如 lease 状态位非法，
	// MS-SMB2 §3.3.5.9.11）。createFile 会据此回收已登记的句柄。
	Respond(ctx *Context, open *Open, resp *wire.CreateResponse, attr *vfs.Attr) error
}

// createContextBase 是 createContextHandler 的全空实现。
// 具体 handler 内嵌它之后只需覆盖自己关心的阶段。
type createContextBase struct{}

func (createContextBase) Parse(*Context, string, []byte) error          { return nil }
func (createContextBase) Opened(*Context, vfs.Handle, vfs.Action) error { return nil }
func (createContextBase) Registered(*Context, *Open) error              { return nil }

func (createContextBase) Respond(*Context, *Open, *wire.CreateResponse, *vfs.Attr) error {
	return nil
}

// createContextSpec 是一种 create context 的注册项。
type createContextSpec struct {
	// Names 是本 handler 认领的 create context 名字（4 字节 ASCII）。
	//
	// 一个 handler 可以认领多个名字 —— 持久句柄的 DHnQ/DHnC/DH2Q/DH2C
	// 之间有互斥与配对规则，必须由同一个实例统一判别，拆成四个 handler
	// 就没法互相看见了。
	Names []string

	// Order 决定**响应链**中的相对次序（升序）。见文件头第 2 条规矩。
	Order int

	// New 为每次 CREATE 创建一个 handler 实例。
	//
	// 传入完整的 CreateRequest 是有意的：不少 context 的语义取决于
	// context 之外的字段（durable 要看 RequestedOplockLevel，
	// AlSi 要看 CreateDisposition）。
	New func(req *wire.CreateRequest) createContextHandler
}

// 响应链次序。规范未规定次序，这些值只用来把顺序钉死，让 golden test 可比。
//
// 前三个的相对次序是**历史沿革**：重构前 createResponseContexts 就是按
// QFid → MxAc → AAPL 硬编码的，改动它会让 create_context_test.go 的
// golden 失配。没有响应 context 的（AlSi）排在后面，值本身不影响任何东西。
const (
	createContextOrderQFid    = 10
	createContextOrderMxAc    = 20
	createContextOrderAAPL    = 30
	createContextOrderAlSi    = 40
	createContextOrderDurable = 50
	// createContextOrderLease 留给 tm-lease 的 RqLs。值大于 Durable：
	// 若一次 CREATE 同时申请 lease 与 durable（§3.3.5.9.6 的常见组合），
	// 让 lease 的 Respond 最后跑、覆写 OplockLevel 后 durable 再读状态，
	// 次序更自然。具体值不影响任何线上行为。
	createContextOrderLease = 60
)

// createContextSpecs 按 Order 升序保存全部注册项。
var createContextSpecs []*createContextSpec

// createContextByName 是名字到注册项的索引。
var createContextByName = map[string]*createContextSpec{}

// registerCreateContext 登记一种 create context。必须在 init() 里调用。
//
// 重复注册同一个名字会 panic —— 这是编码错误，发生在进程启动阶段，
// 不会影响运行中的服务（与 dispatch.go 的 register 同样处理）。
func registerCreateContext(spec createContextSpec) {
	if len(spec.Names) == 0 || spec.New == nil {
		panic("command: create context 注册项缺少 Names 或 New")
	}
	s := &spec
	for _, n := range s.Names {
		if _, dup := createContextByName[n]; dup {
			panic(fmt.Sprintf("command: create context %q 重复注册", n))
		}
		createContextByName[n] = s
	}
	// 按 Order 升序插入。注册发生在 init，条目数是个位数，线性插入足够。
	i := len(createContextSpecs)
	for i > 0 && createContextSpecs[i-1].Order > s.Order {
		i--
	}
	createContextSpecs = append(createContextSpecs, nil)
	copy(createContextSpecs[i+1:], createContextSpecs[i:])
	createContextSpecs[i] = s
}

// createContexts 是一次 CREATE 里被激活的 handler 集合，按响应链次序排列。
type createContexts struct {
	// req 是本次 CREATE 的完整请求，构造 handler 时传给 spec.New。
	req    *wire.CreateRequest
	active []activeCreateContext
}

// activeCreateContext 是一个被激活的注册项及其本次请求的 handler 实例。
type activeCreateContext struct {
	spec *createContextSpec
	h    createContextHandler
}

// newCreateContexts 扫描请求的 context 链，实例化命中的 handler 并跑完
// Parse 阶段。
//
// **解析次序跟随客户端给的链顺序**（先出现的先解析，先失败的决定
// NTSTATUS）；重复出现的同名 context 只处理第一次，与
// wire.FindCreateContext 的语义保持一致。
// 响应次序另由 spec.Order 决定，与这里无关。
func newCreateContexts(ctx *Context, req *wire.CreateRequest) (*createContexts, error) {
	cc := &createContexts{req: req}
	byName := make(map[string]createContextHandler, len(req.Contexts))

	for _, c := range req.Contexts {
		spec, ok := createContextByName[c.Name]
		if !ok {
			// 不认识的 context：静默忽略（MS-SMB2 §3.3.5.9）。
			continue
		}
		if _, seen := byName[c.Name]; seen {
			continue
		}
		h := cc.handlerFor(spec)
		byName[c.Name] = h
		if err := h.Parse(ctx, c.Name, c.Data); err != nil {
			return nil, err
		}
	}
	return cc, nil
}

// handlerFor 取本次请求中 spec 对应的 handler 实例，没有就新建并按 Order
// 插入 active。
func (c *createContexts) handlerFor(spec *createContextSpec) createContextHandler {
	for _, a := range c.active {
		if a.spec == spec {
			return a.h
		}
	}
	h := spec.New(c.req)
	i := len(c.active)
	for i > 0 && c.active[i-1].spec.Order > spec.Order {
		i--
	}
	c.active = append(c.active, activeCreateContext{})
	copy(c.active[i+1:], c.active[i:])
	c.active[i] = activeCreateContext{spec: spec, h: h}
	return h
}

// opened 跑 Opened 阶段。
func (c *createContexts) opened(ctx *Context, h vfs.Handle, action vfs.Action) error {
	for _, a := range c.active {
		if err := a.h.Opened(ctx, h, action); err != nil {
			return err
		}
	}
	return nil
}

// registered 跑 Registered 阶段。
func (c *createContexts) registered(ctx *Context, open *Open) error {
	for _, a := range c.active {
		if err := a.h.Registered(ctx, open); err != nil {
			return err
		}
	}
	return nil
}

// respond 依次回调各 handler 的 Respond 阶段，把响应 context 追加进
// resp.Contexts（次序为 spec.Order 升序），并允许 handler 覆写 resp 的其它
// 字段（如 OplockLevel）。只要有一个 handler 返回错误，立即向上传递，
// 由 createFile 回收已登记的句柄。
func (c *createContexts) respond(ctx *Context, open *Open, resp *wire.CreateResponse, attr *vfs.Attr) error {
	for _, a := range c.active {
		if err := a.h.Respond(ctx, open, resp, attr); err != nil {
			return err
		}
	}
	return nil
}
