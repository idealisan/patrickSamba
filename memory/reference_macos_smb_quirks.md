---
name: macOS SMB 客户端的两个反直觉行为（私用区字符 / xattr 当 ADS 发）
description: 流名里的冒号是 U+F03A 不是裸冒号；macOS 把扩展属性当 named stream 发过来——两条都在 MS 规范里查不到
type: reference
---

这两条在 MS-SMB2 / MS-FSCC 里**完全没有定义**，唯一权威是 Samba 源码与 `vfs_fruit(8)` 手册。
都在本项目真实踩到过。

## 1. macOS 把 NTFS 非法字符映射到 Unicode 私用区

> OS X maps NTFS illegal characters to the Unicode private range in SMB requests.
> —— `vfs_fruit(8)` 手册

映射规则是 `U+F000 + ASCII`。最要命的是冒号：macOS 真实用的流名
`com.apple.metadata:kMDItemFinderComment` 里那个冒号，**线上发的是 U+F03A**，
不是裸 `:`（0x3A）。

**Why 重要：** 冒号同时是 SMB 流名的分隔符（`path:stream:$DATA`）。
看起来矛盾，实际不冲突 —— 正因为客户端做了私用区编码，按裸冒号切分才是对的。

**How to apply:**
- 按裸冒号切 `path:stream:$DATA`、以及在流名里拒绝裸冒号，**都是正确的**，
  不要因为「macOS 流名有冒号」就去放宽。
- 反过来，**任何地方都不许把私用区字符当非法字符过滤掉** ——
  那会毙掉整类 `com.apple.metadata:*`。ValidateStreamName 只拒 ASCII 非法字符，
  私用区字符（UTF-8 三字节，无一 < 0x20）自然放行，这是对的。
- 写测试时流名要用 `"a\uF03Ab"` 这种**线上真实形态**，不要用裸冒号 ——
  用裸冒号会得到一个 ErrInvalidPath，让人误以为自己的校验太严。
- 存储侧选择「原样存，不还原成 ASCII」：Samba 的 `fruit:encoding` 默认值就是
  `private`（保留私用区字符），`native`（还原）是可选项，且手册明确警告它
  "is known to not fully work with fruit:metadata=stream or fruit:resource=stream"。

## 2. macOS 把扩展属性当 ADS 发过来

`vfs_fruit.c` 文件头注释：The OS X SMB client sends xattrs as ADS too。

即 `com.apple.metadata:*`、`com.apple.TimeMachine.*`、`com.apple.quarantine`
这些 xattr，在 SMB 上是以 **named stream** 的形式收发的。

**Why 重要：** 只支持 `AFP_AfpInfo`/`AFP_Resource` 两个特例是不够的。
一旦向客户端宣告了 `FILE_NAMED_STREAMS`，客户端就会真的去用通用流；
回 `STATUS_NOT_SUPPORTED` 会让 Time Machine 卡住。

**How to apply:**
- 通用流落 xattr `user.DosStream.<流名>:$DATA`
  （前缀出处 `source3/include/smb.h:530` 的 `SAMBA_XATTR_DOSSTREAM_PREFIX`；
  名字里的 `:$DATA` 后缀来自 `store_stream_type`，该选项默认为真）。
- **xattr 的值不是裸数据**：Samba 在末尾多存 1 字节 marker
  （`vfs_streams_xattr.c` 顶部设计注释：xattr 值不能为空，所以零长度流也要占
  一字节；该字节同时用作「还有几个续存 xattr」的计数）。照做才能与 Samba
  二进制兼容，用户可以在两者之间切换而不丢流。
- 「流不存在」要回 `STATUS_OBJECT_NAME_NOT_FOUND` 而**不是** NOT_SUPPORTED ——
  后者会让客户端以为整个共享不支持 ADS 从而彻底放弃使用 ADS。

## 3. 目录上的流：三种流三种待遇

`.sparsebundle` **本身就是目录**，macOS 会往它上面设 FinderInfo，
所以「目录不支持流」是错的。但也不能全放开：

| 流 | 目录上 | 依据（Samba master） |
|---|---|---|
| `AFP_AfpInfo` | 可用 | `fruit_open_meta_netatalk()` vfs_fruit.c:1455 无目录判断 |
| 通用流 | 可用 | `vfs_streams_xattr.c` 全文无目录判断 |
| `AFP_Resource` | **ENOENT** | `fruit_open_rsrc_adouble()` vfs_fruit.c:1561，注释原话 "sorry, but directories don't have a resource fork"；`fruit_streaminfo_rsrc()` :4047 对目录一条不列 |

另外：目录上的流句柄，其属性**必须清掉 DIRECTORY 位**，
否则客户端会拿这个流句柄去发 QUERY_DIRECTORY。
枚举侧与开流侧必须严格一致 —— 列得出却打不开、或写得进却读不到列表，
都是极难排查的错乱。
