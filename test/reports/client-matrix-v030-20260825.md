# v0.3.0 第三方客户端交叉功能验证基线报告

- **被测版本**: origin/main `31d2902e8e6ba707cdd980a1f86c8bbdc86eafc5`（v0.3.0 发布点）
- **测试日期**: 2026-08-25 15:20–15:45 CST
- **测试角色**: qa-clients（黑盒：只测不改产品代码；全部配置与临时文件在 `/tmp/opencode/qac-24613/`，仓库内仅本报告）
- **服务端二进制**: `CGO_ENABLED=0 go build -o stupidsamba ./cmd/stupidsamba`（自报版本串 `dev (commit none…)`，构建未注入 ldflags 版本信息——见 §6 备注）

---

## 1. 结论先行

**v0.3.0 与三家第三方客户端（smbclient 4.22.10 / impacket 0.12.0 / go-smb2 v1.1.0）的交叉功能验证全部达标**：
认证、guest、只读强制、8MB 完整性回环、目录/文件全操作集在三个独立协议栈上均 PASS，
仓库集成套件 21/21 PASS（含 go-smb2 第三方栈用例）。

**唯一发现的硬缺陷是 OI-1 的现场复现成立**：portable 模式下两个进程服务同一共享根目录时，
第二个进程启动约 5 秒后失败退出（bbolt 旁路存储 flock 超时）。该场景下「一目录多进程」部署不可用。

次要观察（不阻塞）：SMB1 协商升级入口在本环境无法被任何第三方客户端触达
（smbclient 4.22 Debian 构建已剔除 SMB1，impacket 默认直发 SMB2），
该路径的证据强度停留在「仓库集成单测」档；纯 SMB1 请求被正确拒绝并有明确 WARN 日志（实测）。

## 2. 客户端 × 场景 × 结果矩阵

证据分档说明：**[实测]** = 本轮真机命令实测；**[单测]** = 仓库自动化测试覆盖；**[推断]** = 仅代码存在。

### 2.1 smbclient 4.22.10-Debian（Samba 官方 CLI）

| 场景 | 结果 | 关键输出摘录 | 分档 |
|---|---|---|---|
| 方言 `-m SMB2` 登录+ls | PASS | rc=0，列出 hello.txt | [实测] |
| 方言 `-m SMB3` 登录+ls | PASS | 服务端日志 `dialect=3.0.2 cipher=1` / `3.1.1 cipher=2` | [实测] |
| 方言 `-m SMB2_02` | PASS | ls 正常返回 | [实测] |
| 方言 `-m SMB2_10` | PASS | ls 正常返回 | [实测] |
| 方言 `-m SMB3_00` | PASS | ls 正常返回 | [实测] |
| 方言 `-m SMB3_02` | PASS | ls 正常返回 | [实测] |
| 方言 `-m SMB3_11` | PASS | ls 正常返回（服务端日志见 `dialect=3.1.1`） | [实测] |
| 方言 `-m NT1`（纯 SMB1） | FAIL(预期) | 客户端默认 min=SMB2_02 本地拒发；强设 min=NT1 后服务端断连并记 WARN `拒绝 SMB1 请求: SMB1 方言列表中没有 SMB2` | [实测] |
| 操作集 mkdir/put/ls/rename/get/rm/rmdir 一条会话 | PASS | 7 步全部成功，目录清理干净 | [实测] |
| 认证：正确口令 | PASS | `会话建立 user=qauser … guest=false` | [实测] |
| 认证：错误口令 | PASS(被正确拒绝) | `NT_STATUS_LOGON_FAILURE` | [实测] |
| 认证：匿名 `-N`（allow_guest=false 时） | PASS(被正确拒绝) | `NT_STATUS_LOGON_FAILURE` | [实测] |
| guest 共享：`-U guest%` 与 `-N` 均可登录+读写回环 | PASS | 服务端日志 `user=guest … guest=true`；put→get 内容一致 | [实测] |
| 只读共享：get 8MB + md5 | PASS | md5 `3bba6a3fb85e0be786d8f2ea1855a171` 与预置一致 | [实测] |
| 只读共享：put/mkdir/rm/rename/rmdir | FAIL(预期) | 全部 `NT_STATUS_MEDIA_WRITE_PROTECTED` | [实测] |
| 完整性：8MB put→rename→get 回环 md5 | PASS | `7d98da841483c0d3f0302f8ccdcbe4a1` 与源文件一致 | [实测] |
| portable 模式共享：ls/put/get/rm 回环 | PASS | md5 同上一致 | [实测] |

