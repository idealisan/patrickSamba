# v0.5 修复波进度板

- 维护人：`pm`（AGENTS.md §7.1：专职项目管理，不写产品代码）
- 工作目录：`/work/pm2`（worktree，分支 `pm/v050-board`，基于 origin/main = `0deebf5`）
- 最后更新：2026-08-26 10:05 CST（第二波开板；`date` 实测）

---

## 0. 结论先行

**本轮（v0.5 修复波）范围 = 三部分**：

1. **对照 Samba 源码排查产出的缺陷修复** —— 高危主线 **B1–B6** 分三条修复支推进
   （lock / meta / stream 三域），另带两条中危随 `fix/stream-b6`；其余中低危见 §1.3 待分派。
2. **native 档移除**（`filesystem_mode` 两态化为 auto/portable）—— **已完成**：
   PR #191（merge `vfs/remove-native-mode`，commit `0deebf5`）已于今日 19:32 合入 main。
3. **性能基线**（v0.4.0 性能基线报告入仓）—— **已完成**：
   PR #192（merge `qa/v040-perf-baseline`，commit `0d33643`）已于今日 19:31 合入 main。

排查证据档已入库本分支：`docs/bughunt-20260825/findings-bh{3,4,5}.md`
（92 / 122 / 128 行，原样拷贝自 `/tmp/opencode`，commit `2bb02a3` @ 19:36）。
**bh1/bh2 的 findings 文件截至 19:40 尚未出现在 `/tmp/opencode/`**，见 §5 待决第 2 条。

---

## 1. 缺陷清单与分派

> 编号对应关系说明：B1/B2 ↔ bh4 报告 A#1/A#2，B3/B4/B5 ↔ bh3 报告 F1/F2/F3，
> B6 ↔ bh5 报告 F1（按「分支名域 + 报告内高危顺序」推定，与三条修复支的命名一一吻合；
> 若 team-lead 的原始编号与此有出入，以 team-lead 口径为准并回改本表）。

### 1.1 高危 B1–B6（已分派，状态均为 TODO）

| 编号 | 报告条目 | 严重度 | 一句话 | Owner 分支 | 状态 |
|---|---|---|---|---|---|
| B1 | bh4-A#1 | 高 | 字节范围锁对 READ/WRITE 完全无阻挡（advisory 空壳）：A 上独占锁后 B 的读写照常穿透，Excel/SQLite 类互斥失效 | `fix/lock-b1-2` | TODO |
| B2 | bh4-A#2 | 高 | 非 CLOSE 路径句柄消失（TREE_DISCONNECT / LOGOFF / 断连 / durable 回收）不释放字节范围锁，泄漏到进程退出 | `fix/lock-b1-2` | TODO |
| B3 | bh3-F1 | 高 | CREATE 请求携带的 FileAttributes 被整体丢弃，初始 ARCHIVE 也不入库（Samba 会剥 DIRECTORY 叠 ARCHIVE 落 xattr） | `fix/meta-b3-5` | TODO |
| B4 | bh3-F2 | 高 | 新建对象从不调 SetCreationTime —— `builtin/times.go:16` 注释声称的行为不存在，btime 在 portable/无 btime 文件系统上永不持久 | `fix/meta-b3-5` | TODO |
| B5 | bh3-F3 | 高 | rename/remove 不迁移不清除 bucketTimes/bucketDOS：路径复用会继承陌生文件的创建时间与 DOS 位（跨对象元数据泄漏），setRename 后句柄仍按旧路径读记录 | `fix/meta-b3-5` | TODO |
| B6 | bh5-F1 | 高 | 流句柄「关闭时删除」要么误删整个基础文件（数据丢失级）、要么静默什么都不删 | `fix/stream-b6` | TODO |

### 1.2 随支中危两条（归 `fix/stream-b6`，状态 TODO）

| 报告条目 | 严重度 | 一句话 | Owner 分支 | 状态 |
|---|---|---|---|---|
| bh5-F2 | 中 | 基础文件不存在时对流做创建性打开直接失败（Samba/Windows 会顺带建出基础文件） | `fix/stream-b6` | TODO |
| bh5-F3 | 中 | SUPERSEDE/OVERWRITE* 不清除既有 ADS：覆盖写旧文件后 FinderInfo/资源派生残留（「新内容 + 前世元数据」） | `fix/stream-b6` | TODO |

> ⚠️ **归属缺口**：bh3-F4（READONLY 零强制）同为**高危**，但不在 B1–B6 六个编号内、
> 指令亦未给其分派。已列入 §5 待决第 4 条催办，暂挂「待分派」。

### 1.3 其余中低危（TODO，待分派）

