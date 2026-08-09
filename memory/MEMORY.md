# Project Memory Index

- [历史痕迹一律保留，不要主动清理](feedback_conversation_exports.md) — 会话导出、测试 Release 都是珍贵记录，别删、别 gitignore、别「用完清掉」
- [记忆要经常备份进仓库并提交](feedback_backup_memory_to_repo.md) — 环境常重启丢文件，仓外记忆不可信，必须写进 /workspace/memory 并 git 提交
- [必须用 3~5 个子 agent 组队并行开发](feedback_parallel_agent_team.md) — 用户判定串行太慢，要求分工组队；附文件所有权与端口隔离的做法
- [团队改用独立工作目录 + 分支 + PR](feedback_branch_pr_workflow.md) — 每人一个 git worktree 才根治干扰；分支只隔离历史不隔离文件；禁止直推 main
- [开发环境固有限制与重启恢复步骤](project_environment_constraints.md) — Go 不在 PATH、mount.cifs 因非初始 user namespace 永远跑不了（与 capability 无关）、pkill -f 自杀坑、worktree 里 .git 是文件（save.sh 因此 100% 推不出去）
- [四类隐形 bug 的排查模式](project_bug_patterns.md) — 死字段/被架空的逻辑/示例配置跑不起来/安全要求有第二个出口；附 worktree 负向实验法
- [共享工作树里的写入纪律](feedback_shared_worktree_write_discipline.md) — Write 前必须 Read，曾整个覆盖队友已完成的 lock.go；附归属判断与 pkill 名字截断坑
- [Samba 权威源码的在线取用方式](reference_samba_source.md) — 容器能联网 curl gitlab raw；Apple 扩展只有 Samba 源码是权威，附关键文件清单与已改名的坑
- [共享工作树里验证自己代码的三个技巧](project_test_verification_tricks.md) — go test -overlay 绕开队友红灯与做变异测试；smbclient 会规范化 .. 验不出路径穿越
- [macOS SMB 客户端两个反直觉行为](reference_macos_smb_quirks.md) — 流名冒号是 U+F022（非裸冒号、非 U+F03A）；xattr 当 ADS 发；目录上三种流三种待遇
- [save.sh 的三道门 + 推送段三个已修 bug](project_save_script_gap.md) — 门禁别顺手简化；推送段 refspec/rebase 目标/worktree 锁都栽过；改完必跑 test/ci/save-sh-scenarios.sh 及其 legacy 对照
- [验收判据必须可证伪](feedback_falsifiable_assertions.md) — 加密曾用「能读到内容」判定而漏掉明文旁路；探针要有反向对照，失败用例不许 skip
- [smbclient 4.22 做加密验证的三个坑](reference_smbclient_quirks.md) — --client-protection 合法值；它总宣告 CAP_ENCRYPTION；NOT_SUPPORTED 与 ACCESS_DENIED 别混为一谈
- [验证策略开关要同时测「允许」与「拒绝」两条路径](feedback_verify_policy_switch_both_paths.md) — 只测默认路径是假阳性（encryption_required 假阳性教训）；拒绝路径也要跑，否则「实测通过」是误导
- [CNB PR API 的调用方式与四个坑](reference_cnb_pr_api.md) — 合并须 PUT 非 POST、参数 merge_style 非 merge_method、commit_title 必填、GET/PUT 都要 Accept: application/json
- [配置里的路径字段按「运行平台」判定绝对性](project_config_path_platform_semantics.md) — metadata_path 那条已修（跳过+平台形参+平台化判定函数三件套）；example.yaml 在 Windows 上仍跑不起来
- [metadata 存储双实现冲突的定夺](project_metadata_store_consolidation.md) — vfs 与 internal/meta 撞同一文件/bucket 静默吐垃圾；定夺由 internal/meta 单一化（posix2 桶+迁移），并暴露 -tags metabolt 的 CI 假绿洞