### 2.2 impacket 0.12.0（Python 独立协议栈）

脚本：附录 A（`impacket_qa.py`），用 `/usr/bin/python3` 运行（系统 python3.13 + apt 的 python3-impacket）。
注：默认 `python3` 是 uv 的 3.12，看不到 dist-packages 里的 impacket，属环境细节非产品问题。

| 场景 | 结果 | 关键输出摘录 | 分档 |
|---|---|---|---|
| 错误凭据登录必须被拒 | PASS | `STATUS_LOGON_FAILURE (0xc000006d)` | [实测] |
| 正确凭据 login | PASS | 协商结果 dialect=0x0300（SMB 3.0） | [实测] |
| listDir（含 . / .. 条目） | PASS | `['.', '..', 'hello.txt']` | [实测] |
| putFile 上传 8MB | PASS | — | [实测] |
| getFile 下载回环 md5 一致 | PASS | md5 前 12 位 `2dc8f2784e19` 匹配 | [实测] |
| createDirectory | PASS | 子目录可枚举 | [实测] |
| rename（重命名后内容不变） | PASS | — | [实测] |
| deleteFile | PASS | 列表中消失 | [实测] |
| deleteDirectory | PASS | 列表中消失 | [实测] |
| 只读共享写入必须失败 | PASS | `STATUS_MEDIA_WRITE_PROTECTED (0xc00000a2)` | [实测] |
| 只读共享 listDir 可读 | PASS | 见到预置 big.bin/hello.txt | [实测] |

小计：RW 9/9 PASS，RO 2/2 PASS。

### 2.3 go-smb2 v1.1.0（纯 Go 第三方栈，hirochachacha/go-smb2）

两层验证：

**(a) 独立验收客户端**（`scripts/clients/gosmb2/main.go` 现场重编译运行）：

| 步骤 | 结果 | 分档 |
|---|---|---|
| NTLM 认证+挂载 | PASS | [实测] |
| ReadDir / ReadFile | PASS | [实测] |
| 写入/回读 9500 字节 | PASS | [实测] |
| 1MiB 大文件读取（分块+credit） | PASS | [实测] |
| Stat | PASS | [实测] |
| 子目录枚举 | PASS | [实测] |
| Mkdir/Rename/Remove | PASS | [实测] |
| 稀疏偏移读写 | PASS | [实测] |
| Statfs | PASS | [实测] |

**(b) 仓库集成套件** `CGO_ENABLED=0 go test -tags integration -count=1 ./test/integration/`
→ **ok，21/21 PASS，0 FAIL，0 SKIP**（2.18s）。用例清单：
TestADSReadWrite, TestCancelNoResponse, TestCompoundUnrelated, TestCompoundRelated,
TestCompoundRelatedPostFailure, TestMultiDialect, TestSMB3EncryptedSession,
TestSMB3EncryptionDisabledStaysPlaintext, TestSMB3EncryptionDisabledRejectsTransformFrame,
TestSMB3EncryptionEnabledAdvertisesCipher, **TestGoSMB2FileOps**,
TestSharingViolation×8, TestSigningEnforced, TestSigningEnforcedRejectsSideEffects。
其中签名强制、加密会话、sharing violation 矩阵、复合链均为协议级回归。分档：[实测]（本轮实跑）。

### 2.4 mount.cifs（Linux 内核客户端）

