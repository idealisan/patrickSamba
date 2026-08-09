# CNB 制品库（Docker 镜像）

stupidSamba 的容器镜像托管在 **CNB 自己的制品库**上，不是 Docker Hub。

本文说明镜像地址、鉴权方式、怎么拉、怎么发，以及几条实测出来的注意事项。
文中标 **[实测]** 的结论都在本项目的开发容器里真跑过；标 **[未验证]** 的是
环境限制没法验的，不要当成已确认的事实。

> 镜像的**构建与推送**由 `scripts/docker-build.sh` 负责（取代了早期方案
> `scripts/publish-image.sh`）。本文只覆盖制品库这一侧与 `docker-build.sh` 的用法，
> 不重复脚本内部实现。

---

## 1. 镜像地址

```
docker.cnb.cool/finalappstore/stupidsamba:<版本>
```

例：

```
docker.cnb.cool/finalappstore/stupidsamba:v0.2.0
```

地址由三段拼成，都有对应的流水线环境变量：

| 段 | 值 | 环境变量 |
|---|---|---|
| Registry | `docker.cnb.cool` | `$CNB_DOCKER_REGISTRY` |
| 仓库路径 | `finalappstore/stupidsamba` | `$CNB_REPO_SLUG_LOWERCASE` |
| 标签 | `v0.2.0` | 与 git tag 同名 |

> **必须用 `$CNB_REPO_SLUG_LOWERCASE`，不能用 `$CNB_REPO_SLUG`。**
> 后者是驼峰的（`finalappstore/stupidSamba`），而 Docker 的 repository 名
> 不允许大写字母，直接拿它拼会在 push 的最后一刻报
> `invalid reference format: repository name must be lowercase`。
> `scripts/docker-build.sh` 里有一道前置检查专门拦这个（镜像名含大写字母直接 `die`）。

