---
name: CNB 云原生开发环境的寿命与「只有 /workspace 会被备份」
description: 无固定最长时长（实测同仓库 2.7h 与 10.6h 两例），回收是闲置触发；/work/* 在 overlay 上不备份，只有 /workspace 卷会保留；附零 CI 额度的保命推送法
type: reference
---

## 怎么查（唯一可信的一手来源）

```sh
cnb workspace list-workspaces --slug <org>/<repo> --page-size 5
```

返回字段里有用的：`status`（running/closed）、`create_time`、`duration`（**毫秒**）、
`workspace`（被跟踪的目录）、`file_count` / `file_list`（关环境时记录的变更文件）、`backup_status`。

`https://api.cnb.cool/user/workspaces` 是 404，别去猜端点。
`cnb workspace list-workspaces` **不带 `--slug` 会打印出 CLI 自身的压缩源码**（参数缺失时的 bug），
看到满屏 JS 不要以为环境坏了。

## 实测结论（2026-08-09，finalappstore/stupidSamba）

| 环境 | 创建 | 状态 | duration |
|---|---|---|---|
| `cnb-k2g-1jvgqe1hh` | 2026-08-08T13:54:21Z | closed | 9727378 ms = **2.70 小时** |
| `cnb-u48-1jvhsr34p` | 2026-08-08T23:55:40Z | running | 38298105 ms = **10.64 小时** |

- **没有固定的最长运行时长**。官方文档只写「闲置时自动回收」，不给数字；
  实测同一个仓库先后两个环境一个 2.7 小时被关、一个跑到 10.6 小时还活着，
  相差近 4 倍 —— 所以**「跑了 N 小时该到期了」是错误的心智模型**，
  它取决于闲置，不取决于累计时长。反过来也成立：**闲置一会儿就可能被收走，
  和已经跑了多久无关**。
- 规格：`nproc`=2、内存 4 GB。也就是说环境本身按 **2 核时/小时** 烧远程开发额度
  （与 CI 额度是两个口子，但都按核时计）。跑 10.6 小时 ≈ 21 核时。

## 最关键的一条：只有 `/workspace` 会被保留

```
git-clone-yyds  512G  /workspace     ← 独立卷，会保留
overlay         256G  /              ← 容器层，/root 和 /work 都在这儿，回收即没
```

`list-workspaces` 的 `workspace` 字段就是 `/workspace/`，`file_list` 里记的也全是
`/workspace` 下的路径。**`/work/*` 的所有 agent worktree 在 overlay 上，不在备份范围内。**

**How to apply**：本项目 §7.3.1 要求每人一个 `/work/<role>` worktree —— 这些目录里
**未提交的改动在环境回收时会 100% 丢失**，`.git` 对象库虽然在 `/workspace/.git`（会保留），
但那只救得了**已经 commit 的东西**。所以「尽快提交」在本环境不是口号，是字面意义的保命。
更稳的做法是推到远端（见下）。

## 零 CI 额度的保命推送：推到非分支 ref

```sh
git push origin HEAD:refs/rescue/<名字>
```

**实测（2026-08-09 18:34）**：用它一口气存了 9 个离队 agent 的未提交改动，
随后查 `/-/build/logs?page_size=8`，这 9 次推送**一条流水线都没触发**；
同一时间段一个普通分支推送（`qa-e2e/ci-rescue-1833`）立刻起了一条。
CNB 的流水线事件只挂在 `refs/heads/*` 与 tag 上。

取回：`git fetch origin refs/rescue/<名字>` 然后 `git log FETCH_HEAD`。

**How to apply**：半成品、编译不过、离队 agent 的残留 —— 一律用 rescue ref 存，
既保命又不烧额度。只有真正要评审/合并的东西才推 `refs/heads/*`。
**Why**：项目每月 CI 额度只有 160 核时，一条流水线约 0.14 核时；
一次性推 10 个保命分支就是 1.4 核时，且这些流水线的结果没人会看。