| 场景 | 结果 | 原因 | 分档 |
|---|---|---|---|
| 内核 cifs 挂载 | **SKIP (rc=77)** | AGENTS.md §10.3 第 2 条：本容器处于非初始 user namespace，内核只放行带 FS_USERNS_MOUNT 的文件系统，cifs 不在其列，与 capability 无关。按既定口径由其余三家满足「至少三种客户端」。 | [环境限制] |

## 3. FAIL / SKIP 清单及原因

| # | 项 | 类别 | 原因与定性 |
|---|---|---|---|
| F1 | smbclient `-m NT1` 连接被断开 | 预期行为（非缺陷） | 服务端契约就是「不做 SMB1 文件操作」；纯 SMB1 方言表（无 "SMB 2.???"）被拒并记 WARN `拒绝 SMB1 请求: SMB1 方言列表中没有 SMB2`。行为正确。 |
| F2 | OI-1：portable 双进程第二实例启动失败 | **真实缺陷（已确认，修复在开发分支未合 main）** | 详见 §4。v0.3.0 发布版在「同一共享根目录、多进程」场景不可用。 |
| S1 | mount.cifs | SKIP(rc=77) | 环境限制（§10.3 第 2 条），非产品缺陷，按 §3 口径由 smbclient+impacket+go-smb2 三家补位。 |
| S2 | SMB1 COM_NEGOTIATE("SMB 2.???") 升级路径经第三方客户端实测 | SKIP | 环境无任何第三方客户端能发出该报文：Debian smbclient 4.22 已剔除 SMB1（连接横幅即显示 `SMB1 disabled`）；impacket 0.12 SMBConnection 默认直发 SMB2 negotiate（服务端日志两次协商均为直连 3.0，无 SMB1 入口痕迹）。该路径当前证据为 [单测] 档（integration 的 TestMultiDialect 等），未经本轮三方实测。 |

## 4. OI-1 现场取证（发布版 31d2902）

**场景**：两份独立配置文件（仅监听端口不同：4454 / 4455），`filesystem_mode: portable`，
共享路径完全相同 `/tmp/opencode/qac-24613/rwroot`，**都不配 metadata_path**（让两边算出同一默认落点）。
先起 P1(4454) 成功监听；再前台启动 P2(4455)，原样捕获输出：

P2 启动日志原文（完整 stderr，一字未改）：

```
time=2026-08-25T15:34:58.113+08:00 level=WARN msg="auth.users[0] \"qauser\" 使用明文口令，建议改用 nt_hash 避免口令落盘"
time=2026-08-25T15:34:58.113+08:00 level=WARN msg="server.signing_required=false：未强制 SMB 签名，存在中间人篡改风险"
stupidsamba: shares[0] "rwshare": oscap: 构造 builtin 适配器失败: oscap/builtin: 打开旁路存储 /tmp/opencode/qac-24613/.stupidsamba-oscap-a79960ed0145aaa6.db 失败（是否已被另一个进程占用？）: timeout
```

计时与退出码：

```
real	0m4.998s
user	0m0.009s
sys	0m0.006s
exit=1
```

要点：
- 报错文本本身已经提示了互斥语义（「是否已被另一个进程占用？」+ `timeout`）；
- 失败发生在 builtin 适配器构造期，早于监听，P2 从未开始服务；
- 耗时 ≈5.0s（内置 flock 等待超时的量级）；
- db 文件名由共享根路径哈希得出：`.stupidsamba-oscap-a79960ed0145aaa6.db`（落在共享根的父目录），
  两份配置指向同一路径必然相撞；
- P2 失败后 P1 继续正常服务（`fuser 4454/tcp` 有 PID，smbclient -L 仍能列出共享）——
  故障被隔离在第二进程，但**多进程共用一个共享目录的部署形态整体不可用**。

结论：OI-1 在 v0.3.0 发布版上**稳定复现**，「第二个进程等满超时后启动失败」，与缺陷描述一致。
（修复已在开发分支完成但未合入 main，不在本次范围。）

