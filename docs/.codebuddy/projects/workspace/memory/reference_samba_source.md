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
| `source3/smbd/smb2_trans2.c` | `smbd_marshall_dir_entry()` —— 所有目录信息类的线格式，readdir_attr 在 `case SMB_FIND_ID_BOTH_DIRECTORY_INFO:` 分支 |
| `source3/modules/vfs_fruit.c` | `check_aapl()`、`readdir_attr_meta_finderi()`、`fruit_freaddir_attr()`；文件头 40-100 行的注释是 Apple 互操作设计说明书 |
| `source3/lib/readdir_attr.h` | `struct aapl` 的真实字段与长度 |
| `source3/lib/adouble.h` | `AD_DATE_DELTA` / `AD_DATE_START` / `ADEDLEN_*` |
| `source3/modules/vfs_streams_xattr.c` | 通用 named stream ↔ xattr 映射 |
| `source3/include/smb.h` | `SAMBA_XATTR_DOSSTREAM_PREFIX = "user.DosStream."` |
| `source3/include/MacExtensions.h` | `AFP_AfpInfo` / `AFP_Resource` 流名与 AfpInfo 结构 |
| `libcli/smb/smb2_constants.h` | `SMB2_CRTCTX_AAPL_*` 能力位 |

注意 Samba 做过文件重命名：目录项编码**不在** `source3/smbd/dir.c`，也不在
`source3/smbd/trans2.c`（只剩 88 行的壳），在 `source3/smbd/smb2_trans2.c`。
找不到就 `grep -n readdir_attr` 定位。

License：Samba 是 GPL-3.0，**只可阅读参考、不得复制代码**（AGENTS.md §4）。
把查到的事实写成注释 + 自己实现，注释里注明文件与函数名。
