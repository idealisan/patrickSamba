# 开发工作流 SOP —— 独立 worktree + 独立分支 + PR

> 适用范围：**v0.2.0 起的所有开发**（v0.1.0 的收尾沿用旧的共用 `main` 模式）。
> 本文是 AGENTS.md §7.3 的操作手册：**§7.3 讲为什么，本文讲怎么做**。
> 冲突时以 AGENTS.md 为准。
>
> 读者假设：你是刚接手本仓库的一个 agent（人或 AI），手上只有这份文档和一个终端。
> 照着从上往下敲即可。

---

## 0. 五分钟速查（老手直接抄这段）

```sh
# ① 开工：给自己开独立工作目录（一次性）
git -C /workspace worktree add /work/<角色> -b <角色>/<主题> origin/main
cd /work/<角色>
. scripts/devenv.sh

# ② 干活 → 编译过就提交推送（每 10~15 分钟一次，别攒）
gobuild                       # 或 gocheck（build+vet+gofmt+test 四连）
git add <你自己的文件>
git commit -m "<模块>: <一句话>"
git push -u origin <角色>/<主题>

# ③ 阶段性成果 → 提 PR
curl -sS -X POST -H "Authorization: Bearer $CNB_TOKEN" -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -d '{"title":"<模块>: <一句话>","head":"<角色>/<主题>","base":"main","body":"<说明>"}' \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/pulls"

# ④ DM team-lead 报 PR 号，等评审
```

**四条红线**（违反会造成不可逆损失）：

| 禁止 | 原因 |
|---|---|
| 在 `/workspace` 里改文件 | 那是 team-lead 的工作目录 |
| `git push` 直推 `main` | 一律走 PR |
| `git push --force`（对任何分支） | 自己分支只能用 `--force-with-lease` |
| `git reset --hard` 丢别人的提交 | 解决冲突，不要绕过冲突 |

---

## 1. 为什么必须一人一个工作目录

**光靠分支是不够的。分支隔离的是提交历史，不隔离工作树上的文件。**

真实事故（AGENTS.md §10.3 第 6 条）：某 agent 分两步做一次重命名——先改声明、
再改所有引用。这两步之间有大约一分钟，工作树处于 `undefined: xxx` 的中间态。
另一个 agent 恰好在那一分钟里跑 `go build ./...`，看到一个自己代码里根本不存在的
编译错误，排查了很久。**问题不在任何一方的代码里，在于两个人共用同一份文件。**

`git worktree` 给每个人一份**物理独立**的检出：

- 文件系统上完全隔离，别人的未提交中间态再也砸不到你。
- 共享同一个 `.git` 对象库 —— `git fetch` / 分支 / 历史全部互通，不额外占空间、不重复下载。
- **一个分支只能被一个 worktree 检出**（git 硬性限制），这天然强制了"一人一分支"。

> 顺带说明：worktree 隔离的是**文件**，不隔离**端口**。起服务调试仍然要用
> 你的专属调试端口（见 §9）。

---

## 2. 建立你的工作目录

```sh
git -C /workspace worktree add /work/<角色> -b <角色>/<主题> origin/main
cd /work/<角色>
. scripts/devenv.sh
```

例：

```sh
git -C /workspace worktree add /work/auth -b auth/smb30-encryption origin/main
```

几点说明：

- `-b <分支名>` 创建并检出新分支；**基点写 `origin/main`**，不要写 `main`
  （本地 `main` 可能落后于远端，也可能被 team-lead 检出着）。
- 路径统一放 `/work/<角色>`，好认。
- `.git` 在 worktree 里是一个**文件**而不是目录（内容是指向主仓库的指针），这是正常的。
- 建完先 `. scripts/devenv.sh`，否则 `go` 不在 PATH 里（见 §9）。

查看当前所有 worktree：

```sh
git worktree list
```

---

## 3. 分支命名

```
<角色>/<简短主题>
```

例：`auth/smb30-encryption`、`vfs/named-streams`、`docs/dev-workflow`、`qa/ci-hardening`。

- 角色前缀就是你的 agent 代号，一眼能看出这个分支归谁。
- 主题用小写连字符，短。不要写成 `auth/fix`（信息量为零）也不要写成一整句话。
- 一个分支做一件事。做完合了再开新的，不要把三件不相关的事塞进一个长命分支。

---

## 4. 提交与推送节奏（**最重要的一节**）

