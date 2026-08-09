# v0.2.0 发版前文档诚实性审计

**审计时间**：2026-08-09 18:06 ~ 18:35 CST
**审计基线**：`origin/main` = `96afd87`（审计开始时）
**审计人**：`docs-honesty` agent
**范围**：`CHANGELOG.md` v0.2.0 段、`README.md`、`AGENTS.md` §1.2 / §5 P7、`configs/example.yaml` 注释

本文件的作用是**让每一条对外声明都能被一条命令当场复验**。
写「我检查过了」没有价值（`memory/feedback_falsifiable_assertions.md`）——
所以下面每一条都附**可复算判据**，读者不必相信我。

---

## 0. 本轮最重要的方法论发现：「结论对、理由过期」

**这是本次审计逮到的主要缺陷形态，值得作为一类审计对象单列。**

CHANGELOG 关于 `filesystem_mode` 原本说了两件事：

| | 原文 | 今天是否成立 |
|---|---|---|
| **结论** | 「三档目前等价，选哪个跑起来都一样」 | ✅ **仍然成立** |
| **理由** | 「因为 `native/` 与 `builtin/` 两个适配器**都还不存在**」 | ❌ **已经失效**（两个适配器都在 main 了） |

**一个正确的结论配一个过期的理由，比一个错误的结论更难被发现**，因为对结论的抽查会通过。
危害在于**读者会依据那个理由去做推断**：看到「等适配器写完就好了」，就会以为
适配器一落地问题自动解决——而实际上适配器早写完了，卡住的是**接线**。
按错误理由排优先级，会把人力投到一件已经做完的事上。

**因此审计时不能只问「这句话对不对」，还要问「它给的理由今天还成立吗」。**
落到操作上：**对每条带因果的断言，结论与理由要分别取证**，不要因为结论抽查通过就放行整句。

本轮按这个方法扫出的同型条目：§1 的 A3/A4（两处同源，必须同时改，否则文档自相矛盾）、
§3 的 C1（`mount.cifs` 归因错误，结论「跑不通」对、理由「缺 CAP_SYS_ADMIN」已被实验推翻）。

---

## 1. 核心结论：oscap 抽象层「已建成，但零运行期消费方」

这是 v0.2.0 的头号特性，也是最容易被写夸大的地方，单独列一节。

### 已建成部分（属实，可复算）

| 断言 | 判据 | 结果 |
|---|---|---|
| 六项能力 port 已定义 | `ls internal/oscap/*.go \| wc -l` | 20 个文件；`go test -v ./internal/oscap/` = **31 PASS / 0 SKIP / 0 FAIL** |
| native 适配器六项齐全 | `go test -v ./internal/oscap/native/` | **37 PASS / 0 SKIP / 0 FAIL** |
| builtin 适配器六项齐全 | `go test -v ./internal/oscap/builtin/` | **49 PASS / 0 SKIP / 0 FAIL** |
| native 包不靠 `t.Skip` 兜底 | `grep -rn 't.Skip' internal/oscap/native/*_test.go` | 唯一命中在 `native_posix_test.go` 的**注释**里；实跑 0 SKIP |
| portable 模式 CI 门禁真跑 | `sh test/ci/portable-mode.sh` | rc=0，「顶层通过 35 条…含子测试的结果行共 50 条」 |
| 该门禁有牙（反向对照） | 同上，脚本内置 4 个变异体 | **4/4 让门禁变红**，且红在 4 个不同判据上 |

**门禁的四个变异体值得单独记住**（它们定义了「什么叫有牙」）：

| 变异体 | 模拟的事故 | 门禁红在哪 |
|---|---|---|
| A | portable 偷偷调用 native factory | 业务断言 `TestPortableIgnoresNativeEvenWhenAvailable` |
| B | 用例被改名 → 门禁空转 | 必需用例清单缺项 |
| C | 子测试全 SKIP、父测试仍 PASS | SKIP 计数 > 0 |
| D | 子测试整体不执行且不留 SKIP 痕迹 | 结果行数低于下界（41 < 50） |

C 和 D 是「**报绿但什么都没跑**」的两种形态。只统计 FAIL 数的门禁对它们**完全无感**——
这正是本仓库反复栽过的坑，`memory/project_silent_success_failures.md` 已记七例。

### ❌ 未建成部分：没有任何运行期消费方

**四条互相独立的判据**（任一条单独成立即可证明，不必全信）：

```sh
# 判据 1：谁 import 了 oscap —— 只有 config，且没人 import 两个适配器
go list -f '{{.ImportPath}}: {{.Imports}}' ./... | grep oscap

# 判据 2（最硬）：发布二进制的依赖闭包里 oscap 包的个数
go list -deps ./cmd/stupidsamba | grep -c oscap      # → 1
```

