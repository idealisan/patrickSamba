---
name: Samba 权威源码的在线取用方式
description: 容器有外网；Samba 源码可直接 curl gitlab raw 拿到，Apple/SMB 扩展的关键文件与行号线索
type: reference
---

AGENTS.md §9 要求「不确定的字段值不要猜」，而 Apple 的 SMB 扩展（AAPL、readdir_attr、
ADS↔xattr 映射）在 MS-SMB2/MS-FSCC 里**根本没有定义**，唯一权威就是 Samba 源码。

**本容器可以直接联网取源码**（已实测，不需要 clone 整个仓库）：

```sh
curl -sSL "https://gitlab.com/samba-team/samba/-/raw/master/<仓库内路径>" -o /tmp/<文件>
```

查 Apple / SMB 扩展时最常用的几个文件：

| 路径 | 里面有什么 |
|---|---|
| `source3/smbd/smb2_trans2.c` | `smbd_marshall_dir_entry()` —— **所有目录信息类的线格式**，readdir_attr 的字段偏移在 `case SMB_FIND_ID_BOTH_DIRECTORY_INFO:` 分支 |
| `source3/modules/vfs_fruit.c` | `check_aapl()`（AAPL 协商）、`readdir_attr_meta_finderi()`（压缩 FinderInfo）、`fruit_freaddir_attr()`。**文件头 40-100 行的大段注释是整个 Apple 互操作设计的说明书** |
| `source3/lib/readdir_attr.h` | `struct aapl` 的真实字段与长度 |
| `source3/lib/adouble.h` | AppleDouble 常量（`AD_DATE_DELTA`、`AD_DATE_START`、`ADEDLEN_*`） |
| `source3/modules/vfs_streams_xattr.c` | 通用 named stream ↔ xattr 的映射 |
| `source3/include/smb.h` | `SAMBA_XATTR_DOSSTREAM_PREFIX = "user.DosStream."` |
| `source3/include/MacExtensions.h` | `AFP_AfpInfo` / `AFP_Resource` 流名与 AfpInfo 结构 |
| `libcli/smb/smb2_constants.h` | `SMB2_CRTCTX_AAPL_*` 各能力位 |
| `source3/smbd/smb2_create.c` | MxAc 的两条真实行为：L1608 请求长度只许 0/8，L1875 mtime 未变则**不回**响应 context |
| `source3/lib/string_replace.c` | `macos_string_replace_map` —— macOS 非法字符 ↔ Unicode 私有区映射表（见下） |

Samba 之外还有一个可 curl 的权威交叉验证源 —— **Linux 内核的 SMB 客户端**：

```sh
curl -sSL "https://raw.githubusercontent.com/torvalds/linux/master/fs/smb/client/<文件>"
# cifs_unicode.h / cifs_unicode.c  非法字符 SFM/SFU 映射
# smb2pdu.c / smb2pdu.h            客户端实际怎么发 SMB2 请求
```

它和 Samba 是**互相独立**的实现，两边一致的事实基本可以当定论；
两边不一致时要格外小心，多半意味着有一方在迁就某个具体服务端。
| `source3/smbd/smb2_ioctl_filesys.c` | `fsctl_qar`（QUERY_ALLOCATED_RANGES）、`fsctl_zero_data`、压缩、dup_extents |
| `source3/smbd/smb2_ioctl_network_fs.c` | copychunk、QUERY_NETWORK_INTERFACE_INFO、VALIDATE_NEGOTIATE_INFO、REQUEST_RESUME_KEY |
| `source3/modules/vfs_default.c` | `vfswrap_fsctl` —— **上面两个文件 switch 里没有的 FSCTL 全部 fall through 到这里**，`FSCTL_GET_SHADOW_COPY_DATA` 就在其中 |
| `source3/smbd/dosmode.c` | `file_set_sparse()` —— Samba 把稀疏位存进 `user.DOSATTRIB`，所以它的 SET_SPARSE(FALSE) 能成功 |
| `source3/libsmb/cli_smb2_fnum.c` | smbclient **客户端侧**的解析。与服务端行为冲突时以它为准 —— 它才是我们的对端 |

## 一个特别容易搞错的事实：macOS 非法字符映射不是 `0xF000 + 字符`

macOS 客户端把 NTFS 非法字符映射到 Unicode 私有区再发上线，所以**流名/文件名里
永远不会出现字面的 `:` `*` `?` `\` `|` `<` `>` `"`**。还原表在
`source3/lib/string_replace.c:186`，是一张**紧凑分配**的表，不是朴素的加偏移：

