# stupidSamba v0.4.0 — Release Plan & Progress Board

> 本文件是 v0.4.0 的**发布进度板**（AGENTS.md §7.1，由 PM 维护，**PM 不写产品代码**）。
> **Step 2 已 consolid（由 PM 基于 plan-v0.4.0-ci-release.md 推导，区域计划文件未单独产出）**：
> §3 Scope、§4 Risks/Decisions、§4.3 Owner 已全部由 backlog A1–A5 / B1–B5 + Known Open Issues
> OI-1/2/3 + Must/Should/Optional 表推导填充。5 份区域计划（oscap-wiring / server-robustness /
> apple-tm / ci-release / auth-security）按团队 lead 原计划应投递，但实际未单独产出；其内容已由
> plan-v0.4.0-ci-release.md（CI/Release 部分，覆盖 A1–A5/B1–B5）充分代表，PM 据此 consolid。
> 开工时间锚点：2026-08-10 23:25 CST。

---

## 会话恢复（2026-08-25，PM 记录）

> 团队于 2026-08-25 重启开工。以下时间戳来自 team-lead 会话，原样记录，
> 作为崩溃复盘的时间锚点证据（AGENTS.md §7.6 / §10）。
> 基线：v0.3.0 已发布，`origin/main` = `31d2902`。

### 时间线

| 时间（CST） | 事件 |
|---|---|
| 2026-08-25 14:20 | 会话恢复 |
| 2026-08-25 14:31 | 开始环境恢复 |
| 2026-08-25 14:32 | Go 工具链重装完成（go1.25.0 linux/amd64） |
| 2026-08-25 14:33 | `origin/main` 基线自检通过：`CGO_ENABLED=0 go build ./...` |

### 分工开工状态（team-lead 已开三个开发 worktree 并推空分支）

| 分支 | Owner | 任务 | 备注 |
|---|---|---|---|
| `vfs/oscap-robustness` | vfs | M1 / OI-1（builtin bbolt 跨进程锁修复） | ✅ 已完成并合入（PR #183 @ b1dc47e，15:51） |
| `qa/v040-ci-hardening` | qa | A1（windows CI 修复）+ A3（acceptance.sh harness 防御） | ✅ 已完成并合入（PR #184 @ 9ce47c6，15:51）；本机全量 acceptance 三客户端真跑绿 |
| `server/v040-deflake` | server | B1（时序测试计数化，OI-2 / M2） | ✅ 已完成并合入（PR #185 @ 1e9641c，15:51）；8/8 变异反向对照变红 |
| `pm/v040-board-merge` | pm | 本进度板合入分支（基于 `origin/pm/v0.4.0-board` = `6ec7383`） | ✅ 已合入（PR #182，15:51） |

### 收敛结果（2026-08-25 16:05 更新，team-lead 记录）

- **六个 PR 合入 main**：#182（本板）、#183（M1/A2）、#184（A1+A3）、#185（B1）、#186（v0.3.0 三客户端交叉验证基线报告，`test/reports/client-matrix-v030-20260825.md`）、#176+#187（metadata_path 全平台校验与注释口径）。合并后 main = `34f22d9`。
- **六个 v0.2.0 时代遗留开放 PR 关闭留档**（分支未删）：#177/#179/#180/#181/#174/#157，理由见各 PR 评论；其中 #157（push 门禁只限 main）思路有效、待基于现行 .cnb.yml 重开。
- **合并后 main 全套门禁实测绿**（16:04）：build/vet/test 全过；check-test-compile（四平台×全 tag 含 _test.go）rc=0；check-constraints rc=0；portable-mode rc=0；**acceptance.sh rc=0**——smbclient/impacket/go-smb2 三客户端 + dialects/signing/encryption/readonly/authfail/guest 全 PASS，mount.cifs 按环境限制 SKIP。OI-1 多进程场景即此验收的一部分，黑盒确认已修复。
- **待办移交**：M4/M5 仍 TODO（区域计划未产出）；A4 必需门禁的镜像仓分支保护是 repo-owner 动作；打不打 v0.4.0 tag 由项目所有者拍板。

---

## 0. Goal（一句话目标 — ✅ 已定稿，Step 2 consolid）

