"""把**生产模板里那份**二进制和目标版本二进制，用同一请求体对比出站 body。

关键：不能拿「源码某个 commit 构建的」当基准——生产部署的二进制不是从当前
源码构建的（符号和字符串都不一样）。这个脚本直接吃模板目录里的 bin/zen-direct。
"""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SINK = 19900
CAP = sys.argv[1] if len(sys.argv) > 1 else "tools/fixtures/dsh_capture.json"
BINS = []
for i in range(2, len(sys.argv), 2):
    BINS.append((sys.argv[i], sys.argv[i + 1]))
if not BINS:
    BINS = [("old", "./zen-old"), ("new", "./zen-new")]

seen = []
_lock = threading.Lock()


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        with _lock:
            seen.append(raw)
        out = json.dumps({"id": "c", "object": "chat.completion", "model": "m",
                          "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"},
                                       "finish_reason": "stop"}],
                          "usage": {"prompt_tokens": 1, "completion_tokens": 1,
                                    "total_tokens": 2}}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *a):
        pass


def wait_health(port, timeout=60):
    end = time.time() + timeout
    while time.time() < end:
        try:
            urllib.request.urlopen("http://127.0.0.1:%d/health" % port, timeout=2).read()
            return True
        except Exception:
            time.sleep(0.5)
    return False


def grab(binary, port, body):
    with _lock:
        seen.clear()
    env = dict(os.environ)
    env.update({"PORT": str(port),
                "OPENCODE_ZEN_BASE": "http://127.0.0.1:%d/inference/v1" % SINK,
                "NO_PROXY": "127.0.0.1,localhost"})
    for k in ("ZEN_SHAPE_MODE", "ZEN_NO_STUB", "ZEN_DESC_VERBATIM", "ZEN_TOOL_CHANNEL"):
        env.pop(k, None)
    gw = subprocess.Popen([binary], env=env,
                          stdout=open("/tmp/gw-%d.log" % port, "wb"),
                          stderr=subprocess.STDOUT)
    try:
        if not wait_health(port):
            return None
        req = urllib.request.Request("http://127.0.0.1:%d/v1/chat/completions" % port,
                                     data=json.dumps(body).encode(),
                                     headers={"content-type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=90).read()
        except Exception:
            pass
        time.sleep(0.4)
        with _lock:
            return seen[0] if seen else None
    finally:
        gw.terminate()
        try:
            gw.wait(timeout=10)
        except Exception:
            gw.kill()


def tools_of(raw):
    try:
        return [(t.get("function") or {}) for t in (json.loads(raw).get("tools") or [])]
    except Exception:
        return []


def main():
    body = json.load(open(CAP, encoding="utf-8"))
    body["stream"] = False
    body.setdefault("max_tokens", 16)
    print("请求体 %s  工具数=%d" % (CAP, len(body.get("tools") or [])))

    srv = ThreadingHTTPServer(("127.0.0.1", SINK), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    time.sleep(1)
    outs = {}
    try:
        for i, (label, binary) in enumerate(BINS):
            raw = grab(binary, 19100 + i, body)
            outs[label] = raw
            if raw is None:
                print("  %-10s 抓取失败" % label)
                continue
            ts = tools_of(raw)
            names = [t.get("name") for t in ts]
            print("  %-10s 出站 %-3d 个  %-7d 字节  工具名[0:3]=%s"
                  % (label, len(ts), len(raw), names[:3]))
    finally:
        srv.shutdown()

    if len(outs) >= 2:
        labels = list(outs)
        a, b = outs[labels[0]], outs[labels[1]]
        print()
        print("=" * 70)
        if a and b:
            ta, tb = tools_of(a), tools_of(b)
            na = [t.get("name") for t in ta]
            nb = [t.get("name") for t in tb]
            print("%s: %d 个工具 / %d 字节" % (labels[0], len(ta), len(a)))
            print("%s: %d 个工具 / %d 字节" % (labels[1], len(tb), len(b)))
            print("名字序列完全相同: %s" % (na == nb))
            print("出站 JSON 完全相同: %s" % (a == b))
            if na != nb:
                print("  只在 %s 里: %s" % (labels[0], [n for n in na if n not in nb][:12]))
                print("  只在 %s 里: %s" % (labels[1], [n for n in nb if n not in na][:12]))
            else:
                diff = 0
                da = {t.get("name"): t for t in ta}
                db = {t.get("name"): t for t in tb}
                for n in na:
                    if json.dumps(da[n], sort_keys=True) != json.dumps(db[n], sort_keys=True):
                        diff += 1
                        if diff <= 5:
                            print("  定义不同: %s" % n)
                            print("    %s desc=%r" % (labels[0], str(da[n].get("description"))[:110]))
                            print("    %s desc=%r" % (labels[1], str(db[n].get("description"))[:110]))
                print("  定义不同的工具数: %d / %d" % (diff, len(na)))


main()
