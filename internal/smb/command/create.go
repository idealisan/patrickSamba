package command

import (
	"errors"
	"strings"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandCreate, true, true, handleCreate)
}

// handleCreate 处理 SMB2 CREATE（MS-SMB2 §3.3.5.9）。
//
// CREATE 是整个协议里语义最重的命令：它同时承担 open / create / truncate /
// mkdir / delete-on-close 五件事，具体做哪件由 CreateDisposition 与
// CreateOptions 的组合决定。这里把这些组合翻译成 vfs.OpenRequest，
// 真正的路径校验与 IO 由 VFS 层负责（AGENTS.md §8：路径穿越防御统一在 VFS）。
func handleCreate(ctx *Context) error {
	req, err := wire.ParseCreateRequest(ctx.Msg)
	if err != nil {
		ctx.Log.Debug("CREATE 请求解析失败", "err", err)
		return status.InvalidParameter
	}

	if ctx.Tree.IsIPC() {
		return createPipe(ctx, req)
	}
	return createFile(ctx, req)
}

// createFile 在磁盘共享上打开/创建对象。
func createFile(ctx *Context, req *wire.CreateRequest) error {
	fs := ctx.Tree.FS()
	if fs == nil {
		return status.NetworkNameDeleted
	}

	// 不支持按 FileId 打开（需要一张全卷 FileId→路径表，且客户端有回退路径）。
	if req.CreateOptions&wire.FileOpenByFileID != 0 {
		return status.NotSupported
	}

	path, stream, err := splitCreateName(req.Name)
	if err != nil {
		return err
	}

	// create context 的请求侧统一走注册表（见 create_context.go）。
	// 这一步在真正打开文件**之前**：AAPL 协商与 MxAc 长度校验都可能让整个
	// CREATE 失败，此时还没有句柄要回收。不认识的 context 在这里被静默忽略。
	cc, err := newCreateContexts(ctx, req)
	if err != nil {
		return err
	}

	// 持久句柄重连（DHnC / DH2C）短路正常 open 路径：直接认领登记表里
	// 等待重连的句柄，不打开任何文件。必须在 newCreateContexts 之后、
	// fs.Open 之前判断。
	if intent, ierr := wire.FindDurableIntent(req.Contexts); ierr != nil {
		return ierr
	} else if intent != nil && intent.IsReconnect() {
		return handleDurableReconnect(ctx, intent)
	}

	// GENERIC_* 要先展开成具体位，后续判定才有意义（MS-DTYP §2.4.3）。
	access := req.DesiredAccess.Expand()
	// MAXIMUM_ALLOWED：按本树允许的上限授予。授权依据是配置，不是宿主 ACL。
	if access&wire.MaximumAllowed != 0 {
		access = maximalAccessFor(ctx.Tree)
	}

	writing := access&(wire.FileWriteData|wire.FileAppendData|wire.FileWriteEA|
		wire.FileWriteAttributes|wire.Delete|wire.WriteDAC|wire.WriteOwner) != 0
	// 除 FILE_OPEN 外的所有 disposition 都会改动文件系统。
	mutating := req.CreateDisposition != wire.FileOpen
	if writing || mutating {
		if err := ctx.RequireWritable(); err != nil {
			return err
		}
	}

	// 共享模式预检（MS-SMB2 §3.3.5.9，算法见 share_access.go）。
	//
	// **必须在 fs.Open 之前**：FILE_SUPERSEDE / FILE_OVERWRITE* 会在 Open
	// 内部就把文件截断，等打开成功再判定冲突时别人的数据已经没了。
	// 目标不存在时 Stat 失败，此时不可能有已存在句柄，跳过即可 ——
	// 真正权威的判定是下面 shareModes.add 里那次原子检查。
	if access&conflictingAccess != 0 {
		if a, serr := fs.Stat(path); serr == nil {
			key := shareModeKey{fileID: a.FileID, stream: stream}
			if st := ctx.Tree.Share.shareModes.check(key, access, req.ShareAccess); st != status.Success {
				ctx.Log.Debug("CREATE 被共享模式拒绝（预检）",
					"path", path, "stream", stream,
					"access", uint32(access), "share", uint32(req.ShareAccess))
				return st
			}
		}
	}

	// bh3-F4 删除面：READONLY 目标不接受 FILE_DELETE_ON_CLOSE
	// （Samba can_set_delete_on_close，source3/smbd/file_access.c:192–224，
	// 回 NT_STATUS_CANNOT_DELETE；目录豁免，与 MxAc 口径一致）。
	//
	// 必须赶在 fs.Open **之前**判：openFlags 会把 FILE_DELETE_ON_CLOSE 翻译成
	// vfs.OpenDeleteOnClose，句柄一旦建立，vfs 层的关闭路径就会真的删文件 ——
	// 打开之后再回错，客户端收到 CANNOT_DELETE 而文件已经没了。
	// 判据同样是「属性快照」语义：这里取的是打开前最后一次 Stat 的快照，
	// 与 #196 写面用 open.FileAttributes 的口径等价（同一时刻的宿主实况）。
	// 新建即带 READONLY 位 + DELETE_ON_CLOSE 的矛盾组合不在此拦截：
	// 目标尚不存在时无快照可依，且该组合没有真实客户端会发。
	if req.CreateOptions&wire.FileDeleteOnClose != 0 {
		if a, serr := fs.Stat(path); serr == nil &&
			a.FileAttributes&vfs.FileAttributeDirectory == 0 &&
			a.FileAttributes&vfs.FileAttributeReadonly != 0 {
			return status.CannotDelete
		}
	}

	openReq := &vfs.OpenRequest{
		Path:           path,
		Stream:         stream,
		Flags:          openFlags(access, req.CreateOptions),
		Disposition:    vfs.Disposition(req.CreateDisposition),
		FileAttributes: uint32(req.FileAttributes),
	}

	h, action, err := fs.Open(openReq)
	if err != nil {
		ctx.Log.Debug("CREATE 打开失败", "path", path, "stream", stream, "err", err)
		return createStatus(err, req.CreateDisposition)
	}

	// Opened 阶段要在 Stat 之前跑：AlSi 的预留结果必须反映进响应的
	// AllocationSize。
	if err := cc.opened(ctx, h, action); err != nil {
		_ = h.Close()
		return err
	}

	attr, err := h.Stat()
	if err != nil {
		_ = h.Close()
		return status.FromVFSError(err)
	}

	isDir := attr.FileAttributes&vfs.FileAttributeDirectory != 0
	// FILE_DIRECTORY_FILE / FILE_NON_DIRECTORY_FILE 是硬性断言，
	// VFS 后端可能没能力提前判断，这里兜底（MS-SMB2 §3.3.5.9）。
	if req.CreateOptions&wire.FileDirectoryFile != 0 && !isDir {
		_ = h.Close()
		return status.NotADirectory
	}
	if req.CreateOptions&wire.FileNonDirectoryFile != 0 && isDir {
		_ = h.Close()
		return status.FileIsADirectory
	}

	open := &Open{
		Tree:           ctx.Tree,
		Path:           path,
		Stream:         stream,
		Handle:         h,
		IsDir:          isDir,
		GrantedAccess:  access,
		ShareAccess:    req.ShareAccess,
		CreateAction:   wire.CreateAction(action),
		CreateOptions:  req.CreateOptions,
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
	}
	// 权威的共享模式判定：判定与登记在同一次持锁内完成，收口并发竞态
	// （两个 CREATE 同时通过预检的情形）。文件身份取自**已打开句柄**的
	// Stat，而不是上面预检用的路径 Stat —— 两者之间目标可能被换掉。
	if st := ctx.Tree.Share.shareModes.add(
		shareModeKey{fileID: attr.FileID, stream: stream}, open); st != status.Success {
		ctx.Log.Debug("CREATE 被共享模式拒绝",
			"path", path, "stream", stream,
			"access", uint32(access), "share", uint32(req.ShareAccess))
		_ = h.Close()
		return st
	}

	// ⚠️ 从这里往下的失败路径一律用 open.close() 而不是 h.Close()：
	// 句柄已经登记进共享模式表，只关底层文件会把登记留在表里，
	// 那个文件从此谁也打不开。
	if req.CreateOptions&wire.FileDeleteOnClose != 0 {
		if err := ctx.RequireWritable(); err != nil {
			open.close()
			return err
		}
		open.SetDeleteOnClose(true)
		// vfs 层在 OpenDeleteOnClose 下会在 Close 时自行删除文件，
		// 命令层 CLOSE handler 必须让出，避免重复删除（见 close.go）。
		open.vfsOwnsDelete = true
	}

	if st := ctx.Session.AddOpen(open); st != status.Success {
		open.close()
		return st
	}
	ctx.Chain.LastOpen = open

	// Registered 阶段：需要一个完整 Open 的 context（durable / lease）在这里
	// 生效。失败要把已经登记的句柄摘掉，不能留孤儿。
	if err := cc.registered(ctx, open); err != nil {
		ctx.Session.RemoveOpen(open.Volatile)
		ctx.Chain.LastOpen = nil
		open.close()
		return err
	}

	resp := &wire.CreateResponse{
		// 不实现 oplock/lease：一律回 NONE。客户端会退化成不缓存，
		// 正确性不受影响（protocol-notes §9）。lease 模块会在 Respond 阶段
		// 覆写 OplockLevel，无需在此特殊对待。
		OplockLevel:    wire.OplockLevelNone,
		CreateAction:   open.CreateAction,
		CreationTime:   vfs.TimeToFiletime(attr.CreateTime),
		LastAccessTime: vfs.TimeToFiletime(attr.AccessTime),
		LastWriteTime:  vfs.TimeToFiletime(attr.WriteTime),
		ChangeTime:     vfs.TimeToFiletime(attr.ChangeTime),
		AllocationSize: uint64(attr.Alloc),
		EndOfFile:      uint64(attr.Size),
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
		FileID:         wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile},
	}

	// Respond 阶段：各 context handler 往 resp.Contexts 追加响应 context，
	// 并可能覆写 resp.OplockLevel（lease）。返回错误表示整个 CREATE 失败，
	// 必须回收已经登记、尚未对外可见的句柄。
	if err := cc.respond(ctx, open, resp, attr); err != nil {
		ctx.Session.RemoveOpen(open.Volatile)
		ctx.Chain.LastOpen = nil
		open.close()
		return err
	}

	out, err := resp.Append(ctx.Out)
	if err != nil {
		ctx.Log.Error("编码 CREATE Response 失败", "err", err)
		return status.InsuffServerResources
	}
	ctx.Out = out

	ctx.Log.Debug("CREATE",
		"share", ctx.Tree.Share.Name, "path", path, "dir", isDir,
		"action", action, "fid", open.Volatile)
	return nil
}

