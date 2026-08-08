#!/usr/bin/env python3
"""impacket 客户端验收测试（AGENTS.md §3 客户端矩阵第 3 项）。

impacket 是与 Samba 完全独立的 Python SMB 实现，用它测试能发现
"只对着 Samba 客户端调通" 的实现偏差。

用法: impacket_test.py <host> <port> <user> <pass> <share> <workdir>
"""
import io
import sys
import traceback

from impacket.smbconnection import SMBConnection


def main() -> int:
    host, port, user, password, share, workdir = sys.argv[1:7]
    port = int(port)

    conn = SMBConnection(host, host, sess_port=port)
    conn.login(user, password)
    print(f"  已登录, dialect={conn.getDialect():#06x}")

    # 1) 列目录
    names = [f.get_longname() for f in conn.listPath(share, "\\*")]
    print(f"  listPath: {names}")
    assert "hello.txt" in names, "目录列表里没有 hello.txt"
    assert "." in names and ".." in names, "目录列表缺少 . 或 .. 条目"

    # 2) 读文件
    buf = io.BytesIO()
    conn.getFile(share, "hello.txt", buf.write)
    body = buf.getvalue().decode()
    print(f"  getFile hello.txt -> {body!r}")
    assert "hello from stupidsamba" in body, "文件内容不匹配"

    # 3) 写文件
    payload = b"written by impacket\n" * 100
    conn.putFile(share, "impacket.txt", io.BytesIO(payload).read)
    back = io.BytesIO()
    conn.getFile(share, "impacket.txt", back.write)
    assert back.getvalue() == payload, "回读内容与写入不一致"
    print(f"  putFile/getFile round-trip {len(payload)} 字节 OK")

    # 4) 大文件读（跨多个 READ 请求，验证分块与 credit）
    big = io.BytesIO()
    conn.getFile(share, "blob.bin", big.write)
    assert big.tell() == 1048576, f"blob.bin 大小应为 1048576, 实际 {big.tell()}"
    print("  1MiB 大文件读取 OK")

    # 5) 子目录
    sub = [f.get_longname() for f in conn.listPath(share, "\\subdir\\*")]
    assert "nested.txt" in sub, "子目录列表里没有 nested.txt"
    print(f"  子目录枚举 OK: {sub}")

    # 6) 通配符过滤
    txt = [f.get_longname() for f in conn.listPath(share, "\\*.txt")]
    assert "hello.txt" in txt, "通配符 *.txt 没匹配到 hello.txt"
    assert "blob.bin" not in txt, "通配符 *.txt 错误匹配了 blob.bin"
    print(f"  通配符匹配 OK: {txt}")

    # 7) 创建/删除目录
    conn.createDirectory(share, "impacket_dir")
    assert "impacket_dir" in [f.get_longname() for f in conn.listPath(share, "\\*")]
    conn.deleteDirectory(share, "impacket_dir")
    print("  创建/删除目录 OK")

    # 8) 重命名（走 SMB2 FILE_RENAME_INFO，impacket 高层 rename 内部用 setInfo 实现）
    conn.rename(share, "impacket.txt", "impacket_renamed.txt")
    after = [f.get_longname() for f in conn.listPath(share, "\\*")]
    assert "impacket_renamed.txt" in after, "重命名后找不到 impacket_renamed.txt"
    assert "impacket.txt" not in after, "重命名后旧名仍存在"
    print("  重命名 OK")

    # 9) 删除文件
    conn.deleteFile(share, "impacket_renamed.txt")
    assert "impacket_renamed.txt" not in [f.get_longname() for f in conn.listPath(share, "\\*")]
    print("  删除文件 OK")

    conn.logoff()
    conn.close()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        traceback.print_exc()
        sys.exit(1)
