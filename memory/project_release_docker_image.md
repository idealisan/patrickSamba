---
name: 发布物形态：裸包 + Docker 镜像双轨
description: 项目所有者 2026-08-09 要求，从 v0.2.0 起每次发布同时产出四平台裸二进制包和 Docker 镜像，两者都要挂到 Release
type: project
---

从 v0.2.0 起，**每个版本必须同时发布「裸二进制包」和「Docker 镜像」两种形态**，不是二选一。

**Why:** 项目所有者 2026-08-09 明确要求（原话：「以后的版本，制作 docker 镜像，同时发布裸包和 docker 镜像」）。
stupidSamba 的卖点是「单个静态二进制开箱即用」（AGENTS.md C1/C2），裸包体现这一点；
而实际部署 SMB 服务的人大多在 NAS / 家庭服务器上跑容器，镜像是他们唯一会用的形态。
两者受众不同，砍掉任何一个都会丢掉一半用户。

**How to apply:**
- `.cnb.yml` 的 `tag_push:` 段现在只有「构建四平台产物 → git:release → attachments 上传」三步，
  需要再加镜像构建与推送。改这段前先读该段顶部那几条已核实的事实（tag_push 下
  `$CNB_BRANCH` 就是 tag 名；`git:release` 必须在 attachments 之前；`overlying=false`
  意味着重跑流水线会先删后建 Release）。
- 镜像必须是多架构的（至少 linux/amd64 + linux/arm64），因为 NAS 大量是 arm64。
  C1 禁 CGO 反而让这件事很简单：`scratch` 或 `distroless` 基础镜像即可，不需要交叉编译工具链。
- SMB 默认端口 445 在容器里要能映射；镜像默认配置不能依赖宿主机上预先存在的用户
  （C8 本来就要求认证自成体系，这一点天然满足）。
- 发布清单（`test/e2e/regression.md` 一类的发布前回归）要把「镜像能起、能被三种客户端连上」
  也算进去 —— 只验裸包等于只验了一半的交付物。
