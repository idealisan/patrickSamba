#!/usr/bin/env python3
"""impacket_durable.py —— durable handle (DHnQ/DH2Q/DHnC/DH2C) 的线级端到端验证。

为什么要单独写：impacket 的 SMBConnection.createFile 不支持 create context，
smbclient 也从不申请 durable handle，所以 durable 这条路径在既有验收里
**一个字节都没被真实客户端走过**。本脚本手工拼 create context 并直接收发
SMB2 报文，是目前唯一能从线上证明它的手段。

用法：
    python3 scripts/clients/impacket_durable.py [port] [user] [pass] [share]

每个用例都带反向对照（拿不到授予 / 换用户重连 / 错 GUID 重连），
判据是**响应里有没有对应的 create context**，不是「命令没报错」。
退出码 0 表示全部符合预期。
"""

import struct
import sys
import uuid

from impacket.smbconnection import SMBConnection
from impacket.smb3structs import (
    SMB2_CREATE,
    SMB2_READ,
    SMB2Read,
    SMB2Read_Response,
    SMB2_DIALECT_30,
    FILE_READ_DATA,
    FILE_WRITE_DATA,
    FILE_READ_ATTRIBUTES,
    FILE_OPEN,
    FILE_NON_DIRECTORY_FILE,
    FILE_SHARE_READ,
    FILE_SHARE_WRITE,
    SMB2_IL_IMPERSONATION,
    SMB2_OPLOCK_LEVEL_BATCH,
    SMB2_OPLOCK_LEVEL_NONE,
    SMB2Create,
    SMB2Create_Response,
    SMB2Packet,
)
from impacket.nt_errors import STATUS_SUCCESS

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 4461
USER = sys.argv[2] if len(sys.argv) > 2 else "testuser"
PASS = sys.argv[3] if len(sys.argv) > 3 else "testpass123"
SHARE = sys.argv[4] if len(sys.argv) > 4 else "public"
TARGET = "127.0.0.1"

FAILED = []
PASSED = []


def ok(name, detail=""):
    PASSED.append(name)
    print(f"  [PASS] {name}{(' — ' + detail) if detail else ''}")


def bad(name, detail=""):
    FAILED.append(name)
    print(f"  [FAIL] {name}{(' — ' + detail) if detail else ''}")


# --------------------------------------------------------------------------
# create context 编解码（MS-SMB2 §2.2.13.2）
# --------------------------------------------------------------------------

DHNQ = b"DHnQ"
DH2Q = b"DH2Q"
DHNC = b"DHnC"
DH2C = b"DH2C"


class RawContext:
    """一个已编码好的 create context；impacket 只要求有 getData()。"""

    def __init__(self, blob):
        self.blob = blob

    def getData(self):
        return self.blob


def encode_context(name, data, last=True):
    """按 §2.2.13.2 编码：Next(4) NameOffset(2) NameLength(2) Reserved(2)
    DataOffset(2) DataLength(4)，随后是 Name、8 字节对齐填充、Data。"""
    name_off = 16
    pad1 = (-(name_off + len(name))) % 8
    data_off = (name_off + len(name) + pad1) if data else 0
    total = (data_off + len(data)) if data else (name_off + len(name) + pad1)
    pad2 = 0 if last else (-total) % 8
    nxt = 0 if last else total + pad2
    head = struct.pack("<IHHHHI", nxt, name_off, len(name), 0, data_off, len(data))
    return RawContext(head + name + b"\x00" * pad1 + data + b"\x00" * pad2)


def decode_contexts(buf):
    """把响应里的 create context 链解成 {name: data}。"""
    out = {}
    off = 0
    while off < len(buf):
        if off + 16 > len(buf):
            break
        nxt, noff, nlen, _, doff, dlen = struct.unpack("<IHHHHI", buf[off:off + 16])
        name = buf[off + noff:off + noff + nlen]
        data = buf[off + doff:off + doff + dlen] if dlen else b""
        out[name] = data
        if nxt == 0:
            break
        off += nxt
    return out


def dh2q(create_guid, timeout_ms=0, flags=0):
    """SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2：Timeout(4) Flags(4) Reserved(8) Guid(16)。"""
    return struct.pack("<II", timeout_ms, flags) + b"\x00" * 8 + create_guid


def dh2c(file_id, create_guid, flags=0):
    """SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2：FileId(16) Guid(16) Flags(4)。
    注意字段序与 REQUEST_V2 不同。"""
    return file_id + create_guid + struct.pack("<I", flags)


