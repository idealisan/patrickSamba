# bh5 对照排查报告：命名流 + Apple 扩展 + FSCTL 稀疏三件套

日期：2026-08-25（18:08–18:40 CST）
我方代码：/work/bh5（只读）；对照：/work/ref/samba。
格式：`[严重度][置信] 标题 | 我方证据 | Samba 证据 | 客户端可见后果 | 修复方向`

---

## 一、分歧发现

### [高][高] F1 流句柄上的「关闭时删除」要么误删整个基础文件、要么静默无效
| | |
|---|---|
| 我方证据 | `set_info.go:249-269` `setDisposition` 不区分流句柄，直接 `open.SetDeleteOnClose(del)`；`close.go:88` 取 `delPath := open.Path` —— 而 `Open.Path` 是**不含流名的基础路径**（`create.go:143-152`），于是 CLOSE 时删掉的是整个文件。另一条路径更糟：CREATE 带 `FILE_DELETE_ON_CLOSE` 打开流时 `create.go:170-179` 置 `vfsOwnsDelete=true` 让命令层跳过删除，但 `stream_handle.go:559-577` 的 `Close()` 根本没有任何删除逻辑（`local.go:460-470` 只给 `localHandle` 设了 `deleteOnClose`）——结果是什么都不删。 |
| Samba 证据 | 删除按 fsp 粒度走：命名流的 fsp 就是流本身，`vfs_streams_xattr.c:1056-1110` `streams_xattr_unlinkat()` 对命名流只删对应 xattr（"A stream can never be rmdir'ed"）；Windows 语义同（对 ADS 句柄设 FileDispositionInformation 只删该流）。 |
| 客户端可见后果 | macOS/Windows 客户端用标准流程删除单个流（如清 FinderInfo、删资源派生）时会把**整个文件连带所有流一起删掉**；而带 FILE_DELETE_ON_CLOSE 的流 CREATE 则无声无息什么都不删。前者是数据丢失级故障。 |
| 修复方向 | `close.go` 对 `open.Stream != ""` 走专用删流路径（AFP_AfpInfo → 删 netatalk xattr；AFP_Resource → 截空 ._ 并清理；通用流 → `removeDosStream`）；同时让 `streamHandle` 实现 `DeleteOnCloser` 并在 CREATE 路径上不再把流句柄标成 `vfsOwnsDelete`。 |

### [中][高] F2 基础文件不存在时对流做创建性打开直接失败（Samba 会自动建基础文件）
| | |
|---|---|
| 我方证据 | `stream_handle.go:64-70`：`openStream` 先 `os.Lstat(host)`，失败即 `mapError(err)` → OBJECT_NAME_NOT_FOUND，不看 Disposition；注释断言"SMB 不允许只创建流不创建文件"。 |
| Samba 证据 | `smbd/open.c:6508-6599`：对流路径先以 `FILE_OPEN_IF`（除 FILE_OPEN 外的所有 disposition）打开基础文件，注释明说"We may be creating the basefile as part of creating the stream"；`vfs_streams_xattr.c:930-940` 也写明 "The higher levels should have created the base file for us"。 |
| 客户端可见后果 | 客户端 CREATE `new.txt:stream`（FILE_OPEN_IF / CREATE_IF 等）在 Windows/Samba 上成功（顺带建出 new.txt），在我们这里吃 OBJECT_NAME_NOT_FOUND。copyfile 类工具与部分备份软件会走这条路。 |
| 修复方向 | `openStream` 在 disposition 带创建语义且 Lstat 不存在时，先走 `openFile` 建出基础对象再继续开流；FILE_OPEN 保持现状（不存在即 NOT_FOUND）。 |

### [中][高] F3 SUPERSEDE / OVERWRITE* 不清除既有 ADS（属性与资源派生残留）
| | |
|---|---|
| 我方证据 | `local.go:373-380`：Supersede 用 O_TRUNC 近似，注释自己承认"语义上原文件的属性、ADS 都应被丢弃……TODO: 严格实现需 unlink + create"；TruncateExisting/OVERWRITE 分支同样只截数据。netatalk xattr 与 `._` 文件、user.DosStream.* 都不受 truncate 影响。 |
| Samba 证据 | `smbd/open.c:3584-3598` `clear_ads()`：SUPERSEDE/OVERWRITE_IF/OVERWRITE 返回 true；`open.c:4436-4446` 据此调 `delete_all_streams()`。NTFS 同语义。 |
| 客户端可见后果 | 覆盖写入一个旧文件后，Finder 仍看到旧的 FinderInfo/颜色标签/资源派生；Time Machine 恢复/迁移场景出现「新内容 + 前世的元数据」。QUERY_INFORMATION 的流清单也与真机不符。 |
| 修复方向 | 在 `openFile` 的 Superseded/Overwritten 动作分支里调一套「清 ADS」例程（复用 F1 的删流原语：删 netatalk xattr、删 `._`、枚举删 DosStream.* xattr）。 |

