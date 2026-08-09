#!/usr/bin/env python3
"""用 impacket 对方言协商做**精确断言**。

smbclient 的 `-m` 只能设客户端上限，验收里只能间接推断协商结果；
impacket 可以显式指定 preferredDialect，并把服务端真正选中的方言读回来，
所以用它来钉住 "要 2.0.2 就必须真给 2.0.2"。

顺带对每个方言都跑一次读操作 —— 只协商成功不算通过，
握手后 credit / 签名密钥推导错了照样会在第一个真实请求上炸。

用法: impacket_dialects.py <host> <port> <user> <pass> <share>
"""
import io
import sys
import traceback

from impacket.smb3structs import (
    SMB2_DIALECT_002,
    SMB2_DIALECT_21,
    SMB2_DIALECT_30,
    SMB2_DIALECT_311,
)
from impacket.smbconnection import SMBConnection

# 这里**没有 3.0.2**，是 impacket 客户端自身的限制，不是服务端不支持：
# impacket 0.12.0 的 smbconnection.py:137 把 preferredDialect 限死在
# [002, 21, 30, 311] 四个值里，传 0x0302 直接 `raise Exception("Unknown dialect %s")`，
# 连报文都发不出去。
# 3.0.2 的覆盖由 acceptance.sh 的 smbclient `-m SMB3_02` 承担，
# 并在那边通过服务端日志断言协商结果确实是 3.0.2。
DIALECTS = [
    ("2.0.2", SMB2_DIALECT_002),
    ("2.1", SMB2_DIALECT_21),
    ("3.0", SMB2_DIALECT_30),
    ("3.1.1", SMB2_DIALECT_311),
]


def main() -> int:
    host, port, user, password, share = sys.argv[1:6]
    port = int(port)

    failed = []
    for label, want in DIALECTS:
        try:
            conn = SMBConnection(host, host, sess_port=port, preferredDialect=want)
            conn.login(user, password)
            got = conn.getDialect()
            if got != want:
                failed.append(f"{label}: 协商结果 {got:#06x}, 期望 {want:#06x}")
                conn.close()
                continue

            # 协商对了还不够，真跑一次读
            buf = io.BytesIO()
            conn.getFile(share, "hello.txt", buf.write)
            if b"hello from stupidsamba" not in buf.getvalue():
                failed.append(f"{label}: 读回内容不匹配 {buf.getvalue()!r}")
            else:
                print(f"  impacket 方言 {label} ({got:#06x}): 协商 + 读取 OK")
            # 只调 close()：它内部已经会发 LOGOFF。
            # 不能写成 logoff() + close() —— impacket 的 logoff() 成功后会把
            # _Session['SessionID'] 清零却**保留加密开关**，close() 再发一次
            # LOGOFF 时就成了「SessionId=0 的加密帧」。服务端按 MS-SMB2
            # §3.3.5.2.1 断开连接是对的，但日志里会多一条吓人的 WARN。
            conn.close()
        except Exception as exc:  # noqa: BLE001 - 逐个方言隔离，一个挂了继续测下一个
            failed.append(f"{label}: {type(exc).__name__}: {exc}")

    if failed:
        print("  方言断言失败:")
        for f in failed:
            print(f"    - {f}")
        return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        traceback.print_exc()
        sys.exit(1)