**判据 2 是本节最强的证据，优于 `grep`**：它是编译器算出来的依赖闭包，
注释、字符串字面量、死代码一律不会混进来，绕不过去。结果 `1` 意味着
**只有 port 包被链进二进制**（还是被 config 为了 `ParseMode` 拉进来的），
`oscap/native` 与 `oscap/builtin` 两个适配器**根本没进最终产物**。

```sh
# 判据 3：包外有没有真实调用点
grep -rn 'oscap\.Open\|oscap\.New\|SelectMatrix\|ProbeNative\|native\.New\|builtin\.New' \
     --include='*.go' . | grep -v '^./internal/oscap/'
# → 唯一命中 internal/config/config.go:25，是一句注释，零个真实调用

# 判据 4：FilesystemMode 字段的消费者
grep -rn 'FilesystemMode' --include='*.go' internal/ cmd/ | grep -v _test.go
# → 只有 defaults.go:48-49（填默认值）与 validate.go:238-245（校验取值）
```

**结论**：`filesystem_mode` 填 `auto` / `native` / `portable`，**运行期行为完全一样**。
真实数据路径仍走 v0.1.0 那套自己的平台代码，与 oscap 并存但互不相干
（例：数据路径的 xattr 是 `internal/vfs/xattr_unix.go`，
`internal/oscap/native/xattr_posix.go` 是另一份、无人调用）。

> **同源陷阱，建议拿去扫别处：「有 CI 门禁跑绿」≠「产品在用它」。**
> `portable-mode.sh` 在 CI 里确实真跑、确实真绿，但它验的是 **oscap 包自己的行为**，
> 不是 `stupidsamba` 进程的行为。门禁绿灯**不构成**「该能力已对用户生效」的证据。

### 逐条修正记录

| 编号 | 位置 | 原文（摘要） | 判定 | 处置 |
|---|---|---|---|---|
| A1 | `AGENTS.md` §1.2 进度表 | native「开发中（分支 `oscap/native`）」 | ❌ 已过期 | 改为「已在 main（PR #138，`d351683`）」 |
| A2 | `AGENTS.md` §1.2 进度表 | builtin「开发中」、portable 门禁「**未完成**」 | ❌ 已过期 | 改为「已在 main（PR #129 / #135）」 |
| A3 | `CHANGELOG.md` 小节标题 | 「接口与开关已就位，**适配器尚未落地**」 | ❌ 已过期 | 改为「两套适配器都已建成，但一处都还没接进数据路径」 |
| A4 | `CHANGELOG.md` ⚠️ 条 | 「两个适配器**都还不存在**」 | ❌ **结论对、理由错** | 整条重写，理由换成「无运行期消费方」，附四条判据 |
| A5 | `CHANGELOG.md` 已知问题 | 同 A4 的错误理由再现一次 | ❌ 同源 | 与 A4 同时改（不同时改会自相矛盾） |
| A6 | `AGENTS.md` §1.2 | 「三个取值实际差异仍然**有限**」 | ❌ 措辞不准 | 改为「**目前为零**」——不是程度问题，是没有消费者 |
| A7 | `AGENTS.md` §1.2 进度表 | （缺失） | ➕ 新增 | 新增「运行期消费方：❌ 无」一行 |
| A8 | `README.md` | 完全没提 `filesystem_mode` | ➕ 新增 | 配置表补一行 + 「已知限制」第 7 条，**明写三档等价** |

**刻意没做的事**：没有写「即将接线」「v0.3.0 生效」之类的预期。
CHANGELOG 写作纪律第 1 条——功能没合入 `main` 之前一个字都不写。
原文里「真正生效在 v0.3.0」这句**已删除**，改为「以真正合入 main 的那一版为准」，
不给版本承诺。

---

## 1b. 同型二例：`native` 档的报错承诺没有兑现（**本轮新增，实测**）

`filesystem_mode` 取值校验失败时程序会打印可选值说明，其中写着
**「native=强制原生、不支持则启动报错」**。这描述的是设计契约；但既然 §1 已证明
**没有任何运行期消费方**，那个报错就**永远不会发生**——
**这是一句程序自己打印给用户看的、当前不成立的承诺**，比文档里的错话更容易被信任。

实测（2026-08-09 18:35，Linux/amd64，本分支基线构建的真二进制，非读代码推断）：

