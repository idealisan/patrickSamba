---
name: 别人正在写文件时如何保命提交：独立索引 + commit-tree + refs/rescue/*
description: 队友工作树里有未提交（尤其未跟踪）文件而环境随时回收时，用临时 GIT_INDEX_FILE 造提交对象并推到 refs/rescue/*，全程不碰对方的 index/HEAD/工作树，且不触发任何 CI 流水线；配套的检测命令是 git ls-remote 而不是 --branches --not --remotes
type: project
---

**场景**：队友的工作树里有大量未提交改动（尤其**未跟踪**文件 —— 它们连 `git stash`
都救不回来），环境随时可能被回收，但**他此刻正在写这些文件**。

**不要用 `git add -A && git commit`。** 那会改写他的 index 与 HEAD，正是本项目
2026-08-09 已发生三次的事故形态（在途文件被另一个实例卷走）。

**正确手法（team-lead 2026-08-09 18:43 实操）**：用一个**临时索引**造出提交对象，
再把裸 SHA 推到 `refs/rescue/*`，全程不触碰对方的 `.git/index`、HEAD 与工作树：

```sh
cd /work/<对方的树>
export GIT_INDEX_FILE=/tmp/<我的角色>-rescue-idx    # 关键：不是 .git/index
git read-tree HEAD && git add -A                    # 只影响临时索引
TREE=$(git write-tree); unset GIT_INDEX_FILE
C=$(git commit-tree "$TREE" -p HEAD -m "wip(rescue): <说明>")
git push origin "$C:refs/rescue/<角色>-inflight-<时间>"
```

实测效果：提交对象 `4e0978e…` 已在远端，而对方工作树复查 **dirty 仍是 16**，
他的 `git status` 毫无变化，可以继续写。

**取回**：`git fetch origin refs/rescue/<名字>`。

## 三个配套事实

1. **`refs/rescue/*` 不触发 CI。** CNB 的事件只挂 `refs/heads/*` 与 tag，
   实测 9 次 rescue 推送在 `/-/build/logs` 里**零流水线**。
   相比之下推 10 个分支 = 10 条流水线 ≈ 1.4 核时（额度每月 160 核时），
   而这些往往是离队 agent 的残留，没人会去看那些流水线。
2. **`git log --branches --not --remotes` 看不见 rescue ref。** `--remotes` 只展开
   `refs/remotes/*`（远程跟踪分支），服务端的 `refs/rescue/*` 本地没有对应跟踪引用。
   PM 因此把 10 笔已推的提交**误报成「未推送」**。查 rescue ref 必须直接问服务端：

   ```sh
   git ls-remote origin 'refs/rescue/*'
   ```

3. **`$(git commit-tree …)` 的输出带一个尾部 CR**。直接拼 `$C:refs/rescue/x` 会得到
   `<sha>efs/rescue/x`（`:r` 被 CR 吃掉），报「源引用规格没有匹配」。
   要么 `tr -d '\r'`，要么把 sha 抄下来单独推。

**Why**：环境会硬回收，而「工作只在磁盘上」是本项目反复发生的真实损失；
但抢救动作本身不能制造新的事故。这个手法把两者解耦：**保命是我的事，
不打断你的事**。

**How to apply**：
- 只要判断出「对方正在写 + 有未跟踪文件 + 环境有回收风险」，直接用它，不必先征得同意
  （它不改变对方看到的任何东西），事后发消息告知 rescue ref 名字即可。
- 自己的树不需要这套 —— 自己的树直接 `commit` + `push` 就行。
- **commit 挡住的是「工作树被清」，挡不住「容器整个没了」**：造完提交对象**必须推**，
  留在本地对象库里等于没救。