## 5. 版本与环境清单

| 组件 | 版本 |
|---|---|
| 被测服务端 | origin/main `31d2902`（v0.3.0），go1.25.0，`CGO_ENABLED=0`，linux/amd64 |
| smbclient | 4.22.10-Debian-4.22.10+dfsg-0+deb13u2（SMB1 已剔除） |
| impacket | 0.12.0-3（apt python3-impacket，跑在 /usr/bin/python3 = 3.13） |
| go-smb2 | github.com/hirochachacha/go-smb2 v1.1.0 |
| Go 工具链 | go1.25.0 linux/amd64 |
| 环境 | 容器内 127.0.0.1 回环；mDNS 全部关闭（mdns.enabled: false）；qa-clients 专属端口 4454/4455 |

## 6. 可复现命令清单

```sh
export PATH=$PATH:/usr/local/go/bin
# 构建被测端（worktree /work/qa-clients @ 31d2902）
cd /work/qa-clients && CGO_ENABLED=0 go build -o stupidsamba ./cmd/stupidsamba

# 四份场景配置（-check 全部通过；guest 配置在启动日志产生 WARN，满足 §8）
./stupidsamba -config cfg-a-auth-rw.yaml   -check   # 认证读写, 127.0.0.1:4454
./stupidsamba -config cfg-b-guest.yaml     -check   # guest 开放, 127.0.0.1:4455, allow_guest: true
./stupidsamba -config cfg-c-readonly.yaml  -check   # 只读,     127.0.0.1:4454
./stupidsamba -config cfg-d-portable.yaml  -check   # 同 a 但 filesystem_mode: portable, :4455

# 起/停（后台）
setsid nohup ./stupidsamba -config <cfg> > srv.log 2>&1 < /dev/null &
fuser 4454/tcp          # 找真实监听 PID；换配置用 fuser -k 4454/tcp 精准停旧实例

# --- smbclient ---
for m in SMB2 SMB3 SMB2_02 SMB2_10 SMB3_00 SMB3_02 SMB3_11; do
  smbclient //127.0.0.1/rwshare -p 4454 -U qauser%QaPassw0rd! -m $m -c "ls"; done
smbclient //127.0.0.1/rwshare -p 4454 -U qauser%QaPassw0rd! -m SMB3 \
  -c "mkdir qadir; put big.bin qadir/big.bin; ls qadir; rename qadir/big.bin qadir/big2.bin; get qadir/big2.bin got.bin; rm qadir/big2.bin; rmdir qadir; ls"
md5sum got.bin                       # 应等于源 big.bin 的 md5
smbclient //127.0.0.1/rwshare -p 4454 -U qauser%WRONGPASS -m SMB3 -c "ls"    # → NT_STATUS_LOGON_FAILURE
smbclient //127.0.0.1/guestshare -p 4455 -U guest% -m SMB3 -c "ls; put gsrc.txt gput.txt; get gput.txt gget.txt; rm gput.txt; del gget.txt"
smbclient //127.0.0.1/roshare -p 4454 -U qauser%QaPassw0rd! -m SMB3 \
  -c "get big.bin roget.bin" && md5sum roget.bin                              # get OK
smbclient //127.0.0.1/roshare -p 4454 -U qauser%QaPassw0rd! -m SMB3 \
  -c "put gsrc.txt n.txt; mkdir d; rm hello.txt; rename hello.txt h2"         # → 全部 MEDIA_WRITE_PROTECTED

# --- impacket ---
/usr/bin/python3 impacket_qa.py rw 127.0.0.1 4454 qauser QaPassw0rd! rwshare $PWD/local   # 9/9
/usr/bin/python3 impacket_qa.py ro 127.0.0.1 4454 qauser QaPassw0rd! roshare              # 2/2

# --- go-smb2 ---
(cd scripts/clients/gosmb2 && go build -o /tmp/gosmb2client .)
/tmp/gosmb2client 127.0.0.1:4454 qauser QaPassw0rd! rwshare                    # 全步 PASS
CGO_ENABLED=0 go test -tags integration -count=1 ./test/integration/           # ok 21/21

# --- OI-1 复现（两份 portable 配置同 share 根、端口 4454/4455）---
setsid nohup ./stupidsamba -config cfg-e-portable-4454.yaml > p1.log 2>&1 &    # P1 起来
time ./stupidsamba -config cfg-d-portable.yaml                                 # P2: ~5s 后 exit=1，flock timeout
```