```sh
timeout 3 ./dhprobe -config native.yaml;   echo $?   # → 124（被 timeout 杀掉 = 一直在跑 = 启动成功）
timeout 3 ./dhprobe -config portable.yaml; echo $?   # → 124（同样正常启动）
# 两次日志里只有「明文口令」「未强制签名」两条常规 WARN，零个能力相关输出

# 反向对照（证明探针有鉴别力，不是恒真）：大小写写错
timeout 3 ./dhprobe -config badcase.yaml;  echo $?   # → 1
# stupidsamba: 配置校验失败: filesystem_mode: 非法取值 "Native"，可选值: auto, native, portable
#（auto=逐项探测自动降级；native=强制原生、不支持则启动报错；…）
```

**反向对照是这条判据成立的关键**：它证明该二进制在配置非法时确实会退出 1，
所以前两个探针的「正常启动」是真的没走能力探测，而不是探针测不出失败。

### 接线之后才会显现的事实（提前记，免得日后被当成回归）

| 位置 | 代码 | 原因（代码注释大意） |
|---|---|---|
| `probe_linux.go` `CapDOSAttributes` | 无条件 `return false` | POSIX 没有存放 DOS 属性位的地方；用 `user.DOSATTRIB` xattr 存一份属于 builtin，不算 native |
| `probe_darwin.go` `CapDOSAttributes` | 无条件 `return false` | 同上；FinderInfo 的标志位语义不重合 |
| `probe_darwin.go` `CapSparseFile` | 目前也 `return false` | 注释写明「native/ 补齐 `F_PUNCHHOLE` 之后再改成真实探测」 |

而 `native` 档的契约是「有一项不支持就启动报错、不降级」
（`internal/oscap/matrix.go:76-88`、`provider.go:118/149`）。两者相乘：

> **一旦接线，`filesystem_mode: native` 在 Linux/macOS 上会恒定启动失败**
> （macOS 上同时命中 `dos_attributes` 与 `sparse_file` 两项），它实际只对 Windows 有意义。
> **不要**把它当成「Linux 上钉死走原生路径」的手段——那条路不存在。

**这不是 bug，是两条各自正确的设计相乘的结果。** 值得单独记，因为它有两副面孔：
接线前表现为「设了没反应」，接线后表现为「升级后服务起不来」——
后者极易被误判成接线 PR 引入的回归。

**另注**：`auto` 档下想知道某一项落在 native 还是 builtin，**目前没有任何办法从日志看**
（`grep -rn '能力矩阵\|Matrix' internal/server internal/config cmd/` 只命中一句注释，
零个日志点）。文档里不要建议读者「去启动日志里看能力矩阵」——那东西还不存在。

**处置**：`configs/example.yaml` 已补三条「实际行为」说明，`CHANGELOG.md` 的
`OSCAP-WIRING-STATUS` 块已补本条，`README.md` 已知限制第 7 条已补一句。

---

## 1c. 反向的不诚实：**谎报未完成**（本轮逮到 1 处，形态值得单列）

诚实性审计最容易只往一个方向查（有没有夸大）。**反向的错误同样是错，而且更隐蔽，
因为它读起来很谦虚。**

**实例**：`README.md`「Time Machine 状态」一节写于 v0.1.0，把
**durable / persistent handle** 列在「以下能力**未实现**」里，还标注它「对 TM 影响最大」。
但 v0.2.0 已经实现（PR #20，缺陷修复 PR #40，独立验证用例 PR #29；
CHANGELOG v0.2.0「协议与功能」第一条即是它）。

```sh
grep -n 'durable' CHANGELOG.md | head        # v0.2.0 段头号新增
grep -rn 'DHnQ\|DH2Q' internal/smb/command/  # 实现存在
```

**为什么这类错更隐蔽**：夸大会被用户用「我试了不行」当场戳穿，谎报未完成不会——
用户只会绕开这个功能，或者干脆换个方案。它的代价是**沉默的**。

**处置**：README TM 一节已改写——durable handle 移出「未实现」清单，
标注为 **B 档**（单测 + impacket 线级用例，无真机），
并明写「已验证的是握手与重连协议正确，不是真实备份过程不会中断」。

---

## 1d. Time Machine 口径订正（2026-08-09 项目所有者决定）

**决定**：TM 真机验收**从 v0.2.0 的强制项降为可选项**，不再是发布阻塞项，
原因是没有可用的真机环境与时间。**降的是验收要求，不是功能。**

审计视角下的要害：**标准是被主动降下来的，所以文档更不许把它抬回去。** 落地三处：

