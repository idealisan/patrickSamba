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

后 8 个接着 `U+F01F` 顺序往下排，与字符本身码点无关。老的 SFM 用的才是
`0xF000+字符`（那样 `:` 会变成 U+F03A）—— **抄错这一个字符**，
`com.apple.metadata:kMDItemFinderComment` 就会落成错的 xattr 名，
Finder 注释与 Spotlight 元数据全部读不回来，且与 Samba/netatalk 不互通。

推论：`ValidateStreamName` 拒绝字面冒号是**正确**的，不要为了让某个用了字面冒号的
测试通过而放行它 —— 放行后 `SplitStreamPath` 就无法区分分隔符与流名内容。

注意 Samba 近年做过文件重命名：目录项编码**不在** `source3/smbd/dir.c` 里
（那里已经没有 readdir_attr 了），也不在 `source3/smbd/trans2.c`（只剩 88 行的壳），
在 `source3/smbd/smb2_trans2.c`。找不到就先 `grep -n readdir_attr` 定位再读。

License 提醒：Samba 是 GPL-3.0，**只可阅读参考、不得复制代码**（AGENTS.md §4）。
把查到的事实写成注释 + 自己实现，并在注释里注明文件与函数名。
