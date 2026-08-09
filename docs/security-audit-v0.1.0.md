# v0.1.0 凭据泄漏审计报告

**审计范围**：`stupidSamba` 仓库全部 git 历史
**扫描截止**：`origin/main` = `30d55e1`，2026-08-09T04:44Z
**执行者**：`audit` agent
**脱敏规则**：本报告**不含任何凭据明文**。所有值一律表示为
`前4字符…后2字符 len=<长度> h=<sha256 前 8 位>`；长度 ≤ 8 的值只给
`<len=N h=XXXX>`。`h=` 可用于判定「两处是不是同一个值」，无法反推原值。

---

## 0. 一句话结论

**除已知的两个 shell snapshot 外，没有第二处凭据泄漏。**
但该 token 在同一文件内**同时绑定了两个变量名**（`CNB_TOKEN` 与
`TWINE_PASSWORD`），轮换时必须两侧一起换。

---

## 1. 扫描覆盖面

| 项目 | 数量 |
|---|---|
| commit（所有分支 + tag + 6 个 PR 的 merge/status ref） | 327 |
| 悬空（dangling）commit | 11 |
| blob | 6055 |
| tree | 2954 |
| tag 对象 | 2 |
| **对象总计** | **9364** |

补充覆盖：

- 7 个归档/二进制 blob（`.zip` / `.tar.gz` / `.pdf` / `.png`）**解压后**扫描内容。
- `history/` 会话存档：93 个 blob（含所有历史版本），累计 **60.1 MB**。
- 远端 `refs/pull/N/{merge,status}` 显式 fetch 后纳入（默认 `git fetch` 不带这些）。

### 方法

1. **模式扫描**：15 类正则（token / 云厂商密钥 / 私钥 / JWT / DB 连接串 /
   `Authorization` 头 / `key=value` 赋值等），流式遍历全部 blob，
   逐条脱敏后输出摘要。
2. **字面量 needle 扫描**（关键）：把**真实凭据值**从已知泄漏 blob 中取出，
   对全部 blob 做字面量比对。这一步能回答模式扫描回答不了的问题——
   「同一个值有没有换个变量名、换个文件再出现一次」。
3. **占位词哈希反查**：对疑似口令做 sha256 前缀比对字典，
   **哈希对上才判定为占位词**，不靠肉眼判断。

---

## 2. 确认的真凭据（1 个值，2 个位置）

| blob | 路径 | 引入 commit | 当前 HEAD |
|---|---|---|---|
| `3c2d9ab` | `docs/.codebuddy/shell-snapshots/snapshot-sh-1786233444982-l64ys8.sh` | `2e327a6` | **已移除** |
| `5516917` | `docs/.codebuddy/shell-snapshots/snapshot-sh-1786235444201-puggtf.sh` | `2e327a6` | **已移除** |

`f9d4e1f` 通过 `git rm -r --cached` 停止跟踪，并在 `.gitignore` 中加入
`.codebuddy/`（未加 `/` 前缀，因此**任何层级**都被忽略，已核实）。
按项目所有者要求，**历史刻意保留，未做任何重写**。

### 2.1 同一 token 绑定了两个变量名（本次新发现）

```
CNB_TOKEN      = <h=1578f30c len=27>
TWINE_PASSWORD = <h=1578f30c len=27>   ← 同一个值
```

`TWINE_PASSWORD` 是 PyPI / 制品库上传口令的标准环境变量名。
**含义：这枚 token 不只是 git/API 凭据，同时是制品库发布口令。
轮换时只处理 git 侧会遗漏制品库侧。**

### 2.2 同快照中的非凭据标识符

`CNB_TOKEN_USER_NAME`(len=3)、`CNB_REPO_ID` / `CNB_GROUP_ID` /
`CNB_BUILD_USER_ID`（均为 19 位数字）、`CNB_BRANCH_SHA`、`VSCODE_NONCE`。

敏感度低（不是凭据），但确实对外暴露了内部 org / repo 的数字 ID。

---

## 3. 判定「没有第二处」的依据

这是本报告最需要可证伪的一条结论，方法如下：

1. 从两个已知泄漏 blob 中提取真实凭据值（仅在进程内存中，从不输出）。
2. 以该值为 needle，对**全部 6055 个 blob** 做字面量比对。
3. 命中集合 = **恰好那 2 个 blob**，无第三个。

同时确认：全历史中只存在过这 **2 个** `shell-snapshots/` blob，
不存在「曾提交过、后来被删」的第三个快照。

**为什么这一步不可省**：模式扫描只能发现「长得像凭据的字符串」，
无法发现「同一个凭据换了个不像凭据的变量名」。§2.1 的 `TWINE_PASSWORD`
正是靠这一步才发现的——它的变量名里没有 `TOKEN` 字样。

---

## 4. 按类型分类的结论