备注：`./stupidsamba -version` 自报 `dev (commit none, built unknown)` —— 构建未通过 ldflags 注入版本信息，
发布产物的版本可追溯性依赖外部记录，建议后续版本在 release 流程注入（不阻塞本报告结论）。

## 7. 建议合入前必须修的点

1. **OI-1（必修，已有分支修好待合）**：portable/builtin 旁路存储的进程级 flock 使「同一共享目录多进程」部署在 v0.3.0 直接不可用。修复合入前，发布说明应明示此限制。
2. （建议，非阻塞）release 构建注入版本号/commit（`-ldflags "-X …"`），消除 `dev (commit none)`。
3. （建议，非阻塞）为 SMB1 "SMB 2.???" 协商升级入口补一条能用自研裸客户端（rawclient）发出的第三方视角回归——现有覆盖在 integration 单测档，环境内无第三方客户端能实测该入口。

---

## 附录 A：impacket 回归脚本全文（本轮实际使用）

```python
#!/usr/bin/env python3
"""qa-clients: impacket 0.12 脚本化回归（AGENTS.md §3 客户端矩阵）。

用法:
  impacket_qa.py rw <host> <port> <user> <pass> <share> <workdir>   # 读写全操作
  impacket_qa.py ro <host> <port> <user> <pass> <share>             # 只读共享写拒绝
"""
import hashlib
import os
import sys

from impacket.smbconnection import SMBConnection, SessionError

MODE = sys.argv[1]
HOST, PORT = sys.argv[2], int(sys.argv[3])
USER, PASS, SHARE = sys.argv[4], sys.argv[5], sys.argv[6]
WORKDIR = sys.argv[7] if MODE == "rw" else None

results = []


def check(name, ok, note=""):
    results.append((name, "PASS" if ok else "FAIL", note))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {note}")


def md5f(p):
    h = hashlib.md5()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


if MODE == "rw":
    # 0) 错误凭据必须被拒
    try:
        bad = SMBConnection(HOST, HOST, sess_port=PORT)
        bad.login(USER, "WRONG-" + PASS)
        check("bad-login-rejected", False, "错误口令竟然登录成功")
    except SessionError as e:
        check("bad-login-rejected", True, f"被拒: {e}")

    conn = SMBConnection(HOST, HOST, sess_port=PORT)
    conn.login(USER, PASS)
    check("login", True, f"dialect={conn.getDialect():#06x}")

    names = [f.get_longname() for f in conn.listPath(SHARE, "\\*")]
    check("listDir", "hello.txt" in names and ".." in names, f"entries={names}")

    src = os.path.join(WORKDIR, "imp_up.bin")
    with open(src, "wb") as f:
        f.write(os.urandom(8 * 1024 * 1024))
    local_md5 = md5f(src)
    remote = "\\imp_up.bin"
    with open(src, "rb") as f:
        conn.putFile(SHARE, remote, f.read)
    check("putFile(8MB)", True)

    dst = os.path.join(WORKDIR, "imp_down.bin")
    with open(dst, "wb") as f:
        conn.getFile(SHARE, remote, f.write)
    check("getFile+md5", md5f(dst) == local_md5, f"md5={local_md5[:12]}…")

    try:
        conn.createDirectory(SHARE, "\\imp_dir")
        sub = [x.get_longname() for x in conn.listPath(SHARE, "\\imp_dir\\*")]
        check("createDirectory", "." in sub or ".." in sub)
    except Exception as e:
        check("createDirectory", False, str(e))

    try:
        conn.rename(SHARE, remote, "\\imp_up_renamed.bin")
        still = os.path.join(WORKDIR, "imp_ren.bin")
        with open(still, "wb") as f:
            conn.getFile(SHARE, "\\imp_up_renamed.bin", f.write)
        check("rename", md5f(still) == local_md5)
    except Exception as e:
        check("rename", False, str(e))

    try:
        conn.deleteFile(SHARE, "\\imp_up_renamed.bin")
        gone = [x.get_longname() for x in conn.listPath(SHARE, "\\*")]
        check("deleteFile", "imp_up_renamed.bin" not in gone)
    except Exception as e:
        check("deleteFile", False, str(e))

    try:
        conn.deleteDirectory(SHARE, "\\imp_dir")
        gone = [x.get_longname() for x in conn.listPath(SHARE, "\\*")]
        check("deleteDirectory", "imp_dir" not in gone)
    except Exception as e:
        check("deleteDirectory", False, str(e))

elif MODE == "ro":
    conn = SMBConnection(HOST, HOST, sess_port=PORT)
    conn.login(USER, PASS)
    payload = b"should not land\n"

    def wfail():
        try:
            import io
            conn.putFile(SHARE, "\\imp_ro_write.bin", io.BytesIO(payload).read)
            return False, "只读共享竟然写入成功"
        except SessionError as e:
            return True, f"被拒: {e}"

    ok, note = wfail()
    check("ro-write-rejected", ok, note)

    listing = [f.get_longname() for f in conn.listPath(SHARE, "\\*")]
    check("ro-listDir", "big.bin" in listing, f"entries={listing}")

fails = [r for r in results if r[1] == "FAIL"]
print(f"== impacket {MODE}: {len(results) - len(fails)}/{len(results)} PASS ==")
sys.exit(1 if fails else 0)
```