### [中][高] F4 SET_ZERO_DATA 不做字节范围锁检查
| | |
|---|---|
| 我方证据 | `ioctl.go:270-291` `ioctlSetZeroData` 只有权限与参数校验，无任何锁检查；全仓库锁表只在 LOCK 命令（`lock.go:76-160`）与 CLOSE（`close.go:92-94`）被使用，READ/WRITE/ZERO 都不查。 |
| Samba 证据 | `smbd/smb2_ioctl_filesys.c:459-468`：fsctl_zero_data 先 `init_strict_lock_struct(...WRITE_LOCK)` 再 `SMB_VFS_STRICT_LOCK_CHECK`，冲突回 NT_STATUS_FILE_LOCK_CONFLICT。 |
| 客户端可见后果 | 客户端 A 持有某区间字节锁时，客户端 B 的打洞会改掉 A 锁定区域的内容（读回来变零），违反 CIFS 字节锁契约；数据库类客户端（Excel/SQLite over SMB）可能因此损坏。 |
| 修复方向 | 在 `sparseTarget(write=true)` 之后补 `Tree.Share.locks.conflict(open.Path, open, off, len, true)` 检查，冲突回 `status.FileLockConflict`。顺带评估 READ/WRITE 是否同样缺锁检查（同类缺口，超出本次范围）。 |

### [低中][高] F5 VALIDATE_NEGOTIATE_INFO 用「整表顺序相等」比对方言列表，规范算法是「最大公共方言匹配」
| | |
|---|---|
| 我方证据 | `ioctl.go:113-120` 要求 `sameDialects(in.Dialects, c.ClientDialects)`，`:437-451` 明确做成逐项、顺序敏感的全表比较。 |
| Samba 证据 | `smbd/smb2_ioctl_network_fs.c:533-620`（fsctl_validate_neg_info）：用 `smbd_smb2_protocol_dialect_match()` 求**最大公共方言**再与 conn->protocol 比较，只要求协商出的方言出现在请求列表里即可；MS-SMB2 §3.3.5.15.12 原文同。 |
| 客户端可见后果 | 合法客户端若在 VNI 里发送与 NEGOTIATE 不同（裁剪/重排）的方言列表——规范允许——会被我们判成降级攻击回 ACCESS_DENIED 而断连失败。真实客户端通常原样重放，触发率低但存在。 |
| 修复方向 | 把校验改成「in.Dialects 中可协商出的最大方言 == c.Dialect」；保留 GUID/SecurityMode/Capabilities 的精确比对（这三个 Samba 也是精确比对）。 |

### [低中][高] F6 VNI 校验失败后服务端不断连
| | |
|---|---|
| 我方证据 | `ioctl.go:113-120` 失败仅 `return status.AccessDenied`；`internal/server/connection.go` 无任何由该错误触发的连接终止路径。 |
| Samba 证据 | 同上 fsctl_validate_neg_info：每处失配都 `*disconnect = true`（由 smbd 终止传输连接）；MS-SMB2 §3.3.5.15.12 "the server MUST terminate the transport connection"。 |
| 客户端可见后果 | 防降级复核的威慑减半：被篡改的客户端收到 AccessDenied 后仍可保持连接与会话继续发命令。正常客户端无感。 |
| 修复方向 | 该 handler 失败分支置一个「要求断连」标记（Context 或返回专用错误类型），由 server 层关闭传输；至少对 dialect/GUID 失配这样做。 |

### [低][高] F7 QUERY_ALLOCATED_RANGES 权限比 Samba 宽：接受 WriteData
| | |
|---|---|
| 我方证据 | `ioctl.go:303-306`：`GrantedAccess&(FileReadData|FileWriteData)==0` 才拒绝。 |
| Samba 证据 | `smbd/smb2_ioctl_filesys.c:632` `check_any_access_fsp(fsp, FILE_READ_DATA)` —— 只认 READ_DATA。 |
| 客户端可见后果 | 只写句柄能探到已分配区间分布。信息泄露面极小（本来就有写权限），主要是与参照实现的语义偏差。 |
| 修复方向 | 收紧为仅 FileReadData。注意先验证 macOS 是否真的会用只写句柄查（保守做法：维持现状并记录分歧）。 |

