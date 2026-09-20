package command

import (
	"math"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

func init() {
	registerCreateContext(createContextSpec{
		Names: []string{wire.CreateContextAlSi},
		Order: createContextOrderAlSi,
		New: func(req *wire.CreateRequest) createContextHandler {
			return &alSiHandler{req: req}
		},
	})
}

// alSiHandler 处理 "AlSi"（SMB2_CREATE_ALLOCATION_SIZE，MS-SMB2 §2.2.13.2.2）。
// 它没有响应 context，只在 Opened 阶段做一次预留。
type alSiHandler struct {
	createContextBase
	req *wire.CreateRequest
}

// Opened 必须跑在 Stat **之前**，响应里的 AllocationSize 才是预留之后的值。
func (h *alSiHandler) Opened(ctx *Context, fh vfs.Handle, action vfs.Action) error {
	applyAllocationSize(ctx, h.req, fh, action)
	return nil
}

// applyAllocationSize 处理 "AlSi" create context（MS-SMB2 §2.2.13.2.2）：
// 客户端要求为新建的文件**预留**这么多磁盘空间。
//
// 为什么要实现：Time Machine 的 .sparsebundle 由成千上万个固定大小
// （通常 8 MiB）的 band 文件组成，macOS 建每个 band 时都会带 AlSi。
// 不预留的话这些文件在 ext4 上会被写得很碎，后续顺序读整个备份会明显变慢。
//
// 语义与 Windows 的 AllocationSize 一致：**只占块、不改 EOF**
// （vfs 的 Preallocate 用 FALLOC_FL_KEEP_SIZE 实现）。
//
// 失败一律只记日志不报错：预留不成功文件照样能用，只是可能更碎片化；
// 为此让整个 CREATE 失败得不偿失。后端不支持（如 Windows/其它平台）同理。
func applyAllocationSize(ctx *Context, req *wire.CreateRequest, h vfs.Handle, action vfs.Action) {
	data, ok := wire.FindCreateContext(req.Contexts, wire.CreateContextAlSi)
	if !ok {
		return
	}
	size, err := wire.AllocationSizeContext(data)
	if err != nil {
		ctx.Log.Debug("AlSi create context 长度非法", "len", len(data))
		return
	}
	// 超出 int64 的值只可能是畸形输入（AGENTS.md §8：用之前先校验边界）。
	if size == 0 || size > math.MaxInt64 {
		return
	}
	// 只在文件被新建/覆盖时预留。对一个**已存在**的文件做预留会悄悄改变
	// 它的磁盘占用，而客户端此时只是想打开它。
	switch action {
	case vfs.ActionCreated, vfs.ActionOverwritten, vfs.ActionSuperseded:
	default:
		return
	}

	sp, ok := h.(vfs.SparseFile)
	if !ok {
		return
	}
	if err := sp.Preallocate(0, int64(size)); err != nil {
		ctx.Log.Debug("AlSi 预留空间失败", "size", size, "err", err)
	}
}
