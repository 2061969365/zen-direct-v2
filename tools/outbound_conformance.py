"""outbound_conformance.py — 网关出站工具契约对拍器（零额度、零外网）。

为什么需要它
    网关在出站前会「整形」工具集：客户端发的 tools 可能被改名成
    ``mcp_<12hex>`` 并追加一整套官方桩定义。于是「客户端拿到 200」
    完全不能证明「我们发出去的就是官方那份」。这个工具把上游指到
    本地 sink，抓真实出站 body，与官方定义逐字段对拍。

全流程自包含（不依赖任何已启动的服务）
    1. 起 ``tools/egress_sink.py``（子进程，随机高位端口）
    2. 起网关：PORT=<随机> OPENCODE_ZEN_BASE=http://127.0.0.1:<sink>/inference/v1 <bin>
    3. 等 /health 200
    4. 发 4 发请求，每发之后取 sink 最新落盘的那份做断言

    为了「零外网」，网关的 HTTP(S)_PROXY 被指到 sink：sink 只放行 loopback
    的 CONNECT（隧道打回本机），其他目标一律 403。网关的传输层对 loopback
    也要先 CONNECT，所以不能塞死代理，只能让 sink 自己当这个「只放行本机」
    的代理；npm 版本 / 出口 IP / Endpoints 表这些背景请求则一秒失败、从不出网。

用法
    python tools/outbound_conformance.py --bin ./zen-direct --out verdict.json
    python tools/outbound_conformance.py --bin ./zen-direct.exe --out verdict.json \
        --tools tools/opencode_tools.json --keep

退出码
    0 = 全部断言通过
    1 = 有断言失败（s12 现在就是这条：出站 26 ≠ 客户端 12）
    2 = 环境错误（二进制不存在 / sink 起不来 / 网关起不来）
"""

import argparse
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

MODEL = "deepseek-v4.1-flash-free"
UA_PATTERN = re.compile(r"^opencode/latest/[^/]+/cli$")
SYNTHETIC_PARAMS = {"type": "object", "properties": {}}
# 官方集里「后加的两个」：核心 12 名之外的 patch 与 experimental_image_generate。
EXTRA_TOOL_NAMES = ("patch", "experimental_image_generate")
ASSERTION_ORDER = (
    "passthrough",
    "names_equal",
    "order_equal",
    "desc_bytes_equal",
    "desc_text_equal",
    "params_deep_equal",
    "strict_absent_or_false",
    "ua_official",
)


class EnvError(RuntimeError):
    """环境错误：二进制缺失、sink/网关起不来等（退出码 2）。"""


# --------------------------------------------------------------------------
# 纯函数区：不碰进程、不碰网络，单测直接调这些
# --------------------------------------------------------------------------


def _kind(value):
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, (int, float)):
        return "number"
    if isinstance(value, str):
        return "string"
    if isinstance(value, list):
        return "array"
    if isinstance(value, dict):
        return "object"
    return type(value).__name__


def first_diff_path(client, outbound, path):
    """深比较两份 JSON，返回 (首个不同路径, 说明)；完全一致返回 (None, None)。

    路径形如 ``read.parameters.properties.path.type``。
    """
    kind = _kind(client)
    if kind != _kind(outbound):
        return path, "%s: client %s(%r) != outbound %s(%r)" % (
            path,
            kind,
            client,
            _kind(outbound),
            outbound,
        )
    if kind == "object":
        for key in sorted(set(client) | set(outbound)):
            sub = "%s.%s" % (path, key)
            if key not in client:
                return sub, "%s: outbound 多出该键 (%s)" % (sub, json.dumps(outbound[key], ensure_ascii=False)[:120])
            if key not in outbound:
                return sub, "%s: outbound 缺少该键 (client %s)" % (sub, json.dumps(client[key], ensure_ascii=False)[:120])
            diff = first_diff_path(client[key], outbound[key], sub)
            if diff[0] is not None:
                return diff
        return None, None
    if kind == "array":
        for index, (left, right) in enumerate(zip(client, outbound)):
            diff = first_diff_path(left, right, "%s[%d]" % (path, index))
            if diff[0] is not None:
                return diff
        if len(client) != len(outbound):
            sub = "%s.length" % path
            return sub, "%s: client %d != outbound %d" % (sub, len(client), len(outbound))
        return None, None
    if kind == "number":
        if float(client) == float(outbound):
            return None, None
    elif client == outbound:
        return None, None
    return path, "%s: client %s != outbound %s" % (
        path,
        json.dumps(client, ensure_ascii=False)[:120],
        json.dumps(outbound, ensure_ascii=False)[:120],
    )