| 位置 | 改动 |
|---|---|
| `AGENTS.md` §2 阶段二 | 验收标准原句**保留**，补一段说明它是「阶段二长期标准、非任何单版本门槛」，记录决定日期/原话/原因，并写死文档口径要求 |
| `README.md` 顶部提要 + TM 状态节 | 「验收仍在进行」→「一次都没跑过，且已降为可选」；新增 A/B/C/D 证据强度分档表并逐条标注；末尾「v0.1.0 即便…」改为不绑版本号 |
| `CHANGELOG.md` v0.2.0 已知问题 | C 档定级条目补记该决定，并加一句「该定级短期内不会有新证据，不要因为版本号往前走就推断它变可靠了」 |

**证据强度分档**（新引入，替代含糊的「已支持 / 部分支持」）：

| 档 | 含义 | 本项目现状 |
|---|---|---|
| A | macOS 真机端到端实测 | **一条都没有** |
| B | 第三方 SMB 客户端协议级实测 | AAPL 协商、命名流/ADS、稀疏三件套、配额上报、大目录枚举、`_adisk._tcp`、durable handle |
| C | 仅本仓库单测覆盖 | —— |
| D | 代码存在，无任何一关执行过 | Darwin 的 `F_FULLFSYNC` 分支（只有交叉编译保证）；§3.3 那 6 个文件 |

**刻意没做**：没有删任何功能描述，没有把已实现的东西写成「未实现」。
所有者降的是验收门槛，不是功能范围——把功能一起删掉是另一种失真。

---

## 2. 存疑项

### ✅ Q1（已消解）：`metadata_path`「所有平台都参与启动校验」与实测相反

> **状态更新（18:33）**：本条在我把它列为「存疑待定夺」之后、提交之前，
> 已由**另一位 agent 在同一分支上独立发现并修正**（提交 `b39b2bf`，同时改了
> `CHANGELOG.md` 与 `README.md` 两处孪生条目）。
> 我的探针结论与其修正内容**完全一致**，两条独立取证路径互为佐证，故本条不再需要定夺。
> 下面的取证过程保留，因为它是这条结论的可复算判据。
>
> ⚠️ **附带记一笔协作事实**：该提交不是我做的，说明**当时有另一位 agent 在
> `/work/docs-honesty` 这个工作树里写文件并推到了 `docs/v020-honesty-audit` 分支**。
> 结果是好的（改对了），但它绕过了 AGENTS.md §7.3.1 的工作树隔离前提。
> 记录在此是因为：若两人同时在同一工作树里编辑同一文件，后写者会**静默覆盖**前者
> （`memory/feedback_shared_worktree_write_discipline.md` 记过一次整文件覆盖事故）。

**原始取证如下。**

**文档怎么说**：

- `README.md`：「⚠️「非 Windows 会忽略它」是**运行时**行为，但**校验在所有平台都做**：
  填的必须是当前运行平台意义上的绝对路径，否则启动直接失败。所以在 Linux 上填 `C:\...` 会起不来」
- `CHANGELOG.md` v0.2.0 配置段：「非 Windows 上跳过运行时使用，但**所有平台都参与启动校验**」

**代码怎么写**（`internal/config/validate.go:390-396`）：

```go
func validateShareMetadataPath(s *Share, prefix string, hostOS string, errs *ValidationErrors) {
	if s.MetadataPath == "" {
		return
	}
	if hostOS != "windows" {
		return          // ← 非 Windows 直接返回，什么都不校验
	}
```

**实测怎么样**（起真二进制，不是读代码推断）：

```sh
# 探针 A：Linux 上填 Windows 绝对路径 C:\ProgramData\stupidsamba\x.db
timeout 3 ./ssprobe -config win.yaml; echo $?    # → 124（被 timeout 杀掉 = 一直在跑 = 启动成功）
# 日志只有一条 WARN：「该字段仅在 Windows 上生效，当前平台（linux）会忽略它」

# 探针 B：填一个明显的相对路径 not-absolute-at-all
timeout 3 ./ssprobe -config rel.yaml; echo $?    # → 124（同样正常启动）

# 反向对照（证明探针有鉴别力，不是恒真）：把共享目录改成不存在的路径
timeout 3 ./ssprobe -config bad.yaml; echo $?    # → 1
# stupidsamba: 配置校验失败: shares[0].path: 共享 "public" 的目录不存在: /nonexistent-dir-xyz
```

**反向对照是这条判据成立的关键**：探针 C 证明「配置非法时这个二进制确实会退出 1 并报错」，
所以 A/B 的「正常启动」是真的没校验，而不是我的探针根本测不出失败。

**判定**：两处文档均与实测相反。推测是 PR #18（标题即
`config: metadata_path 非 Windows 平台跳过校验（修跨平台判定 bug）`）改了行为，
文档停留在改动前的理解上。