| 类型 | 确认真凭据 | 疑似 | 确认测试向量 / 误报 |
|---|---|---|---|
| CNB token | **1** | 0 | — |
| AWS / 云厂商密钥 | 0 | 0 | 全历史 **0 命中** |
| GitHub `ghp_*` / GitLab `glpat-*` | 0 | 0 | 全历史 **0 命中** |
| Slack / Google API Key / JWT | 0 | 0 | 全历史 **0 命中** |
| SSH / PGP / PEM 私钥 | 0 | 0 | 1 处标记，见 §4.1 |
| 数据库连接串口令 | 0 | 0 | 口令位均为占位词 |
| NT hash | 0 | 0 | 2 处，见 §4.2 |
| 通用 `key=value` 赋值 | 0 | 0 | 148 条，均为代码标识符 |

### 4.1 唯一的私钥标记 = 文档占位符

`docs/.codebuddy/plugins/marketplaces/.../k8s-manifest-generator/SKILL.md`
含 `-----BEGIN PRIVATE KEY-----`，但其后紧接字面量 `...` 再接 `-----END`，
**无任何密钥体**。判定：文档占位符，非泄漏。

### 4.2 NT hash 两处均非泄漏

- `configs/example.yaml`、`internal/config/testdata/good.yaml`：
  `31d6cfe0d16ae931b73c59d7e0c089c0` —— 这是**空口令**的 NT hash，
  公开常量（MD4 of empty UTF-16LE string），不是任何人的真实口令。
- `internal/config/testdata/many_errors.yaml`：4 字符、单一字符重复，
  是**故意构造的非法 hex**，用于覆盖 `isHex32` 的错误分支。

### 4.3 148 条 `key=value` 命中为何全是误报

去重后为 148 个不同值，逐个核查后归类：

- 绝大多数是**代码标识符**：`process.env.X_SECRET`、`get_api_key(`、
  `hashPassword(`、`os.environ[` 等，正则把函数名/属性名当成了「值」。
- 全部落在 `docs/.codebuddy/plugins/marketplaces/` 下的**上游插件模板**中，
  非本项目代码。
- 两个 `.env.example` / `.env.template` 的值全部形如 `your_xxx_here`。

---

## 5. `history/`（会话存档）—— 干净

`history/` 是 `scripts/save-history.sh` 落地的 CodeBuddy 会话 jsonl 快照。
它是最容易被忽略的泄漏面：会话里可能出现工具调用回显的明文 token。

扫描做法不止查明文，还查了 **8 种编码形态**：

`plain` / `base64` / URL-encode / `sha256hex` / `md5hex` /
前 12 字符 / 后 12 字符 / JSON 转义

| 项 | 结果 |
|---|---|
| 覆盖 blob（含所有历史版本） | 93 |
| 累计扫描 | 60.1 MB |
| **8 种形态命中总数** | **0** |
| 反向对照（把真值塞进探针缓冲区） | **命中** → 检测器有效 |

反向对照这一步不可省：否则「0 命中」既可能是真干净，
也可能是检测器根本没工作。

**遗留风险**：`save-history.sh` **没有任何脱敏逻辑**。本次未泄漏属于运气，
只要某次会话里执行过 `env` 之类的命令，下次存档就会把凭据带进库。
建议在存档前增加一道脱敏。（`scripts/` 归 qa，本报告只提出，不修改。）

---

## 6. 示例口令：真示例 vs 真实用过

我方源码目录（`configs/` `docs/` `test/` `scripts/` `internal/` `cmd/`
`README` `AGENTS` 等，排除 `docs/.codebuddy/`）全历史共 **17 条**
口令形态字面量。

**结论：全部是示例/测试值，没有任何一条是真实用过的凭据。**

判定依据分两层：

1. **哈希反查占位词字典**（哈希对上才算数）——已坐实为字面占位词的有：
   `pass`、`password`、`pwd`、`secret`、`secret123`、`testpass123`、
   `123456`、`abc123`、`changeme`、`YOUR_TOKEN`、`api_key`、`string`、
   `Password`、`smoke-secret`、`tok`。

2. **交叉核对**：把泄漏快照中**每一个** opaque 环境变量值收集成集合，
   将剩余 9 条逐一比对——**无一落在该集合内**，即它们从未作为真实凭据出现过。

剩余 9 条的形状（进一步佐证其为平凡测试值）：

| 位置 | 长度 | 字符集 |
|---|---|---|
| `README.md` `pass` / `password` | 4 / 3 | 符号 / 纯小写 |
| `configs/example.yaml` `nt_hash` | 19 | 字面说明文字 `MD4(UTF16LE(口令))` |
| `internal/auth/ntlmssp_test.go` | 5 | 纯小写 |
| `internal/auth/static_test.go` | 14 | 小写 + 符号 |
| `test/integration/*` | 8 | 小写 + 大写 |
| `internal/vfs/local_test.go` | 24 | 小写 + 大写 + 符号 |
| `docs/test-infra.md` `pwd` | 5 | 纯小写 |

---

## 7. 第二泄漏出口复核：发布脚本与 CI