| 报告条目 | 严重度 | 一句话 | Owner | 状态 |
|---|---|---|---|---|
| bh3-F4 | **高** | READONLY 位没有任何强制面：设只读后照样可写、可 DELETE_ON_CLOSE；MxAc 已向客户端报告无写权限却实际放行 | 待分派 ⚠️ | TODO |
| bh3-F5 | 中 | DOS 属性合成方向与 Samba 相反：永远叠加 POSIX 推导位（OR 语义），存储记录无法清除推导位，「清除只读」静默失效 | 待分派 | TODO |
| bh3-F6 | 中 | 显式设置 write time 后缺 sticky/pending 语义，后续写入冲掉所设值 | 待分派 | TODO |
| bh3-F7 | 低 | btime 回退值口径不同（我们用 ctime，Samba 用 MIN(ctime,mtime,atime)） | 待分派 | TODO |
| bh3-F8 | 低 | SET_INFO 的 FileAttributes 未滤客观位（DIRECTORY/SPARSE/REPARSE）即入库 | 待分派 | TODO |
| bh4-A#3 | 中 | 零长度 READ 回 END_OF_FILE 且该校验排在目录/权限检查之前（应成功回 0 字节） | 待分派 | TODO |
| bh4-A#4 | 中 | 阻塞锁（未置 FAIL_IMMEDIATELY）应挂起等待，我们一律立即拒绝 | 待分派 | TODO |
| bh4-A#5 | 低中 | 零长度锁的「分界点」冲突语义缺失（EOF 处零长独占锁互斥失效） | 待分派 | TODO |
| bh4-A#6 | 低 | 同一句柄重复上独占锁放行（Samba/torture 要求拒绝） | 待分派 | TODO |
| bh4-A#7 | 低 | 回绕锁区间应报 INVALID_LOCK_RANGE，现被当作「与一切冲突」 | 待分派 | TODO |
| bh4-A#8 | 低 | FLUSH 缺访问校验：只读文件句柄/目录一律回成功 | 待分派 | TODO |
| bh4-A#9 | 低 | 多元素 LOCK 含任一阻塞元素应报 INVALID_PARAMETER | 待分派 | TODO |
| bh4-A#10 | 低 | 目录句柄 LOCK 应报 INVALID_DEVICE_REQUEST，现照常授锁 | 待分派 | TODO |
| bh4-A#11 | 提示 | WRITE 的 DataOffset 未做「必须等于头+49」强校验 | 待分派 | TODO |
| bh4-A#12 | 提示 | CreditCharge 不校验是否覆盖载荷 | 待分派 | TODO |
| bh4-A#13 | 提示 | WRITE_UNBUFFERED 标志被忽略（≥3.0.2 应并入 Sync） | 待分派 | TODO |
| bh5-F4 | 中 | SET_ZERO_DATA 不做字节范围锁检查（与 B1 同型缺口，若 lock 支顺手可考虑带走，待拍板） | 待分派 | TODO |
| bh5-F5 | 低中 | VALIDATE_NEGOTIATE_INFO 用整表顺序相等比对方言，规范算法是最大公共方言匹配 | 待分派 | TODO |
| bh5-F6 | 低中 | VNI 校验失败后服务端不断连（规范 MUST terminate transport connection） | 待分派 | TODO |
| bh5-F7 | 低 | QUERY_ALLOCATED_RANGES 接受 WriteData 权限（Samba 仅认 ReadData） | 待分派 | TODO |
| bh5-F8 | 低 | SET_SPARSE 少认 APPEND_DATA 访问位 | 待分派 | TODO |
| bh5-F9 | 低 | 稀疏三件 FSCTL 打到流句柄上一律 NOT_SUPPORTED（Samba 映射到基础文件） | 待分派 | TODO |
| bh5-F10 | 低 | 通用流名查找大小写敏感（Samba 大小写不敏感共享上有兜底匹配） | 待分派 | TODO |
| bh5-F11 | 提示 | 「file:」尾冒号被当主数据流打开；VFS 与命令层两套解析器类型后缀口径不一 | 待分派 | TODO |
| bh5-F12 | 提示 | AFP_AfpInfo 新建后直到首次写入才进入流清单（netatalk 模式固有语义，需钉测试或改行为） | 待分派 | TODO |

> 各报告的「查证无误项」（bh3 OK1–OK7、bh4 B 组 15 条、bh5 P1–P16）不入缺陷清单，
> 原文见 `docs/bughunt-20260825/` 对应报告。

---

## 2. 时间线（2026-08-25，时间取自 git log / date 实测，CST）