**为什么当初不自行拍板**：这牵涉 PR #18 的**意图**——到底是「跳过是有意为之，文档该改」，
还是「文档写的是期望行为，代码是 bug」。前者改文档，后者该由 config owner 改代码。

**实际处置**：判定为前者（PR #18 标题即
`config: metadata_path 非 Windows 平台跳过校验（修跨平台判定 bug）`，说明跳过是目的），
文档已按实测修正——`CHANGELOG.md:169-176` 与 `README.md:333`。

**仍需注意的残留风险**：非 Windows 上这个字段**唯一的出口是一条 WARN**。
配置写错（比如把 Windows 路径带到 Linux）不会有任何硬性反馈，
而 WARN 在日志量大时极易被淹没。这不是文档问题，是**产品行为的可用性问题**，
建议由 config owner 评估是否值得在非 Windows 上也校验「至少得是个合法路径形状」。
本轮不改代码，仅记录。

### Q2（待定夺）：`internal/meta` 那条「v0.3.0 接线」是不是同一种版本承诺？

`CHANGELOG.md`（R11 双 bbolt 撞车那条）写着「在 v0.3.0 把 `internal/meta` 接线时
必须一并拆掉旧实现」。本轮已经把 **oscap** 的「v0.3.0 生效」承诺删掉了
（理由：功能没合入 `main` 之前不给版本承诺），**但 meta 这条同型的留着没动**。

**为什么不自行拍板**：两者形式相同、性质未必相同——oscap 那条是**对用户承诺功能生效时间**，
meta 这条更像**给维护者的施工注意事项**（「动它的时候记得一起拆」），
后者即便版本号漂了也不误导用户。**倾向保留**，但两条同型表述一留一删，
读者可能读出「一个有排期、一个没有」的言外之意。请 team-lead 定夺：
统一去版本化，还是保留并注明「这是施工提醒，不是发布承诺」。

### Q3（待定夺）：`native` 报错文案要不要现在就改软

程序打印的「native=强制原生、不支持则启动报错」当前不成立（见 §1b）。
三个选项：(a) 不动，接线后自然成立；(b) 现在改成描述性措辞，接线后再改回；
(c) 不动文案，但在 `example.yaml` / README 里标注它当前不生效（**本轮已按 (c) 做**）。

**倾向 (c)**：改文案属于产品代码改动，会与 `oscap-wire` 正在做的接线撞车，
且接线一落地就要改回去，是一次纯往返。但这是**产品行为**不是文档，
最终该由 team-lead 或 config owner 拍。

### ✅ Q4（已消解，且当场演了一遍 §1c）：6 个从未被编译过的文件

见 §3.3。我列这条时写了一句「若发版前有人修了，记得把 CHANGELOG 那条删掉，
否则又变成一条『谎报未完成』」——**十几分钟后这件事真的发生了。**

`origin/main` 的 `e664a47`（`vfs: 判据抽成纯函数并补自身反向对照 + check-test-compile
补 freebsd 编译盲区`）已把 `freebsd/amd64` 追加进平台列表。判据（18:45 实跑）：

```sh
git show origin/main:test/ci/check-test-compile.sh | grep 'for t in'
# → for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64 freebsd/amd64
```

修的人做得比我建议的更完整：加了注释「**故意多出来的一档，不在 C7 的支持矩阵里，
别当成手滑删掉**」（防后人当噪声清理）。

**⚠️ 但这里有一处不实描述，是我自己写的，现就地订正**（这正是本节要立的规矩，
不在自己身上破例）：我初稿据 `check-test-compile.sh:113` 的注释
「负向对照见 `test/ci/negative-verify.sh` 的 freebsd 段」，写成「已补 freebsd 段做反向对照」。
**没有核实那个段落是否真的存在。** 复核结果（18:59 CST 实跑）：

```sh
git show origin/main:test/ci/negative-verify.sh | grep -c freebsd
# → 0
```

**注释引用了一个不存在的实体。** 所以 `freebsd/amd64` 这一档目前是「跑了但没牙」：
它若被人从平台列表删掉、或被 `continue` 提前跳过，没有任何一关会变红。
按本仓库自己的标准（「一个从来没红过的门禁，和没有门禁是一回事」），这一步还没走完。

这是本仓库「写了但从未被验证过」的**第 10 例**，但形态是新的：
**前 9 例是代码没被执行，这一例是「注释里引用了一个不存在的实体」。**
危害方向也相反——前者只是没保护，后者会**主动劝阻**后来者去补：
读到那句注释的人会认为反向对照已经存在，于是不再去写。
**文档/注释里的「见 X」必须当断言核实**，它和「X 已经存在」是等价的。