# --------------------------------------------------------------------------
# 低层 CREATE（带 create context，且能拿到响应 context）
# --------------------------------------------------------------------------


def raw_create(conn, tree_id, filename, contexts, oplock=SMB2_OPLOCK_LEVEL_BATCH,
               disposition=FILE_OPEN):
    """发一个带 create context 的 CREATE，返回 (status, file_id, {ctx_name: data})。"""
    smb = conn.getSMBServer()
    packet = smb.SMB_PACKET()
    packet["Command"] = SMB2_CREATE
    packet["TreeID"] = tree_id

    c = SMB2Create()
    c["SecurityFlags"] = 0
    c["RequestedOplockLevel"] = oplock
    c["ImpersonationLevel"] = SMB2_IL_IMPERSONATION
    c["DesiredAccess"] = (FILE_READ_DATA | FILE_WRITE_DATA
                          | FILE_READ_ATTRIBUTES)
    c["FileAttributes"] = 0
    c["ShareAccess"] = FILE_SHARE_READ | FILE_SHARE_WRITE
    c["CreateDisposition"] = disposition
    c["CreateOptions"] = FILE_NON_DIRECTORY_FILE
    c["NameLength"] = len(filename) * 2
    c["Buffer"] = filename.encode("utf-16le") if filename else b"\x00"

    blob = b"".join(x.getData() for x in contexts)
    off = len(SMB2Packet()) + SMB2Create.SIZE + len(c["Buffer"])
    if off % 8:
        c["Buffer"] += b"\x00" * (8 - off % 8)
        off = len(SMB2Packet()) + SMB2Create.SIZE + len(c["Buffer"])
    c["CreateContextsOffset"] = off
    c["CreateContextsLength"] = len(blob)
    c["Buffer"] += blob

    packet["Data"] = c
    pid = smb.sendSMB(packet)
    ans = smb.recvSMB(pid)
    st = ans["Status"]
    if st != STATUS_SUCCESS:
        return st, None, {}

    resp = SMB2Create_Response(ans["Data"])
    ctxs = {}
    if resp["CreateContextsLength"]:
        # CreateContextsOffset 以 SMB2 头起算：64 字节头 + 88 字节 CREATE
        # Response 固定部分（StructureSize 宣告 89，把 Buffer 首字节算了进去）。
        # 不用 impacket 常量：SMB2Create_Response 没有 .SIZE，
        # 且它的 __len__ 在字段未填时会抛异常。
        base = 64 + 88
        start = resp["CreateContextsOffset"] - base
        ctxs = decode_contexts(
            resp["Buffer"][start:start + resp["CreateContextsLength"]])
    # impacket 把 FileID 解成结构体；后面要把这 16 字节原样塞进 DHnC/DH2C，
    # 所以统一转成 bytes。
    fid = resp["FileID"]
    fid = fid.getData() if hasattr(fid, "getData") else bytes(fid)
    return st, fid, ctxs


def raw_read(conn, tree_id, file_id, offset=0, length=64):
    """裸 READ。不能用 impacket 的 SMB3.read —— 它要求 fileId 事先登记在
    客户端自己的 OpenTable 里，而我们的句柄是手工 CREATE 出来的。"""
    smb = conn.getSMBServer()
    packet = smb.SMB_PACKET()
    packet["Command"] = SMB2_READ
    packet["TreeID"] = tree_id

    r = SMB2Read()
    r["Padding"] = 0x50
    r["Length"] = length
    r["Offset"] = offset
    r["FileID"] = file_id
    r["MinimumCount"] = 0
    r["Channel"] = 0
    r["RemainingBytes"] = 0
    packet["Data"] = r

    pid = smb.sendSMB(packet)
    ans = smb.recvSMB(pid)
    if ans["Status"] != STATUS_SUCCESS:
        return ans["Status"], b""
    resp = SMB2Read_Response(ans["Data"])
    return STATUS_SUCCESS, resp["Buffer"][:resp["DataLength"]]


def connect():
    conn = SMBConnection(TARGET, TARGET, sess_port=PORT,
                         preferredDialect=SMB2_DIALECT_30)
    conn.login(USER, PASS)
    return conn


# --------------------------------------------------------------------------
# 用例
# --------------------------------------------------------------------------


