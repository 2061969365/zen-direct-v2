"""raw_relay.py — 透传 TCP 中继，不终结 TLS。

为什么不能用普通 HTTP 代理抓官方 CLI 的包：
    CLI(Bun TLS) -> 代理 -> 上游，这条链路里代理用 Python 的 TLS 栈去连
    opencode.ai，ClientHello 指纹彻底变了，上游会直接回 429。于是"抓到 429"
    是代理自己造成的，与网关无关，结论被污染。

本中继只做字节搬运：客户端与 opencode.ai 之间 TLS 端到端握手，指纹逐字节
不变；配合 SSLKEYLOGFILE 解出明文。

用法：
    python raw_relay.py --listen 19100 --target opencode.ai:443 --out relay
"""

import argparse
import os
import socket
import threading
import time

LOCK = threading.Lock()
STATE = {"n": 0, "out": "relay"}


def _pipe(src, dst, log, tag):
    try:
        while True:
            chunk = src.recv(65536)
            if not chunk:
                break
            with LOCK:
                log.write(chunk)
            dst.sendall(chunk)
    except OSError:
        pass
    finally:
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def handle(client, addr, target):
    with LOCK:
        STATE["n"] += 1
        n = STATE["n"]
    os.makedirs(STATE["out"], exist_ok=True)
    c2s_path = os.path.join(STATE["out"], f"{n:03d}-c2s.bin")
    s2c_path = os.path.join(STATE["out"], f"{n:03d}-s2c.bin")
    print(f"#{n} connect from {addr} -> {target}", flush=True)

    try:
        upstream = socket.create_connection(target, timeout=20)
    except OSError as exc:
        print(f"#{n} upstream dial failed: {exc}", flush=True)
        client.close()
        return
    upstream.settimeout(None)
    client.settimeout(None)

    with open(c2s_path, "wb") as c2s, open(s2c_path, "wb") as s2c:
        t1 = threading.Thread(target=_pipe, args=(client, upstream, c2s, "c2s"))
        t2 = threading.Thread(target=_pipe, args=(upstream, client, s2c, "s2c"))
        t1.start()
        t2.start()
        t1.join()
        t2.join()
    print(f"#{n} closed  c2s={os.path.getsize(c2s_path)}B s2c={os.path.getsize(s2c_path)}B", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", type=int, default=19100)
    ap.add_argument("--target", default="opencode.ai:443")
    ap.add_argument("--out", default="relay")
    args = ap.parse_args()
    STATE["out"] = args.out
    host, _, port = args.target.partition(":")
    target = (host, int(port or 443))

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", args.listen))
    srv.listen(64)
    print(f"raw relay 127.0.0.1:{args.listen} -> {target[0]}:{target[1]}  out={args.out}", flush=True)
    while True:
        client, addr = srv.accept()
        threading.Thread(target=handle, args=(client, addr, target), daemon=True).start()


if __name__ == "__main__":
    main()