**把 v0.3.0 已经接好的 6/6 `oscap` 能力从「能跑」打磨到「在真实 CI 与多进程场景下可靠」，
同时补齐服务端健壮性（时序测试抖动、连接限速/超时）与 CI 多 OS 门禁（GitHub Actions +
CNB），并完成认证安全审计与 Apple 扩展的真机/协议级验证收尾。**

> 定稿说明：本目标由 PM 基于 plan-v0.4.0-ci-release.md 与 AGENTS.md §1.2 / §2 定稿。
> 注意：任务原始草稿建议「close the oscap 2/6 → 6/6 wiring gap」，但 AGENTS.md §1.2
> （2026-08-10 更新，v0.3.0）已确认该接线缺口在 v0.3.0 已闭合（6/6 接进 VFS 数据路径）。
> 因此 v0.4.0 的 oscap 工作不再是「接线」，而是「builtin 旁路存储在多进程下的可靠性」
> （见 Known Open Issues #1 与 Open Decision OD-1）。原区域计划名 `oscap-wiring` 已按 OD-1
> 决议重命名为 `oscap-robustness`（见 §4.2）。
> CI/Release 范围（A1–A5/B1–B5）已在 Step 2 完全 consolid 进 §3/§4。auth-security 与
> apple-tm 两份区域计划未单独产出，其 Must 项（M4/M5）与 Optional 项（O1）暂为 TODO，待
> 对应区域计划补齐后回填验收准则。

---

## 1. Baseline（v0.3.0，锚定自 AGENTS.md §1.2 / §2）

| 维度 | v0.3.0 状态（事实） | 来源 |
|---|---|---|
| `oscap` 适配器 | 6 项能力（Xattr / NamedStream / Sparse / StableFileID / CreationTime / DOSAttributes）native + builtin **双双齐备**，均有单测（native 37 PASS、builtin 49 PASS，均 0 SKIP / 0 FAIL） | AGENTS.md §1.2 表，PR #138 / #129 |
| `oscap` 接线 | **6/6 已全部接进 VFS 数据路径**（v0.3.0 由 `vfs-sparse`/`vfs-attr` 经 `caps.*()` 接进；`cmd/stupidsamba` 逐共享下传 `filesystem_mode`）。`go list -deps ./cmd/stupidsamba \| grep -c oscap` = 3 | AGENTS.md §1.2「OSCAP 接线状态（v0.3.0）」行 193-208 |
| `filesystem_mode` 三态 | auto / native / portable 对全部六项能力**均已有真实运行期效果** | 同上 |
| builtin 旁路存储 | 纯 Go bbolt 嵌入式 KV（无 CGO），满足 C1 | PR #129 |
| freebsd 编译缺口 | **已闭合**（v0.3.0）：`test/ci/check-test-compile.sh` 平台列表含 `freebsd/amd64`，6 个 `!linux && !darwin && !windows` 文件已在 CI 编译验证 | AGENTS.md §1.2 行 225-230 |
| Apple 扩展 | AAPL create context、ADS（`file:AFP_AfpInfo`/`file:AFP_Resource`/`com.apple.*` xattr）、`_adisk._tcp` 广播、稀疏文件 FSCTL **代码已实现并保留**，但 **Time Machine 真机验收降为可选项、未真机验证** | AGENTS.md §2 阶段二（2026-08-09 决定） |
| CI 门禁 | CNB（`linux`）+ **新增 GitHub Actions 多 OS**（新于 v0.3.0，见下） | 本仓库 `.cnb.yml` 与 `.github/workflows/ci.yml` |
| 发布基线 commit | `0dbff50`（origin/main，任务给定）；当前 HEAD 在 `teamlead/probe-push`（`f2d5a26`，含 GitHub Actions 扩展工作流） | `git log` |

> 备注：v0.3.0 的发布目标是「闭合 2/6→6/6 接线缺口」，现已达成。v0.4.0 基线起点即「接线已闭合，
> 但存在运行期缺陷（builtin 锁冲突）与 CI 多 OS 未稳定」——见 Known Open Issues。

---

## 2. Known Open Issues（来自 GitHub Actions 首跑，run 31402256524 @ github.com/idealisan/patrickSamba）

> 以下为团队 lead 提供的 **run 31402256524 实测事实**，PM 原样记录，待对应 Owner 在 Step 2 细化根因与修复计划。

