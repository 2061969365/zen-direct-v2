"""补桩 vs 不补桩：同一出口、同一请求体，只改 ZEN_NO_STUB。

要回答的问题：
  DSH 发 45 个工具 → 补桩后出站 59 个 → 实测 429。
  那么「不补桩、出站 45 个」是 200 还是 429？

如果 200：修法明确（补桩要按数量做条件判断，或者干脆不补）。
如果 429：说明上游认的就是「名字集合必须官方原样」，补桩不能省，
           DSH 这条路只能靠减少客户端工具数解决。

环境变量：
  DSH_CAP     请求体
  AB_OUT      报告输出
  BIN         网关二进制（同一份，行为由 ZEN_NO_STUB 控制）
  GATEWAY_PROXY  出口代理
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
OUT = os.environ.get("AB_OUT", "stub_ab.json")
GATEWAY_PROXY = os.environ.get("GATEWAY_PROXY", "").strip()
BIN = os.environ.get("BIN", "./zen-gateway")

_seen = []
_lock = threading.Lock()


class Sink(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            parsed = json.loads(raw or b"{}")
        except Exception:
            parsed = {}
        with _lock:
            _seen.append({"size": len(raw),
                          "count": len(parsed.get("tools") or [])})
        op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        req = urllib.request.Request(UPSTREAM.rstrip("/") + "/chat/completions", data=raw,
                                     headers={"content-type": "application/json",
                                              "authorization": "Bearer public",
                                              "user-agent": UA})
        try:
            resp = op.open(req, timeout=180)
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
        return json.loads(urllib.request.urlopen(
            "http://127.0.0.1:%d/health" % port, timeout=10).read().decode())
    except Exception as exc:
        return {"_error": str(exc)}


def run_once(label, no_stub, gw_port, body, use_sink):
    with _lock:
        _seen.clear()
    env = dict(os.environ)
    env.update({"PORT": str(gw_port),
                "OPENCODE_ZEN_BASE": ("http://127.0.0.1:%d/inference/v1" % SINK_PORT)
                if use_sink else UPSTREAM,
                "NO_PROXY": "127.0.0.1,localhost",
                "no_proxy": "127.0.0.1,localhost"})
    env.pop("ZEN_SHAPE_MODE", None)
    for k in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
              "http_proxy", "https_proxy", "all_proxy"):
        env.pop(k, None)
    if GATEWAY_PROXY:
        env["HTTPS_PROXY"] = GATEWAY_PROXY
        env["HTTP_PROXY"] = GATEWAY_PROXY
        env["ALL_PROXY"] = GATEWAY_PROXY
    if no_stub:
        env["ZEN_NO_STUB"] = "1"
    else:
        env.pop("ZEN_NO_STUB", None)

    logf = open("/tmp/gws-%s.log" % label.replace(" ", "_"), "wb")
    gw = subprocess.Popen([BIN], env=env, stdout=logf, stderr=subprocess.STDOUT)
    try:
        if not wait_health(gw_port):
            return {"label": label, "error": "网关起不来"}
        h0 = health(gw_port)
        req = urllib.request.Request(
            "http://127.0.0.1:%d/v1/chat/completions" % gw_port,
            data=json.dumps(body).encode(),
            headers={"content-type": "application/json", "user-agent": UA})
        op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            resp = op.open(req, timeout=200)
            status, text = resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            status, text = exc.code, exc.read().decode("utf-8", "replace")
        except Exception as exc:
            status, text = 0, "%s: %s" % (type(exc).__name__, exc)
        h1 = health(gw_port)
        last = (h1.get("tool_shape") or {}).get("last") or {}
        with _lock:
            sink = dict(_seen[0]) if _seen else {}
        low = text.lower()
        return {"label": label, "no_stub": no_stub,
                "egress_ip": h0.get("egress_ip"), "http": status,
                "quota": any(k in low for k in ("freeusagelimit", "free usage limit",
                                                "usage limit")),
                "body_head": text[:200],
                "branch": last.get("mode"),
                "client_tools": last.get("client_tools"),
                "outbound_tools": last.get("outbound_tools"),
                "sink_tools": sink.get("count"), "sink_bytes": sink.get("size")}
    finally:
        gw.terminate()
        try:
            gw.wait(timeout=15)
        except Exception:
            gw.kill()
        logf.close()


def main():
    body = json.load(open(CAP, encoding="utf-8"))
    body["stream"] = False
    body.setdefault("max_tokens", 16)
    n_tools = len(body.get("tools") or [])
    print("请求体: %s  工具数=%d" % (CAP, n_tools))
    print("网关出口代理: %s" % (GATEWAY_PROXY or "（直连）"))

    use_sink = os.environ.get("USE_SINK", "0" if GATEWAY_PROXY else "1") == "1"
    srv = None
    if use_sink:
        srv = ThreadingHTTPServer(("127.0.0.1", SINK_PORT), Sink)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        time.sleep(1)
    else:
        print("本地桩关闭：直连真实上游（代理模式下 loopback 会被 sing-box 吃掉）")

    results = []
    try:
        # 交替两轮，排除「谁先跑谁吃额度」
        plan = [("补桩", False), ("不补桩", True), ("不补桩", True), ("补桩", False)]
        for i, (label, no_stub) in enumerate(plan):
            r = run_once(label, no_stub, 18800 + i, body, use_sink)
            results.append(r)
            print("  %-8s ip=%-16s 出站=%-4s http=%-4s %s"
                  % (label, r.get("egress_ip"), r.get("outbound_tools"),
                     r.get("http"), "QUOTA" if r.get("quota") else ""))
    finally:
        if srv is not None:
            srv.shutdown()

    json.dump(results, open(OUT, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("\n" + "=" * 66)
    ips = sorted({r.get("egress_ip") for r in results if r.get("egress_ip")})
    print("出口 IP: %s" % ips)
    withs = [r.get("http") for r in results if not r.get("no_stub")]
    without = [r.get("http") for r in results if r.get("no_stub")]
    print("补桩  : %s" % withs)
    print("不补桩: %s" % without)

    verdict = "无法判定"
    if len(ips) != 1:
        verdict = "无效：出口不一致 %s" % ips
    elif withs and without and len(set(withs)) == 1 and len(set(without)) == 1:
        if withs[0] == without[0]:
            verdict = "补桩与否结果相同（%s）→ 补桩不是成因" % withs[0]
        elif without[0] == 200:
            verdict = "不补桩 200、补桩 %s → 补桩就是 429 的成因，可以去掉" % withs[0]
        else:
            verdict = "补桩 %s、不补桩 %s → 两者都失败" % (withs[0], without[0])
    else:
        verdict = "结果不稳定：补桩=%s 不补桩=%s" % (withs, without)
    print("判决: " + verdict)
    print("报告: " + OUT)
    return 0


sys.exit(main())
