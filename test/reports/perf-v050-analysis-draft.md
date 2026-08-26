# v0.5 性能瓶颈定位分析与优化结果（perf 角色，2026-08-26）

> **性质**：服务端 profile 实测分析 + P2 优化结果，分支 `perf/v050-hotpath`。
> 测量基础设施：`test/perf`（P0 收编的 go-smb2 基准）+ 服务端可选管理端口
> `STUPIDSAMBA_ADMIN_ADDR=127.0.0.1:6455`（仅 loopback，默认关闭，
> 提供 /debug/pprof/* 与传输帧计数 expvar；commit 见 git log `ci/test` 段）。
> 环境：与 perf-v040 基线同容器（4 vCPU / overlayfs / Go 1.25.0 / CGO off）。
> 本文档只测量、只报告，不含产品语义改动（pprof 接线为任务书明确要求的测量设施）。

## 0. 一句话结论

**签名模式（默认配置）下服务端 68.6% 的 CPU 烧在 AES-CMAC 的逐块软件实现上**
（`internal/smb/crypto/cmac.go`）：39.9% 在每 16 字节一次的 `aes.encryptBlockAsm`
接口调用，18.0% 在**逐字节**的 `xorInto`。把它换成硬件加速的 CBC 批处理 +
字长 XOR，是当前唯一的大头；其余各项（分配削减、bbolt 写放大）都是二档收益。

## 1. 测量口径

| 项 | 值 |
|---|---|
| 被测 | 本分支 HEAD（=origin/main `73f96cf` + 测量设施 commit） |
| 负载 | `test/perf` large 场景：128 MiB 单流写+读回（1 MiB chunk），r0 预热 |
| 并发 | `-concurrency 16 -size 134217728`（各 worker 独立连接/会话/树） |
| 安全模式 | signing_required / encryption_required 两份配置分别重启测 |
| profile | CPU（负载期 14–20s 窗口）、heap(alloc_space)、mutex/block（12s delta 窗口） |

⚠️ 本容器邻居噪声大：同一负载不同轮次吞吐可差 2×（实测签名写 108→240 MB/s 都出现过）。
**结论一律以 profile 占比与结构性证据为准，绝对吞吐仅作参考**；
优化前后对比必须用紧挨着跑的成对测量（见 §5）。

## 2. 发现 F1：CMAC 逐块软件循环是压倒性热点（假设 #2 证实）

CPU profile（signing 模式，128 MiB 写+读负载期，总采样 2.61s）：

```
      flat  flat%   cum   cum%
     1.04s 39.85%  1.04s 39.85%  crypto/internal/fips140/aes.encryptBlockAsm
     0.47s 18.01%  0.47s 18.01%  internal/smb/crypto.xorInto (inline)
     0.31s 11.88%  0.31s 11.88%  internal/runtime/syscall.Syscall6
     ...
     0       0%    1.79s 68.58%  internal/smb/crypto.CMACWithBlock
     0       0%    0.90s 34.48%  server.(*Connection).finishChain      （响应签名）
     0       0%    0.89s 34.10%  command.(*Context).checkSignature     （请求验签）
```

解读：

- `crypto/aes` 底层**已经是 AES-NI**（encryptBlockAsm），问题不在「没走硬件」，
  而在调用形态：`CMACWithBlock`（cmac.go:66-96）对每 16 字节做一次
  **接口方法调用** `b.Encrypt(x,x)`，外加每次消息重算 subkeys（cmacSubkeys 又是
  2 次 AES）。
- `xorInto` 是**逐字节** XOR（cmac.go:99-103），纯标量循环，占 18%。
- 签名（finishChain，对 ≤1 MiB 的响应）与验签（checkSignature，对客户端 WRITE 请求）
  各占 ~34%，两者对称 —— **这同时解释了「写≈½读」之谜的一半**：
  写方向的请求体（客户端签名→我们验签）与读方向的响应体（我们签名）都要过这条
  慢路径，再叠加 overlayfs pwrite 慢于页缓存命中读、以及客户端自身在同 4 个
  vCPU 上的对称开销，写方向成为最先饱和的方向。

> **⚠️ 口径订正（P2 复测后）**：profile 占比是「服务端活跃 CPU 内的占比」，
> 不等于墙钟收益。overlay 微基准证实旧 CMAC 热缓存实为 ~758 MB/s（非当初从
> profile 推算的 ~500），因此 CMAC 批处理化的真实单点收益是 1.27×（大消息）、
> 2.1×（64 B 小消息，密钥状态缓存贡献一半）。端到端净收益见 §7，
> 其中并发场景最显著——那正是 CMAC 在 4 vCPU 上最先打满的地方。
- 微观上每字节成本 ≈ (1.04+0.47)s / (2×128MiB×2 方向…) 量级在
  **~0.5 GB/s**，远低于硬件 CBC 可达的数 GB/s。

**对策（P2 Opt-1）**：CMAC 的链式定义 X_{i}=E(X_{i-1}⊕M_i) 与 CBC-MAC（IV=0）
完全同构 —— 除最后一个块外可整段交给 `cipher.NewCBCEncrypter`（amd64 上是
批量化的 cbcEncAsm，AES-NI + 分组间流水），末块按 RFC 4493 单独处理；
XOR 改字长运算。语义零变化（RFC 4493 测试向量 + 既有 golden test 钉住）。

## 3. 发现 F2：「签名模式多拆 PDU」不成立（假设 #3 否证）

v040 报告称签名模式下客户端收到的 PDU 数比加密模式多 24%（396 vs 319），
疑似 credit/max-size 记账分叉。本次用**帧级计数器**（test/perf 的 net.Conn
包装器增量解析 Direct TCP 帧）复测：

| 配置 | 写相位 tx/rx PDU | 读相位 tx/rx PDU |
|---|---|---|
| signing_required | 131 / 131 | 130 / 130 |
| encryption_required | 131 / 131 | 130 / 130 |

**两种模式的 PDU 数逐帧一致**（128 MiB = 128 个数据 PDU + 会话开销，两模式相同）。
credit 授予逻辑（server/credit.go）与 MaxRead/WriteSize 协商值也与安全模式无关
（代码核对一致）。归因结论：v040 的 396/319 计的是**客户端 socket 读次数**
（包装 net.Conn 计 Read() 调用会把一次 TCP 读到半个/一个半 PDU 的情况都算进去），
不是 SMB PDU 数；该差异不需要协议层解释。此项从跟进清单划掉。

## 4. 发现 F3/F4/F5/F6：其余假设的核验结果

### F3 加密模式已近 IO/GC 边界（假设 #2 加密侧）

encryption 模式 CPU profile：syscall 27.5%、GCM 硬件路径（gcmAesEnc/Dec）19.5%、
GC 相关（memclr 12.8% + scanobject 12.1% + madvise…）合计 >25%，无 CMAC。
协商走的是 **AES-GCM**（3.1.1 双方都支持 GCM，优先于 CCM），即标准库硬件路径。
剩余优化空间主要在**分配削减**而非密码学本身。

分配热点（heap, alloc_space，累计 1135 MB）：

| 位置 | 字节 | 占比 | 说明 |
|---|---|---|---|
| command.handleRead flat | 384 MB | 34% | 每次 READ `make([]byte, Length)`（command 层，非本角色文件） |
| transport.ReadFrame flat | 374 MB | 33% | 每帧 `make([]byte, n)`，最大 1 MiB（**本角色文件**） |
| wire.ReadResponse.Append flat | 364 MB | 32% | 数据二次拷进 Out 缓冲（wire 层 Append 语义，本角色文件边缘） |

另 crypto/transform.go Encrypt 用 `a.Seal(nil,…)` 再拼 TRANSFORM 头，
加密响应每条多一次整载荷分配+拷贝（crypto 层，本角色文件）。

### F4 并发写饱和点不是应用锁（假设 #5 部分否证）

conc16（签名模式，2 轮）期间 mutex/block profile（12s delta 窗口）：

- mutex 总争用仅 459.8 ms，且 89% 是 GC 后台标记工人自阻塞 —— **没有应用级互斥热点**；
- block 里唯一的应用级条目：`oscap/builtin store.put → SetDOSAttributes`
  共 **1.11 s** 阻塞。追因：CREATE/DELETE 每次触发
  `vfs.applyCreateDOSAttrs`（local.go:473）/ forgetPathMetadata →
  bbolt 单写事务（全局锁 + fsync）。16 流 × 每轮删+建 = 数十个串行化事务，
  每个 ~20-35 ms（overlayfs fsync 慢），表现为轮边界附近的延迟毛刺
  （p95 劣化），**不是稳态带宽的天花板**。
  → 归 oscap/builtin 与 vfs 其余文件所有者；本角色不动（见 §5 存疑清单）。

稳态写带宽的限制仍是 F1 的签名 CPU（16 流聚合时 4 个 vCPU 被签名/验签吃满）。

### F5 VFS 写路径没有每写必 fsync（假设 #1 否证）

`localHandle.WriteAt`（local_handle.go:99-122）只在句柄带 FILE_WRITE_THROUGH 时
Sync；WRITE handler 只在 WRITEFLAG_WRITE_THROUGH / CreateOptions 要求时
Sync（read_write.go:184-189）；FLUSH 才强制 Sync(true)。go-smb2 默认不申请
write-through（加密模式写 p50 仅 1.5 ms 也佐证没有 fsync 在路上）。
双重拷贝亦无：frame 缓冲直接 pwrite。**写慢的主因回到 F1**。

### F6 TCP 设置无异常（假设 #6 否证）

全仓无 SetNoDelay/SetReadBuffer/SetWriteBuffer 调用；Go 对 TCP 连接默认开
TCP_NODELAY；WriteFrame 已用 net.Buffers 把 4 字节头与载荷一次 writev。
无可机改项。

## 5. P2 优化计划（按预期收益排序）

| # | 目标 | 文件 | 预期 | 正确性钉子 |
|---|---|---|---|---|
| Opt-1 | CMAC 批处理（CBC 化）+ 字长 XOR | internal/smb/crypto/cmac.go | 签名模式吞吐大幅提升（69% CPU → 预期 <20%） | RFC 4493 向量 + cmac_test 全绿 + sign_test golden |
| Opt-2 | CCM 同型批处理（CTR 用 cipher.NewCTR；macBlocks 复用 chunked-CBC） | internal/smb/crypto/ccm.go | 3.0/3.0.2 CCM 客户端受益；GCM 协商场景不受影响 | ccm_test 全部向量 |
| Opt-3 | transform.go Encrypt/Decrypt 减少整载荷分配/拷贝 | internal/smb/crypto/transform.go | 加密模式 GC 压力下降 | transform_test + encryption 集成用例 |
| Opt-4 | ReadFrame 帧缓冲 sync.Pool | internal/server/transport.go | 每帧 33% 堆分配消除 | transport_test + 集成套件 |

**不做/不敢动的**：

- command.handleRead 的 buf 与 Out 双拷贝（66% 分配所在）—— command/* 非本角色
  文件，动它会碰 dispatch/signing 判定逻辑；写成结论上报 team-lead 派单。
- bbolt 写放大（F4）—— oscap/builtin 所有者地盘，且有持久化语义取舍
  （能否 NoSync/合批属于数据安全决策，不该由性能角色顺手决定）。
- credit/PDU 记账 —— 无问题（F2），不碰。
- connection.go 的 VNI 校验段 —— fsctl 角色正在改，全程避开。

## 6. 附：profile 原始产物

- 二进制 pprof：`/tmp/opencode/perf-w2/prof/*.pbin`（易失，不入库）。
  本文 §2/§3/§4 的 top 文本即为入库凭据；如需复核可在本分支重建后按 §1 口径重采。

## 7. P2 优化结果（2026-08-26 实测）

四个优化 commit（每个含微基准前后对照，见各自 commit message）：

| # | commit | 内容 | 微基准 |
|---|---|---|---|
| 1 | `9248948`+`4a427a5` | CMAC 批处理化 + 混合路径 + 密钥状态缓存 | 64B 215→458 MB/s(2.13×)；64KiB 760→969(1.27×)；1MiB 758→960(1.27×) |
| 2 | `76f7f14` | CCM：CTR 批量化、CBC-MAC 分段、Open 免二次分配 | Seal/Open 1MiB 342→803 / 329→747 MB/s(≈2.3×) |
| 3 | `efcdae1` | TRANSFORM AEAD 缓存 + Encrypt 单次分配 | 省每消息密钥扩展与整载荷二次拷贝（GC 压力） |
| 4 | `383c766` | ReadFrame 帧缓冲复用 | 消除每帧 make 的稳定分配源 |

**端到端 A/B**（交替 ×2 控噪声；old = 优化前代码 overlay 构建，同一测量设施；
负载 = large c1 128MiB×3 轮 / c16 128MiB×2 轮 / small 500 文件；单位 MB/s 与服务端 CPU ticks）：

| 场景（signing） | old | new | Δ |
|---|---|---|---|
| c1 写 | 249.3 / 250.3 | **287.2 / 291.4** | **+15.8%** |
| c1 读 | 244.3 / 243.5 | 256.4 / 254.1 | +4.7% |
| **c16 写** | 599.3 / 574.0 | **781.5 / 684.7** | **+25%** |
| c16 读 | 622.9 / 595.6 | 711.4 / 572.1 | +5% |
| c16 服务端 CPU（固定负载） | 202 / 200 ticks | **147 / 150 ticks** | **−26%** |
| c1+small 服务端 CPU | 340.5 avg | 304 avg | −10.7% |

| 场景（encryption） | old | new | Δ |
|---|---|---|---|
| c1 写 | 538.3 / 465.4 | **578.2 / 603.7** | **+17.7%** |
| c1 读 | 422.5 / 433.6 | 485.1 / 432.3 | +7.2% |
| 服务端 CPU | 295 / 323 | **252 / 262** | −16.8% |
| small create/delete | ~575/690 | ~600/685 | 噪声内持平 |

要点：

1. **写方向收益最大**（+16~25%），因为服务端验签在写关键路径上；
   读方向受客户端 go-smb2 自己的软件验签钳制（两端同机共享 4 vCPU），只 +5~7%
   —— 这不是服务端没变快（CPU −11~26%），而是墙钟的短板在对面。
   「读≈2×写」的原始谜题随签名路径提速已明显收窄。
2. **小文件 IOPS 无变化**：其瓶颈是 CREATE 触发的 bbolt DOS 属性落库
   （每文件一个带 fsync 的写事务，~1.8 ms/个），属 F4 上报项，非本角色文件。
3. 正确性：RFC 4493 / SP800-38B / SP800-38C 全部向量、wire golden、
   server 单测、integration tag 套件全绿。Encrypt 的 append 别名 bug 被
   TestTransformRoundTrip 在提交前拦下（golden 即「语义零变化」的机器证明，
   名不虚传）。race 门禁因容器无 gcc 无法执行，已如实记录。

## 8. 遗留与移交清单

| 项 | 归属 | 说明 |
|---|---|---|
| command.handleRead 双拷贝（buf + Append，占堆分配 66%） | command 角色 | 读路径每响应少一次整载荷拷贝需改 handler/Context 契约 |
| bbolt DOS 落库写放大（CREATE/DELETE 各一次 fsync 事务） | oscap/builtin + vfs | 小文件 IOPS 的当前天花板；能否合批/延迟属持久化语义决策 |
| AES-GMAC 协商（TODO M4） | 协商角色 | GMAC 走 GCM 硬件路径，比 CMAC 更快且规范允许 |
| race 门禁补跑 | 有 gcc 的环境 | `CGO_ENABLED=1 go test -race ./internal/server/ ./internal/vfs/` |

## 9. 合并后 main 复测（2026-08-26 13:58 CST，team-lead 补记）

第二波六支分支全部合入 main（HEAD `130f99b`）后，用同一设施（test/perf/run-bench.sh，
128 MiB 口径）重跑一轮。原始输出存 `/tmp/opencode/perf_main_{signing,enc}.log`
（易失），SUMMARY 如下：

| 场景 | signing | encryption |
|---|---|---|
| large c1 写 | **257.9 MB/s** [p50=3751µs p95=4623µs] | **573.6 MB/s** [p50=1564µs p95=2476µs] |
| large c1 读 | 204.9 MB/s [p50=4595µs p95=6111µs] | 495.1 MB/s [p50=1752µs p95=3028µs] |
| large c16 写（聚合） | **726.0 MB/s** [p50=14.0ms p95=37.8ms] | **1119.6 MB/s** [p50=6.2ms p95=16.2ms] |
| large c16 读（聚合） | 745.5 MB/s [p50=17.7ms p95=39.2ms] | **1371.2 MB/s** [p50=8.5ms p95=22.3ms] |
| small create/delete | 531 / 609 ops/s | 465 / 555 ops/s |

解读（沿用 §1 噪声声明：本容器同负载可差 2×，绝对值仅参考）：

- 与 §7 分支内成对 A/B 一致：签名 c1 写落在当时测得的 new 区间附近（287→258，
  噪声内）；「加密比签名快」的结构性现象保持（GCM 硬件路径 vs CMAC 软件链，
  现在约 2.2×）。
- 与 v040 报告的 94.7/199.6 **不可直接对比**：那是另一套基准程序（512 MiB +
  客户端逐 chunk MD5），口径不同；同 harness 的可信对照只有 §7 的 A/B
  （c1 写 +15.8% / c16 写 +25% / 服务端 CPU −26%）。
- c16 读 1371 MB/s（加密）为该项目迄今实测最高聚合读；p95 未出现 v040 报告的
  45 ms 级恶化（本轮 c16 写 p95=37.8 ms，其中含 bbolt DOS 落库毛刺，见 F4）。
- 小文件 IOPS 维持 §8 移交项结论：天花板是 CREATE 触发的 bbolt 单事务 fsync
  （~1.8 ms/个），非协议栈。
