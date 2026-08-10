# v0.2.0 发布资产与镜像完整性 —— 独立核验报告

- **角色**: release-eng（发布资产与镜像完整性）
- **性质**: 独立验证，**不重新发布**（未跑 `publish-release.sh`、未重推 tag、未触发 tag_push）
- **核验时间**: 2026-08-10 10:22–10:45 CST
- **核验主机**: linux/amd64（go1.25.0），`file`/`qemu` 不在场，故用 `readelf`/`objdump`/字节魔数/`go version -m` 交叉判定
- **仓库**: finalappstore/stupidSamba　**tag**: v0.2.0　**期望 commit**: ccc4302
- **数据来源**: `cnb releases get-releases-asset ... --share true`（CLI 落盘到 /tmp/cnb-api/）；
  `docker.cnb.cool/finalappstore/stupidsamba:latest`
- **说明**: 任务书写「4 个 tar.gz」，实测 Windows 资产是 `.zip`（与 CHANGELOG「Windows 为 .zip」一致）。
  故平台资产为 **3 个 tar.gz + 1 个 zip**，连同 SHA256SUMS 共 **5 个资产**。

---

## 1. Release 资产清单（5 个）

| 资产 | 期望 | 实际 | 判定 |
|---|---|---|---|
| SHA256SUMS | 存在，424 B | 存在，424 B | **PASS** |
| stupidsamba_v0.2.0_linux_amd64.tar.gz | 存在，2266776 B | 2266776 B | **PASS** |
| stupidsamba_v0.2.0_linux_arm64.tar.gz | 存在，2061912 B | 2061912 B | **PASS** |
| stupidsamba_v0.2.0_darwin_arm64.tar.gz | 存在，2125389 B | 2125389 B | **PASS** |
| stupidsamba_v0.2.0_windows_amd64.zip | 存在，2385348 B | 2385348 B | **PASS** |

所有资产大小与 Release 元数据 `size` 字段逐一相符。

---

## 2. SHA256 完整性校验（`sha256sum -c SHA256SUMS`，rc=0）

| 资产 | 期望 sha256（来自 SHA256SUMS） | 实际 | 判定 |
|---|---|---|---|
| darwin_arm64.tar.gz | 1ed2f5b2…36406c | 匹配 | **PASS** |
| linux_amd64.tar.gz | 67f8cf20…48ff39 | 匹配 | **PASS** |
| linux_arm64.tar.gz | 4849566f…df847d | 匹配 | **PASS** |
| windows_amd64.zip | 0896491c…86e7ef | 匹配 | **PASS** |

`sha256sum -c` 全部输出「成功」，退出码 0。SHA256SUMS 本身 424 B，与元数据一致。

---

## 3. 各平台二进制核验

每个归档解包后含 `stupidsamba(.exe)` + `README.md` + `CHANGELOG.md` + `configs/example.yaml`（与 CHANGELOG 描述一致）。

**构建信息（`go version -b/-m`，四平台全部一致）**：`go1.25.12`、`-trimpath=true`、
**`CGO_ENABLED=0`**、`vcs.revision=ccc4302987242781a10b2611aa41f980c04ba3a7`（短 = **ccc4302**，与期望完全相符）。

| 平台 | 版本/commit | 文件格式·架构 | 静态链接 | 判定 |
|---|---|---|---|---|
| linux/amd64 | **实跑** `-version`: `v0.2.0 (commit ccc4302, built 2026-08-10T01:40:04Z, linux/amd64, go1.25.12)` | ELF64, e_machine=0x3E (x86-64) | `ldd`=不是动态可执行文件；无 dynamic section | **PASS** |
| linux/arm64 | buildinfo: v0.2.0 / ccc4302（本机非 arm64，无法执行；qemu 不在场） | ELF64, e_machine=0xB7 (AArch64) | `ldd`=不是动态可执行文件；无 dynamic section | **PASS** |
| darwin/arm64 | buildinfo: v0.2.0 / ccc4302（无法执行） | Mach-O 64（magic cf fa ed fe），cputype=0x0100000C (arm64) | CGO_ENABLED=0（`ldd`/`objdump` 不解析 Mach-O，见备注） | **PASS\*** |
| windows/amd64 | buildinfo: v0.2.0 / ccc4302（无法执行） | PE（magic `MZ`），GOOS=windows GOARCH=amd64 | CGO_ENABLED=0（PE 只链 kernel32/ntdll 等平台 ABI，符合 C9） | **PASS\*** |

- 版本注入核验：linux/amd64 直接执行 `-version` 得到 v0.2.0 + commit ccc4302；其余三平台
  由 `go version -m` 的 `vcs.revision`（ccc4302…）+ 二进制内 `ccc4302` 出现 3 次佐证。
