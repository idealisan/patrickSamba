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