// createPipe 在 IPC$ 上打开一个命名管道。
func createPipe(ctx *Context, req *wire.CreateRequest) error {
	opener := ctx.Conn.Settings.Pipes
	if opener == nil {
		// 管道后端尚未装配：报「没有这个对象」而不是 NOT_SUPPORTED，
		// 客户端会当成共享枚举不可用而优雅退化。
		return status.ObjectNameNotFound
	}

	// 管道名不含路径分隔符，且大小写不敏感。
	name := strings.ToLower(strings.Trim(req.Name, `\`))
	if name == "" || strings.ContainsAny(name, `\/`) {
		return status.ObjectNameInvalid
	}

	pipe, err := opener.OpenPipe(name, ctx.Session.Identity())
	if err != nil {
		if errors.Is(err, ErrNoSuchPipe) {
			ctx.Log.Debug("请求了未提供的命名管道", "pipe", name)
			return status.ObjectNameNotFound
		}
		ctx.Log.Warn("打开命名管道失败", "pipe", name, "err", err)
		return status.FromVFSError(err)
	}

	open := &Open{
		Tree:          ctx.Tree,
		Path:          name,
		Pipe:          pipe,
		GrantedAccess: req.DesiredAccess.Expand(),
		ShareAccess:   req.ShareAccess,
		CreateAction:  wire.FileOpened,
		CreateOptions: req.CreateOptions,
		// 管道在 Windows 上报为 NORMAL。
		FileAttributes: wire.FileAttributeNormal,
	}
	if st := ctx.Session.AddOpen(open); st != status.Success {
		_ = pipe.Close()
		return st
	}
	ctx.Chain.LastOpen = open

	// 管道没有真正的时间戳与长度，全部回 0 —— 客户端不看这些字段。
	resp := &wire.CreateResponse{
		OplockLevel:    wire.OplockLevelNone,
		CreateAction:   wire.FileOpened,
		FileAttributes: wire.FileAttributeNormal,
		FileID:         wire.FileID{Persistent: open.Persistent, Volatile: open.Volatile},
	}
	out, err := resp.Append(ctx.Out)
	if err != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out

	ctx.Log.Debug("打开命名管道", "pipe", name, "fid", open.Volatile)
	return nil
}

// splitCreateName 把 CREATE 的 Name 拆成「共享内相对路径」与「流名」。
//
// 线格式用反斜杠分隔且无前导反斜杠（protocol-notes §8），VFS 用正斜杠。
// 流名语法是 `path:stream:$DATA`（MS-FSCC §2.1.5.4）。
//
// 真正的路径穿越防御在 VFS 层（AGENTS.md §8），这里只做**语法**校验：
// 拒绝绝对路径与显然畸形的输入，避免把垃圾喂给下层。
func splitCreateName(name string) (path, stream string, err error) {
	if strings.ContainsRune(name, 0) {
		return "", "", status.ObjectNameInvalid
	}
	// 客户端偶尔会带前导反斜杠，容忍之。
	name = strings.TrimPrefix(name, `\`)
	name = strings.ReplaceAll(name, `\`, "/")

	if i := strings.IndexByte(name, ':'); i >= 0 {
		path, stream = name[:i], name[i+1:]
		if stream == "" {
			// "f:" —— 冒号后面什么都没有。Samba check_path_syntax 对
			// ':' 后无字符的情况报 OBJECT_NAME_INVALID（bh5 F11）；
			// 当成主数据流放行会让残缺输入悄悄成功，Windows/Samba
			// 都是报错的。
			return "", "", status.ObjectNameInvalid
		}
		// 去掉 `:$DATA` 后缀；类型不是 $DATA 的流我们不支持
		// （与 VFS 层 SplitStreamPath 的类型后缀口径一致，bh5 F11）。
		if j := strings.IndexByte(stream, ':'); j >= 0 {
			if !strings.EqualFold(stream[j:], ":$DATA") {
				return "", "", status.NotSupported
			}
			stream = stream[:j]
		}
	} else {
		path = name
	}

	// vfs.CleanPath 负责规范化并拒绝 ".."、绝对路径等越界写法。
	clean, cerr := vfs.CleanPath(path)
	if cerr != nil {
		return "", "", status.ObjectPathInvalid
	}
	return clean, stream, nil
}

// openFlags 把 SMB 的 DesiredAccess/CreateOptions 翻译成 vfs.OpenFlags。
func openFlags(access wire.Access, opts wire.CreateOptions) vfs.OpenFlags {
	var f vfs.OpenFlags

	if access&(wire.FileReadData|wire.FileExecute) != 0 {
		f |= vfs.OpenRead
	}
	if access&(wire.FileWriteData) != 0 {
		f |= vfs.OpenWrite
	}
	if access&wire.FileAppendData != 0 {
		f |= vfs.OpenAppend
	}
	// 只要了属性/EA/ACL 而没要数据流：告诉后端可以走轻量路径。
	// Explorer 与 Finder 会为列表里的每个文件做这种探测，量很大。
	if f == 0 {
		f |= vfs.OpenAttrOnly
	}

	if opts&wire.FileDirectoryFile != 0 {
		f |= vfs.OpenDirectory
	}
	if opts&wire.FileNonDirectoryFile != 0 {
		f |= vfs.OpenNonDirectory
	}
	if opts&wire.FileOpenReparsePoint != 0 {
		f |= vfs.OpenNoFollow
	}
	if opts&wire.FileWriteThrough != 0 {
		f |= vfs.OpenWriteThrough
	}
	if opts&wire.FileDeleteOnClose != 0 {
		f |= vfs.OpenDeleteOnClose
	}
	return f
}

// maximalAccessFor 返回 MAXIMUM_ALLOWED 在本树上应当授予的访问掩码。
func maximalAccessFor(t *Tree) wire.Access {
	if t.Writable() {
		return wire.Access(wire.MaximalAccessReadWrite)
	}
	return wire.Access(wire.MaximalAccessReadOnly)
}

// createStatus 把 VFS 错误映射为 CREATE 语境下的 NTSTATUS。
//
// 与通用映射的差别只有一处：CREATE 语境里 ErrExist 的含义取决于
// disposition —— FILE_CREATE 下是「已存在」，其它情况下是「名字冲突」。
func createStatus(err error, disp wire.CreateDisposition) status.Status {
	if errors.Is(err, vfs.ErrExist) && disp == wire.FileCreate {
		return status.ObjectNameCollision
	}
	return status.FromVFSError(err)
}