### OI-1 — builtin oscap bbolt 旁路存储跨进程锁冲突
- **现象**：多个 server 进程共享同一 share 目录时，打开旁路存储报 `打开旁路存储 ... timeout`，阻塞 GitHub CI 上的 acceptance。
- **影响**：`portable` 模式在多进程/CI 场景下不可用；直接阻塞 acceptance 门禁。
- **Owner**：`vfs` / `oscap`（区域计划 `oscap-robustness`，即任务名 `oscap-wiring` 的实际内容）。
- **关联**：C1（禁止 CGO）/ C9（旁路存储须纯 Go），不能用带文件锁的外部 KV。
- **对应 backlog**：A2（产品修复）+ A3（harness 防御）。

### OI-2 — `internal/server` 时序测试在 CI runner 上抖动
- **现象**：以下测试在 ubuntu / macos runner 上偶发失败（flaky）：
  - `TestMaxConnectionsEnforced`
  - `TestRejectLogIsThrottled`
  - `TestOversizedFrameDropsOnlyThatConnection`
  - `TestMalformedFrameDropsOnlyThatConnection`
  - `TestIdleTimeoutClosesConnection`
  - `TestSlowlorisCannotHoldAllSlots`
  - `TestSlowlorisPartialFrameCannotHoldSlot`
  - `TestEstablishedSessionKeepsLongIdleTimeout`
- **影响**：CI 红绿不可信，可能掩盖真实回归。
- **Owner**：`server`（区域计划 `server-robustness`）。
- **方向**：用确定性时钟/调度注入替代真实 wall-clock 等待；设宽松但合理的上下界；考虑 `-race` 与 runner 负载。
- **对应 backlog**：B1（产品修复：计数替代计时）+ A5（隔离/重试）。

### OI-3 — GitHub Actions `windows-latest` 单测任务因内联 `CGO_ENABLED=0` 失败
- **现象**：workflow 内联 `CGO_ENABLED=0 go ...`（PowerShell 环境下）在 windows-latest 上破坏。
- **影响**：windows 平台单测门禁形同虚设（或全红）。
- **Owner**：`ci-release`（区域计划 `ci-release`，工作流修复）。
- **佐证**：`.github/workflows/ci.yml` 行 50/66/68/71/77 均为内联 `CGO_ENABLED=0 go ...`，需改为 `$env:CGO_ENABLED=0` 或 `go env -w CGO_ENABLED=0` 后再调用。
- **对应 backlog**：A1。

---

## 3. Scope（Step 2 由 plan-v0.4.0-ci-release.md 推导 consolid）

> 来源：plan-v0.4.0-ci-release.md 的 (a) 失败根因表与 (c) v0.4.0 CI/Release backlog A1–A5、B1–B5。
> 每项映射到 Owner 角色 + 状态（TODO/IN-PROGRESS/DONE）+ 验收准则（原文引用 plan）。
> 状态由 PM 按各 Owner 分支实际开工情况更新（2026-08-25 起首批 IN-PROGRESS，见「会话恢复」小节）。