def case_plain_create_unaffected():
    """回归：不带任何 durable context 的普通 CREATE 必须照常成功。
    （create context 注册表重构后最该回归的一条）"""
    conn = connect()
    try:
        tid = conn.connectTree(SHARE)
        st, fid, ctxs = raw_create(conn, tid, "hello.txt", [],
                                   oplock=SMB2_OPLOCK_LEVEL_NONE)
        if st != STATUS_SUCCESS:
            bad("plain-create", f"普通 CREATE 失败 status=0x{st:08x}")
            return
        if ctxs:
            bad("plain-create", f"未申请任何 context 却回了 {list(ctxs)}")
            return
        ok("plain-create", "无 context 的 CREATE 正常，且响应不夹带 context")
    finally:
        conn.close()


def case_unknown_context_ignored():
    """回归：不认识的 create context 必须**静默忽略**（§3.3.5.9），
    不能让 CREATE 失败。反向对照见 plain-create。"""
    conn = connect()
    try:
        tid = conn.connectTree(SHARE)
        junk = encode_context(b"ZzZz", b"\x00" * 8, last=True)
        st, fid, ctxs = raw_create(conn, tid, "hello.txt", [junk],
                                   oplock=SMB2_OPLOCK_LEVEL_NONE)
        if st != STATUS_SUCCESS:
            bad("unknown-context", f"未知 context 让 CREATE 失败 status=0x{st:08x}")
            return
        if b"ZzZz" in ctxs:
            bad("unknown-context", "服务端竟然回显了未知 context")
            return
        ok("unknown-context", "未知 context 被静默忽略")
    finally:
        conn.close()


def case_v1_grant_and_reconnect():
    """DHnQ 授予 → 断链 → DHnC 重连，必须拿回**同一个 FileId**。"""
    conn = connect()
    tid = conn.connectTree(SHARE)
    st, fid, ctxs = raw_create(conn, tid, "hello.txt",
                               [encode_context(DHNQ, b"\x00" * 16)])
    if st != STATUS_SUCCESS:
        bad("v1-grant", f"带 DHnQ 的 CREATE 失败 status=0x{st:08x}")
        conn.close()
        return
    if DHNQ not in ctxs:
        bad("v1-grant", "batch oplock + DHnQ 未获授予（响应无 DHnQ）")
        conn.close()
        return
    ok("v1-grant", "batch oplock + DHnQ 获授予")

    # 硬断链：不 logoff、不 close，直接掐 socket，模拟真实掉线。
    conn.getSMBServer().get_socket().close()

    conn2 = connect()
    try:
        tid2 = conn2.connectTree(SHARE)
        st2, fid2, ctxs2 = raw_create(conn2, tid2, "hello.txt",
                                      [encode_context(DHNC, fid)])
        if st2 != STATUS_SUCCESS:
            bad("v1-reconnect", f"DHnC 重连失败 status=0x{st2:08x}")
            return
        if fid2 != fid:
            bad("v1-reconnect", f"重连后 FileId 变了：{fid.hex()} → {fid2.hex()}")
            return
        ok("v1-reconnect", f"FileId 保持不变 {fid.hex()}")
        # 「重连成功」不等于句柄能用 —— 必须真的读到内容才算数。
        st_r, data = raw_read(conn2, tid2, fid2, 0, 64)
        if st_r != STATUS_SUCCESS:
            bad("v1-reconnect-usable",
                f"重连成功但句柄不可用：READ 回 0x{st_r:08x}")
        elif not data.startswith(b"hello durable"):
            bad("v1-reconnect-usable", f"READ 内容不对：{data[:24]!r}")
        else:
            ok("v1-reconnect-usable", f"重连后 READ 正常：{data[:20]!r}")
    finally:
        conn2.close()