### [低][高] F8 SET_SPARSE 少认 APPEND_DATA 访问位
| | |
|---|---|
| 我方证据 | `ioctl.go:202-210` `sparseTarget(write=true)` 要求 `FileWriteData|FileWriteAttributes`。 |
| Samba 证据 | `smbd/dosmode.c:1118-1128`：Windows Server 2008/2012 允许 WRITE_DATA \| WRITE_ATTRIBUTES \| SEC_FILE_APPEND_DATA 任一。 |
| 客户端可见后果 | 以 AppendData 打开的句柄设稀疏位会吃 ACCESS_DENIED；实际客户端几乎都以读写打开，触发面趋近于零。 |
| 修复方向 | 掩码加 `wire.FileAppendData`（仅 SET_SPARSE 分支）。 |

### [低][高] F9 三件稀疏 FSCTL 打到流句柄上一律 NOT_SUPPORTED；Samba 映射到基础文件
| | |
|---|---|
| 我方证据 | `ioctl.go:212-217` `sparseTarget` 断言 `h.(vfs.SparseFile)`，而 `streamHandle`（`stream_handle.go:37-57`）未实现该接口 → NotSupported。 |
| Samba 证据 | `modules/vfs_default.c:1537`：vfswrap_fsctl 开头 `fsp = metadata_fsp(fsp)` —— 所有 fsctl 落到基础文件；`dosmode.c:1147-1155` 对流上的 SET_SPARSE 更是直接假装成功（MS-FSA 2.1.1.5 IsSparse 注释）。 |
| 客户端可见后果 | 极少见：对 `file:stream` 句柄发 SET_SPARSE/QAR 的客户端在 Samba 上得到基础文件的答案，在我们这里得 STATUS_NOT_SUPPORTED。macOS 对 band 文件本体操作，不碰这条。 |
| 修复方向 | 至少把 SET_SPARSE 对流句柄改为无操作成功（对齐 dosmode.c 的"pretend success"）；QAR/ZERO 维持 NotSupported 可接受，但要记录。 |

### [低][中] F10 通用流名查找大小写敏感；Samba 在不区分大小写的共享上有兜底匹配
| | |
|---|---|
| 我方证据 | `stream_xattr.go:128-149`：xattr 名 = `"DosStream." + <原名> + ":$DATA"`，字节精确匹配；只有 AFP 两流经 `canonicalStreamName`（`stream.go:147-157`）做了 EqualFold 归一。 |
| Samba 证据 | `smbd/filename.c:983-999`：NOT_FOUND 且共享不区分大小写时调 `get_real_stream_name()`（:444-476）对流名做大小写不敏感匹配后再开。 |
| 客户端可见后果 | 客户端以不同大小写回访同一流（如先写 `:Meta` 后读 `:meta`）时我们报 NOT_FOUND、Samba 能找到。macOS 自身发的 com.apple.* 名大小写稳定，第三方工具可能踩。 |
| 修复方向 | `readDosStream/openXattrStream` 未命中时枚举 ListStreams 做 EqualFold 匹配一次（只在共享 case-insensitive 时）。 |

### [提示][高] F11 「file:」尾冒号被当成主数据流打开；两套解析器对类型后缀口径不一
| | |
|---|---|
| 我方证据 | `create.go:307-330` `splitCreateName`：`"f:"` → path="f"、stream="" → 按**主数据流**成功打开；类型后缀非 `:$DATA` 回 STATUS_NOT_SUPPORTED。VFS 层另一套 `stream.go:71-110` 却接受 `$INDEX_ALLOCATION` 当数据流（CREATE 主路径到不了它，仅 Streams/AppleInfo/link 内部使用），非法输入回 ErrInvalidPath。 |
| Samba 证据 | `smbd/smb2_reply.c:93-108` check_path_syntax：`:` 后必须还有字符（`s[1]=='\0'` → OBJECT_NAME_INVALID），所以 "f:" 报错；类型后缀在 `vfs_streams_xattr.c:527-539` 只认 `:$DATA`，其余 EINVAL；词法层（:73-91）对流名放行通配符/控制字符（posix_path 重载），我们的 `ValidateStreamName`（stream.go:117-136）更严——这是防御 xattr 名注入的合理选择，记录备查。 |
| 客户端可见后果 | "f:" 这类残缺输入我们当正常文件打开，Windows/Samba 报 OBJECT_NAME_INVALID；$INDEX_ALLOCATION 后缀我们回 NOT_SUPPORTED、Samba 回 INVALID_PARAMETER 类错误。状态码层面的差异，普通客户端无感。 |
| 修复方向 | `splitCreateName` 对 `stream==""` 且原名以 ':' 结尾的情况回 ObjectNameInvalid；统一两层解析的类型后缀口径（建议都收窄到 `:$DATA`）。 |