### Must（发布阻塞）
| # | 条目 | Owner | 来源 (A/B) | 状态 | 验收准则（引用 plan） |
|---|---|---|---|---|---|
| M1 | 修复 builtin bbolt 跨进程锁冲突（OI-1）：产品侧 A2 + harness 侧 A3 防御 | vfs/oscap + qa | A2, A3 | **DONE（2026-08-25 16:00）**：PR #183（A2，`vfs/oscap-robustness` @ b1dc47e）+ PR #184（A3，`qa/v040-ci-hardening`）均已合入 main。验收证据：helper-process 多进程回归测试（变异体 go test -overlay 反向对照 3/3 变红）；合并后 main 上 `acceptance.sh` 全量重跑 rc=0（16:04，三客户端全 PASS——即 A2 准则「单 share 目录 3 server 进程」的黑盒确认）。另：#176（metadata_path 全平台校验）、#187（字段注释口径同步）已随收敛一并合入 | A2：「`acceptance.sh` smbclient case passes on a single share dir with 3 server processes; add a regression test that starts ≥2 servers on one share.」A3：「harness runs green even if a future product change re-introduces shared paths.」 |
| M2 | 收敛 server 时序测试抖动（OI-2）：产品计数化 B1 + 隔离/重试 A5 | server + qa | B1, A5 | **DONE（2026-08-25 15:51 合入）**：PR #185（B1，`server/v040-deflake` @ 1e9641c）。8 个 flaky 用例全部计数化，overlay 变异 8/8 变红，8 忙循环+GOGC=5 高负载 `-count=12` 全绿；**无需 -skip 隔离**（第 9 个 TestPathLookupScaling 已由 PR #149 在 vfs 修复），故 A5 无需单独落地 | B1：「the timing test is green under the local false-red recipe … and the `server-timing` job can become required.」A5：「`unit` is green on shared runners regardless of timing noise.」 |
| M3 | 修复 GitHub Actions `windows-latest` 单测（OI-3） | qa | A1 | **代码侧 DONE（2026-08-25 15:51 合入）**：PR #184 顶层 `defaults: run: shell: bash` + 移除内联前缀。⚠️ runner 真机验证待镜像仓下次 GA run 后回填（本容器无法跑 Windows）——在此之前按「只交叉编译过」档对待 | A1：「`windows-latest` unit job goes green on a push.」 |
| M4 | 认证安全审计（NTLMv2 校验 / 常量时间比较 / 日志不落口令） | auth | auth-security 区域计划（未产出） | TODO（阻塞于区域计划） | 验收准则待 auth-security 区域计划回填 |
| M5 | Apple 扩展协议级/真机验证收尾，口径按证据强度分档 | mdns/apple | apple-tm 区域计划（未产出） | TODO（阻塞于区域计划） | 验收准则待 apple-tm 区域计划回填（见 OD-3） |

### Should（强烈建议）
| # | 条目 | Owner | 来源 (A/B) | 状态 | 验收准则（引用 plan） |
|---|---|---|---|---|---|
| S1 | 把 3 客户端 acceptance 设为必需门禁（单一聚合步骤替代原 3 个 fail-fast 步骤） | qa + team-lead | A4 | TODO | A4：「a push with any of smbclient/impacket/gosmb2 broken turns the required check red.」 |
| S2 | `portable` 模式在 GitHub CI 真跑（对齐现有 `portable-mode.sh` 反向对照） | qa | plan §(b) `unit` 任务 | TODO | `test/ci/portable-mode.sh` 在 `ubuntu-latest` 上绿 |
| S3 | 保留 `cross-vet` 作为 freebsd 缺口补齐（覆盖 CNB 未编译的 6 个 freebsd build-tag 回退文件） | qa | B2 | TODO | B2：「remains in the workflow.」 |
| S4 | build-tag 集合不变量：`ci.yml` 与 `check-test-compile.sh` 的 `TAGS=` 字节一致 | qa | B3 | TODO | B3：「adding a tag to one and not the other is caught.」 |
| S5 | 发版窗口纪律：CNB `tag_push` 为唯一发版路径，GA 不作为发版门禁 | team-lead | B5 | TODO | B5：「pushing a `v*` tag still produces artifacts + image + Release only via CNB.」 |

### Optional
| # | 条目 | Owner | 来源 (A/B) | 状态 | 验收准则（引用 plan） |
|---|---|---|---|---|---|
| O1 | Time Machine 真机验收（仍为可选项，不强制） | mdns/apple | apple-tm 区域计划（未产出） | TODO（可选） | 证据门槛按 OD-3 决议 |
| O2 | 其他文档口径诚实性审计（README/CHANGELOG 与四处 OSCAP 块一致） | docs/pm | — | TODO | 文档口径与代码/状态板一致 |
| O3 | 可选二进制产物上传（仅诊断用途，非发版产物） | qa | B4 | TODO | B4：「artifact appears on every run; clearly documented as diagnostic only, not a release artifact.」 |

---

## 4. Risks / Open Decisions / Owner Mapping

