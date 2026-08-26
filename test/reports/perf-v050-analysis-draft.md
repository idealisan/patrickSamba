# v0.5 性能瓶颈定位分析（P1 草稿，2026-08-26）

> **性质**：服务端 profile 实测分析，perf 角色（分支 `perf/v050-hotpath`）。
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