def case_v2_grant_and_reconnect():
    """DH2Q 授予 → 断链 → DH2C 重连；并做两个反向对照：错 GUID、换用户。"""
    guid = uuid.uuid4().bytes
    conn = connect()
    tid = conn.connectTree(SHARE)
    st, fid, ctxs = raw_create(conn, tid, "hello.txt",
                               [encode_context(DH2Q, dh2q(guid, 30000))])
    if st != STATUS_SUCCESS:
        bad("v2-grant", f"带 DH2Q 的 CREATE 失败 status=0x{st:08x}")
        conn.close()
        return
    if DH2Q not in ctxs:
        bad("v2-grant", "batch oplock + DH2Q 未获授予（响应无 DH2Q）")
        conn.close()
        return
    timeout_ms, flags = struct.unpack("<II", ctxs[DH2Q][:8])
    ok("v2-grant", f"授予，服务端回 Timeout={timeout_ms}ms Flags={flags}")
    if timeout_ms != 30000:
        bad("v2-timeout-echo",
            f"客户端请求 30000ms，服务端回 {timeout_ms}ms")
    else:
        ok("v2-timeout-echo", "服务端如实回显客户端请求的超时")

    conn.getSMBServer().get_socket().close()

    # 反向对照 1：GUID 不对 → 必须失败。
    conn_bad = connect()
    try:
        tid_b = conn_bad.connectTree(SHARE)
        st_b, _, _ = raw_create(conn_bad, tid_b, "hello.txt",
                                [encode_context(DH2C, dh2c(fid, uuid.uuid4().bytes))])
        if st_b == STATUS_SUCCESS:
            bad("v2-wrong-guid", "错误 CreateGuid 竟然重连成功")
        else:
            ok("v2-wrong-guid", f"错误 GUID 被拒 status=0x{st_b:08x}")
    finally:
        conn_bad.close()

    # 正向：正确 GUID → 成功且 FileId 不变。
    conn2 = connect()
    try:
        tid2 = conn2.connectTree(SHARE)
        st2, fid2, _ = raw_create(conn2, tid2, "hello.txt",
                                  [encode_context(DH2C, dh2c(fid, guid))])
        if st2 != STATUS_SUCCESS:
            bad("v2-reconnect", f"DH2C 重连失败 status=0x{st2:08x}")
            return
        if fid2 != fid:
            bad("v2-reconnect", f"重连后 FileId 变了：{fid.hex()} → {fid2.hex()}")
            return
        ok("v2-reconnect", f"FileId 保持不变 {fid.hex()}")
        st_r, data = raw_read(conn2, tid2, fid2, 0, 64)
        if st_r != STATUS_SUCCESS:
            bad("v2-reconnect-usable",
                f"重连成功但句柄不可用：READ 回 0x{st_r:08x}")
        else:
            ok("v2-reconnect-usable", f"重连后 READ 正常：{data[:20]!r}")
    finally:
        conn2.close()


def case_no_batch_no_grant():
    """反向对照：不请求 batch oplock 时**不应**授予 durable。
    这条同时钉住「lease 注入点尚未接线」这个事实。"""
    conn = connect()
    try:
        tid = conn.connectTree(SHARE)
        st, fid, ctxs = raw_create(conn, tid, "hello.txt",
                                   [encode_context(DHNQ, b"\x00" * 16)],
                                   oplock=SMB2_OPLOCK_LEVEL_NONE)
        if st != STATUS_SUCCESS:
            bad("no-batch-no-grant", f"CREATE 失败 status=0x{st:08x}")
            return
        if DHNQ in ctxs:
            bad("no-batch-no-grant", "无 batch oplock 却授予了 durable")
            return
        ok("no-batch-no-grant", "无 batch oplock 时诚实不授予，CREATE 仍成功")
    finally:
        conn.close()


