package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	registerCreateContext(createContextSpec{
		Names: []string{wire.CreateContextQFid},
		Order: createContextOrderQFid,
		New:   func(*wire.CreateRequest) createContextHandler { return qfidHandler{} },
	})
}

// qfidHandler 处理 "QFid"（SMB2_CREATE_QUERY_ON_DISK_ID，MS-SMB2 §2.2.13.2.9
// 请求 / §2.2.14.2.9 响应）。
//
// 请求侧没有载荷可解析，出现本身就是「请把稳定 FileId 告诉我」；
// macOS 与 Windows 都会带，回它可以让客户端少发一轮 QUERY_INFO。
type qfidHandler struct{ createContextBase }

// Respond 回 DiskFileId。VolumeId 与 Reserved 恒 0 —— 我们没有稳定的卷标识，
// 填个假值不如留 0（客户端只把 DiskFileId 当句柄去重的键）。
func (qfidHandler) Respond(_ *Context, _ *Open, resp *wire.CreateResponse, attr *vfs.Attr) error {
	resp.Contexts = append(resp.Contexts, wire.CreateContext{
		Name: wire.CreateContextQFid,
		Data: wire.DiskIDContext{DiskFileID: attr.FileID}.Encode(),
	})
	return nil
}
