---
name: 验证策略类开关要同时测「允许」与「拒绝」两条路径
description: 测加密/签名/只读/guest/valid_users 等策略开关时，必须同时验证「应当通过的能通过」与「应当被拒的被拒」，只测默认路径是假阳性
type: feedback
---

验证任何「策略类开关」（加密 `encryption_required`、签名 `signing_required`、只读
`read_only`、guest `allow_guest`、授权 `valid_users` 等）时，**必须同时测它应当拒绝的情况**，
不能只验证「允许的请求能通过」。

**Why:** v0.1.0 文档 agent 自测 `encryption_required: true` 时只用了默认路径
（smbclient 默认协商 3.1.1，唯一正常工作的路径），下的「实测通过」结论是**假阳性**；
qa 用 `-m SMB2_02` / `-m SMB3_00` 显式压低方言才暴露明文泄露。同一类盲区也出现在
`dialect_test.go:88`：测了默认路径，bug 恰好只在非默认路径上。只验证「允许的能通过」
等于没验证策略——策略的本质是「拒绝不该发生的」。

**How to apply:** 每测一个策略开关，列两张表：(1) 应当放行的输入 → 期望成功；
(2) 应当拒绝的输入 → 期望 `ACCESS_DENIED` / 失败。两条都跑过才算验证完成。写进文档的
「实测通过」承诺尤其必须包含拒绝路径，否则是误导。对加密类，验证手段不能只看「能否读到
内容」（那测的是功能不是保密）；要用线级探针（TRANSFORM_HEADER 存在性 + 明文帧全称否定）
或抓包，并做反向对照。相关：`feedback_falsifiable_assertions.md`（验收判据必须可证伪）。
