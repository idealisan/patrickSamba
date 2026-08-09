package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	registerCreateContext(createContextSpec{
		Names: []string{wire.CreateContextAAPL},
		Order: createContextOrderAAPL,
		New: func(req *wire.CreateRequest) createContextHandler {
			return &aaplContextHandler{req: req}
		},
	})
}

// aaplContextHandler 把 aapl.go 的 negotiateAAPL 接进注册表。
//
// 协商必须发生在 Parse 阶段（真正打开文件**之前**）：与 Samba vfs_fruit
// 的 check_aapl() 位置一致，且失败时没有句柄要回收。
type aaplContextHandler struct {
	createContextBase
	req  *wire.CreateRequest
	resp []byte
}

func (h *aaplContextHandler) Parse(ctx *Context, _ string, _ []byte) error {
	out, err := negotiateAAPL(ctx, h.req)
	if err != nil {
		return err
	}
	h.resp = out
	return nil
}

func (h *aaplContextHandler) Respond(_ *Context, _ *Open, resp *wire.CreateResponse, _ *vfs.Attr) error {
	if len(h.resp) == 0 {
		return nil
	}
	resp.Contexts = append(resp.Contexts, wire.CreateContext{Name: wire.CreateContextAAPL, Data: h.resp})
	return nil
}