def normalize_tool(entry, protocol="chat"):
    """把一条工具定义摊平成 {name, description, parameters, strict}。"""
    if not isinstance(entry, dict):
        return None
    if protocol == "chat" and isinstance(entry.get("function"), dict):
        inner = entry["function"]
        strict = entry.get("strict", inner.get("strict"))
    else:
        inner = entry
        strict = entry.get("strict")
    params = inner.get("parameters")
    if params is None:
        params = {}
    return {
        "name": inner.get("name") or "",
        "description": inner.get("description") or "",
        "parameters": params,
        "strict": strict,
    }


def normalize_tools(tools, protocol="chat"):
    return [t for t in (normalize_tool(e, protocol) for e in (tools or [])) if t is not None]


def first_param_diff(tool_name, client_params, outbound_params):
    """返回 (路径, 说明)：路径以客户端工具名开头，例如 read.parameters.properties.path.type。"""
    return first_diff_path(client_params, outbound_params, "%s.parameters" % tool_name)


def desc_bytes_equal(client_tools, outbound_tools):
    """逐位比 description 的 UTF-8 字节数，返回 (ok, 说明)。

    条数不同也先把公共前缀比完：改名/补全的场景里，前缀对比才能回答
    「描述有没有被一起改掉」这个问题。
    """
    limit = min(len(client_tools), len(outbound_tools))
    for index in range(limit):
        left, right = client_tools[index], outbound_tools[index]
        left_size = len(left["description"].encode("utf-8"))
        right_size = len(right["description"].encode("utf-8"))
        if left_size != right_size:
            return False, "tool[%d] %r/%r: client %dB != outbound %dB" % (
                index,
                left["name"],
                right["name"],
                left_size,
                right_size,
            )
    if len(client_tools) != len(outbound_tools):
        return False, "前 %d 个工具描述字节数一致，但条数不同（client %d / outbound %d）" % (
            limit,
            len(client_tools),
            len(outbound_tools),
        )
    return True, "%d 个工具的 description 字节数一致" % len(client_tools)


def desc_text_equal(client_tools, outbound_tools):
    """description 逐字比对（字节数一致不等于内容一致）。"""
    limit = min(len(client_tools), len(outbound_tools))
    for index in range(limit):
        left, right = client_tools[index], outbound_tools[index]
        if left["description"] != right["description"]:
            return False, "tool[%d] %r/%r: 描述内容不同（%dB / %dB）" % (
                index,
                left["name"],
                right["name"],
                len(left["description"].encode("utf-8")),
                len(right["description"].encode("utf-8")),
            )
    if len(client_tools) != len(outbound_tools):
        return False, "前 %d 个工具描述逐字一致，但条数不同（client %d / outbound %d）" % (
            limit,
            len(client_tools),
            len(outbound_tools),
        )
    return True, "%d 个工具的 description 逐字一致" % len(client_tools)


def names_equal(client_tools, outbound_tools):
    left = [t["name"] for t in client_tools]
    right = [t["name"] for t in outbound_tools]
    if set(left) == set(right):
        return True, "%d 个名字完全一致" % len(left)
    missing = sorted(set(left) - set(right))
    extra = sorted(set(right) - set(left))
    return False, "缺失=%s 多出=%s" % (missing[:8], extra[:8])


def order_equal(client_tools, outbound_tools):
    left = [t["name"] for t in client_tools]
    right = [t["name"] for t in outbound_tools]
    if left == right:
        return True, "顺序一致"
    limit = min(len(left), len(right))
    for index in range(limit):
        if left[index] != right[index]:
            return False, "首个不同位 [%d]: client %r != outbound %r" % (index, left[index], right[index])
    return False, "前缀一致但长度不同: client %d / outbound %d" % (len(left), len(right))


def params_deep_equal(client_tools, outbound_tools):
    """逐条深比较 parameters，返回 (ok, 说明)。同样先比完公共前缀。"""
    limit = min(len(client_tools), len(outbound_tools))
    for index in range(limit):
        left, right = client_tools[index], outbound_tools[index]
        path, detail = first_param_diff(left["name"], left["parameters"], right["parameters"])
        if path is not None:
            return False, "tool[%d] %s" % (index, detail)
    if len(client_tools) != len(outbound_tools):
        return False, "前 %d 个工具 parameters 深比较一致，但条数不同（client %d / outbound %d）" % (
            limit,
            len(client_tools),
            len(outbound_tools),
        )
    return True, "%d 个工具的 parameters 深比较一致" % len(client_tools)