### [提示][中] F12 AFP_AfpInfo 新建后直到首次写入才进入流清单（与通用流行为不一致）
| | |
|---|---|
| 我方证据 | `stream_handle.go:176-184`：新建/覆盖 AfpInfo 时因 writeAfpInfo 对全零 FinderInfo 是删除语义而**不落盘**，`Streams()`（stream_store.go:209-217）自然列不出；对比 `openXattrStream:144-156` 对通用流是立即落空流。 |
| Samba 证据 | `vfs_streams_xattr.c:946-974`：O_CREAT 即 `fsetxattr` 一个占位字节，流立刻存在可枚举（fruit:metadata=stream 档）；metadata=netatalk 档则与 FinderInfo xattr 存在性绑定（fruit_open_meta_netatalk, vfs_fruit.c:1455-1490），与我们当前行为一致。 |
| 客户端可见后果 | 客户端 CREATE AFP_AfpInfo 成功后立即查 FileStreamInformation 看不到它，直到写入非零 FinderInfo。macOS 实际总是紧接着写 60 字节，窗口期极短。 |
| 修复方向 | 可维持现状（netatalk 模式固有语义），或在 CREATE 成功后落一个默认 AfpInfo blob 使其立即可见；二选一并写进测试钉住。 |

---

## 二、查过没问题的（双方证据）

