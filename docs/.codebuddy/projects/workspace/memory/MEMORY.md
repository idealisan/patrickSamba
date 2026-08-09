# Project Memory Index

- [会话导出文件必须入库](feedback_conversation_exports.md) — 用户刻意把 conversation-*.txt 提交到仓库作为项目历史，切勿自动移除
- [记忆要经常备份进仓库并提交](feedback_backup_memory_to_repo.md) — 环境常重启丢文件，仓外记忆不可信，必须写进 /workspace/memory 并 git 提交
- [必须用 3~5 个子 agent 组队并行开发](feedback_parallel_agent_team.md) — 用户判定串行太慢，要求分工组队；附文件所有权与端口隔离的做法
- [开发环境固有限制与重启恢复步骤](project_environment_constraints.md) — Go 不在 PATH、mount.cifs 缺 CAP_SYS_ADMIN 永远跑不了、pkill -f 自杀坑、smbclient -c 分号坑
- [Samba 权威源码的在线取用方式](reference_samba_source.md) — 容器能联网 curl gitlab raw；Apple 扩展只有 Samba 源码是权威，附关键文件清单与已改名的坑