def strict_absent_or_false(tools):
    """出站每个工具的 strict 必须是 false 或不存在。"""
    for index, tool in enumerate(tools):
        strict = tool.get("strict")
        if strict is not None and strict is not False:
            return False, "tool[%d] %r strict=%r（应为 false 或缺省）" % (index, tool.get("name"), strict)
    return True, "%d 个工具均无 strict 或为 false" % len(tools)


def ua_official(user_agent):
    """User-Agent 是否是官方 CLI 形态。"""
    if not isinstance(user_agent, str) or not user_agent:
        return False, "缺少 User-Agent"
    if UA_PATTERN.match(user_agent):
        return True, user_agent
    return False, "UA %r 不匹配 %s" % (user_agent, UA_PATTERN.pattern)


def passthrough(client_tools, outbound_tools):
    if len(client_tools) == len(outbound_tools):
        return True, "出站 %d 个 == 客户端 %d 个" % (len(outbound_tools), len(client_tools))
    delta = len(outbound_tools) - len(client_tools)
    return False, "客户端 %d -> 出站 %d (%+d)，被改名/补全" % (
        len(client_tools),
        len(outbound_tools),
        delta,
    )


def exit_code(passed, failed, env_error=False):
    """0=全通过，1=有失败，2=环境错误（优先级最高）。"""
    if env_error:
        return 2
    if failed:
        return 1
    if passed:
        return 0
    return 1


# --------------------------------------------------------------------------
# 官方工具定义 / 请求体
# --------------------------------------------------------------------------


def load_tool_definitions(path):
    """读取官方工具定义，兼容两种形态：

    - dict：{"read": {"description":..., "parameters":...}, ...}
      （仓库里的 tools/opencode_tools.json 就是这种，含 patch，共 13 项）
    - list：[{"name": "read", "description":..., "parameters":...}, ...]
      （tools/opencode_tools_2_2.json 是官方 CLI 2.2 的抓包，12 项）
    """
    with open(path, "r", encoding="utf-8") as handle:
        raw = json.load(handle)
    defs = {}
    if isinstance(raw, dict):
        for name, value in raw.items():
            definition = _coerce_definition(name, value)
            if definition:
                defs[definition["name"]] = definition
    elif isinstance(raw, list):
        for entry in raw:
            definition = _coerce_definition(None, entry)
            if definition:
                defs[definition["name"]] = definition
    else:
        raise ValueError("工具定义文件既不是 dict 也不是 list: %s" % path)
    return defs


def _coerce_definition(name, value):
    if not isinstance(value, dict):
        return None
    inner = value["function"] if isinstance(value.get("function"), dict) else value
    tool_name = inner.get("name") or name
    if not tool_name:
        return None
    params = inner.get("parameters")
    if not isinstance(params, dict):
        params = SYNTHETIC_PARAMS
    return {
        "name": tool_name,
        "description": inner.get("description") or "",
        "parameters": params,
    }


def chat_tool(name, definition):
    """拼一条 chat 形态（type=function）的工具定义。"""
    params = definition.get("parameters") if isinstance(definition, dict) else None
    return {
        "type": "function",
        "function": {
            "name": name,
            "description": (definition or {}).get("description") or "",
            "parameters": params if isinstance(params, dict) else SYNTHETIC_PARAMS,
        },
    }


def core_twelve(defs):
    """核心 12 名：官方集去掉 patch 与 experimental_image_generate。

    仓库里的 opencode_tools.json 是 13 项（12 核心 + patch），
    opencode_tools_2_2.json 是 12 项；两种输入都应该得到同一份 12。
    """
    return [name for name in defs if name not in EXTRA_TOOL_NAMES]


def build_scenarios(defs):
    """四个场景：s12 / s14 / s1 / s0。返回 [(name, [tool entries])]。"""
    core = core_twelve(defs)
    fourteen = list(core) + [n for n in EXTRA_TOOL_NAMES if n not in core]
    scenarios = []
    for name, names in (("s12", core), ("s14", fourteen), ("s1", ["read"])):
        tools = [chat_tool(tool_name, defs.get(tool_name, {"parameters": SYNTHETIC_PARAMS})) for tool_name in names]
        scenarios.append((name, tools))
    scenarios.append(("s0", []))
    return scenarios


