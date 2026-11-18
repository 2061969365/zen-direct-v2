"""同出口双版本 A/B（指纹场景）：客户端只发官方工具名、描述是客户端自有文本。

为什么单拎这个场景：
  45 个工具的真实 DSH 请求里只有 8 个是官方名，两个版本都会走 rename_complete，
  出站必然相同——拿它下结论是废话。版本差异只在这一种输入上体现：

    v0.9.39 → rename_complete  文本被换成官方原文 → 指纹安全
    v0.9.40 → passthrough      文本原样发出       → 指纹暴露

所以这里只喂「官方名 + 客户端自有文本」，看真上游给 200 还是 429。

出口：
  两个版本必须在同一出口 IP 上跑。走 vpngate 时，网关通过 HTTP(S)_PROXY 走
  sing-box，探针直连网关（NO_PROXY 必须包含 127.0.0.1 与 localhost，否则网关会
  把 loopback 也塞进 CONNECT）。

环境变量：
  DSH_CAP      请求体路径（默认指纹夹具）
  AB_OUT       报告输出
  BIN_OLD/BIN_NEW  两个二进制
  GATEWAY_PROXY   网关出站代理（如 http://127.0.0.1:18080）；空则直连
  ROUNDS       轮数
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
CAP = os.environ.get("DSH_CAP", "tools/fixtures/dsh_official_names_dsh_text.json")
SINK_PORT = int(os.environ.get("SINK_PORT", "19500"))
UPSTREAM = os.environ.get("UPSTREAM", "https://opencode.ai/inference/v1")
OUT = os.environ.get("AB_OUT", "ab_fingerprint.json")
GATEWAY_PROXY = os.environ.get("GATEWAY_PROXY", "").strip()
BINARIES = [
    ("v0.9.39", os.environ.get("BIN_OLD", "./zen-0.9.39")),
    ("v0.9.40", os.environ.get("BIN_NEW", "./zen-0.9.40")),
]

_seen = []
_lock = threading.Lock()


class Sink(BaseHTTPRequestHandler):
    """透明转发到真上游，记录出站 body 的工具数与关键描述文本。"""

    protocol_version = "HTTP/1.1"

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            parsed = json.loads(raw or b"{}")
        except Exception:
            parsed = {}
        tools = parsed.get("tools") or []
        descs = {}
        for t in tools:
            fn = t.get("function") or {}
            name = fn.get("name")
            if name in ("read", "mcp__read", "edit", "mcp__edit"):
                descs[name] = (fn.get("description") or "")[:60]
        with _lock:
            _seen.append({"size": len(raw), "count": len(tools),
                          "names": [(t.get("function") or {}).get("name") for t in tools],
                          "descs": descs})

        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        req = urllib.request.Request(
            UPSTREAM.rstrip("/") + "/chat/completions", data=raw,
            headers={"content-type": "application/json",
                     "authorization": "Bearer public",
                     "user-agent": UA})
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
                "NO_PROXY": "127.0.0.1,localhost",
                "no_proxy": "127.0.0.1,localhost"})
    env.pop("ZEN_SHAPE_MODE", None)
    # 网关出站出口：走代理时把 *_PROXY 交给网关；探针自己不发代理。
    for k in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
              "http_proxy", "https_proxy", "all_proxy"):
        env.pop(k, None)
    if GATEWAY_PROXY:
        env["HTTPS_PROXY"] = GATEWAY_PROXY
        env["HTTP_PROXY"] = GATEWAY_PROXY
        env["ALL_PROXY"] = GATEWAY_PROXY
    logpath = "/tmp/gwf-%s.log" % label.replace(".", "")
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
            "sink_descs": sink.get("descs"),
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


OFFICIAL_NAMES = {
    "edit", "execute", "glob", "grep", "question", "read", "shell", "skill",
    "subagent", "webfetch", "websearch", "write", "patch", "experimental_image_generate",
}


def check_fixture(path):
    """确认夹具正好是「全部官方名 + 非官方文本」，否则测不到透传分支。"""
    if not os.path.exists(path):
        print("夹具不存在: %s" % path)
        return 1
    data = json.load(open(path, encoding="utf-8"))
    tools = data.get("tools") or []
    names = [(t.get("function") or {}).get("name") for t in tools]
    foreign = [n for n in names if n not in OFFICIAL_NAMES]
    print("工具数=%d" % len(names))
    print("名字: %s" % ", ".join(n for n in names))
    print("非官方名: %s" % (foreign or "无"))
    if foreign:
        print("混了非官方名 → 两个版本都会走 rename_complete，测不到指纹差异")
        return 1
    descs = [((t.get("function") or {}).get("description") or "") for t in tools]
    print("描述样本: %r" % descs[:3])
    if any(d.startswith("Read a file or directory") or "Use when:" in d for d in descs):
        print("描述看着像官方原文 → 两版本都会 200，测不出差异")
        return 1
    print("夹具合格：官方名 + 客户端自有文本")
    return 0


def main():
    if len(sys.argv) >= 3 and sys.argv[1] == "--check":
        return check_fixture(sys.argv[2])
    if not os.path.exists(CAP):
        print("找不到请求体：%s" % CAP)
        return 2
    body = json.load(open(CAP, encoding="utf-8"))
    body["stream"] = False
    body.setdefault("max_tokens", 16)
    names = [(t.get("function") or {}).get("name") for t in body.get("tools") or []]
    print("请求体: %s" % CAP)
    print("工具数=%d 全部官方名=%s" % (len(names), all(n in (
        "edit", "execute", "glob", "grep", "question", "read", "shell", "skill",
        "subagent", "webfetch", "websearch", "write", "patch",
        "experimental_image_generate") for n in names)))
    print("网关出口代理: %s" % (GATEWAY_PROXY or "（直连）"))

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
                r = run_once(label, binary, 18400 + ri * 10 + li, body)
                results.append(r)
                d = r.get("sink_descs") or {}
                print("  %-8s ip=%-16s http=%-4s branch=%-16s %s->%s%s"
                      % (r.get("label"), r.get("egress_ip"), r.get("http"), r.get("branch"),
                         r.get("client_tools"), r.get("outbound_tools"),
                         "  QUOTA" if r.get("quota") else ""))
                if d:
                    print("           出站 read 描述=%r  edit 描述=%r"
                          % (d.get("read") or d.get("mcp__read"),
                             d.get("edit") or d.get("mcp__edit")))
    finally:
        srv.shutdown()

    json.dump(results, open(OUT, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("\n" + "=" * 70)
    ips = sorted({r.get("egress_ip") for r in results if r.get("egress_ip")})
    print("出口 IP: %s（必须只有 1 个）" % ips)

    verdict = "无法判定"
    old = [r.get("http") for r in results if r.get("label") == "v0.9.39"]
    new = [r.get("http") for r in results if r.get("label") == "v0.9.40"]
    if len(ips) != 1:
        verdict = "无效：出口 IP 不一致 %s" % ips
    elif not old or not new:
        verdict = "缺数据：旧=%s 新=%s" % (old, new)
    elif len(set(old)) == 1 and len(set(new)) == 1:
        if old[0] == new[0]:
            verdict = "同出口同状态码（%s）→ 该指纹场景两版本无差异" % old[0]
        else:
            verdict = "同出口状态码不同（旧=%s 新=%s）→ 版本确有影响" % (old[0], new[0])
    else:
        verdict = "结果不稳定：旧=%s 新=%s" % (old, new)
    print("判决: " + verdict)
    print("报告: " + OUT)
    return 0


sys.exit(main())