### 7.1 `scripts/publish-release.sh` —— 存在一个出口（argv）

脚本头部 `:18` 自查注释称「不会把它打印出来、写进文件或塞进 URL」。
这三条经复核**均属实**，但**漏了第四个维度：进程命令行**。

`publish-release.sh:113` 与 `:120`：

```sh
curl -sS -X "$_m" -H "Authorization: Bearer $CNB_TOKEN" ...
```

shell 展开后 token 成为 curl 进程的 argv，因而出现在 `/proc/<pid>/cmdline`，
**同机任意进程可读**。

实测（使用假 token，未接触真实凭据）：

| 写法 | `/proc/*/cmdline` 命中 |
|---|---|
| `curl -H "Authorization: Bearer $TOK"` | **命中** |
| `curl -K <配置文件>`（头写在文件里） | **0 命中** |

建议改为：

```sh
umask 077
printf 'header = "Authorization: Bearer %s"\n' "$CNB_TOKEN" > "$TMP/curlrc"
curl -sS -K "$TMP/curlrc" ...
```

`$TMP` 已有 `trap cleanup EXIT INT TERM`，无需额外收尾。

**危害等级：中。** 利用前提是攻击者已能在同机执行代码——真到那一步，
直接读环境变量更省事。因此**不改变 token 轮换的紧迫性**。
但本项目十个 agent 共用一台容器、互相可见对方进程，故仍建议修复。

### 7.2 `.cnb.yml` —— 干净，紧迫性不升级

当前为 **15 个 stage**（非 11 个；`6813805` / `eb69fb5` 新增了
「测试代码编译校验」与「race 单独开 CGO」）。逐个核查 9 类会把环境
写进构建日志的写法：

`set -x` / xtrace、裸 `env`、`printenv`、`declare -x` / `export -p`、
`echo $XXX_TOKEN`、`curl -v`、token 拼进 URL、
`git push https://tok@...`、`cat .env`

| 目标 | 非注释命中 |
|---|---|
| `.cnb.yml` | **0** |
| `scripts/build-release.sh` | **0** |
| `scripts/check-constraints.sh` | **0** |

且 `.cnb.yml` 中**未出现任何凭据类变量引用**——token 由平台直接注入给
`git:release` 内置任务与 `cnbcool/attachments` 插件，YAML 不经手。

**检测器自身做了双向对照**（这一步抓到了本次审计自己的一个 bug）：

- 反向对照：注入 9 类危险写法 → **9/9 全部检出**。
  首版 `bare env` 检测器曾漏报（正则缺 `MULTILINE`，匹配不到
  `script: env`），改为词元法后修正。
- 阴性对照：`env FOO=1 ./x`、`envsubst`、`$ENV_HOME` **均不误报**。

若无这两步，「0 命中」这一结论不可信。

---

## 8. 明确未覆盖的范围（可证伪边界）

「我扫了全部」不是可接受的结论。以下是本次**确实没有覆盖**的部分：

1. **CNB 服务端侧数据**：构建日志正文、PR 评论、Issue、Release 描述、
   webhook 配置、CI 变量面板。均在 git 之外，本地无法访问。
   §7.2 只能证明「YAML 与脚本不会打印环境」，
   **无法证明**第三方镜像（`git:release`、`cnbcool/attachments`）
   内部不打印——需人工查看控制台日志正文。
2. **2 个远端对象取不到**：`git ls-remote` 列出但无法 fetch
   （PR 临时 merge ref，服务端已回收），其内容未经验证。
3. **已被 gc 回收的对象**：按内存约束**未执行 `git gc`**
   （仓库含 149 MB 历史快照，重打包会耗尽内存）。
   因此只覆盖当前对象库中仍存在的对象；
   「曾提交、后被 gc 清除」的对象**任何本地手段都无法追溯**。
4. **加密 / 加壳内容**：7 个归档已解压扫描，
   但若有人将凭据加密后提交，模式扫描无效。
5. **时间边界**：截止 `origin/main` = `30d55e1`，2026-08-09T04:44Z。
   此后的新提交不在覆盖范围内。

---

## 9. 建议动作

| # | 动作 | 责任方 | 优先级 |
|---|---|---|---|
| 1 | 轮换泄漏的 token，**git 侧与制品库侧（`TWINE_PASSWORD`）一并轮换** | 项目所有者 | 高 |
| 2 | 人工核查 CNB 控制台历史构建日志正文（本地不可达，见 §8.1） | 项目所有者 | 中 |
| 3 | `publish-release.sh` 改用 `curl -K`，消除 argv 泄漏（§7.1） | qa | 中 |
| 4 | `save-history.sh` 增加存档前脱敏（§5） | qa | 中 |

**不建议**重写历史：项目所有者已明确要求保留历史痕迹，
且凭据一经泄漏即应视为已泄漏，轮换才是根治手段，
`filter-branch` 只会破坏协作历史而不消除风险。