# --------------------------------------------------------------------------
# 进程编排
# --------------------------------------------------------------------------


def free_port():
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def _http_get(url, timeout=1.0):
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        return resp.status


def wait_http(url, process, timeout, what):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        if process is not None and process.poll() is not None:
            raise EnvError("%s 进程已退出（code=%s）" % (what, process.returncode))
        try:
            if _http_get(url) == 200:
                return True
        except urllib.error.HTTPError as exc:
            last = "HTTP %s" % exc.code
        except (urllib.error.URLError, OSError) as exc:
            last = str(exc)
        time.sleep(0.15)
    raise EnvError("%s 未就绪（%s）: %s" % (what, url, last))


def start_sink(sink_script, port, out_dir):
    cmd = [sys.executable, sink_script, "--port", str(port), "--out", out_dir]
    # stdout 直接继承：sink 每收一发就打一行，跑在这里时能直接看到取证过程。
    proc = subprocess.Popen(cmd)
    try:
        wait_http("http://127.0.0.1:%d/health" % port, proc, 15, "sink")
    except EnvError:
        proc.terminate()
        raise
    return proc


def start_gateway(binary, port, sink_port, log_path, extra_env):
    env = dict(os.environ)
    # 干净对照：不要被宿主环境里的整形开关带跑。
    for key in ("ZEN_TOOL_CHANNEL", "ZEN_TOOL_PREFIX", "DIRECT_UA", "DIRECT_UA_VERSION", "ZEN_DESC_VERBATIM"):
        env.pop(key, None)
    env["PORT"] = str(port)
    env["OPENCODE_ZEN_BASE"] = "http://127.0.0.1:%d/inference/v1" % sink_port
    env["DIRECT_SIDECAR"] = "off"          # 不 spawn bun（loopback upstream 本来也不 spawn）
    env["DIRECT_UA_REFRESH"] = "0"         # 不挂 6h npm 轮询
    env["NO_PROXY"] = ",".join(filter(None, [env.get("NO_PROXY", ""), "127.0.0.1,localhost"]))
    env["no_proxy"] = env["NO_PROXY"]
    # 代理指到 sink：sink 只放行 loopback 的 CONNECT，其余目标 403。
    # 这样网关能到 sink（它的 bunTransport 对 loopback 也要先 CONNECT），
    # 而 npm 版本、出口 IP、Endpoints 表这些背景请求一律秒失败、绝不出网。
    proxy = "http://127.0.0.1:%d" % sink_port
    for key in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"):
        env[key] = proxy
    for item in extra_env or []:
        key, _, value = item.partition("=")
        env[key] = value
    log = open(log_path, "wb")
    proc = subprocess.Popen([binary], stdout=log, stderr=subprocess.STDOUT, env=env, cwd=os.path.dirname(os.path.abspath(binary)) or None)
    proc._conformance_log = log  # noqa: SLF001 — 收尾时关掉句柄
    return proc


def stop_process(proc, name):
    if proc is None:
        return
    try:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
    except Exception:  # noqa: BLE001 — 收尾尽力而为
        try:
            proc.kill()
        except Exception:  # noqa: BLE001
            pass
    log = getattr(proc, "_conformance_log", None)
    if log is not None:
        try:
            log.close()
        except Exception:  # noqa: BLE001
            pass


def tail(path, size=2000):
    try:
        with open(path, "rb") as handle:
            handle.seek(0, os.SEEK_END)
            length = handle.tell()
            handle.seek(max(0, length - size))
            return handle.read().decode("utf-8", "replace")
    except OSError:
        return ""


def sink_files(out_dir):
    files = []
    for name in os.listdir(out_dir):
        if not name.endswith(".json") or name.endswith(".tmp"):
            continue
        seq_text = name.split("-", 1)[0]
        try:
            files.append((int(seq_text), os.path.join(out_dir, name)))
        except ValueError:
            continue
    files.sort()
    return files


