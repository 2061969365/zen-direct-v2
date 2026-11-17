"""egress_sink.py — 出站对拍用的本地上游接收端（零额度、零外网）。

用途
    把网关的 OPENCODE_ZEN_BASE 指到这里的 ``http://127.0.0.1:<port>/inference/v1``，
    网关**真正发出去**的那份 body 会整份落盘，再交给
    ``outbound_conformance.py`` 与官方工具定义逐字段对拍。

    为什么必须抓出站而不是看客户端响应：网关会重命名客户端工具
    （``mcp_<12hex>``）并补一套官方桩，客户端拿到 200 完全不代表
    上游收到的是官方那份。

用法
    python tools/egress_sink.py --port 19100 --out bodies_egress

落盘
    <out>/<seq>-<path>.json，其中 path 里的 ``/`` 换成 ``_``：

    {
      "seq": 1, "ts": 1763450000.1, "method": "POST",
      "path": "/inference/v1/chat/completions",
      "headers": {...}, "headers_lower": {...},
      "body_len": 12345, "body": {...},
      "stream": false, "tools": 26, "tool_names": ["mcp_ab12cd34ef56", ...]
    }

    原子写（先 .tmp 再 os.replace），对拍器不会读到半份文件。

响应
    一律回一份合法的 OpenAI ``chat.completion``（``finish_reason=stop``），
    所以网关侧不会因为上游 5xx 而重试、改写或短路整形逻辑。
    请求 body 里 ``stream: true`` 时同样回正常 JSON（按需加 ``--sse`` 才真发 SSE）。

顺带当「只放行 loopback 的代理」
    网关的传输层（bunTransport）把 HTTP(S)_PROXY 当成全局代理，连
    ``127.0.0.1`` 也要先 CONNECT——所以对拍时不能塞死代理，否则它连 sink
    都到不了。这里因此实现 ``CONNECT``：目标只允许 loopback（隧道打回本机），
    其他目标一律 403。配合把 HTTP(S)_PROXY 指到本 sink，既保证网关能到
    sink，又保证 npm / 出口 IP / Endpoints 表这些背景请求一秒失败、绝不出网。
"""

import argparse
import json
import os
import select
import socket
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {
    "out": "bodies_egress",
    "seq": 0,
    "sse": False,
    "lock": threading.Lock(),
    "model": "deepseek-v4.1-flash-free",
}

LOOPBACK_HOSTS = {"127.0.0.1", "localhost", "::1", "[::1]"}


def _slug(path):
    """把 URL 路径变成安全的文件名片段。"""
    cleaned = path.split("?", 1)[0].strip("/").replace("/", "_")
    keep = []
    for ch in cleaned:
        keep.append(ch if (ch.isalnum() or ch in "._-") else "_")
    slug = "".join(keep).strip("_")
    return slug or "root"


def _read_body(handler):
    """读请求体，兼容 Content-Length 与 chunked。"""
    encoding = (handler.headers.get("Transfer-Encoding") or "").lower()
    if "chunked" in encoding:
        chunks = []
        while True:
            line = handler.rfile.readline(65536).strip()
            if not line:
                break
            try:
                size = int(line.split(b";", 1)[0], 16)
            except ValueError:
                break
            if size == 0:
                handler.rfile.readline(65536)
                break
            chunks.append(handler.rfile.read(size))
            handler.rfile.read(2)
        return b"".join(chunks)
    length = int(handler.headers.get("Content-Length") or 0)
    return handler.rfile.read(length) if length > 0 else b""


def _tool_names(body):
    tools = body.get("tools") if isinstance(body, dict) else None
    if not isinstance(tools, list):
        return []
    names = []
    for entry in tools:
        if not isinstance(entry, dict):
            continue
        fn = entry.get("function")
        if isinstance(fn, dict) and isinstance(fn.get("name"), str):
            names.append(fn["name"])
        elif isinstance(entry.get("name"), str):
            names.append(entry["name"])
    return names


def _completion(model, seq):
    return {
        "id": "chatcmpl-sink-%d" % seq,
        "object": "chat.completion",
        "created": int(time.time()),
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "sink-ok"},
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
    }


