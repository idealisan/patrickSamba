---
name: 在共享工作树里验证自己代码的三个技巧
description: go test -overlay 绕开队友的编译红灯；用 overlay 做变异测试证明回归测试真的有效；smbclient 会客户端侧规范化 .. 所以验不出路径穿越
type: project
---

8 个 agent 共用 `/workspace` 一个工作树时，「验证自己的改动」本身就是个难题。
三个已实测有效的技巧：

## 1. 别人的文件编译不过时，用 `go test -overlay` 绕开，不要干等

真实场景：`internal/smb/command` 包里 apple 的 `query_directory_test.go` 与 `create.go`
连续十几分钟编译不过，导致**整个包所有人的测试都跑不了**。

`go test -overlay=<json>` 可以在**不碰工作树**的前提下临时替换/新增文件：

```sh
printf 'package command\n\nconst missingSymbol = 80\n' > /tmp/stub_test.go
printf '{"Replace":{"/workspace/internal/smb/command/zz_stub_test.go":"/tmp/stub_test.go"}}\n' > /tmp/overlay.json
go test -count=1 -overlay=/tmp/overlay.json ./internal/smb/command/ -run 'MyTest' -v
```

`Replace` 的 key 可以是**不存在的路径**（等于新增文件），也可以是现有文件（等于替换）。
补一个缺失符号往往比整份替换更省事。

⚠️ **反噬**：我给缺失常量瞎填了个值，结果 AAPL 的解析用例失败，差点当成 apple 的 bug 报出去。
用 overlay 时只信**自己那几条用例**的结果，别人的失败一律先怀疑是自己 stub 的锅。

## 2. 回归测试必须在**坏代码**上失败过，才算数

只在修好的代码上跑绿证明不了任何事。用同一个 overlay 手法把被修的函数换回旧实现，
确认测试**如期爆红**，再换回来：

```sh
# 用 python 改出一份有 bug 的 lock.go 到 /tmp，overlay 进去跑
go test -overlay=/tmp/overlay_buggy.json ./internal/smb/command/ -run 'ReleaseAll'
```

实测价值：`TestReleaseAllSurvivesRename` 就是这样确认它真能抓住锁泄漏的。

## 3. `smbclient` 验不出路径穿越 —— 它在客户端侧就把 `..` 规范化掉了

```sh
smbclient //127.0.0.1/share -c 'rename t.txt ..\escaped.txt'
```
服务端收到的是 `\escaped.txt`，**`..` 根本没上网线**，文件老老实实落在共享里。
看起来"防住了"，其实服务端压根没被考验过。

**结论：防穿越只能靠单元测试直接喂原始字符串给那个规范化函数**
（本项目是 `command.renameTarget` → `vfs.CleanPath`）。
真实攻击者是自己构造报文的，不会用规矩的 smbclient。

同理，`smbclient` 的 `lock`/`unlock` 是 **POSIX 锁**（unix extensions），
不走 SMB2 LOCK，验不了 `internal/smb/command/lock.go`。
