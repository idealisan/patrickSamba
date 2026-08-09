---
name: 校验循环必须自报「实际比对次数」，0 次要判红
description: 本容器默认 shell 是 zsh，`for f in $files` 不做换行分词，整个多行串当成一个元素，循环体条件恒假 → 一次都没比对却打印「零丢失」
type: feedback
---

**任何遍历式校验器都必须打印「实际比对了多少次」，并把 0 次当成失败。**

**Why（2026-08-09 22:00 亲身踩中）：** 给 PR #172 做记忆合流时写了个丢行审计器：

```sh
files=$(git ls-tree --name-only A memory/; git ls-tree --name-only B memory/)
for f in $files; do
    if git cat-file -e "$side:$f" 2>/dev/null; then ...比对... ; fi
done
echo "(以上为空 = 零丢失)"
```

它打印了「零丢失」，我差点据此提交。真相是**一次比对都没跑**：
本容器默认 shell 是 **zsh**，而 zsh 默认关闭 `SH_WORD_SPLIT`，
`for f in $files` **不按换行分词** —— 整个 45 行的多行字符串成了单个 `$f`，
`git cat-file -e "A:memory/x.md\nmemory/y.md\n…"` 必然失败，`if` 恒假，
循环体一行没执行，最后那句无条件 `echo` 照常输出「零丢失」。
加上 `n=$((n+1))` 计数后实测 **n=0**；改成 `while IFS= read -r f` 后 **n=77，当场报出 10 处丢行**，
其中一处是真丢失（我自己写的整节内容被对方版本覆盖）。

同一天同一族的另外四个变体，一起记着：
- oscap-wire 用 `grep -qxF "$l"` 逐行核对索引，每行以 `- [` 开头 → grep 当成选项报「无效的选项」，
  循环照跑，结论「7 行全缺失」（真值缺 1）。修法：`grep -qxF -- "$l"`。
- 我自己写 `grep -c 'block_rot'` 想统计 `feedback_doc_block_outside_rot.md` 的索引 ——
  后者并不含子串 `block_rot`，返回 0 被我误读成「索引被删了」。**判据问错了问题，不是结果错了。**
- pm 批量跑三对 PR 的两两可合性，`set -- $pair` 同样没分词，命令变成
  `merge-tree origin/"a b" origin/` → **三对全 rc=1**，差点广播「三个 PR 互相冲突」。
  拿不存在的分支做负向对照坐实：**也是 rc=1**。真值只有一对冲突。
  由此得出的通则：`merge-tree` 的非 0 同时表示「有冲突」和「你命令写错了」，
  **凡靠退出码下结论的批量扫描都要配一个已知结果的正向对照**（如 `main × main` → 期望 rc=0），
  先证明命令在这套参数下是通的，rc=1 才有意义。

**(c) zsh 内建 `echo` 默认展开反斜杠转义，会悄悄把数据改坏**（vfs-deflake 记录，pm 22:10 实测）：

```sh
r=$(curl -s ...); echo "$r" | python3 -c "import json,sys; d=json.load(sys.stdin) ..."
```

10 个请求全 `PARSE_FAIL`。第一反应是 token 过期（当时唯一的外部卡点，最容易信）。
但同一响应**落盘**再喂同一句 python → rc=0、http=200，反向对照当场打掉「token 过期」假设。
真因：JSON body 里的 `\n`、`\"` 这些**字面值转义序列**被 zsh 的 `echo` 展开成真换行/真引号，
JSON 当场破掉。与 (a) 同源 —— 都是 zsh 与 POSIX sh 语义分叉，**都不报错、只给一个与「真失败」同形的结果**；
区别是 (a) 让循环空转，(c) 把数据改坏再喂下去。
修法：管道传数据一律 `printf '%s' "$r"`，或干脆 `curl -o file` 落盘再解析。

这条还带一个更贵的教训：**怀疑「外部依赖坏了」时，先用一个已知好的输入跑同一条命令。**
这次若没做，会向 team-lead 报一条假的「CNB_TOKEN 已失效」，
而 token 轮换恰好是他手上的待决卡 —— **假情报会直接推动一个错误决策**。

**How to apply:**
- 遍历文件名一律 `while IFS= read -r f; do … done < 列表文件`，不要 `for f in $(...)`。
- **校验器要自报工作量**：`echo "实际比对次数: $n"`，`n=0` 视同失败。
  只打印「未发现问题」的校验器无法与「没跑」区分。
- 上线前用**已知会红的输入**跑一次反向对照（本次用「已知回退 54 行的 ref」当输入，
  确认检查器报 LOST 54 才敢信它的绿）。
- grep 逐行核对写 `grep -Fx -- "$line"`；统计前先确认 pattern 真是目标串的子串。
