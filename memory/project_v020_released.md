---
name: v0.2.0 已发布并独立验证完成（2026-08-10）
description: v0.2.0 的 tag/Release 页面/Docker 镜像均已上线且经 5 agent 独立验证；再被要求"发 0.2.0"时应验证而非重发
type: project
---

v0.2.0 已于 2026-08-10 完成发布并经验证。

**事实（可复算）**：
- tag `v0.2.0` → commit `ccc4302`，已推 origin；`main` 在 tag 之后另有 2 个 doc-only 提交（v0.2.1 连贯修），不影响发布物。
- CNB Release 页面：`tag_name=v0.2.0`、`prerelease=false`、`is_latest=true`、`draft=false`、5 个资产（SHA256SUMS + 3 个 tar.gz + 1 个 windows .zip），published `2026-08-10T01:40:27Z`。
- Docker 镜像：`docker.cnb.cool/finalappstore/stupidsamba:latest` 与 `:v0.2.0` index digest 相同（`sha256:2e7c8fad…`），含 linux/amd64 + linux/arm64。
- `tag_push` pipeline：`success=1 fail=0`。

**Why:** CodeBuddy 崩溃后 team-lead 重建 5 人团队（pm/qa/release-eng/vfs/server）做发布收尾与独立验证；4 个 agent 在发布 commit `ccc4302` 上复跑，结论全 PASS（发布链路 5/5、资产完整+镜像同源+静态链接、vfs/oscap 304 子测试全绿、server 7 包全绿且 CHANGELOG 声明 5/5 一致）。验证报告已合入 `main`（RELEASE_VERIFY_REPORT.md / RELEASE_ASSET_REPORT.md / VFS_TAG_REPORT.md / SERVER_TAG_REPORT.md），进度板闭环见 `docs/status-v0.2.0.md` §21。

**How to apply:**
- 再被要求"完成 / 执行 0.2.0 发版"时，**不要重新 push tag 或跑 `scripts/publish-release.sh`** —— 发布已上线，重做会撞 `feedback_release_push_window.md` 的「3 分钟窗口」教训且可能覆盖线上 Release。应改为**验证**（照 `docs/v020-release-verification.md` 跑一遍）并报告。
- 若发现发布物有问题需要修复，走 v0.2.1 或补丁 tag，不要回改 v0.2.0 tag。
- 已知未变项（非阻塞）：oscap 仅 2/6 能力接线；Time Machine 仍 C 档未真机验收；`native` 档当前无平台可用。
