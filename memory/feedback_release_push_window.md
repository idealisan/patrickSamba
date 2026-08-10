---
name: Release tag push is irreversible within ~3 minutes
description: stupidSamba v0.2.0 发版实测——推 tag 到对外 Release 仅约 3 分钟窗口，事后中止不现实，必须推前闸门（门禁 owner 全绿 + team-lead 显式授权）
type: feedback
---

推 v0.2.0 tag（09:37:19 CST）到 CNB Release 对外发布（09:40:27 CST）只有约 **3 分 08 秒**窗口。tag 本身受 AGENTS.md §7.5 `git push --delete` 禁用约束无法删除；Release 虽可 `PATCH /-/releases/<id>` 改正文，但叙事已对外、且 annotated tag 正文不可改。

**Why:** 2026-08-10 实测。team-lead 在门禁 owner（doc-audit / qa / build-verify）回报前就 push 了 tag——结果因三路独立门禁最终全绿而无害，但这纯属运气。pm 指出「指望发现后赶紧中止来兜底不现实」，量化窗口证实事后补救路线不成立。

**How to apply:** 发版流程必须坚持两步闸门，缺一不可：
1. 门禁 owner 全绿——至少 `release-executor` 的完整 `check-test-compile.sh`（5 平台 × 全部 build tag × 含 `_test.go`）跑过且 rc=0，外加 `check-constraints.sh` rc=0、四平台交叉编译、二进制静态、版本注入；
2. team-lead 显式一句「可以打 tag」。
任何一环未满足不得 push tag。门禁结论由负责人出，不要由执行者自证（tag 正文里的「门禁全绿」必须是 owner 的结论，不是执行者自己写的）。AGENTS.md 是否据此新增硬规则，作为 PR 提案交项目所有者拍板，不单方面改最高准则文件。