### 4.1 Risks（合并 plan R1–R7 + OI 派生风险）
| # | 风险 | 影响 | 关联 | 来源 |
|---|---|---|---|---|
| R1 | **Source-of-truth divergence**：CNB `.cnb.yml` 为发版门禁，GitHub Actions 为补充；两者在「必需检查集 / build-tag 列表」上漂移 → 「CNB 绿 / GA 红」或反之 | 高 | A4, B5（plan R1） | plan R1 |
| R2 | **A2 fix scope**：已批准在 PR 内的修复是「同进程按路径引用计数」，**不解决** `acceptance.sh` 触发的跨进程场景（3 个独立进程）。A2 必须显式处理多进程共享，或 harness(A3) 保证每实例唯一 `metadata_path`。需拍板契约：「builtin 旁路店为单进程」vs「多进程安全」 | 高 | OI-1, A2, A3（plan R2） | plan R2 |
| R3 | **Timing regex 占位**：`-run`/`-skip` 正则（`Timing\|Scaling\|Latency`）为占位；落地 A5/B1 前须确认 `internal/server` 中真实 flaky 用例名，否则隔离会跳过错用例（或全不跳） | 中 | B1, A5（plan R3） | plan R3 |
| R4 | **`set -e` vs GitHub step 语义**：GA 的 `bash` 步骤默认以 `bash -e` 运行，原三步 acceptance 因此 fail-fast。单一聚合步骤依赖 `acceptance.sh` 自身不在中途 `exit`（已核对 `acceptance.sh:892`）。未来若脚本加早退，`bash -e` 症状复发 | 中 | A4（plan R4） | plan R4 |
| R5 | **Mirror lag**：GA 跑在 `github.com/idealisan/patrickSamba`（CNB 镜像）。镜像落后于 `origin/main` 则 GA 测陈旧树 | 中 | A4（plan R5） | plan R5 |
| R6 | **`qadefect` tag 语义**：`qadefect` 故意*失败*直至缺陷修复，须留在 `go test ./...`（gate_test）之外、但在 `cross-vet` tag 并集内。`unit` 用无 tag 的 `go test ./...` 正确忽略它；勿把 `qadefect` 加进 `unit` | 低 | B2, B3（plan R6） | plan R6 |
| R7 | **Artifact vs release**：GA 上传二进制可能被误当作发版产物。须明确仅诊断用途；CNB 的 `build-release.sh` + 镜像推送才是唯一发版产物 | 低 | B4（plan R7） | plan R7 |
| R8 | builtin 锁冲突不修 → `portable` 模式在多进程 CI 不可用，acceptance 阻塞（原 R1） | 高 | OI-1 | 原状态板 R1 |
| R9 | 时序测试持续 flaky → CI 红绿失真，真实回归被掩盖（原 R2） | 高 | OI-2 | 原状态板 R2 |
| R10 | windows CI 不修 → 四象限平台覆盖缺口（C7 跨平台受损）（原 R3） | 中 | OI-3 | 原状态板 R3 |
| R11 | 区域计划名 `oscap-wiring` 与已闭合的接线缺口语义冲突，易误导范围（原 R4） | 低/流程 | OD-1 | 原状态板 R4 |

### 4.2 Open Decisions（待团队 lead / 项目所有者拍板）
| # | 决策点 | 选项 | 现状 / 决议（team-lead sign-off @ 2026-08-12） |
|---|---|---|---|
| OD-1 | `oscap-wiring` 区域计划的实际范围 | (a) 重命名为 `oscap-robustness`，专注 builtin 锁/多进程可靠性；(b) 仍含其他 oscap 增强 | **✅ RESOLVED（APPROVED (a)）**：因 v0.3.0 接线缺口已闭合（AGENTS.md §1.2），区域计划名由 `oscap-wiring` → **`oscap-robustness`**；实际范围 = M1（A2 产品修复 + A3 harness 防御）。区域计划文件未单独产出，但内容已由 plan A2/A3 代表。`vfs` 已使用分支 `vfs/oscap-robustness`，命名一致。 |
| OD-2 | v0.4.0 是否以「多进程共享 share 的 acceptance 通过」为发布门槛 | 是 / 否（降级为 Should） | **✅ RESOLVED（APPROVED "是"）**：多进程共享 share 的 acceptance 为发布门禁——经 A4 把「3 客户端 acceptance（其本身起 3 个 server 进程共享一个 share）」设为必需 status check 实现；该门禁**以 A2（vfs 跨进程锁修复）先合为前提**。行动项：`qa` 将 A4 设为必需检查。**⚠️ 镜像仓 `github.com/idealisan/patrickSamba` 的分支保护须配置才能真正 enforce** —— 这是 repo-owner 动作，超出本团队直接控制范围，须在 PR 描述/向用户显式 flag。 |
| OD-3 | Apple 验证证据强度门槛（仅协议级实测即可，还是必须真机） | 协议级实测即可 / 必须真机 | **✅ RESOLVED（APPROVED）**：沿用 AGENTS.md §2 阶段二口径 —— Time Machine 真机验收**保持 OPTIONAL**；协议级实测证据即视为 M5 达标，真机为加分项。行动项：`mdns` 按证据强度分档汇报，不阻塞于真机。 |