| # | 项目 | 我方证据 | Samba 证据 | 结论 |
|---|---|---|---|---|
| P1 | AAPL 请求容错：长度≠24 → INVALID_PARAMETER；CommandCode≠kAAPL_SERVER_QUERY → INVALID_PARAMETER | `aapl.go:146-155,172-176` | vfs_fruit.c:792-803（`length != 24` / `cmd != SERVER_QUERY` 均 INVALID_PARAMETER）。⚠️ 任务简报说「Samba 对未知 code 跳过」，实查为报错——我方与 Samba **一致** | ✅ 一致 |
| P2 | AAPL 响应布局：16B 头(cmd+resv+bitmap) + ServerCaps(8)+VolumeCaps(8)+Resv(4)+Len(4)+Model(UTF16LE) | `aapl.go:245-286` | vfs_fruit.c:806-900（SIVAL/SBVAL 序列完全相同） | ✅ 一致 |
| P3 | readdir_attr 只在请求了 kAAPL_SERVER_CAPS 且客户端声明支持时启用 | `aapl.go:182-191` | vfs_fruit.c:813-820（readdir_attr_enabled 在 req_bitmap 分支内部） | ✅ 一致 |
| P4 | readdir_attr 条目布局：ShortNameLen=24、rfork_size 小端 uint64 **字节**、压缩 FinderInfo（type/creator 仅普通文件、flags[8:10]、ext[24:26]、date added 大端 btime−AD_DATE_DELTA）、无元数据时 AD_DATE_START 兜底 | `aapl.go:288-417` | smb2_trans2.c:1543-1551（SSVAL 24 / SBVAL=LE64，lib/util/byteorder.h:130）、readdir_attr_macmeta vfs_fruit.c:1153-1185（RSIVAL=BE32、AD_DATE_START 先行）、readdir_attr_meta_finderi :1012-1060（S_ISREG 门控）；rfork_size 单位=st_ex_size 字节（:1073-1150），无块换算坑 | ✅ 一致（含字节序陷阱） |
| P5 | Reserved2/unix_mode 恒 0 且不宣告 NFS_ACE | `aapl.go:84-87,316-326` | vfs_fruit.c:340-341,841,4541-4543（Samba 默认 nfs_aces=true 会填 mode 并置位）——**有意分歧**，我方自洽（不宣告就不填） | ✅ 有意且安全 |
| P6 | VNI 是真比对不是照抄：GUID/SecurityMode/Capabilities/Dialects 全部对照 NEGOTIATE 存档值 | `ioctl.go:104-128`、`conn.go:35-46` | smb2_ioctl_network_fs.c:533-620（GUID_equal/security_mode/capabilities 逐一比对） | ✅ 无串改攻击面 |
| P7 | VNI 3.1.1 回 FILE_CLOSED | `ioctl.go:89-95` | Samba 此版本不特判 3.1.1（照常校验）；Windows/规范要求 FILE_CLOSED（preauth hash 已覆盖）。有意跟随 Windows | ✅ 有意分歧 |
| P8 | VNI 请求/响应线格式（Cap4+GUID16+SecMode2+Count2 / Cap4+GUID16+SecMode2+Dialect2） | `wire/ioctl.go:310-405` | smb2_ioctl_network_fs.c:551-557,600-607 偏移逐字对齐 | ✅ 一致 |
| P9 | SET_SPARSE：空输入视为 TRUE；目录 → INVALID_PARAMETER；重复设置幂等；reserved 位忽略 | `ioctl.go:251-257`、`sparse_unix.go:103-122` | vfs_default.c:1543-1546（in_len>=1 && [0]==0 才 FALSE）、dosmode.c:1131-1139（is_directory → INVALID_PARAMETER） | ✅ 一致（FALSE→NotSupported 为已记录的有意分歧，理由成立） |
| P10 | SET_ZERO_DATA：BFZ<off → INVALID_PARAMETER；len==0 → OK；PUNCH_HOLE\|KEEP_SIZE 不延伸文件；两侧都不做 cluster 对齐预检（错对齐由底层 syscall 报错） | `wire/ioctl.go:708-727`、`sys_linux.go:47-54` | smb2_ioctl_filesys.c:436-448,476-486（同款校验 + fallocate KEEP_SIZE，注释引 MS-FSCC <58>） | ✅ 一致（缺锁检查见 F4） |
| P11 | QAR：len==0/文件空/off≥EOF → 空+SUCCESS 且**先于**缓冲下限检查；<16B → BUFFER_TOO_SMALL；截断按整条 + BUFFER_OVERFLOW（QAR 的 OVERVIEW 属警告级非失败） | `ioctl.go:322-352`、测试 TestIoctlQueryAllocatedRangesEmptyBeatsBufferCheck 等 | smb2_ioctl_filesys.c:648-664（早退顺序相同）、:658-664（must have enough space for at least one range）、:585-597（整条截断）；smb2_ioctl.c:237-245（QAR BUFFER_OVERFLOW 非 failure） | ✅ 一致 |
| P12 | QAR 越界窗口裁剪到 EOF、全洞文件返回空数组 | `optional.go:370-400`（clamp end→EOF） | smb2_ioctl_filesys.c:683-688（max_off=MIN(size, off+len)−1） | ✅ 一致 |
| P13 | QUERY_DIRECTORY 从不把流当独立条目返回；流只经 FileStreamInformation 呈现 | `query_directory.go` 无任何流引用；`stream_store.go:197-235` 仅服务 QUERY_INFO | Samba 默认（streams_xattr 只挂 streaminfo/fstat 钩子，不改目录枚举） | ✅ 一致 |
| P14 | 目录支持 AFP_AfpInfo、不支持 AFP_Resource（NotFound）；目录不带 DIRECTORY 位呈现流 | `stream_handle.go:76-96`、`:517-524` | fruit_open_meta_netatalk（vfs_fruit.c:1455，无目录判断）、fruit_open_rsrc（:1561 附近 "directories don't have a resource fork"） | ✅ 一致 |
| P15 | 主流匿名形态 `::$DATA` / 显式省略 / 大小写不敏感类型后缀 / 多余冒号拒绝 | `stream.go:71-136`、`create.go:313-325` | check_path_syntax（:77-91 冒号规则）、streams_xattr_get_name（:527-539 strcasecmp ":$DATA"） | ✅ 基本一致（细节差异见 F11） |
| P16 | 流名长度上限（组件 255、xattr 预算 234 字节，超长如实拒绝不截断） | `path.go:65`、`stream_xattr.go:111-165` | streams_xattr 依赖 xattr 名上限（ENAMETOOLONG 自然发生）；NTFS 上限 255 UTF-16 | ✅ 等价或更严 |
