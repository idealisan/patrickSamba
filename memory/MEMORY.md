# Project Memory Index

- [历史痕迹一律保留，不要主动清理](feedback_conversation_exports.md) — 会话导出、测试 Release 都是珍贵记录，别删、别 gitignore、别「用完清掉」
- [记忆要经常备份进仓库并提交](feedback_backup_memory_to_repo.md) — 环境常重启丢文件，仓外记忆不可信，必须写进 /workspace/memory 并 git 提交
- [必须用 3~5 个子 agent 组队并行开发](feedback_parallel_agent_team.md) — 用户判定串行太慢，要求分工组队；附文件所有权与端口隔离的做法
- [团队改用独立工作目录 + 分支 + PR](feedback_branch_pr_workflow.md) — 每人一个 git worktree 才根治干扰；分支只隔离历史不隔离文件；禁止直推 main
- [开发环境固有限制与重启恢复步骤](project_environment_constraints.md) — Go 不在 PATH、mount.cifs 缺 CAP_SYS_ADMIN 永远跑不了、pkill -f 自杀坑、smbclient -c 分号坑
- [三类隐形 bug 的排查模式](project_bug_patterns.md) — 死字段/被上游架空的逻辑/示例配置跑不起来，只有端到端能发现；附 worktree 负向实验法
- [共享工作树里的写入纪律](feedback_shared_worktree_write_discipline.md) — Write 前必须 Read，曾整个覆盖队友已完成的 lock.go；附归属判断与 pkill 名字截断坑
- [Samba 权威源码的在线取用方式](reference_samba_source.md) — 容器能联网 curl gitlab raw；Apple 扩展只有 Samba 源码是权威，附关键文件清单与已改名的坑
- [共享工作树里验证自己代码的三个技巧](project_test_verification_tricks.md) — go test -overlay 绕开队友红灯与做变异测试；smbclient 会规范化 .. 验不出路径穿越
- [macOS SMB 客户端两个反直觉行为](reference_macos_smb_quirks.md) — 流名冒号是 U+F022（非裸冒号、非 U+F03A）；xattr 当 ADS 发；目录上三种流三种待遇
- [save.sh 的校验盲区](project_save_script_gap.md) — 传文件路径时零编译校验；go build 不编译 _test.go，提交前手跑 go vet