| 时间 | 动作 | 证据 |
|---|---|---|
| 16:23–16:26 | **v0.4.0 发版收尾**：CHANGELOG 入库（7081106 @ 16:23），merge release/v040-prep（2af4771 @ **16:26**） | git log main |
| 18:07–18:40 | 三路 bughunt 对照 Samba 源码只读排查窗口（bh3 18:07–18:25 / bh4 18:07–18:35 / bh5 18:08–18:40） | 三份报告头 |
| 18:28–18:34 | native 档移除提交链落盘（59770f5 oscap @ 18:28 → 9d36cce config @ 18:31 → 428417c docs @ 18:34） | git log main |
| 19:21–19:31 | **性能基线报告入仓并合入**（d498a88 @ 19:21 → merge qa/v040-perf-baseline 0d33643 @ **19:31**，PR #192） | git log main |
| 19:32 | **native 档移除合入 main**（merge vfs/remove-native-mode `0deebf5` @ **19:32**，PR #191）；此后 `filesystem_mode` 仅剩 auto/portable 两态 | git log main |
| 19:36 | 本分支把 bh3/bh4/bh5 三份报告从 `/tmp/opencode` **抢救入库**（2bb02a3，342 行）——环境不稳定，仓库外成果先落地 | pm/v050-board |
| 19:38 | 盘点三条修复分支远端状态：均已建立、均指向 `0deebf5`（零独立提交） | §3 表 |

---

## 3. 修复分支远端状态

来源：`git ls-remote --heads origin | grep fix/`，2026-08-25 19:38 CST 实测。

| 分支 | 远端 HEAD | 相对 origin/main(0deebf5) | 判读 |
|---|---|---|---|
| `fix/lock-b1-2` | `0deebf5` | 持平（= main HEAD） | 已立项、开工基线就绪，**尚无提交推送** |
| `fix/meta-b3-5` | `0deebf5` | 持平（= main HEAD） | 同上 |
| `fix/stream-b6` | `0deebf5` | 持平（= main HEAD） | 同上 |

PM 判读：三条支均已切出并推上远端（空分支可见，符合 §7.3.1 开工流程），
但截至 19:38 无任何一条有自己的提交。若 19:50 后仍为空，逐支催办一次。

---

## 4. 本轮已完成项（非缺陷类）

| 项 | 状态 | 证据 |
|---|---|---|
| native 档移除（两态化） | ✅ 完成，已合 main | PR #191，merge commit `0deebf5` @ 19:32 |
| 性能基线（v0.4.0 报告入仓） | ✅ 完成，已合 main | PR #192，merge commit `0d33643` @ 19:31 |
| bughunt 证据档入库（bh3/bh4/bh5） | ✅ 完成于本分支 | commit `2bb02a3` @ 19:36 |

---

## 5. 待决事项（均催办对象：team-lead）

1. **CHANGE_NOTIFY 立项与否** —— 未决。若立项需给独立分支与 Owner，进下一版范围讨论。
2. **bh1/bh2 结果回收** —— `/tmp/opencode/findings-bh1.md`、`findings-bh2.md` 截至 19:40 未出现。
   出现后由本板第一时间原样入库 `docs/bughunt-20260825/`，并把新发现补进 §1 清单。
3. **README 三态措辞订正尾巴** —— native 档已移除，但 `README.md` 仍残留三态表述
   （实测 19:40：L287 配置表、L530 校验说明、L545–550 已知限制第 7 条仍在讲 native 档启动失败）。
   AGENTS.md / CHANGELOG / configs/example.yaml 均已两态化，唯 README 未跟上，需派一名 owner 订正。
4. **bh3-F4（READONLY 零强制，高危）归属** —— 高危但不在 B1–B6 编号内、指令未给分派；
   需 team-lead 拍板是否入本轮、归哪条支（语义上偏 meta 支的访问判定面，也可能随 lock 支走强制面）。
5. **bh5-F4 是否随 lock 支带走** —— SET_ZERO_DATA 缺锁检查与 B1 同型同修法
   （同一张 locks 表同一个 conflict 谓词），若 B1 动手时顺路成本极低；待 team-lead 决定。

---

## 6. 板子维护约定

