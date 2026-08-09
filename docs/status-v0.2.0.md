# stupidSamba v0.2.0 进度看板

> 维护者：`pm`（专职项目管理 agent）。**本文件的判断只采信客观证据**：分支上的 commit、
> 已合并的 PR、可复现的实测记录。agent 的自我汇报不作为进度依据。
>
> 最近盘点：**2026-08-09 12:15 CST**（第 1 轮 · 基线）

---

## 0. 完成度分档定义（不许含糊）

v0.1.0 吃过一次亏：Time Machine 文档第一版写「已实现全部前置能力」，
实际上 oplock/lease 是 0。为杜绝再犯，本看板一切条目只用下面四档，**不许出现第五种说法**：

| 档 | 含义 | 准入证据 |
|---|---|---|
| ✅ **已合入并实测** | 代码在 `main` 上，且有可复现的验证记录 | PR 号 + 验证方式（含**反向对照**） |
| 🟡 **已提 PR 待评审** | PR 开着，未合入 | PR 号 |
| 🔵 **在写** | 分支上有 commit，未提 PR | 分支名 + 最近 commit |
| ⚪ **没开始** | 分支未建，或建了但无 commit | — |

「已合入但没实测」记 🟡 不记 ✅。**判据不可证伪等于没验**
（`encryption_required` 就这么假阳性过一次：只测了默认路径，没测拒绝路径）。

---

## 1. 三大块总览

v0.2.0 范围由项目所有者定义为三件事：

| 块 | 完成度 | 依据 |
|---|---|---|
| A. 测试基础设施调研（Docker/VM、Win/mac 准备、硬件要求分档） | 🔵 在写 | `r-infra/test-infra` 1 commit：容器能力边界实测（结论：user namespace 才是 `mount.cifs` 的真死因） |
| B. Time Machine 兼容功能 | 🔵 在写 | 3 条分支各 1 commit，**均为「重构前基线 / golden test」性质，尚无功能落地** |
| C. Windows 后端 | 🔵 在写 | `win-meta/metadata-store` 1 commit（Store 接口 + bbolt + noop）；`win-vfs` 未建分支 |

**整体判断：v0.2.0 处于开工后约 10 分钟，全部内容尚无一行合入 `main`。
除 PR #1（devenv 削峰脚本）外，`main` 与 v0.1.0 tag 一致。**

---

## 2. 逐人盘点（第 1 轮 · 基线）

| 成员 | 分支 | commit | 改动量 | 最近产出 | 档 |
|---|---|---|---|---|---|
| `r-infra` | `r-infra/test-infra` | 1 | +232 | test-infra A 组：本容器能力边界实测 | 🔵 |
| `tm-handle` | `tm-handle/durable` | 1 | +261 | create context 响应链 golden test（重构前基线） | 🔵 |
| `tm-lease` | `tm-lease/oplock` | 1 | +327 | 租约 create context golden test（RqLs v1/v2） | 🔵 |
| `tm-vfs` | — | 0 | — | 未建分支 | ⚪ |
| `win-meta` | `win-meta/metadata-store` | 1 | +704 | `internal/meta`：Store 接口 + bbolt 实现 + noop | 🔵 |
| `win-vfs` | — | 0 | — | 未建分支（且被 `tm-vfs` 阻塞） | ⚪ |
| `qa` | — | 0 | — | 未建分支 | ⚪ |
| `docs` | — | 0 | — | 未建分支 | ⚪ |
| `audit` | — | 0 | — | 未建分支 | ⚪ |
| `pm` | `pm/status` | — | — | 本文件 | 🔵 |

开着的 PR：**0 个**（`GET /-/pulls?state=open` 返回 `[]`）。

> 基线阶段「未建分支」**不判哑火**——全队开工仅约 10 分钟。
> 哑火线是「分支建了但 >25 分钟无新 commit」或「>25 分钟仍无分支」，从下一轮起执行。

---

## 3. 关键路径与瓶颈

```
qa: CI 假绿修复  ──────────────►  在它合入前，全队的「CI 绿」都不可信【最高优先级】

tm-handle: create context 注册表重构 ──► tm-lease PR-2（真正授予 lease）
                                          （tm-lease PR-1 不依赖它，可并行）

tm-vfs: quota PR ──────────────────────► win-vfs: local.go 平台拆分
                                          （两人碰同一文件，必须串行）

win-meta: MetadataStore 接口定稿 ──────► win-vfs 接线
```

**当前瓶颈（按紧迫度排序）**：

1. **`qa` 尚未开工，而它是全队信任基础。** 在 CI 假绿修好之前，任何人说「CI 绿了」
   都不构成验收证据。这条卡的是**所有人的 ✅ 档准入**，不是某一个人的进度。
2. **`tm-vfs` 尚未开工，`win-vfs` 在其下游空等。** 这是唯一一条「前置未动 + 后继已被
   任务系统标记 blocked」的链路，滑坡代价最大。
3. `win-meta` 的 704 行已落盘但**接口尚未定稿宣告**，`win-vfs` 接线无法开始。
   建议 `win-meta` 一旦接口稳定就立即提 PR 并 DM `win-vfs`，不要等实现全做完。

---

## 4. 风险登记

| # | 风险 | 现状 | 应对 |
|---|---|---|---|
| R1 | **CI 假绿**：`test/` 因 build tag 从未被真正编译，gofmt 门禁在 HEAD 就是红的 | 未修复 | `qa` 的首个 PR，最高优先级 |
| R2 | **憋大提交**：环境已多次真实崩溃并清空 git 以外一切 | 目前健康，4 条分支均已推送 | 每轮盘点检查「久不 push」 |
| R3 | **单点文件冲突**：`internal/vfs/local.go` 被 `tm-vfs`/`win-vfs` 共同需要 | 未发生 | 串行，`tm-vfs` 先合 |
| R4 | **依赖引入**：`win-meta` 引入 bbolt。需确认纯 Go、License 兼容、`CGO_ENABLED=0` 可编译 | 待核 | 提 PR 时评审确认（AGENTS.md §4） |
| R5 | **「已验证」无反向对照** | 待防 | 任何 ✅ 档准入必须写明失败用例 |
| R6 | 容器 2C4G 无交换分区，V8 堆 2096 MB 上限触发 SIGABRT | 已有 `scripts/devenv.sh` 削峰（PR #1 已合） | 全员 `. scripts/devenv.sh`、输出加 `| head -N` |

---

## 5. 待决事项（需 `team-lead` 拍板）

| # | 事项 | 谁在等 |
|---|---|---|
| D1 | `qa` 的 CI 假绿修复是否应优先于其它一切 PR 评审插队合入？ | 全队（决定 ✅ 档何时可用） |
| D2 | `win-meta` 引入 bbolt 依赖是否批准（纯 Go / License / 零 CGO 三项核验） | `win-meta`、`win-vfs` |
| D3 | `tm-vfs`、`win-vfs`、`qa`、`docs`、`audit` 五人本轮尚无分支，是否已全部启动？ | `pm` 需确认，否则无法区分「刚起步」与「没收到任务」 |

---

## 6. 纪律检查（每轮必查）

| 项 | 本轮结果 |
|---|---|
| 有无向 `main` 直推 | ❎ 无。v0.2.0 唯一一次 `main` 变更走的是 PR #1，合规 |
| 有无久不 push | ❎ 无。已建的 4 条分支均在 4 分钟内有推送 |
| 有无在 `/workspace` 改文件 | 未发现（`/workspace` 停在 `main`） |
| 有无「已验证」缺反向对照 | 本轮无 ✅ 档条目，暂不适用 |
