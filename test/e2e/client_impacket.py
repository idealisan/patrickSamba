#!/usr/bin/env python3
"""冒烟客户端 2/3：impacket（Python 独立协议栈，AGENTS.md §3 矩阵第 3 项）。

用法: client_impacket.py <host> <port> <user> <pass> <share> <workdir> <前缀>
退出码: 0 成功 / 1 失败 / 77 环境缺 impacket（驱动会记为 SKIP）

选它的价值：它与 Samba 完全无关，能发现「只对着 Samba 客户端调通」的实现偏差。

本脚本只负责**发操作**并对目录枚举做断言。文件内容与磁盘副作用的判据一律由
test/e2e/smoke.sh 在服务端磁盘上独立核对 —— 客户端说成功不算数。

安装：apt-get install python3-impacket（不要用 pip，会和已装的 cryptography 冲突）。
"""
import os
import sys
import traceback

try:
    from impacket.smbconnection import SMBConnection
except ImportError:
    sys.exit(77)


def main() -> int:
    host, port, user, password, share, workdir, p = sys.argv[1:8]
    port = int(port)
    failed = False

    conn = SMBConnection(host, host, sess_port=port)
    conn.login(user, password)
    print(f"  [impacket] 已登录, dialect={conn.getDialect():#06x}")

    # --- 1. 目录枚举。磁盘看不出「客户端有没有看见」，只能在客户端断言。
    names = [f.get_longname() for f in conn.listPath(share, "\\*")]
    for want in (f"seed-{p}.bin", f"mov-{p}.bin", f"del-{p}.bin", f"rmd-{p}",
                 "subdir", ".", ".."):
        if want not in names:
            print(f"  [impacket] 目录列表里没有 {want}: {names}")
            failed = True
    # 反向断言：不存在的名字绝不能出现（防「把请求里的名字原样回显」的实现）。
    if f"nosuch-{p}.bin" in names:
        print(f"  [impacket] 目录列表里出现了根本不存在的 nosuch-{p}.bin")
        failed = True

    # --- 2. 下载。落到 workdir，由驱动做整字节比对。
    #     用二进制模式逐块写盘，不在内存里拼 —— 载荷 1 MiB+，也顺带验证分块。
    with open(os.path.join(workdir, f"got-{p}.bin"), "wb") as fh:
        conn.getFile(share, f"seed-{p}.bin", fh.write)

    # --- 3. 上传。上传出来的 up-*.bin 之后不再被任何操作碰，避免自毁证据。
    with open(os.path.join(workdir, f"up-{p}.bin"), "rb") as fh:
        conn.putFile(share, f"up-{p}.bin", fh.read)

    # --- 4. 建目录。
    conn.createDirectory(share, f"dir-{p}")

    # --- 5. 重命名（跨目录）。走 SMB2 SET_INFO / FileRenameInformation。
    #     mov-*.bin 由驱动预先放在共享目录里，不是这里 put 出来的。
    conn.rename(share, f"mov-{p}.bin", f"dir-{p}\\moved-{p}.bin")

    # --- 6. 删文件（对象由驱动预先放好）。
    conn.deleteFile(share, f"del-{p}.bin")

    # --- 7. 删目录（对象由驱动预先放好，是空目录）。
    conn.deleteDirectory(share, f"rmd-{p}")

    # close() 内部已经会发 LOGOFF，不要再单独调 logoff()：impacket 的 logoff()
    # 会把 SessionID 清零但保留加密开关，于是 close() 发出的第二个 LOGOFF 变成
    # 「SessionId=0 的加密帧」，服务端只能按 MS-SMB2 §3.3.5.2.1 断开连接。
    conn.close()
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        traceback.print_exc()
        sys.exit(1)