修脚本不归我（`test/` 是 vfs-deflake 的文件，已知会他在修），本轮只如实登记。
**CHANGELOG 的「已知问题」条目已同步改为「已修复（但反向对照仍缺）」并保留形态记录。**

> **这条留着当活样本**：`main` 在你写文档的这半小时里是会动的。
> 「已知问题」是**所有文档里最容易腐烂的一节**——它天然记录「还没做的事」，
> 而那恰恰是别人此刻正在做的事。**发版前必须逐条重跑判据，不能沿用审计当时的结论。**

---

## 3. 其余逐条核验（`CHANGELOG.md` v0.2.0 段不抽查，全条过）

### 3.1 属实、无需改动

| 断言 | 判据 | 结果 |
|---|---|---|
| `internal/vfs/metadata_other.go` 非 Windows 无旁路兜底 | `cat internal/vfs/metadata_other.go` | ✅ 仍是 `return nil, nil`（8-10 行）。**builtin 适配器存在 ≠ 这个洞被堵上**，两件事，未合并陈述 |
| `internal/meta` 无产品调用点（R11 双 bbolt 撞车） | `go list -tags metabolt -f '{{.ImportPath}} {{.Imports}}' ./... \| grep internal/meta` | ✅ 无任何包 import 它；bucket `posix.v2`（`meta/bolt.go:72`）vs 旧实现 `posix`（`vfs/metadata_windows.go:30`），两者派生同一个 db 文件路径 |
| `CHANGE_NOTIFY` 仍返回 `STATUS_NOT_SUPPORTED` | `internal/smb/command/notify.go:48` | ✅ `return status.NotSupported` |
| 不宣告 `CAP_LEASING`、一律授予 NONE oplock | `internal/smb/command/oplock.go:17,91` | ✅ 属实 |
| `validateWindowsName` 已接线 | `internal/vfs/path.go:203` | ✅ 已被 `ValidateComponent` 调用 |
| crypto / auth / dialect 三目录自 v0.1.0 零改动 | `git diff --stat v0.1.0..origin/main -- internal/smb/crypto internal/auth internal/smb/dialect` | ✅ 输出为空 |
| CREATE context 注册表现登记 5 族 | `grep -rn 'registerCreateContext' internal/smb/command/` | ✅ AAPL / AlSi / MxAc / QFid / durable |

### 3.2 已修正

| 编号 | 位置 | 问题 | 处置 |
|---|---|---|---|
| C1 | `README.md` 客户端矩阵 | `mount.cifs` 标「✅ 服务端支持」，理由写「缺 `CAP_SYS_ADMIN`」 | 归因**已被实验推翻**（AGENTS.md §10.3 第 2 条）：真因是非初始 user namespace 内核只放行带 `FS_USERNS_MOUNT` 的文件系统，与 capability 无关。且我们**从未在真实 Linux 主机验证过**，「✅ 支持」是未经验证的声明 → 改为「⏭️ 本环境跳过，未实测」并写明真因与反向对照 |
| C2 | `README.md` 许可证段 | 「当前处于内部测试阶段（**v0.1.0** prerelease）」 | 版本号过期 → 改为 v0.2.0，并写明**仍按 prerelease 发布**（`.cnb.yml:235-240` 硬编码 `preRelease: true` / `latest: false`） |
| C3 | `CHANGELOG.md` 标题行 | 标「v0.2.0（2026-08-09，**release**）」 | 与实际发布渠道不符（同上，流水线硬编码 prerelease）→ 标题标注「实际发布渠道仍是 prerelease」，并在「发布物形态」新增一条说明如何改 |
| C4 | `CHANGELOG.md` 发布物形态 | 「镜像**尚未推送**至远端制品库」 | 不完整：PR #134 已把推送做进 `tag_push`（`.cnb.yml:234`）→ 补充「打 tag 时自动构建并 push，且刻意排在 `git:release` 之前」 |
| C5 | `CHANGELOG.md` 写作纪律第 4 条 | 核对 PR 号的命令 `git log --merges --ancestry-path …` | **该命令有盲区**，见下方 §4 |
| C6 | `README.md` 构建与自检 | 只列 build / vet / test 三条 | 漏了 `check-test-compile.sh`——`go build` 不编译 `_test.go`，正是 R15 事故的成因 → 补上并说明理由 |
| C7 | `CHANGELOG.md` + `README.md` | `metadata_path`「所有平台都参与启动校验」 | 与实测相反，见 §2 Q1。已修正（`b39b2bf`），两处孪生条目同时改 |