def wait_new_sink_file(out_dir, after_seq, timeout=20.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        files = sink_files(out_dir)
        fresh = [item for item in files if item[0] > after_seq]
        if fresh:
            return fresh[-1][1]
        time.sleep(0.1)
    return None


def post_chat(port, body, timeout=60.0):
    data = json.dumps(body, ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(
        "http://127.0.0.1:%d/v1/chat/completions" % port,
        data=data,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": "Bearer conformance",
            "User-Agent": "opencode/latest/0.0.0/cli",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()


# --------------------------------------------------------------------------
# 断言
# --------------------------------------------------------------------------

def evaluate(client_tools_raw, record, protocol="chat"):
    """对一份 sink 落盘记录做全套断言，返回 (assertions, client_tools, outbound_tools)。"""
    body = record.get("body") if isinstance(record, dict) else None
    outbound_raw = body.get("tools") if isinstance(body, dict) else None
    client_tools = normalize_tools(client_tools_raw, protocol)
    outbound_tools = normalize_tools(outbound_raw or [], protocol)
    headers = (record.get("headers_lower") or {}) if isinstance(record, dict) else {}

    checks = {
        "passthrough": passthrough(client_tools, outbound_tools),
        "names_equal": names_equal(client_tools, outbound_tools),
        "order_equal": order_equal(client_tools, outbound_tools),
        "desc_bytes_equal": desc_bytes_equal(client_tools, outbound_tools),
        "desc_text_equal": desc_text_equal(client_tools, outbound_tools),
        "params_deep_equal": params_deep_equal(client_tools, outbound_tools),
        "strict_absent_or_false": strict_absent_or_false(outbound_tools),
        "ua_official": ua_official(headers.get("user-agent")),
    }
    assertions = {}
    for key in ASSERTION_ORDER:
        ok, detail = checks[key]
        assertions[key] = {"ok": bool(ok), "detail": detail}
    return assertions, client_tools, outbound_tools


def summarize(assertions):
    passed = sum(1 for item in assertions.values() if item["ok"])
    failed = len(assertions) - passed
    return passed, failed


# --------------------------------------------------------------------------
# 主流程
# --------------------------------------------------------------------------


def run(args):
    binary = os.path.abspath(args.bin)
    if not os.path.isfile(binary):
        raise EnvError("二进制不存在: %s" % binary)
    here = os.path.dirname(os.path.abspath(__file__))
    sink_script = os.path.join(here, "egress_sink.py")
    if not os.path.isfile(sink_script):
        raise EnvError("找不到 sink: %s" % sink_script)
    tools_path = args.tools
    if not os.path.isabs(tools_path):
        candidate = os.path.join(os.getcwd(), tools_path)
        if not os.path.isfile(candidate):
            candidate = os.path.join(here, os.path.basename(tools_path))
        tools_path = candidate
    if not os.path.isfile(tools_path):
        raise EnvError("工具定义不存在: %s" % args.tools)
    defs = load_tool_definitions(tools_path)
    scenarios = build_scenarios(defs)
    core = core_twelve(defs)
    if len(core) != 12:
        print("[warn] 工具定义里核心官方名是 %d 个（期望 12）: %s" % (len(core), core), file=sys.stderr, flush=True)

    work_root = args.workdir or os.environ.get("ZEN_CONF_TMP") or None
    workdir = tempfile.mkdtemp(prefix="zen-egress-", dir=work_root)
    sink_dir = os.path.join(workdir, "sink")
    gateway_log = os.path.join(workdir, "gateway.log")
    sink_port = args.sink_port or free_port()
    gateway_port = args.gateway_port or free_port()

    sink_proc = None
    gateway_proc = None
    report = {
        "bin": binary,
        "sink_port": sink_port,
        "gateway_port": gateway_port,
        "scenarios": [],
        "passed": 0,
        "failed": 0,
        "ok": False,
    }

    try:
        print("[conf] sink  -> 127.0.0.1:%d  out=%s" % (sink_port, sink_dir), flush=True)
        try:
            sink_proc = start_sink(sink_script, sink_port, sink_dir)
        except EnvError as exc:
            raise EnvError("sink 起不来: %s" % exc) from exc

        print("[conf] gateway -> 127.0.0.1:%d  bin=%s" % (gateway_port, binary), flush=True)
        gateway_proc = start_gateway(binary, gateway_port, sink_port, gateway_log, args.gateway_env)
        try:
            wait_http("http://127.0.0.1:%d/health" % gateway_port, gateway_proc, args.timeout, "网关")
        except EnvError as exc:
            raise EnvError("%s\n--- gateway log ---\n%s" % (exc, tail(gateway_log, 3000))) from exc
        print("[conf] gateway /health 200，开始发请求", flush=True)

        total_passed = 0
        total_failed = 0
        for name, tools in scenarios:
            body = {
                "model": MODEL,
                "messages": [{"role": "user", "content": "egress conformance probe"}],
                "stream": bool(args.stream),
                "max_tokens": 16,
            }
            if tools:
                body["tools"] = tools
            before = sink_files(sink_dir)
            baseline = before[-1][0] if before else 0
            status, _ = post_chat(gateway_port, body, timeout=args.http_timeout)
            path = wait_new_sink_file(sink_dir, baseline, timeout=args.http_timeout)
            entry = {
                "name": name,
                "client_tools": len(tools),
                "outbound_tools": 0,
                "http_status": status,
                "sink_file": path or "",
                "ua": "",
                "client_tool_names": [t["function"]["name"] for t in tools],
                "outbound_tool_names": [],
                "assertions": {},
                "passed": 0,
                "failed": 0,
                "ok": False,
            }
            if path is None:
                failure = {"ok": False, "detail": "sink 没抓到出站 body（网关 HTTP %s）" % status}
                entry["assertions"] = {key: dict(failure) for key in ASSERTION_ORDER}
                entry["failed"] = len(ASSERTION_ORDER)
            else:
                with open(path, "r", encoding="utf-8") as handle:
                    record = json.load(handle)
                assertions, client_norm, outbound_norm = evaluate(tools, record)
                entry["assertions"] = assertions
                entry["outbound_tools"] = len(outbound_norm)
                entry["ua"] = (record.get("headers_lower") or {}).get("user-agent", "")
                entry["outbound_tool_names"] = [t["name"] for t in outbound_norm]
                entry["passed"], entry["failed"] = summarize(assertions)
                entry["ok"] = entry["failed"] == 0
            total_passed += entry["passed"]
            total_failed += entry["failed"]
            report["scenarios"].append(entry)

            verdict = "PASS" if entry["ok"] else "FAIL"
            print(
                "[%s] client_tools=%d outbound_tools=%d %s  (%d/%d 断言通过)"
                % (name, entry["client_tools"], entry["outbound_tools"], verdict, entry["passed"], entry["passed"] + entry["failed"]),
                flush=True,
            )
            for key in ASSERTION_ORDER:
                item = entry["assertions"][key]
                if not item["ok"]:
                    print("       x %-22s %s" % (key, item["detail"]), flush=True)

        report["passed"] = total_passed
        report["failed"] = total_failed
        report["ok"] = total_failed == 0
    finally:
        stop_process(gateway_proc, "gateway")
        stop_process(sink_proc, "sink")
        if args.keep:
            print("[conf] 保留工作目录: %s" % workdir, flush=True)
        else:
            shutil.rmtree(workdir, ignore_errors=True)

    out_dir = os.path.dirname(os.path.abspath(args.out))
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    print(
        "[conf] 断言 %d 通过 / %d 失败 -> %s  (%s)"
        % (report["passed"], report["failed"], "OK" if report["ok"] else "FAIL", os.path.abspath(args.out)),
        flush=True,
    )
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description="网关出站工具契约对拍（零额度、零外网）")
    parser.add_argument("--bin", required=True, help="网关可执行文件")
    parser.add_argument("--out", default="verdict.json", help="报告输出路径")
    parser.add_argument("--tools", default="tools/opencode_tools.json", help="官方工具定义 JSON")
    parser.add_argument("--timeout", type=float, default=40.0, help="网关 /health 就绪超时（秒）")
    parser.add_argument("--http-timeout", type=float, default=60.0, help="单发请求 + sink 落盘超时（秒）")
    parser.add_argument("--sink-port", type=int, default=0, help="固定 sink 端口（默认随机）")
    parser.add_argument("--gateway-port", type=int, default=0, help="固定网关端口（默认随机）")
    parser.add_argument("--stream", action="store_true", help="请求带 stream:true")
    parser.add_argument("--keep", action="store_true", help="保留 sink 落盘目录与网关日志")
    parser.add_argument("--workdir", default="", help="工作目录（默认系统临时目录）")
    parser.add_argument("--gateway-env", action="append", default=[], help="额外网关环境变量 KEY=VALUE，可重复")
    args = parser.parse_args(argv)

    try:
        report = run(args)
    except EnvError as exc:
        print("[conf] 环境错误: %s" % exc, file=sys.stderr, flush=True)
        return 2
    return exit_code(report["passed"], report["failed"])


if __name__ == "__main__":
    sys.exit(main())