def _sse_chunks(model, seq):
    """--sse 模式下的最小合法 SSE 流（默认不走这条路）。"""
    base = {
        "id": "chatcmpl-sink-%d" % seq,
        "object": "chat.completion.chunk",
        "created": int(time.time()),
        "model": model,
    }
    first = dict(base)
    first["choices"] = [
        {"index": 0, "delta": {"role": "assistant", "content": "sink-ok"}, "finish_reason": None}
    ]
    last = dict(base)
    last["choices"] = [{"index": 0, "delta": {}, "finish_reason": "stop"}]
    frames = ["data: " + json.dumps(first, ensure_ascii=False) + "\n\n"]
    frames.append("data: " + json.dumps(last, ensure_ascii=False) + "\n\n")
    frames.append("data: [DONE]\n\n")
    return "".join(frames).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "egress-sink/1.0"

    def log_message(self, fmt, *args):  # 不刷默认的 access log
        return

    def _json(self, code, obj):
        payload = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        # 就绪探针（对拍器用），顺手让手工 curl 也能确认 sink 活着。
        # 代理形态的绝对 URL（http://host/...）一律挡掉：既不联网，也不让
        # 网关的出口 IP 解析把这里的 200 当成一个 IP。
        if self.path.startswith("http://") or self.path.startswith("https://"):
            print("[sink] GET blocked (proxy form) -> %s" % self.path, flush=True)
            self._json(403, {"ok": False, "blocked": self.path})
            return
        self._json(200, {"ok": True, "sink": "egress", "seq": STATE["seq"]})

    def do_CONNECT(self):
        """只放行 loopback 的 CONNECT 隧道；其他目标直接 403（保证零外网）。"""
        target = self.path
        host, _, port = target.rpartition(":")
        host = host.strip("[]").lower()
        port = port or "443"
        if host not in {h.strip("[]") for h in LOOPBACK_HOSTS}:
            print("[sink] CONNECT blocked -> %s" % target, flush=True)
            self.close_connection = True
            self.wfile.write(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            self.wfile.flush()
            return
        try:
            upstream = socket.create_connection((host, int(port)), timeout=5)
        except OSError as exc:
            print("[sink] CONNECT fail -> %s (%s)" % (target, exc), flush=True)
            self.close_connection = True
            self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            self.wfile.flush()
            return
        print("[sink] CONNECT tunnel -> %s" % target, flush=True)
        self.close_connection = True
        self.wfile.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        self.wfile.flush()
        self._relay(upstream)

    def _relay(self, upstream):
        """把 tunnel 两端对拷，直到任一侧关闭。"""
        peers = {self.connection: upstream, upstream: self.connection}
        deadline = time.time() + 600
        try:
            while time.time() < deadline:
                readable, _, broken = select.select([self.connection, upstream], [], [self.connection, upstream], 5)
                if broken:
                    return
                for sock in readable:
                    data = sock.recv(65536)
                    if not data:
                        return
                    peers[sock].sendall(data)
        except OSError:
            return
        finally:
            try:
                upstream.close()
            except OSError:
                pass

    def do_POST(self):
        raw = _read_body(self)
        try:
            body = json.loads(raw.decode("utf-8")) if raw else {}
        except (UnicodeDecodeError, json.JSONDecodeError):
            body = {"_non_json_body": raw.decode("utf-8", "replace")}

        with STATE["lock"]:
            STATE["seq"] += 1
            seq = STATE["seq"]

        headers = {k: v for k, v in self.headers.items()}
        names = _tool_names(body)
        stream = bool(body.get("stream")) if isinstance(body, dict) else False
        record = {
            "seq": seq,
            "ts": time.time(),
            "method": "POST",
            "path": self.path,
            "headers": headers,
            "headers_lower": {k.lower(): v for k, v in headers.items()},
            "body_len": len(raw),
            "body": body,
            "stream": stream,
            "tools": len(names),
            "tool_names": names,
        }

        out = STATE["out"]
        os.makedirs(out, exist_ok=True)
        target = os.path.join(out, "%03d-%s.json" % (seq, _slug(self.path)))
        tmp = target + ".tmp"
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(record, handle, ensure_ascii=False, indent=1)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, target)

        model = STATE["model"]
        if isinstance(body, dict) and isinstance(body.get("model"), str) and body["model"]:
            model = body["model"]

        if stream and STATE["sse"]:
            payload = _sse_chunks(model, seq)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            mode = "sse"
        else:
            self._json(200, _completion(model, seq))
            mode = "json"

        head = ", ".join(names[:4])
        if len(names) > 4:
            head += ", ..."
        print(
            "[sink] #%d %s tools=%d stream=%s resp=%s -> %s"
            % (seq, self.path, len(names), str(stream).lower(), mode, target),
            flush=True,
        )
        if names:
            print("[sink]     tool_names=[%s]" % head, flush=True)


class SinkServer(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        """隧道被对端 RST 属于正常收尾，不刷 traceback；其他错误照旧暴露。"""
        exc = sys.exc_info()[1]
        if isinstance(exc, (ConnectionResetError, ConnectionAbortedError, BrokenPipeError, TimeoutError)):
            return
        super().handle_error(request, client_address)


def serve(port, out, sse, model):
    STATE["out"] = out
    STATE["sse"] = sse
    STATE["model"] = model
    os.makedirs(out, exist_ok=True)
    httpd = SinkServer(("127.0.0.1", port), Handler)
    real_port = httpd.server_address[1]
    print("[sink] listening on http://127.0.0.1:%d  out=%s" % (real_port, os.path.abspath(out)), flush=True)
    try:
        httpd.serve_forever(poll_interval=0.2)
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()


def main(argv=None):
    parser = argparse.ArgumentParser(description="本地上游 sink：落盘网关出站 body")
    parser.add_argument("--port", type=int, default=19100, help="监听端口，0 表示随机高位端口")
    parser.add_argument("--out", default="bodies_egress", help="落盘目录")
    parser.add_argument("--sse", action="store_true", help="stream:true 时回真 SSE（默认回普通 JSON）")
    parser.add_argument("--model", default="deepseek-v4.1-flash-free", help="回包里的兜底 model")
    args = parser.parse_args(argv)
    try:
        serve(args.port, args.out, args.sse, args.model)
    except OSError as exc:
        print("[sink] 启动失败: %s" % exc, file=sys.stderr, flush=True)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