> **⚠️ C2 / C3 的处置本身已被 PR #156（`25e92d2`）推翻，上表两格保留作为审计留痕、不再是当前事实。**
> 那两格写的是「如实承认仍按 prerelease 发布」，前提是 `.cnb.yml` 硬编码 `preRelease: true`。
> #156 之后发布渠道**按 tag 名的 SemVer 判定**（带连字符→预发布，不带→正式版且标记 latest），
> 已用 `v0.0.99-probe` / `v0.0.99` 两条真 tag 双向实测并做过跨配置对照。
> `README.md` 与 `CHANGELOG.md` 的对应表述已随之订正。
> 记这一行是因为**审计表里「已修正」的条目同样会过期** —— 一份只记录「当时改成了什么」
> 而不记录「后来又被推翻」的审计，读起来比没有审计更危险。

### 3.3 新发现的缺口（**已于 18:45 前后由他人在 `main` 修复**，见 §2 Q4）

> **状态**：本节描述的是发现当时（18:1x）的状态。`origin/main` 的 `e664a47` 已修，
> CHANGELOG 对应条目也已改为「已修复」。**下面保留原始取证过程，因为缺口的形态
> 比缺口本身更值得记住。**

**6 个带 `!linux && !darwin && !windows` 约束的文件从进仓库起一行都没被编译过。**

```sh
grep -rln '!linux && !darwin && !windows' --include='*.go' internal/
# internal/oscap/native/native_other.go
# internal/oscap/probe_other.go
# internal/oscap/probe_helper_other_test.go
# internal/vfs/attr_other.go
# internal/vfs/sparse_other.go
# internal/vfs/sys_other.go

sed -n '104p' test/ci/check-test-compile.sh
# for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
```

四个目标平台**没有一个满足**那个约束 → 这 6 个文件不被任何一关编译。
与 AGENTS.md §1.2 点名的「薛定谔的实现」完全同型。

本次**手工补跑了一次**（这是缺口存在的证明，也是修复可行性的证明）：

```sh
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go vet -tags integration,smoke,metabolt,qadefect ./...
# → rc=0
```

即这些文件**当前**是能编译的；但 CI 不看，就随时会在无人察觉时坏掉。
**修法**：往 `check-test-compile.sh:104` 的平台列表末尾**追加** `freebsd/amd64`（一行）。
按 §7.1 共享文件规矩：只追加，不重排。
**这条如果发版时还没修，CHANGELOG 里已如实列为已知问题。**

---

## 4. 顺带修正一条会误导后来者的「核对方法」

CHANGELOG 写作纪律第 4 条推荐用这条命令核对 PR 号：

```sh
git log --oneline --merges --ancestry-path <功能提交>..origin/main | tail -1
```

**它有盲区**：`--merges` 只保留多父提交，而本仓库有一部分 PR 是以**单父提交**
落进 `main` 的（squash / fast-forward），它们的 subject 照样写着
`Merge pull request #NNN`，却会被 `--merges` **整个滤掉**。

实测（18:12）：

```sh
git log --oneline --merges origin/main | grep '#135'        # → 空
git merge-base --is-ancestor ff77acb origin/main; echo $?   # → 0（是祖先，确实在主干上）
git rev-list --parents -n1 ff77acb | wc -w                  # → 2（即只有 1 个父提交）
```

PR #135 与 #138 都是这种。**照原命令核对会得出「#135 不存在」**，
或误挂到后面某个真 merge 上——而这正是那条纪律本身要防的错误。

**稳妥查法**（不依赖父提交个数）：

```sh
git log --oneline origin/main | grep -a "#<号> "
git merge-base --is-ancestor <sha> origin/main    # 再确认在主干上
```

已写进 `CHANGELOG.md` 的写作纪律第 4 条。

---

## 5. 本轮审计**未覆盖**的范围（避免读者高估本文的效力）

诚实性审计本身也要诚实：

- **只核了对外文档的断言，没有做代码正确性审查。** 「文档说 X，代码确实写了 X」
  不等于「X 的实现是对的」。
- **v0.1.0 段落只抽查了与 v0.2.0 有交叉的条目**（crypto/auth/dialect 零改动那条），
  没有逐条重核 v0.1.0 的全部声明。
- **`docs/` 下的其余文档**（`timemachine-status.md`、`acceptance-v0.1.0.md`、
  `protocol-notes.md`、`status-v0.2.0.md`）**不在本轮范围内**。
  其中 `timemachine-status.md` 是 Time Machine C 档定级的依据文件，
  CHANGELOG 引用了它的结论，但我没有反向核验该文件自身的内容。