```
0x01..0x1F 控制字符 → U+F001..U+F01F
0x22 "  → U+F020      0x2A *  → U+F021
0x3A :  → U+F022      0x3C <  → U+F023      ← 冒号是 F022，不是 F03A
0x3E >  → U+F024      0x3F ?  → U+F025
0x5C \  → U+F026      0x7C |  → U+F027
```

后 8 个接着 `U+F01F` 顺序往下排，**与字符本身码点无关**。

**误解的来源**（团队内已有人踩过，坚持 `:`→U+F03A）：`0xF000 + 字符` 这条规则
**只适用于控制字符 0x01–0x1F**，标点是另一张手工分配的表。把控制字符那条规则
外推到标点就会得出 F03A。抄错这一个字符，
`com.apple.metadata:kMDItemFinderComment` 会落成错的 xattr 名，
Finder 注释与 Spotlight 元数据全部读不回来，且与 Samba/netatalk 不互通。

**独立佐证**（这类事实值得交叉验证，不要只信一个来源）：
Linux 内核 cifs.ko `fs/smb/client/cifs_unicode.h` 的 `SFM_*` 常量与上表**逐项一致**
（`SFM_COLON = 0xF022`），且 `convert_to_sfm_char()` 同样只对 0x01–0x1F 做加偏移。
Linux 还多两个 Samba 没有的、**仅用于名字结尾**的：
`SFM_SPACE = 0xF028`、`SFM_PERIOD = 0xF029`（Windows 不允许名字以空格/句点结尾）。

这张表**就是** catia 的线上↔磁盘映射表，不是别的用途 —— `vfs_fruit.c:1354`：
`fruit:encoding = native` 时把它整个装进 `catia:mappings`。所以它权威地回答了
「macOS 线上到底发什么」。Samba 默认是 `private`（`vfs_fruit.c:319`），即**原样保留**
私用区字符不还原成 ASCII；我们跟随这个默认。

推论：`ValidateStreamName` 拒绝字面冒号是**正确**的，不要为了让某个用了字面冒号的
测试通过而放行它 —— 放行后 `SplitStreamPath` 就无法区分分隔符与流名内容。

注意 Samba 近年做过文件重命名：目录项编码**不在** `source3/smbd/dir.c` 里
（那里已经没有 readdir_attr 了），也不在 `source3/smbd/trans2.c`（只剩 88 行的壳），
在 `source3/smbd/smb2_trans2.c`。找不到就先 `grep -n readdir_attr` 定位再读。

## 找 FSCTL 实现时别按控制码的 DeviceType 猜文件

真实教训：`FSCTL_SRV_ENUMERATE_SNAPSHOTS = 0x00144064`，DeviceType 是 0x14
（FILE_DEVICE_NETWORK_FILE_SYSTEM），照分类猜应该在 `smb2_ioctl_network_fs.c`。
结果那里的 switch **根本没有它** —— 它走 `default:` 落到 `SMB_VFS_FSCTL`，
真正实现在 `source3/modules/vfs_default.c` 的 `vfswrap_fsctl`。
连抓两个文件都扑空，白费三次 fetch。**先 grep 函数名，别按分类猜。**

## Samba 的服务端与客户端会互相不一致

遇到分歧看 `source3/libsmb/cli_smb2_fnum.c`（smbclient 走的就是它）。
实例 —— shadow copy 应答长度：

- 服务端 `vfswrap_fsctl`：`max_out_len == 16` 回 16 字节，`> 16` 回
  `12 + labels_data_count`，而 `labels_data_count = n*50 + 2`，
  **零快照时 SnapShotArraySize 是 2 不是 0**，整个应答只有 14 字节；
- 客户端 `cli_smb2_shadow_copy_data_fnum_recv`：硬性要求 `length >= 16`，
  否则回 `NT_STATUS_INVALID_NETWORK_RESPONSE`。

也就是说**严格按规范/Windows 的 14 字节形式回，会被 smbclient 判为坏响应**。
本项目取 16 字节（14 的超集）。同一族的坑还有：`max_out_len < 16` 时
Samba 回 `INVALID_PARAMETER` 而不是 `BUFFER_TOO_SMALL`（后者只用于
「头装得下但标签装不下」的第二道检查）。

License 提醒：Samba 是 GPL-3.0，**只可阅读参考、不得复制代码**（AGENTS.md §4）。
把查到的事实写成注释 + 自己实现，并在注释里注明文件与函数名。