> **本开发容器会不定期崩溃并重启，清空 git 仓库以外的一切。已经真实发生过多次。
> 凡是没推到远端的东西都可能永久消失。**

规矩：

1. **只要能编译（`gobuild` 通过），就可以提交推送。** 功能没做完没关系，
   用 `TODO` 标注，或让未实现路径返回 `STATUS_NOT_SUPPORTED`。
2. **提交之后立刻推送。** 不要"做完再一起推"。
3. 大致每完成一个文件、或每 10~15 分钟有产出，就推一次。
4. **发现自己改了很多还没提交 —— 立刻停下来提交。**
5. 宁可 20 个小 commit，也不要憋一个大 commit 然后丢掉。

提交信息格式（AGENTS.md §7.4）：

```
<模块>: <一句话说明>

模块取值：wire / auth / vfs / server / mdns / config / cmd / test / docs / ci
```

例：`wire: 实现 SMB2 Packet Header 编解码与 golden test`

一次完整的推送：

```sh
gobuild && git add <你自己的文件> && \
  git commit -m "vfs: 命名流的 :name:$DATA 后缀解析" && \
  git push -u origin vfs/named-streams
```

`-u` 只在第一次推这个分支时需要，之后 `git push` 即可。

### 4.1 `git add` 用路径，不要用 `-A`

即使有了 worktree 隔离，也请**显式列出你要提交的路径**。`git add -A` 会把你临时
生成的 `/tmp` 产物、调试二进制、日志一起裹进提交。构建产物尤其容易漏（`go build`
不带 `-o` 会把二进制吐在仓库根）。

### 4.2 不要在新流程里用 `scripts/save.sh`

`scripts/save.sh` 是**共用 `main` 时代**的脚本，它最后一步写死了
`git push origin main`。在 worktree 里跑它，推的是**本地 `main` 分支**（也就是别人的
东西），根本不是你的分支。它的两道校验（路径归约 vet、gofmt 门禁）依然有价值，
但提交推送这一段与 PR 流程不兼容。

**新流程请手工 `git add` / `commit` / `push`，校验用 `gocheck`。**

---

## 5. 提 PR

### 5.1 先自检

PR 要能独立编译通过、要小、一个 PR 只做一件事。推之前跑：

```sh
gocheck      # go build + go vet + gofmt -l + go test，任一不过就别提
```

改了平台相关代码再跑：

```sh
gocross      # linux/amd64, linux/arm64, darwin/arm64, windows/amd64
```

### 5.2 创建 PR

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $CNB_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -d '{"title":"auth: 补齐 SMB 3.0 AES-128-CCM 加密","head":"auth/smb30-encryption","base":"main","body":"修复 encryption_required 在 3.0/3.0.2 下被静默忽略的问题。\n\n- 实测：smbclient --client-protection=encrypt 通过\n- 反向测试：2.1 客户端被拒绝"}' \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/pulls"
```

- `$CNB_TOKEN` 与 `$CNB_REPO_SLUG`（值为 `finalappstore/stupidSamba`）在容器环境变量里已有，
  **不要把 token 打印出来、也不要写进任何文件或日志**。
- `head` 是你的分支名，`base` 固定 `main`。
- 返回体里的 `number` 就是 PR 号，记下来。

### 5.3 合并 PR（评审通过后）

```sh
curl -sS -X PUT \
  -H "Authorization: Bearer $CNB_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -d '{"merge_style":"merge","commit_title":"Merge PR #12: auth: 补齐 SMB 3.0 AES-128-CCM 加密"}' \
  "https://api.cnb.cool/$CNB_REPO_SLUG/-/pulls/12/merge"