- **Windows / macOS 上的行为一律没有实测**（开发容器是 Linux）。
  凡涉及这两个平台的断言，置信度上限是「只交叉编译过」。
- **`configs/example.yaml` 只核了 `filesystem_mode` 与 `metadata_path` 两处注释**，
  其余字段注释未逐条对代码核。

---

## 附：复算本审计的最短路径

```sh
export PATH=$PATH:/usr/local/go/bin

# 核心结论（oscap 零消费方）——一条命令
go list -deps ./cmd/stupidsamba | grep -c oscap        # 期望 1

# 三个包的测试真跑且零 SKIP
go test -v ./internal/oscap/... 2>&1 | grep -c -- '--- SKIP'   # 期望 0

# portable 门禁有牙
sh test/ci/portable-mode.sh                            # 期望 rc=0 且打印 4 行「变异体…如期变红」

# 未被编译的 6 个文件仍然存在
grep -rln '!linux && !darwin && !windows' --include='*.go' internal/ | wc -l   # 期望 6
sed -n '104p' test/ci/check-test-compile.sh            # 平台列表里是否已追加 freebsd/amd64

# native 档不兑现「不支持则启动报错」（§1b）——必须连反向对照一起跑，否则不算数
CGO_ENABLED=0 go build -o /tmp/dhprobe ./cmd/stupidsamba
timeout 3 /tmp/dhprobe -config <填 native 的配置>;   echo $?   # 期望 124 = 照常启动
timeout 3 /tmp/dhprobe -config <填 Native 的配置>;   echo $?   # 期望 1   = 反向对照，探针有鉴别力
```

> **这份最短路径本身也有前提**：它复算的是**运行本命令时的工作树**，不是发布产物。
> `oscap-wire` 的接线 PR 一旦合入，第一条与最后两条的期望值都会变——
> 那时**期望值变了不等于本文档写错了**，请对照当时的 `git log` 判断，
> 不要直接把本文当成当前状态的断言。

---

## 6. 收口轮补充（2026-08-09 18:25 ~ 18:45，按 team-lead 三条追加指令）

本轮回了三件事，均已落进产品文档（非本审计文档本身），并各自用源码/真二进制复验：

1. **Time Machine 口径三处订正**（所有者 18:25 决定：真机验收降为可选，非功能降级）。
   - `AGENTS.md` §2 阶段二：验收标准注明「阶段二长期标准，非 v0.2.0 门槛」+ 决定日期与原因。
   - `README.md` / `CHANGELOG.md`：TM 现状按 **A/B/C/D 证据强度分档**（A=macOS 真机，本项目 0 条；
     B=第三方客户端协议级实测；C=单测；D=仅代码存在），删去「支持/通过」类无真机证据表述。
   - 写进去的硬性约束：「标准是被主动降下来的，就更不许在文档里把它抬回去」。

2. **`configs/example.yaml:21-35` 校准**：补「设计语义 vs 当前行为」说明，含
   `native` 在 POSIX 接线后将**恒定启动失败**这一事实。
   - 根因已读源码确认：`internal/oscap/probe_linux.go:36` / `probe_darwin.go:39` 的
     `case CapDOSAttributes: return false`（注释明文「POSIX 没有存放 DOS 属性位的地方」）。
   - **刻意不引 oscap-wire 未定稿的报错原文**（team-lead 明确要求），只描述机制。
   - 同时写明：当前未接线时 native 是 no-op、能正常启动；失败是**接线之后**才会发生。

3. **`go list -deps ./cmd/stupidsamba | grep -c oscap` = 1** 作为最硬判据，写进
   `example.yaml` 注释与本文 §附，供接线 PR 合入后当场复算。

### 本轮协作事实（记入，防重演）

收口过程中发现**有另一位 agent 在同一工作树 `/work/docs-honesty` 内直接改文件并推我的分支**
（`docs/v020-honesty-audit`）：提交 `b39b2bf`（metadata_path 修正）非我所做却在我分支上；
工作区里凭空出现 TM 三处 + example.yaml + native 说明的未提交改动（即上面 1/2 的实质内容）；
分支历史出现内容重复但 SHA 不同的并行提交。结果是好的（对方也做对了，且与指令吻合），
但**绕过了 AGENTS.md §7.3.1 工作树隔离**——同工作树并行编辑会静默覆盖前者
（`memory/feedback_shared_worktree_write_discipline.md` 记过整文件覆盖事故）。
已用 `git push --force-with-lease` 把远端收敛成一份完整、自洽、所有判据复核过的分支（超集，无丢失）。