- **PASS\* 备注**：本机无法运行 darwin/windows 二进制，也无 `file`/qemu。darwin(Mach-O) 与
  windows(PE) 的 `ldd` 输出对这两类格式**无意义**（工具不解析），因此静态性判定依据是
  **`CGO_ENABLED=0`**（无 CGO、无第三方动态库依赖，满足 C2 语义）。C2 的 `ldd`
  「not a dynamic executable」这条**在两个 linux 目标上已直接实测通过**。
  darwin 二进制按平台惯例仍会链 libSystem（平台 ABI，非第三方库，符合 C9）。

---

## 4. Docker 镜像核验（`docker.cnb.cool/finalappstore/stupidsamba:latest`）

**多架构 manifest（`docker buildx imagetools inspect`）**

| 项 | 值 | 判定 |
|---|---|---|
| MediaType | `application/vnd.oci.image.index.v1+json`（OCI 镜像索引） | PASS |
| 含 linux/amd64 | manifest `sha256:b8a8a9b8…23f6ba` | **PASS** |
| 含 linux/arm64 | manifest `sha256:325b8ef9…2bb7b0` | **PASS** |
| `:latest` 索引 digest | `sha256:2e7c8fad…55d784` | — |
| `:v0.2.0` 索引 digest | `sha256:2e7c8fad…55d784` | **PASS（与 :latest 逐位相同 → latest==v0.2.0）** |

**镜像内二进制（从 `/usr/local/bin/stupidsamba` 取出）**

| 项 | amd64 | arm64 | 判定 |
|---|---|---|---|
| `-version`（amd64 实跑） | `v0.2.0 (commit ccc4302, built 2026-08-10T01:40:04Z, linux/amd64, go1.25.12)` | — | PASS |
| `ldd` / dynamic section | 不是动态可执行文件 / 无 dynamic section | — | **PASS（scratch 基础，静态）** |
| buildinfo | CGO_ENABLED=0, trimpath, go1.25.12, rev ccc4302 | — | PASS |
| **与裸包同源（逐位 sha256）** | image==tar.gz: `ef726b04…5bb1ec` | image==tar.gz: `434b8614…74f0a2` | **PASS（两架构均字节相同）** |

**正向对照（smbclient，非强制项，已做）**：容器以内置试用配置启动（guest 开、share `public`、
mDNS 关），日志打印「SMB 服务已监听 dialects=2.0.2..3.1.1」。`smbclient -m SMB3` 匿名：
- `-L` 列出共享 `public` + `IPC$` → **握手/协商 PASS**
- put + get 往返，字节 `diff` 一致 → **读写往返 PASS**

---

## 5. 总体结论

| 核验项 | 结论 |
|---|---|
| 5 个资产是否完整 | **是**（SHA256SUMS + 3 tar.gz + 1 zip，大小全部与元数据相符） |
| 完整性（sha256sum -c） | **全部 PASS**（rc=0） |
| 二进制版本与 commit | **全部为 v0.2.0 / commit ccc4302**（与期望一致） |
| 静态链接（C2） | linux amd64/arm64 **实测 not a dynamic executable**；四平台 **CGO_ENABLED=0**；darwin/windows 因格式无法用 ldd 判定，依 CGO=0 认定符合 C2 语义 |
| 镜像是否同源 | **是**：`:latest` 与 `:v0.2.0` 索引 digest 逐位相同；镜像 amd64/arm64 二进制与对应裸包 tar.gz **逐字节 sha256 相同**；含 linux/amd64+arm64 双架构 |
| 功能正向对照 | 镜像可服务 SMB3，匿名列共享 + 读写往返均成功 |

**判定：v0.2.0 发布资产与镜像通过独立核验，未发现完整性/版本/架构/同源性问题。**

唯一与任务书面表述的出入：Windows 资产为 `.zip` 而非 `.tar.gz`（属预期，CHANGELOG 已载明）。

### 附：关键指纹
- 索引 digest（:latest = :v0.2.0）: `sha256:2e7c8fade9fab98b9a11e1705e8c79f3b35d7da50132797c6bc0798fb155d784`
- linux/amd64 二进制 sha256（镜像==裸包）: `ef726b047750a55607ca6a1fe0d40fe2f95480666e5574fa57b027bf215bb1ec`
- linux/arm64 二进制 sha256（镜像==裸包）: `434b8614d3cbc4e5c7bc8108cf56ee54bbe7a7d4655f66b85eca9fade674f0a2`
- vcs.revision（四平台一致）: `ccc4302987242781a10b2611aa41f980c04ba3a7`