### 4.3 Owner Mapping（模块角色，AGENTS.md §7.1）+ v0.4.0 backlog 列
| 角色 | 负责范围 | v0.4.0 预期职责 | v0.4.0 backlog 项（A1–A5/B1–B5） |
|---|---|---|---|
| `wire` | `internal/smb/wire`、`internal/smb/status` | 报文/NTSTATUS 维护（按需） | （无直接 A/B 项；按 R1–R7 无涉及，按需支撑） |
| `auth` | `internal/auth`、`internal/smb/crypto` | 认证安全审计（M4） | M4（auth-security 区域计划未产出，暂 TODO） |
| `vfs` | `internal/vfs`、`internal/oscap` | builtin 锁修复（M1/OI-1） | **A2**（产品修复：跨进程 bbolt 锁） |
| `server` | `internal/server`、`internal/smb/command`、`internal/smb/dialect` | 时序测试收敛（M2/OI-2） | **B1**（计数替代计时，产品修复）、**A5**（协同 qa：隔离/重试） |
| `mdns` | `internal/mdns`、`internal/config`、`cmd/`、Apple 扩展 | Apple 验证（M5）、装配层 | M5、O1（apple-tm 区域计划未产出，暂 TODO/可选） |
| `qa` | `test/`、`scripts/`、CI 接线 | 客户端回归（S3）、验收、CI 修复 | **A1**（windows CI）、**A3**（harness 防御）、**A4**（协同 team-lead：必需门禁）、**A5**（协同 server：隔离）、**B2**（保留 cross-vet）、**B3**（tag 不变量）、**B4**（可选产物） |
| `pm` | 进度/阻塞/决策（本文档维护者） | **不写产品代码**；Step 2 consolid | 本文档（Step 2 已 consolid） |
| `team-lead` | 协调 / 发版纪律 / 门禁 enforced | 跨进程契约拍板（OD-2）、分支保护 | **A4**（协同 qa：必需门禁）、**B5**（发版窗口纪律，唯一发版路径 CNB） |

---

## 5. Next Step（PM 待办）
- [x] **Step 2（consolid 进 §3 Scope、§4 Risks/Decisions、§4.3 Owner）**：由 PM 基于 plan-v0.4.0-ci-release.md（A1–A5/B1–B5）+ OI-1/2/3 + Must/Should/Optional 推导填充（区域计划文件未单独产出）。
- [x] **OD-1（RESOLVED）**：`oscap-wiring` → `oscap-robustness` 重命名已 sign-off（team-lead @ 2026-08-12）；`vfs` 分支命名一致。
- [x] **OD-2（RESOLVED "是"）**：多进程 shared-share acceptance 为发布门禁（经 A4 必需检查），前提 A2 先合。`qa` 行动项：把 A4 设为必需检查。**待 repo-owner 配置镜像仓分支保护（repo-owner 动作，已在 PR 描述 flag）。**
- [x] **OD-3（RESOLVED）**：TM 真机保持 OPTIONAL，协议级证据即达标。`mdns` 行动项：按证据强度分档汇报。
- [ ] **合入进度板**：team-lead 经 CNB API 为 `pm/v040-board-merge` 建 PR（本文件），CI 绿后合并。
- [ ] 跟踪 A1–A5 / B1–B5 各 Owner 的落地与 PR 进度，按角色 agent 回报更新 §3 状态列（TODO→IN-PROGRESS→DONE）；**各 Owner PR 就绪后回收验证证据**（验收命令输出 / CI run 链接）再置 DONE，未经证据不得标 DONE。
- [ ] 待 auth-security / apple-tm 区域计划补齐后，回填 M4/M5/O1 的验收准则（当前 TODO，阻塞于区域计划）。