## 附录 B：四份场景配置（本轮实际使用，存于 /tmp/opencode/qac-24613/）

<details><summary>cfg-a-auth-rw.yaml（认证读写, :4454）</summary>

```yaml
server:
  name: QACLIENTS
listen:
  addresses: [127.0.0.1]
  port: 4454
mdns: { enabled: false }
log: { level: info }
auth:
  users:
    - name: qauser
      password: "QaPassw0rd!"
shares:
  - name: rwshare
    path: /tmp/opencode/qac-24613/rwroot
    read_only: false
    guest_ok: false
```
</details>

<details><summary>cfg-b-guest.yaml（guest 开放, :4455）</summary>

```yaml
server:
  name: QAGUEST
listen:
  addresses: [127.0.0.1]
  port: 4455
mdns: { enabled: false }
auth:
  allow_guest: true
shares:
  - name: guestshare
    path: /tmp/opencode/qac-24613/guestroot
    read_only: false
    guest_ok: true
```
</details>

<details><summary>cfg-c-readonly.yaml（只读, :4454）</summary>

```yaml
server:
  name: QARO
listen:
  addresses: [127.0.0.1]
  port: 4454
mdns: { enabled: false }
auth:
  users:
    - name: qauser
      password: "QaPassw0rd!"
shares:
  - name: roshare
    path: /tmp/opencode/qac-24613/roroot
    read_only: true
    guest_ok: false
```
</details>

<details><summary>cfg-d-portable.yaml（portable, :4455；OI-1 第二进程同款）＋ cfg-e-portable-4454.yaml（仅 port 改 4454）</summary>

```yaml
filesystem_mode: portable
server:
  name: QACLIENTS
listen:
  addresses: [127.0.0.1]
  port: 4455        # cfg-e 为 4454
mdns: { enabled: false }
auth:
  users:
    - name: qauser
      password: "QaPassw0rd!"
shares:
  - name: rwshare
    path: /tmp/opencode/qac-24613/rwroot
    read_only: false
    guest_ok: false
```
</details>