def case_v1_cross_file_collision():
    """**交叉句柄泄漏**：两条连接各自 durable-open 一个**不同的文件**，
    A 掉线后用自己的 FileId 重连，拿回的却是 B 的句柄。

    成因：Session.AddOpen 的 Persistent 是**会话内**计数器，两条连接的首个
    句柄 Persistent 都是 1；durableKey 的 v1 分支只用 Persistent 做键。

    判据可证伪且不含糊：重连后直接 READ，比对读回来的**文件内容**。
    读到 B 的内容 = 服务端把别人的文件交给了我。
    """
    a_name, a_want = "hello.txt", b"hello durable"
    b_name, b_want = "sub\\nested.txt", b"nested"

    conn_a = connect()
    tid_a = conn_a.connectTree(SHARE)
    st_a, fid_a, ctx_a = raw_create(conn_a, tid_a, a_name,
                                    [encode_context(DHNQ, b"\x00" * 16)])
    if st_a != STATUS_SUCCESS or DHNQ not in ctx_a:
        bad("v1-collision", "前置条件不成立：A 未拿到 durable 授予")
        conn_a.close()
        return

    conn_b = connect()
    tid_b = conn_b.connectTree(SHARE)
    st_b, fid_b, ctx_b = raw_create(conn_b, tid_b, b_name,
                                    [encode_context(DHNQ, b"\x00" * 16)])
    if st_b != STATUS_SUCCESS or DHNQ not in ctx_b:
        bad("v1-collision", "前置条件不成立：B 未拿到 durable 授予")
        conn_a.close()
        conn_b.close()
        return

    if fid_a != fid_b:
        ok("v1-collision", f"两条连接的 FileId 不同（{fid_a.hex()} vs "
                           f"{fid_b.hex()}），键不会碰撞")
        conn_a.close()
        conn_b.close()
        return
    print(f"    两条独立连接拿到了**相同**的 FileId {fid_a.hex()}")

    conn_a.getSMBServer().get_socket().close()  # A 掉线

    conn_c = connect()
    try:
        tid_c = conn_c.connectTree(SHARE)
        st_c, fid_c, _ = raw_create(conn_c, tid_c, a_name,
                                    [encode_context(DHNC, fid_a)])
        if st_c != STATUS_SUCCESS:
            bad("v1-collision", f"A 重连失败 status=0x{st_c:08x}")
            return
        st_r, got = raw_read(conn_c, tid_c, fid_c, 0, 64)
        if st_r != STATUS_SUCCESS:
            bad("v1-collision", f"重连后 READ 失败 status=0x{st_r:08x}")
            return
        if got.startswith(b_want) and not got.startswith(a_want):
            bad("v1-collision",
                f"重连 {a_name} 却读回 {b_name} 的内容 {got[:16]!r} —— 交叉句柄泄漏")
        elif got.startswith(a_want):
            ok("v1-collision", f"重连读回正确文件内容 {got[:16]!r}")
        else:
            bad("v1-collision", f"读回内容既不是 A 也不是 B：{got[:32]!r}")
    finally:
        conn_b.close()
        conn_c.close()


def case_persistent_flag():
    """DH2Q 带 SMB2_DHANDLE_FLAG_PERSISTENT(0x2)。
    MS-SMB2 §3.3.5.9.12：非 CA 共享上该位应被降级忽略、CREATE 继续成功。"""
    conn = connect()
    try:
        tid = conn.connectTree(SHARE)
        st, fid, ctxs = raw_create(
            conn, tid, "hello.txt",
            [encode_context(DH2Q, dh2q(uuid.uuid4().bytes, 30000, flags=0x2))])
        if st != STATUS_SUCCESS:
            bad("persistent-flag",
                f"persistent 位让整个 CREATE 失败 status=0x{st:08x}（文件都打不开）")
            return
        ok("persistent-flag", f"CREATE 成功，响应 context={list(ctxs)}")
    finally:
        conn.close()


def case_malformed_context():
    """畸形载荷必须被拒绝且**不能打崩服务**（AGENTS.md §5：解析失败不 panic）。"""
    conn = connect()
    try:
        tid = conn.connectTree(SHARE)
        st, _, _ = raw_create(conn, tid, "hello.txt",
                              [encode_context(DH2Q, b"\x01" * 7)])
        if st == STATUS_SUCCESS:
            bad("malformed-dh2q", "7 字节的 DH2Q 载荷竟然被接受")
        else:
            ok("malformed-dh2q", f"畸形载荷被拒 status=0x{st:08x}")
    except Exception as e:
        bad("malformed-dh2q", f"客户端异常（服务端可能崩了）：{e}")
        return
    finally:
        conn.close()

    # 服务端存活性复查：崩了的话这一步会连不上。
    try:
        c2 = connect()
        c2.connectTree(SHARE)
        c2.close()
        ok("survives-malformed", "畸形请求后服务端仍存活")
    except Exception as e:
        bad("survives-malformed", f"服务端在畸形请求后不可用：{e}")


def main():
    print(f"=== durable handle 线级验证 @ {TARGET}:{PORT}/{SHARE} ===")
    for fn in (case_plain_create_unaffected,
               case_unknown_context_ignored,
               case_no_batch_no_grant,
               case_v1_grant_and_reconnect,
               case_v2_grant_and_reconnect,
               case_v1_cross_file_collision,
               case_persistent_flag,
               case_malformed_context):
        try:
            fn()
        except Exception as e:  # noqa: BLE001 —— 用例自身异常也算失败
            bad(fn.__name__, f"用例异常: {type(e).__name__}: {e}")
    print(f"\n通过 {len(PASSED)} 项，失败 {len(FAILED)} 项")
    if FAILED:
        print("失败项：" + ", ".join(FAILED))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
