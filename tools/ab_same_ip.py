"""同出口双版本 A/B：在同一个 runner（同一出口 IP）上先后跑两个版本的网关，
各发一次真实 DSH 抓包体，记录分支与上游状态码。

关键约束：
  * 两个版本必须在同一台 runner 里跑，出口 IP 才相同。跨节点对比时额度差异会
    污染结论——上一版实验（zen10 vs zen9）就是栽在这里。
  * 交替顺序跑多轮（A→B，再 B→A），排除「谁先跑谁拿到额度」的顺序偏差。
  * 出口 IP 不一致时直接判无效，不给结论。

环境变量：
  DSH_CAP   真实 DSH 请求体路径
  AB_OUT    报告输出路径
  BIN_OLD   旧版网关二进制
  BIN_NEW   新版网关二进制
"""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UA = "opencode/latest/2.3.1/cli"
CAP = os.environ.get("DSH_CAP", "tools/fixtures/dsh_capture.json")
SINK_PORT = int(os.environ.get("SINK_PORT", "19500"))
UPSTREAM = os.environ.get("UPSTREAM", "https://opencode.ai/inference/v1")
OUT = os.environ.get("AB_OUT", "ab_same_ip.json")
BINARIES = [
    ("v0.9.39", os.environ.get("BIN_OLD", "./zen-0.9.39")),
    ("v0.9.40", os.environ.get("BIN_NEW", "./zen-0.9.40")),
]

_seen = []
_lock = threading.Lock()


class Sink(BaseHTTPRequestHandler):
    """透明转发到真实上游，同时记录出站 body 的工具数与体积。"""

    protocol_version = "HTTP/1.1"

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            parsed = json.loads(raw or b"{}")
        except Exception:
            parsed = {}
        tools = parsed.get("tools") or []
        with _lock:
            _seen.append({
                "size": len(raw),
                "count": len(tools),
                "names": [(t.get("function") or {}).get("name") for t in tools],
            })
        req = urllib.request.Request(
            UPSTREAM.rstrip("/") + "/chat/completions", data=raw,
            headers={"content-type": "application/json",
                     "authorization": "Bearer public",
                     "user-agent": UA})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            resp = opener.open(req, timeout=180)
            status, body = resp.status, resp.read()
            ctype = resp.headers.get("content-type", "application/json")
        except urllib.error.HTTPError as exc:
            status, body = exc.code, exc.read()
            ctype = exc.headers.get("content-type", "application/json")
        except Exception as exc:
            status, body = 0, ("%s: %s" % (type(exc).__name__, exc)).encode()
            ctype = "text/plain"
        self.send_response(status or 502)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def wait_health(port, timeout=90):
    end = time.time() + timeout
    while time.time() < end:
        try:
            urllib.request.urlopen("http://127.0.0.1:%d/health" % port, timeout=2).read()
            return True
        except Exception:
            time.sleep(1.0)
    return False


def health(port):
    try:
        raw = urllib.request.urlopen("http://127.0.0.1:%d/health" % port, timeout=10).read()
        return json.loads(raw.decode())
    except Exception as exc:
        return {"_error": str(exc)}