```

### 5.4 CNB API 的四个坑（**已经有人替你踩过了，别再试一遍**）

| 症状 | 原因 | 正确做法 |
|---|---|---|
| `404` | 合并用了 `POST` | 合并是 **`PUT`**，只有创建 PR 是 `POST` |
| `400` | 参数名写成 `merge_method` | CNB 用的是 **`merge_style`**（不是 GitHub 的那套） |
| `400` | 漏了 `commit_title` | **`commit_title` 必填**，不是可选项 |
| `406` `{"errcode":406,"errmsg":"either of 'application/json' or 'application/vnd.cnb.api+json' content type supported"}` | **GET 和 PUT 都要求 `Accept: application/json`**，缺了就报 406。报错文案说的是 content type，极易误导你去查 `Content-Type` 头——但 `Content-Type: application/json` 明明已经带了，真正缺的是 `Accept` | 请求务必带 `-H "Accept: application/json"`（上面的 curl 已经带了，照抄别漏） |

### 5.5 评审与合并规则

- PR 由 team-lead 或另一个 agent 评审后合并，**作者不自行合并**。
- 合并前 CI 必须绿。
- 建完 PR **立刻 DM team-lead 报 PR 号**，不然没人知道它在等评审。

---

## 6. 跟进主干与冲突处理

分支落后 `main` 时：

```sh
git pull --rebase origin main
```

有冲突就**解决冲突**（编辑冲突文件 → `git add` → `git rebase --continue`）。
解决不了就 `git rebase --abort` 回到 rebase 前的状态，然后 DM team-lead。

rebase 之后本地历史被改写，推送需要：

```sh
git push --force-with-lease
```

**是 `--force-with-lease`，不是 `--force`。**
区别：`--force-with-lease` 会先检查远端分支是否还停在你上次见到的位置，
如果别人在这期间推了东西上去，它会拒绝而不是覆盖。`--force` 无脑覆盖，会静默
删掉别人的提交。

绝对禁止：

- 对 `main` 做任何 force push。
- 用 `git reset --hard` 把别人的提交丢掉来"解决"冲突。
- 用 `git checkout --ours/--theirs` 一把梭而不看内容。

---

## 7. 文件所有权与共享文件

### 7.1 所有权是**按文件**划的，不是按目录

v0.2.0 的分工细到文件级（谁拥有哪些文件见 team-lead 下发的任务描述）。
worktree 保证你不会**物理上**破坏别人，但两个人在各自分支里改同一个文件，
合并时照样冲突。

**需要改别人的文件时，先 DM team-lead 申请，不要自己动手。**
需要别人改接口时，也是发消息沟通，不是自己去改。

### 7.2 共享文件是「只追加」

下面这些文件多人都要往里加东西：

- `internal/config/*.go`
- `internal/smb/wire/const.go`
- `internal/smb/status/status.go`
- `internal/smb/command/dispatch.go`

规矩：

1. **只加你那一条**，不要顺手重构、不要重排序、不要"清理一下"别人的代码。
   重排 = 整个文件都算你改过 = 所有人跟你冲突。
2. 加完**立刻单独提交一个小 PR**（只含这一个文件的改动），别和你的功能改动混在一起。
3. 合入后**通知 team-lead**，好让其他人 rebase。

共享的接口定义文件（如 `internal/vfs/fs.go` 的接口部分）一旦定稿，
修改前必须先通知所有相关 agent。

---

## 8. 收尾清理

任务结束、分支已合入后：

```sh
git -C /workspace worktree remove /work/<角色>
```

如果目录里还有未提交的改动，`remove` 会拒绝 —— **这是保护，不是障碍**。
先确认那些改动真的不要了，再加 `--force`。

**不要直接 `rm -rf /work/<角色>`。** 那会在 `.git/worktrees/` 下留一条悬空记录，
之后想用同名 worktree 会报错。已经删了就用：

```sh
git worktree prune
```

分支合入后删远端分支（可选，保持分支列表干净）：

```sh
git push origin --delete <角色>/<主题>
```

---

## 9. 环境须知（每个新 shell 都要过一遍）

### 9.1 `. scripts/devenv.sh`

**每开一个新 shell 都要 source 一次。** 它做三件事：

- 把 `/usr/local/go/bin` 加进 PATH（Go 默认不在 PATH 里，容器重启后也一样）
- 设 `CGO_ENABLED=0`（C1 硬性约束）与 `GOFLAGS=-p=1`（编译串行，削峰）
- 定义 `gobuild` / `gocheck` / `gocross` / `golock`，都带**跨 agent 的全局 flock**

### 9.2 内存削峰

容器只有 2 核 4 GiB，**开不了交换分区**（缺 `CAP_SYS_ADMIN`）。
已实测确认崩溃形态是 **SIGABRT + core dump**，即 CodeBuddy 的 V8 堆撞上
**2096 MB** 上限自行 abort ——**不是**容器 OOM-kill。

两条战线：

1. **别把大输出灌进上下文**（这条最要命）：
   - 读文件用 Read 工具（有分页），不要 `cat`
   - 搜索用 Grep 工具带 `head_limit`，不要 `grep -r` 全量打印
   - `git log -p` 一律加 `-n` / `--stat` 限制
   - 长命令用后台运行 + `tail`/filter 取结果，不要把几万行日志整个捞回来
   - **判断标准：任何一次工具输出超过几百行，就说明你少加了一个界。**
2. **别让多个 agent 的重活峰值叠加**：重活一律走 `gobuild` / `gocheck` / `gocross`
   或 `golock <命令>`，它们共用 `/tmp/stupidsamba-go.lock` 全局串行。

### 9.3 专属调试端口

每人一个，避免抢 445 / 4445：

| agent | 端口 |
|---|---|
| r-infra | 4451 |
| tm-handle | 4452 |
| tm-vfs | 4453 |
| tm-lease | 4454 |
| win-meta | 4455 |
| win-vfs | 4456 |
| docs | 4457 |
| qa | 4458 |
| audit | 4459 |

### 9.4 进程管理的坑

- **`pkill -f <路径>` 会自杀。** 该模式会匹配到执行它的 shell 自己的命令行，
  把父 shell 一起杀掉，表现为"命令无输出 / 被 SIGTERM / 服务起不来"，极难排查。
  **一律用 `pkill -x <进程名>`。**
- **`pkill -x` 也有坑**：进程名超过 15 字符时内核 `comm` 字段被截断，pkill 会
  静默匹配不到 —— 旧进程还活着占着端口，新进程绑不上，表现和"服务起不来"一模一样。
  **调试二进制名必须 ≤ 15 字符**（`stupidsamba4457` 正好 15，再长就废了）。
- 起后台服务：`setsid nohup ./ss > log 2>&1 < /dev/null & disown`
- **`setsid nohup ... &` 之后 `$!` 不是监听进程**（那是 setsid 包装进程的 PID）。
  找真实 PID 用 **`fuser <port>/tcp`**（本容器 `ss -ltnp` 拿不到 pid 列）。

### 9.5 测试客户端的坑

- **`smbclient` 的 `-c` 多条命令必须用分号 `;` 分隔。** 写成多行会产生
  `NT_STATUS_NO_SUCH_FILE listing \get` 这类**假故障**，看起来像服务端 bug。
- **`mount.cifs` 在本容器永远跑不通**（缺 `CAP_SYS_ADMIN`，报
  `Unable to apply new capability set`）。这是环境限制，不是服务端 bug；
  `scripts/acceptance.sh` 已把它做成 skip(rc=77)。**不要因为它 fail 就判定验收不通过，
  也不要把它写进"已验证"清单。**
- `impacket` 用 `apt-get install python3-impacket` 装，**不要用 pip**（会和已有的
  cryptography 版本冲突）。

### 9.6 被别人挡住时的应急手段

如果因为某种原因你必须在共用工作树里干活，又撞上了别人的中间态：

```sh
git worktree add /tmp/clean-$$ origin/main && cd /tmp/clean-$$
```

拿一个干净且可编译的基线继续，用完 `git worktree remove /tmp/clean-$$`。
**这只是应急**；常态应当按 §2 一开始就待在自己的 worktree 里。

---

## 10. 常见问题

**Q：`worktree add` 报 `fatal: '<分支>' is already checked out at ...`**
一个分支只能被一个 worktree 检出。换个分支名，或先 `git worktree list` 看谁占着。

**Q：`go: command not found`**
没 source `scripts/devenv.sh`。每个新 shell 都要。

**Q：`gocheck` 卡住不动**
它在等全局锁（别的 agent 正在跑重活），最长等 20 分钟。这是设计如此，别绕过。

**Q：我的 worktree 里 `git branch` 看到别的分支带 `+` 号**
表示那个分支被别的 worktree 检出着，你不能切过去。正常。

**Q：`git status` 干净，但 `go build` 报别人文件的错**
在你自己的 worktree 里不该发生。如果发生了，先 `git log --oneline -3` 确认基点，
再 `git pull --rebase origin main` 跟进主干 —— 很可能是主干上确实合进了坏代码，
立刻 DM team-lead。

**Q：能不能直接推 `main` 就一次，很小的改动**
不能。上一版就是因为"很小的改动"直接进 `main`，导致 gofmt 门禁在 HEAD 就是红的、
`test/` 因 build tag 从未被真正校验，直到发布前才发现。**PR 就是为了拦住这个。**