- 本板只由 `pm` 维护；各 Owner 完成一项后知会 pm 回填状态（TODO → INPROGRESS(分支@sha) → DONE(PR#)）。
- 所有时间戳一律 `date` 实测后落笔（AGENTS.md §7.6）。
- 证据档（findings 原文）只增不改；订正在本板引用处标注勘误，不动原文。

---

# 第二波（2026-08-26）

> 本章由 `pm` 于第二波开工时追加。第一波内容（§0–§6）为历史证据，**不再改动**；
> 第一波遗留的待决项在本章 §W2.4 关闭。

## W2.0 结论先行

**第二波范围 = 四组缺陷修复 + 一个性能专项**：

1. **bh4 A#3–A#13**（锁/读写边界 11 条）→ `lock`
2. **bh3 F4 残余 + F5–F8**（元数据 5 条；F4 即第一波 §5 待决第 4 条那个高危 READONLY 缺口，本轮已分派）→ `meta`
3. **bh5 F4/F7/F8/F9 + VNI F5/F6**（6 条；F4 即第一波 §5 待决第 5 条，本轮拍板归 fsctl 而非随 lock 支带走）→ `fsctl`
4. **bh5 F10/F11/F12**（命名流 3 条）→ `stream`
5. **协议栈性能调优专项** → `perf`

team-lead 已拍板两项决策（见 §W2.4）：bh5-F4 归 fsctl；CHANGE_NOTIFY 本波不做、留待下一版讨论。
bh1/bh2 findings 文件确认丢失，不再追（见 §W2.3）。

## W2.1 分工表（开工基线，状态 TODO）

| 角色 | 分支 | 工作目录 | 任务 | 端口 | 状态 |
|---|---|---|---|---|---|
| lock | `lock/bh4-a3-a13` | /work/w2-lock | bh4 A#3–A#13 锁/读写边界 11 条 | 4451 | TODO |
| meta | `meta/bh3-f4-f8` | /work/w2-meta | bh3 F4 残余+F5–F8 元数据 5 条 | 4452 | TODO |
| fsctl | `fsctl/bh5-f4-f9-vni` | /work/w2-fsctl | bh5 F4/F7/F8/F9 + VNI F5/F6 | 4453 | TODO |
| stream | `stream/bh5-f10-f12` | /work/w2-stream | bh5 F10/F11/F12 命名流 3 条 | 4454 | TODO |
| perf | `perf/v050-hotpath` | /work/w2-perf | 协议栈性能调优专项 | 4455 | TODO |
| pm（本板） | `pm/wave2-board` | /work/w2-pm | 进度板 | — | INPROGRESS |

缺陷详情证据档：各 worktree 内 `docs/bughunt-20260825/findings-bh{3,4,5}.md`（已在库，
commit `2bb02a3` @ 2026-08-25 19:36）。上一波 B1–B6 已全部合入 main。

## W2.2 开工时间线（时间均为 `date` 实测 CST）

| 时间 | 动作 |
|---|---|
| 09:59:55 | pm 上岗：确认工位 `/work/w2-pm`、分支 `pm/wave2-board` @ `73f96cf`（= origin/main），树干净；`ls /work` 见 6 个 wave2 工位齐 |
| 10:00:59 | 复查证据档：`docs/bughunt-20260825/` 现有 bh3/bh4/bh5 三份（146 行板子 + 342 行报告在库）；`/tmp/opencode/findings-bh{1,2}.md` **均不存在**（→ §W2.3） |
| 10:01:59 | 巡检第 0 轮（开工基线）：五条工作分支远端 HEAD 全部 = `73f96cf`，零独立提交 |
| 10:05 | 第二波章节建板并推送 |

## W2.3 bh1/bh2 回收复查结果

2026-08-26 10:00:00 CST 实测：`/tmp/opencode/findings-bh1.md` 与
`/tmp/opencode/findings-bh2.md` **仍不存在**（`ls` 返回「没有那个文件或目录」）。
判定：两份 findings 随环境丢失，**不再追**，也不重跑排查。
第一波 §5 待决第 2 条就此关闭——若后续任何角色在自己的工作目录里发现 bh1/bh2 的副本，
再知会 pm 原样入库（只增不改），届时重新打开本条。

## W2.4 决策记录（team-lead 已拍板）

| # | 决策 | 内容 | 关闭的第一波待决项 |
|---|---|---|---|
| D1 | bh5-F4 归属 | SET_ZERO_DATA 缺字节范围锁检查归 **fsctl 角色**修（不随 lock 支顺路带走） | §5 待决第 5 条 |
| D2 | CHANGE_NOTIFY | **本波不做**，留待下一版讨论是否立项 | §5 待决第 1 条 |

注：第一波 §5 待决第 4 条（bh3-F4 归属）亦已闭环——分派给 meta 角色（见 §W2.1 分工表）。
README 三态措辞订正尾巴（第一波 §5 待决第 3 条）不在本波分工表内，维持待派状态。

## W2.5 巡检记录（分支 HEAD 变化）

巡检命令：`git fetch origin && git ls-remote --heads origin`，取五条工作分支 HEAD。
催办阈值：某分支超过 20 分钟零提交 → 板内 ⚠️ 催办。

| 轮次 | 时间(CST) | lock/bh4-a3-a13 | meta/bh3-f4-f8 | fsctl/bh5-f4-f9-vni | stream/bh5-f10-f12 | perf/v050-hotpath | 备注 |
|---|---|---|---|---|---|---|---|
| 0 | 10:01:59 | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 开工基线，均零独立提交 |
| 1 | 10:07:59 | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 均无新提交（距基线 6 分钟，未到催办阈值） |
| 2 | 10:13:02 | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 73f96cf | 均无新提交（距基线 11 分钟，未到催办阈值） |