def run_once(label, binary, gw_port, body):
    with _lock:
        _seen.clear()
    env = dict(os.environ)
    env.update({"PORT": str(gw_port),
                "OPENCODE_ZEN_BASE": "http://127.0.0.1:%d/inference/v1" % SINK_PORT,
                "NO_PROXY": "127.0.0.1,localhost"})
    env.pop("ZEN_SHAPE_MODE", None)
    logpath = "/tmp/gw-%s.log" % label.replace(".", "")
    logf = open(logpath, "wb")
    gw = subprocess.Popen([binary], env=env, stdout=logf, stderr=subprocess.STDOUT)
    try:
        if not wait_health(gw_port):
            return {"label": label, "error": "网关起不来（见 %s）" % logpath}
        h0 = health(gw_port)
        req = urllib.request.Request(
            "http://127.0.0.1:%d/v1/chat/completions" % gw_port,
            data=json.dumps(body).encode(),
            headers={"content-type": "application/json", "user-agent": UA})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        started = time.time()
        try:
            resp = opener.open(req, timeout=200)
            status, text = resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            status, text = exc.code, exc.read().decode("utf-8", "replace")
        except Exception as exc:
            status, text = 0, "%s: %s" % (type(exc).__name__, exc)
        elapsed = int((time.time() - started) * 1000)
        h1 = health(gw_port)
        last = (h1.get("tool_shape") or {}).get("last") or {}
        with _lock:
            sink = dict(_seen[0]) if _seen else {}
        low = text.lower()
        return {
            "label": label, "version": h0.get("version"), "egress_ip": h0.get("egress_ip"),
            "http": status, "latency_ms": elapsed,
            "quota": any(k in low for k in ("freeusagelimit", "free usage limit", "usage limit")),
            "body_head": text[:200],
            "branch": last.get("mode"),
            "client_tools": last.get("client_tools"),
            "outbound_tools": last.get("outbound_tools"),
            "reason": last.get("reason"),
            "sink_tools": sink.get("count"), "sink_bytes": sink.get("size"),
            "up429_before": (h0.get("stats") or {}).get("up429"),
            "up429_after": (h1.get("stats") or {}).get("up429"),
        }
    finally:
        gw.terminate()
        try:
            gw.wait(timeout=15)
        except Exception:
            gw.kill()
        logf.close()


def main():
    if not os.path.exists(CAP):
        print("找不到抓包：%s" % CAP)
        return 2
    body = json.load(open(CAP, encoding="utf-8"))
    body["stream"] = False
    body.setdefault("max_tokens", 16)
    tools_n = len(body.get("tools") or [])
    print("抓包: %s  工具数=%d" % (CAP, tools_n))

    rounds_n = int(os.environ.get("ROUNDS", "2"))
    srv = ThreadingHTTPServer(("127.0.0.1", SINK_PORT), Sink)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    time.sleep(1)

    results = []
    try:
        for ri in range(1, rounds_n + 1):
            order = BINARIES if ri % 2 == 1 else list(reversed(BINARIES))
            print("\n=== 第 %d 轮（顺序：%s）===" % (ri, " -> ".join(l for l, _ in order)))
            for li, (label, binary) in enumerate(order):
                r = run_once(label, binary, 18300 + ri * 10 + li, body)
                results.append(r)
                print("  %-8s ip=%-16s http=%-4s branch=%-16s %s->%s sink=%s工具/%sB%s"
                      % (r.get("label"), r.get("egress_ip"), r.get("http"), r.get("branch"),
                         r.get("client_tools"), r.get("outbound_tools"),
                         r.get("sink_tools"), r.get("sink_bytes"),
                         "  QUOTA" if r.get("quota") else ""))
    finally:
        srv.shutdown()

    json.dump(results, open(OUT, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("\n" + "=" * 70)
    ips = sorted({r.get("egress_ip") for r in results if r.get("egress_ip")})
    print("出口 IP: %s（必须只有 1 个）" % ips)

    verdict = "无法判定"
    if len(ips) != 1:
        verdict = "无效：出口 IP 不一致 %s —— 差异不能归因到版本" % ips
    else:
        old = [r.get("http") for r in results if r.get("label") == "v0.9.39"]
        new = [r.get("http") for r in results if r.get("label") == "v0.9.40"]
        if not old or not new:
            verdict = "缺数据：旧=%s 新=%s" % (old, new)
        elif len(set(old)) == 1 and len(set(new)) == 1:
            if old[0] == new[0]:
                verdict = "同出口同状态码（%s）→ 429 与版本无关，是出口额度" % old[0]
            else:
                verdict = "同出口状态码不同（旧=%s 新=%s）→ 版本确实有影响" % (old[0], new[0])
        else:
            verdict = "结果不稳定：旧=%s 新=%s —— 需要更多轮次" % (old, new)
    print("判决: " + verdict)
    print("报告: " + OUT)
    return 0


sys.exit(main())
