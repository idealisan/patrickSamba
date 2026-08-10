# Memory Index

- [v0.2.0 已发布并独立验证完成](project_v020_released.md) — tag/Release 页面/Docker :latest 均上线且 5 agent 验证全 PASS；再被要求"发 0.2.0"应验证而非重发
- [Release tag push is irreversible within ~3 minutes](feedback_release_push_window.md) — 推 tag 到对外 Release 仅约 3 分钟窗口，必须推前闸门（门禁 owner 全绿 + team-lead 显式授权），事后中止不现实
- [metadata_path 真实行为 v0.2.0 过期文档](project_metadata_path_v020_defects.md) — PR #159 后全平台消费（oscap builtin），但 config 注释/WARN/example.yaml 仍写"仅 Windows/会被忽略"，v0.2.1 整体修；#176 实为回退 PR #18 应拒