CNB 的制品路径有两种规则（[官方文档](https://docs.cnb.cool/zh/artifact/docker.md)）：

- **同名制品**：`docker.cnb.cool/<仓库路径>` —— 本项目用这个，地址最短。
- **非同名制品**：`docker.cnb.cool/<仓库路径>/<镜像名>` —— 一个仓库要出多个镜像时才用。

---

## 2. 鉴权

### 登录

```sh
docker login docker.cnb.cool -u cnb -p <访问令牌>
```

两个容易搞错的点：

- **用户名是固定字面量 `cnb`**，不是你的账号名，也不是仓库名。
  （流水线里对应 `$CNB_TOKEN_USER_NAME`，值就是 `cnb`。）
- **密码是 CNB 访问令牌**，不是账号密码。令牌在流水线里是 `$CNB_TOKEN`；
  本地使用需要自己[创建访问令牌](https://docs.cnb.cool/zh/artifact/intro.html#创建访问令牌)。

脚本里请用 `--password-stdin`，不要用 `-p`：

```sh
printf '%s' "$CNB_TOKEN" | docker login docker.cnb.cool -u cnb --password-stdin
```

`-p <token>` 会让令牌进入进程命令行，`ps` 就能看见，CI 日志里也容易被 echo 出来。

### 私密性

**本仓库是私密仓库，镜像同样是私密的。[实测]**

用一份干净的、没有登录态的 docker 配置去匿名拉取：

```sh
DOCKER_CONFIG=$(mktemp -d) docker pull docker.cnb.cool/finalappstore/stupidsamba:v0.2.0
```

结果：

```
pull access denied, repository does not exist or may require authorization:
authorization failed: no basic auth credentials
```

也就是说**必须先 `docker login` 才能拉**。把镜像地址发给没有本仓库权限的人，
对方拉不到。仓库若日后转为公开，镜像的可见性预计随之改变，但这一点
**[未验证]**，届时请重新用上面这条匿名拉取命令实测确认，不要假设。

---

## 3. 拉取与运行

```sh
docker login docker.cnb.cool -u cnb -p <访问令牌>
docker pull docker.cnb.cool/finalappstore/stupidsamba:v0.2.0
```

镜像是**多架构**的，`docker pull` 会自动挑当前平台，无需指定：

| 平台 | 说明 |
|---|---|
| `linux/amd64` | x86_64 服务器、大多数 PC |
| `linux/arm64` | 树莓派 4/5、多数 ARM NAS、Apple Silicon 上的 Linux 容器 |

要显式指定（比如在 amd64 机器上检查 arm64 那份）：

```sh
docker pull --platform linux/arm64 docker.cnb.cool/finalappstore/stupidsamba:v0.2.0
```

运行方式与端口映射见 README 的 Docker 章节。

---

## 4. 发布镜像（scripts/docker-build.sh）

镜像的构建与推送统一由 `scripts/docker-build.sh` 完成。它自己**不编译二进制**——
二进制来自 `scripts/build-release.sh` 产出的发布包（见 §4 末尾），Dockerfile 是
`COPY`-only，镜像里那份二进制就是裸包里的同一份字节。

```sh
# 完整流程：先 build-release.sh 出四平台裸包，再出多架构镜像并推送
sh scripts/docker-build.sh --tag v0.2.0 --push

# 裸包已经构建好了，跳过 build-release.sh，只出镜像
sh scripts/docker-build.sh --tag v0.2.0 --skip-build --push

# 额外打 latest（默认不打，见 §6）
sh scripts/docker-build.sh --tag v0.2.0 --push --latest

# 只构建到本地、不推送（调试用）
sh scripts/docker-build.sh --tag v0.2.0 --skip-build
```

常用参数：

| 参数 | 含义 |
|---|---|
| `--tag <版本>` | 镜像标签，也作为版本号来源。省略时从 `dist/` 里的发布包名推断。 |
| `--push` | 构建后推送到 `$CNB_DOCKER_REGISTRY/$CNB_REPO_SLUG_LOWERCASE`。不带则只留本地。 |
| `--skip-build` | 复用 `dist/*.tar.gz`，不再跑 `build-release.sh`。 |
| `--latest` | 额外打一个 `:latest` 标签。默认不打。 |
| `--image <名>` | 覆盖镜像名（默认取自上面的 Registry + 仓库路径）。 |
| `--platforms <列表>` | 覆盖目标平台，默认 `linux/amd64,linux/arm64`（调试单架构用）。 |

需要环境变量 `$CNB_TOKEN`（推送时）。脚本任何路径都不会打印它。

### 二进制从哪来（为什么 Dockerfile 不编译）

`docker-build.sh` 先把 `build-release.sh` 产出的
`dist/stupidsamba_<版本>_linux_{amd64,arm64}.tar.gz` 解出来，摆进
`dist/docker/linux_<arch>/stupidsamba`，再交给 `docker buildx` 用
`Dockerfile`（`COPY` 进镜像）。**Dockerfile 里一条 `RUN` 都没有**，因此：

- 多架构构建不需要 QEMU 模拟（目标 rootfs 里没有任何指令被执行），出双架构 manifest
  list 是纯文件搬运，实测约 0.5 秒；
- 镜像里的二进制就是 `build-release.sh` 那份，而 `build-release.sh` 对每个产物做了
  五项自检（CGO 关没关、`-trimpath` 生效没、GOOS/GOARCH 对不对、版本号有没有真注入、
  linux 产物是不是静态链接）。**绝不在 Dockerfile 里 `RUN go build` 重编**——那会让
  镜像里的二进制变成整个发布物里唯一没被自检过的东西，且与裸包不是同一份。

版本号/commit/构建时间由 `build-release.sh` 通过 `-ldflags` 注入二进制，再由
`docker-build.sh` 作为 `--build-arg VERSION/COMMIT/BUILD_DATE` 传给 Dockerfile
仅用于写 OCI label（见 §8），**不参与任何编译**。

---

## 5. 回读校验（为什么这条链路不能省）

本项目有一条反复付出代价的教训：**「成功回显」不等于事情真的发生了。**
已经发生过好几起同形态事故——推错 refspec 却打印成功、CI 从未真正跑完却全绿、
`build tag` 后的代码从未被编译过。所以「能 push 成功」只是最低门槛，下面每一层都
有独立的断言，任何一层失败都会让整条发布非零退出：

| 层 | 断言 | 由谁负责 |
|---|---|---|
| 二进制自检 | CGO=0、`-trimpath`、GOOS/GOARCH、版本注入、静态链接 | `build-release.sh`（每项不通过直接退出） |
| 镜像构建后自检 | manifest list 含请求的**每个**平台；且一个**未构建**的架构（如 `linux/s390x`）解析探针必须失败 | `docker-build.sh`（用 `docker create --platform` 探针 + s390x 反向对照） |
| 镜像能跑 | 在宿主架构上真的把容器跑起来，`-version` 输出含正确版本号 | `docker-build.sh`（`docker run --rm ... -version`） |
| 端到端 SMB | 真实客户端（smbclient + impacket）连得通、读写往返、共享不存在被拒、卷持久化等 8 项 | `scripts/verify-image.sh`（从制品库或本地镜像起容器跑） |

要点：

- **push 成功不代表 manifest list 完整。** `docker-build.sh` 的探针用
  `docker create --platform <p>` 必须真的在 manifest 里解析出 `<p>` 才能建容器，
  解析不到就失败；并以此反向对照一个未构建的架构也必须失败，否则探针本身不可信。
- **`docker image inspect` 救不了多架构核对**——面对 manifest list 它只回落到宿主
  那一份，看不见另一个架构。所以必须用 `docker create --platform` 或
  `docker buildx imagetools inspect` 逐平台查。
- **端到端验证要拉真实的镜像，不要用 buildx 本地缓存里那份。** 本地缓存和远端制品库
  可能是两份不同的东西（缓存没更新、推送半途失败等）。`verify-image.sh` 默认针对
  一个给定的 `--image` 跑，金丝雀发布时必须传 `--image docker.cnb.cool/...:<版本>`
  从远端拉。

---

## 6. 标签策略

**默认只打版本号标签，不打 `latest`。** 想打必须显式加 `--latest`，
`docker-build.sh` 才会额外打一个 `:latest`。

原因：`latest` 的语义是「最新版」。给历史版本回填镜像时（比如事后给某个旧 tag 补镜像），
如果顺手打上 `latest`，所有不写标签的 `docker pull …/stupidsamba` 都会拿到**旧版本**，
而且下次发新版还得把它覆盖回来，中间那段窗口期是错的。

约定：

- 每个发布版本 → `v<语义化版本号>`，例如 `v0.1.0`、`v0.2.0`。与 git tag、
  CNB Release 的命名保持一致（都带 `v` 前缀）。
- **不提供**不带 `v` 前缀的别名（如 `0.1.0`）。两套写法并存只会让人猜哪个是对的。
- `latest` 只在发布**当前最新正式版**时打，由常态化发布流程负责，
  历史版本回填一律不碰。

---

## 7. 关掉 provenance / SBOM（已由 PR #144 修复）

> **当前状态：[已修复]** —— 见 PR #144：`docker-build.sh` 显式 `--provenance=false --sbom=false`，
> `verify-image.sh` 增加 `manifest-clean` 用例并带 rc0（未修，FAIL）/ rc0-provfix（已修，PASS）/
> 本地独有 tag（SKIP，不 PASS）三条反向对照，skip 路径假绿洞已堵。

`docker buildx build` 默认会往 manifest list 里额外塞两条 `platform: unknown/unknown`
的 attestation manifest（provenance / SBOM）。功能上无害，但本项目的目标用户是 NAS，
那边的 Docker 版本常年很旧，遇到 `unknown/unknown` 会有各种奇怪表现。发布物要的是
「干净、谁都能拉」，不是「元数据最全」。

attestation 造成的 `unknown/unknown` 平台条目**曾是已知缺口**，已由 PR #144 修复：
`docker-build.sh` 的 buildx 调用显式加 `--provenance=false --sbom=false`，
`verify-image.sh` 增加 `manifest-clean` 用例断言 manifest 的 platform 列不得出现
`unknown/unknown`，并有 rc0（未修，FAIL）/ rc0-provfix（已修，PASS）两端反向对照。

两端证据（基于同一份代码 commit `8b48f91` 构建，仅差 buildx 那两个参数）：
- `v0.2.0-rc0`（未传这两个参数）：`imagetools inspect` 含 2× `unknown/unknown`，
  `verify-image.sh` 退出码 1，manifest-clean FAIL；
- `v0.2.0-rc0-provfix`（补上参数重建）：manifest 仅 `linux/amd64`/`linux/arm64`，
  `verify-image.sh` 退出码 0，manifest-clean PASS。

---

## 8. 镜像 label

镜像带 OCI 标准 label，`docker image inspect` 可查（由 `Dockerfile` 的 `LABEL` 写入）：

| Label | 内容 |
|---|---|
| `org.opencontainers.image.title` | `stupidSamba` |
| `org.opencontainers.image.description` | 一句话简介 |
| `org.opencontainers.image.source` | 仓库地址 |
| `org.opencontainers.image.licenses` | `MIT` |
| `org.opencontainers.image.version` | 版本号，如 `v0.2.0` |
| `org.opencontainers.image.revision` | 完整 commit sha（由 `--build-arg COMMIT` 传入） |
| `org.opencontainers.image.created` | 构建时间（UTC，由 `--build-arg BUILD_DATE` 传入） |

> **为什么版本号和 commit sha 两个都记：**
> 仓库历史里曾有过已泄露的令牌，后续可能需要用
> `git fast-export | sed | git fast-import` 重写历史来清除它。
> **那会改掉所有 commit sha（含发版 tag 指向的那个）。**
> 届时 `revision` 这一栏会变成一个在仓库里找不到的悬空 sha，
> 而 `version` 记的 tag 名不受影响，仍然指得准。遇到对不上号的 sha，以 tag 名为准。

> 早期方案里还有 `cool.cnb.stupidsamba.source-ref` / `dockerfile-origin` 两个
> 自定义 label，用于记录「源码/ Dockerfile 取自哪个 ref」。切换到 `docker-build.sh`
> 后这两个 label **已不再写入**（Dockerfile 始终来自仓库根、源码来自当前 checkout），
> 故本文不再列出。

---

## 9. 配额与保留策略

CNB 官方文档中明确写出的限制只有：

- 单层最大 64 GB
- 单个镜像最大 64 层
- 制品元数据最大 64 KB
- 需要 Docker 20.10+ 客户端（不支持 Registry V1 API）

**官方文档中没有查到总容量配额，也没有查到自动过期/清理策略。[未验证]**
这里如实标注为「未查到」，而不是「没有限制」—— 二者不是一回事。
如果后续要长期堆积每个版本的多架构镜像，建议先向 CNB 确认配额，
再决定要不要定期清理旧标签。

---

## 10. 在流水线里推送

CNB **没有**推送镜像的专用内置任务或插件，官方写法就是加 `docker` service
之后直接跑 `docker buildx build --push`：

```yaml
$:
  tag_push:
    - runner:
        cpus: 4
      docker:
        image: golang:1.25
      # 出多架构镜像必须开 dind 并启用 rootlessBuildkitd：默认的 docker 驱动
      # 存不下 manifest list，只有 buildkitd 后端能一次出双架构并 --push。
      services:
        - name: docker
          options:
            rootlessBuildkitd:
              enabled: true
      stages:
        # ... 与 push 相同的门禁 ...
        - name: 构建四平台发布产物
          script: VERSION=$CNB_BRANCH sh scripts/build-release.sh
        - name: 构建并推送多架构容器镜像
          script: |
            set -e
            # CNB 自动注入了 registry 凭据；这里再显式登一次是兜底，
            # 用 --password-stdin 避免令牌进进程 argv / CI 日志。
            printf '%s' "$CNB_TOKEN" | docker login -u "$CNB_TOKEN_USER_NAME" --password-stdin "$CNB_DOCKER_REGISTRY"
            sh scripts/docker-build.sh --tag "$CNB_BRANCH" --push
        - name: 创建 Release（预发布）
          type: git:release
          options:
            preRelease: true
            latest: false
        # ... 上传四平台产物到 Release ...
```

要点：

- 流水线里 `$CNB_BRANCH` 在 `tag_push` 事件下就是 **tag 名**（不是分支名），直接喂给
  `docker-build.sh --tag`，所以镜像标签与 git tag 天然一致。
- 多架构走 `docker buildx build --platform linux/amd64,linux/arm64 --push`，
  需要 **rootlessBuildkitd** service（默认的 docker 驱动存不下 manifest list）。
- CNB 的构建环境预置了对自家 registry 的凭据，理论上无需显式 `docker login`；
  上例仍显式登一次是防御性的（万一自动登录失效，失败点会明确停在这一行，
  而不是拖到 `--push` 最后一秒才报 denied 让人误以为是构建问题）。
- 镜像 stage 刻意排在 `git:release` **之前**：镜像出不来就不建 Release，
  避免发出「只有裸包、没有镜像」的半成品版本。

---

## 参考

- [CNB Docker 制品库](https://docs.cnb.cool/zh/artifact/docker.md)
- [CNB 创建访问令牌](https://docs.cnb.cool/zh/artifact/intro.html#创建访问令牌)
- 本仓库 `scripts/docker-build.sh`（镜像构建与推送）
- 本仓库 `scripts/build-release.sh`（裸二进制包的构建方式，镜像与之对齐）
- 本仓库 `scripts/verify-image.sh`（端到端 SMB 验收，8 项可证伪校验）
- 本仓库 `Dockerfile`（COPY-only，镜像二进制来自发布包）
- 本仓库 `.cnb.yml`（`tag_push` 事件的发布流水线）
