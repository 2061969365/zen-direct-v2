"""shape_acceptance.py — 用真实上游验「更宽松的透传判据」会不会被接受（subset / content）。

背景
    网关判断「这份 tools 能不能整份透传」原本是**集合完全相等**：客户端工具名必须与
    官方 14 个一字不差。官方 CLI 现在只发 12 或 13 个，于是判据永远为假，每个请求都
    被改成 ``mcp__<name>`` 再补 14 个官方桩 —— 出站 26 个，同一批能力出现两次。

    ``toolshape.go`` 新加了三档开关（``ZEN_SHAPE_MODE``）：

        exact   （默认）集合必须完全相等，现有行为
        subset  客户端工具名只要都 ∈ 官方集合就透传
        content subset 之上再要求描述与 parameters 与官方逐字一致

    本脚本负责回答一个只能问上游的问题：这两条更宽松的判据，真上游给 200 还是 429。

为什么要在意额度
    免费层按**出口 IP** 计量，每个 IP 的额度都很小。所以一次跑 6~8 发封顶：
    每个 mode 一个网关实例，4 个场景各发一发；撞 429/402 立刻停掉该 mode 的后续场景
    （记 ``skipped``），绝不硬打。

流程（每个 mode）
    1. 起网关：``PORT=<随机>``、``ZEN_SHAPE_MODE=<mode>``
    2. 等 ``/health`` 200
    3. 逐个场景：读 ``/health`` 的 ``tool_shape.last`` → POST 一发 → 再读一次
    4. 杀掉网关

场景
    s12_pristine   12 个官方名（除 patch、experimental_image_generate），原始定义
    s13_pristine   12 个 + experimental_image_generate，原始定义
    s14_pristine   全部 14 个，原始定义
    s12_rewritten  12 个官方名，仓库当前 ``tools/opencode_tools.json`` 的定义（描述覆盖层改写过）

    原始定义默认从公共资产下载：见 ``ASSET_URL``。下载失败退到仓库的
    ``tools/opencode_tools.json`` 并打 ``::warning::``（报告里 ``pristine_source.fallback``）。

用法
    python tools/shape_acceptance.py --bin ./zen-direct --out acceptance.json \\
        [--pristine PATH] [--modes subset,content] [--only s12_pristine,s14_pristine] [--dry-run]

      真实上游（经 sing-box）：
        env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY \\
          python tools/shape_acceptance.py --bin ./zen-current --out acceptance.json \\
            --gateway-proxy http://127.0.0.1:18080

      本地假上游（零额度）：
        python tools/shape_acceptance.py --bin ./zen-direct.exe --out /tmp/acc.json \\
            --gateway-env OPENCODE_ZEN_BASE=http://127.0.0.1:PORT/inference/v1 \\
            --gateway-env DIRECT_SIDECAR=off --gateway-env DIRECT_UA_REFRESH=0

退出码
    0 = 全部符合预期（每个场景都是 200 且走的是判据预期的分支）
    1 = 有不符合（429 额度、上游报错、或分支与判据不符）
    2 = 环境错误（二进制不存在 / 定义文件缺失 / 网关起不来）
"""

from __future__ import annotations

import argparse
import copy
import datetime
import json
import os
import platform
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

# 官方工具名全集（与 toolshape.go 的 v2OfficialToolOrder 同序）。
OFFICIAL_ORDER = [
    "edit", "execute", "glob", "grep", "question", "read", "shell",
    "skill", "subagent", "webfetch", "websearch", "write", "patch",
    "experimental_image_generate",
]
# 「后加的两个」：核心 12 名之外。
EXTRA_NAMES = ("patch", "experimental_image_generate")
CORE_NAMES = [n for n in OFFICIAL_ORDER if n not in EXTRA_NAMES]

MODEL = "deepseek-v4.1-flash-free"
ASSET_URL = "https://raw.githubusercontent.com/2061969365/zen-assets-runner/main/assets/official_tools.json"

VALID_MODES = ("exact", "subset", "content")
VALID_SCENARIOS = ("s12_pristine", "s13_pristine", "s14_pristine", "s12_rewritten",
                   "s12_dsh", "s12_banned")

# 额度指纹：命中即认为「这份样本作废」，不能算成「上游拒绝了判据」。
QUOTA_KEYS = ("freeusagelimit", "usage limit", "quota", "rate limit")
QUOTA_STATUS = (402, 429)

# 网关对「官方集里没有定义的名字」用的兜底形状（见 toolshape.go renderOfficialTool）。
STUB_DESCRIPTION = "Agent tool %s"
STUB_PARAMETERS = {"type": "object", "properties": {}}

HEALTH_TIMEOUT = 5.0


class EnvError(RuntimeError):
    """环境错误：二进制缺失、定义缺失、网关起不来（退出码 2）。"""


# --------------------------------------------------------------------------
# 纯函数区：定义装载、场景构造、归类、汇总（单测只碰这些）
# --------------------------------------------------------------------------


def _coerce_definition(name, value):
    """把一条定义摊平成 {description, parameters}；形状不认识返回 None。"""
    if not isinstance(value, dict):
        return None
    inner = value["function"] if isinstance(value.get("function"), dict) else value
    tool_name = inner.get("name") or name
    if not tool_name:
        return None
    params = inner.get("parameters")
    if not isinstance(params, dict):
        params = dict(STUB_PARAMETERS)
    return tool_name, {
        "description": inner.get("description") or "",
        "parameters": params,
    }


def load_definitions(payload):
    """把三种已知形状统一成 {name: {description, parameters}}。

    - ``{order: [...], tools: {name: {...}}}``：公共资产 official_tools.json
    - ``{name: {...}}``：仓库 tools/opencode_tools.json
    - ``[{name, description, parameters}, ...]`` / `[{type, function:{...}}]`：抓包
    """
    defs = {}
    if isinstance(payload, dict) and isinstance(payload.get("tools"), dict):
        source = payload["tools"]
    elif isinstance(payload, dict):
        source = payload
    elif isinstance(payload, list):
        source = payload
    else:
        raise ValueError("工具定义既不是 dict 也不是 list")

    if isinstance(source, list):
        for entry in source:
            coerced = _coerce_definition(None, entry)
            if coerced:
                defs[coerced[0]] = coerced[1]
        return defs
    for name, value in source.items():
        if name in ("cli_version", "captured_at", "source", "order"):
            continue
        coerced = _coerce_definition(name, value)
        if coerced:
            defs[coerced[0]] = coerced[1]
    return defs


def load_definitions_file(path):
    with open(path, "r", encoding="utf-8") as handle:
        return load_definitions(json.load(handle))


def download_pristine(url, dest, timeout=25.0):
    """下载公共资产到 dest；失败抛异常（调用方决定是否降级）。"""
    os.makedirs(os.path.dirname(os.path.abspath(dest)) or ".", exist_ok=True)
    request = urllib.request.Request(url, headers={"User-Agent": "zen-shape-acceptance/1"})
    with urllib.request.urlopen(request, timeout=timeout) as resp:
        raw = resp.read()
    with open(dest, "wb") as handle:
        handle.write(raw)
    return load_definitions(json.loads(raw.decode("utf-8", "replace")))


def resolve_pristine(args, workdir):
    """按 --pristine > 下载 > 仓库文件 的优先级拿到「原始定义」。

    返回 (defs, source)。source 里记录实际来源与是否降级，报告要照实写出来：
    降级后的「原始定义」其实就是仓库里那份被改写过的定义，s12_pristine 与
    s12_rewritten 会退化成同一个东西，结论不能当成真原始定义来读。
    """
    if args.pristine:
        path = os.path.abspath(args.pristine)
        if not os.path.isfile(path):
            raise EnvError("--pristine 指定的文件不存在: %s" % path)
        return load_definitions_file(path), {"path": path, "origin": "local", "fallback": False}

    dest = os.path.join(workdir, "official_tools.json")
    try:
        defs = download_pristine(ASSET_URL, dest)
        if not defs:
            raise ValueError("资产是空的")
        return defs, {"path": dest, "origin": "download", "url": ASSET_URL, "fallback": False}
    except Exception as exc:  # noqa: BLE001 — 下载失败要降级，不能整轮作废
        fallback = os.path.abspath(args.tools_file)
        print("::warning::原始定义下载失败（%s），退到本地 %s；本轮 pristine 场景等于改写版"
              % (exc, fallback), flush=True)
        return load_definitions_file(fallback), {
            "path": fallback,
            "origin": "fallback",
            "url": ASSET_URL,
            "fallback": True,
            "error": str(exc),
        }


#: s12_dsh 用的参数级描述：2026-11-17 实测被判过的正是这一类文本
#: （官方工具名 + 客户端自有 schema 文本 → 上游判「自称官方但 schema 不是原文」）。
#: 这是 A 判据（只看名字）的反证场景：subset 会放行它，但上游应当 429。
DSH_PARAM_DESCRIPTIONS = {
    "path": "Path to edit, resolved by the filesystem backend",
    "oldString": "Exact text to replace, taken from the numbered read output",
    "newString": "Replacement text; strip the line-number prefix first",
    "content": "Full contents to write as a utf-8 text file",
    "pattern": "Glob pattern matched against paths exposed by the filesystem backend",
}


def apply_dsh_text(tools):
    """把参数级描述换成 DSH 风格，模拟「官方名 + 自有 schema 文本」。"""
    for entry in tools:
        fn = entry.get("function") or {}
        params = fn.get("parameters") or {}
        props = params.get("properties")
        if not isinstance(props, dict):
            continue
        for pname, prop in props.items():
            if isinstance(prop, dict) and pname in DSH_PARAM_DESCRIPTIONS:
                prop["description"] = DSH_PARAM_DESCRIPTIONS[pname]
    return tools


#: s12_banned 用的工具级描述：直接抄 toolshape.go 注释里二分定位出来的原句。
#: 这是「官方名 + 被判过的文本」的组合，用来验 subset（只看名字）会不会放行它。
#: 已知历史结论：这个形态 429；如果今天复现不出来，说明上游判定变了。
DSH_TOOL_DESCRIPTIONS = {
    "read": "Read a UTF-8 text file and return line-numbered content.",
    "skill": "Load the requested skill from the session skill catalog.",
    "write": "Write a UTF-8 text file to the local filesystem.",
}


def apply_banned_tool_text(tools):
    """把工具级描述换成当年实测被判 429 的原句（名字和参数保持官方原文）。"""
    hit = 0
    for entry in tools:
        fn = entry.get("function") or {}
        name = fn.get("name")
        text = DSH_TOOL_DESCRIPTIONS.get(name)
        if text:
            fn["description"] = text
            hit += 1
    return tools, hit


def scenario_names(scenario):
    """场景 → 工具名列表（顺序跟官方集一致）。"""
    if scenario in ("s12_pristine", "s12_rewritten", "s12_dsh", "s12_banned"):
        return list(CORE_NAMES)
    if scenario == "s13_pristine":
        return list(CORE_NAMES) + ["experimental_image_generate"]
    if scenario == "s14_pristine":
        return list(OFFICIAL_ORDER)
    raise KeyError(scenario)


def definition_for(name, defs):
    """取一份定义；缺失时用网关同款兜底形状（experimental_image_generate 就是这种）。

    返回深拷贝：场景之间共享同一份参数子树的话，任何就地改写都会污染下一发请求
    （也污染「网关官方定义」那份参考对象），测出来的分支就不可信了。
    """
    found = defs.get(name)
    if found:
        return copy.deepcopy({
            "description": found.get("description") or "",
            "parameters": found.get("parameters") if isinstance(found.get("parameters"), dict)
            else dict(STUB_PARAMETERS),
        })
    return {"description": STUB_DESCRIPTION % name, "parameters": dict(STUB_PARAMETERS)}


def chat_tool(name, definition):
    return {
        "type": "function",
        "function": {
            "name": name,
            "description": definition.get("description") or "",
            "parameters": definition.get("parameters") or dict(STUB_PARAMETERS),
        },
    }


def build_tools(names, defs):
    return [chat_tool(name, definition_for(name, defs)) for name in names]


def scenario_tools(scenario, pristine_defs, rewritten_defs):
    """场景 → chat 形态的 tools 数组。"""
    defs = rewritten_defs if scenario == "s12_rewritten" else pristine_defs
    tools = build_tools(scenario_names(scenario), defs)
    if scenario == "s12_dsh":
        apply_dsh_text(tools)
    if scenario == "s12_banned":
        apply_banned_tool_text(tools)
    return tools


def gateway_official_defs(repo_defs):
    """网关眼里的「官方定义」= 内嵌 tools/opencode_tools.json（+ windows 的 shell 改写）。

    toolshape.go 里 renderOfficialTool 对 shell 在 Windows 上把
    "on Linux using bash" 换成 "on Windows using powershell"，content 判据比的是
    改写后的文本。这里复刻同一条规则，否则本地（Windows）算出来的预期分支会和
    网关实际行为对不上。Linux runner 上这一步是恒等变换。
    """
    out = {}
    for name, definition in repo_defs.items():
        description = definition.get("description") or ""
        if name == "shell" and platform.system() == "Windows":
            description = description.replace("on Linux using bash", "on Windows using powershell", 1)
        params = definition.get("parameters")
        out[name] = {
            "description": description,
            "parameters": params if isinstance(params, dict) else dict(STUB_PARAMETERS),
        }
    return out


def content_matches(tools, official):
    """逐条比对描述与 parameters；缺失的官方项用兜底形状（与网关一致）。"""
    for entry in tools:
        fn = entry.get("function") or {}
        name = fn.get("name") or ""
        want = official.get(name) or {
            "description": STUB_DESCRIPTION % name,
            "parameters": STUB_PARAMETERS,
        }
        if (fn.get("description") or "") != want["description"]:
            return False
        if (fn.get("parameters") or {}) != want["parameters"]:
            return False
    return True


def expected_branch(mode, tools, official):
    """按判据推出「这次请求应当走哪条分支」，用于判断结果是否合规。

    exact   —— 名字集合必须与官方 14 个完全相等
    subset  —— 名字都落在官方集合内
    content —— subset + 描述/参数与网关官方定义逐字一致
    """
    names = [(t.get("function") or {}).get("name") or "" for t in tools]
    if not tools:
        return "none"
    official_names = set(OFFICIAL_ORDER)
    if mode == "exact":
        return "passthrough" if set(names) == official_names else "rename_complete"
    if not set(names).issubset(official_names):
        return "rename_complete"
    if mode == "subset":
        return "passthrough"
    return "passthrough" if content_matches(tools, official) else "rename_complete"


def quota_suspected(status, body_text):
    """额度判定：429/402，或 body 里点名额度。

    必须与「网关自己回 4xx」（比如上游 400 invalid tool）区分开：后者是判据被拒，
    前者是这份样本作废。混在一起会把「上游不接受 subset」错读成「额度不够」。
    """
    if status in QUOTA_STATUS:
        return True
    text = (body_text or "").lower()
    return any(key in text for key in QUOTA_KEYS)


def body_head(body_text, limit=200):
    text = (body_text or "").replace("\r", " ").replace("\n", " ")
    return text[:limit]


def upstream_error(status, body_text):
    """非 200 时抽一条人能读的错误；200 返回 None。"""
    if status == 200:
        return None
    text = body_text or ""
    try:
        data = json.loads(text)
    except Exception:  # noqa: BLE001 — 不是 JSON 就走原文
        return text[:300] or "http %s" % status
    if isinstance(data, dict):
        err = data.get("error")
        if isinstance(err, dict):
            for key in ("message", "type", "code"):
                if err.get(key):
                    return str(err[key])[:300]
        for key in ("message", "detail", "error"):
            if isinstance(data.get(key), str) and data[key]:
                return data[key][:300]
    return text[:300] or "http %s" % status


def verdict(record):
    """hit / quota / error 三分类（汇总表与退出码都用它）。"""
    if record.get("skipped"):
        return "quota" if record.get("skipped") == "quota" else "error"
    if record.get("quota_suspected"):
        return "quota"
    if record.get("http") == 200 and record.get("branch_ok"):
        return "hit"
    return "error"


def format_table(records):
    """mode × scenario → branch / http / verdict。返回表格行列表。"""
    header = "%-8s %-14s %-18s %-16s %-6s %s" % (
        "mode", "scenario", "branch", "expected", "http", "verdict")
    lines = [header, "-" * len(header)]
    for record in records:
        branch = record.get("gateway_branch") or "-"
        expected = record.get("expected_branch") or "-"
        http = record.get("http")
        lines.append("%-8s %-14s %-18s %-16s %-6s %s" % (
            record.get("mode", "-"),
            record.get("scenario", "-"),
            branch,
            expected,
            "-" if http is None else http,
            verdict(record),
        ))
    return lines


def judgement(records):
    """一句话判词。"""
    lines = []
    subset_rows = [r for r in records if r.get("mode") == "subset" and r.get("scenario") == "s12_pristine"]
    subset_rows = [r for r in subset_rows if not r.get("skipped")]
    if subset_rows:
        row = subset_rows[0]
        if row.get("quota_suspected"):
            lines.append("subset 不可用，官方名 + 非完整集合会被上游拒")
        elif row.get("http") == 200:
            lines.append("subset 可用")
        else:
            lines.append("subset 未能确认（http=%s）" % row.get("http"))

    dsh_rows = [r for r in records if r.get("mode") == "subset" and r.get("scenario") == "s12_dsh"]
    for row in dsh_rows:
        if row.get("skipped"):
            lines.append("subset/s12_dsh 未测（额度）")
            continue
        http = row.get("http")
        if row.get("quota_suspected") or http == 429:
            lines.append("subset/s12_dsh 被上游拒（http=%s）→ 只看名字不够，必须配文本校验"
                         % http)
        elif http == 200:
            lines.append("subset/s12_dsh 也 200 → 文本校验不是必需的（与历史结论相反，需复核）")
        else:
            lines.append("subset/s12_dsh http=%s" % http)

    banned_rows = [r for r in records if r.get("mode") == "subset" and r.get("scenario") == "s12_banned"]
    for row in banned_rows:
        if row.get("skipped"):
            lines.append("subset/s12_banned 未测（额度）")
            continue
        http = row.get("http")
        if row.get("quota_suspected") or http == 429:
            lines.append("subset/s12_banned 被拒（http=%s）→ 只看名字不够，必须加文本校验"
                         % http)
        elif http == 200:
            lines.append("subset/s12_banned 也 200 → 描述指纹今天复现不出来，subset 可单独用")
        else:
            lines.append("subset/s12_banned http=%s" % http)

    content_rows = [r for r in records if r.get("mode") == "content"]
    for scenario in ("s14_pristine", "s12_rewritten", "s12_pristine", "s13_pristine"):
        for row in content_rows:
            if row.get("scenario") != scenario:
                continue
            if row.get("skipped"):
                lines.append("content/%s 未测（额度）" % scenario)
                continue
            branch = row.get("gateway_branch") or "-"
            http = row.get("http")
            if branch == "passthrough" and http == 200:
                lines.append("content/%s 透传且上游 200 → 官方定义可直接过闸" % scenario)
            elif branch == "passthrough":
                lines.append("content/%s 透传但上游 %s" % (scenario, http))
            else:
                lines.append("content/%s 未透传（%s，http=%s）→ 这一发没验到 content 判据"
                             % (scenario, branch, http))
    if not lines:
        lines.append("没有可判定的样本")
    return lines


def exit_code(records, env_error=False):
    if env_error:
        return 2
    if not records:
        return 1
    return 0 if all(verdict(r) == "hit" for r in records) else 1


# --------------------------------------------------------------------------
# 进程 / HTTP
# --------------------------------------------------------------------------


def free_port():
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def build_opener():
    """探针只连 127.0.0.1：完全绕开宿主代理。

    网关把 HTTP(S)_PROXY 当**全局**代理（连 127.0.0.1 也先 CONNECT），所以探针这边
    不能靠 NO_PROXY 走运气，直接给一个空 ProxyHandler。
    """
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def health(opener, port, timeout=HEALTH_TIMEOUT):
    """读 /health，返回 (ok, payload)。"""
    try:
        with opener.open("http://127.0.0.1:%d/health" % port, timeout=timeout) as resp:
            raw = resp.read()
        if resp.status != 200:
            return False, None
        return True, json.loads(raw.decode("utf-8", "replace"))
    except Exception:  # noqa: BLE001 — 起停窗口里失败是正常的
        return False, None


def health_branch(opener, port):
    """取 tool_shape.last.mode（没发生过整形时返回 None）。"""
    ok, payload = health(opener, port)
    if not ok or not isinstance(payload, dict):
        return None
    shape = payload.get("tool_shape") or {}
    last = shape.get("last") or {}
    return last.get("mode") or None


def wait_health(opener, port, process, timeout, what):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        if process is not None and process.poll() is not None:
            raise EnvError("%s 进程已退出（code=%s）" % (what, process.returncode))
        ok, _ = health(opener, port, timeout=2.0)
        if ok:
            return True
        last = "未就绪"
        time.sleep(0.2)
    raise EnvError("%s 未就绪（http://127.0.0.1:%d/health）: %s" % (what, port, last))


def start_gateway(args, binary, port, mode, log_path, workdir):
    env = dict(os.environ)
    # 干净对照：宿主里的整形开关会污染这次测量。
    for key in ("ZEN_TOOL_CHANNEL", "ZEN_TOOL_PREFIX", "DIRECT_UA", "DIRECT_UA_VERSION",
                "ZEN_DESC_VERBATIM", "ZEN_SHAPE_MODE"):
        env.pop(key, None)
    env["PORT"] = str(port)
    env["ZEN_SHAPE_MODE"] = mode
    # 探针自己连 127.0.0.1 不看代理；这里只保证网关到本机不被代理劫走。
    env["NO_PROXY"] = ",".join(filter(None, [env.get("NO_PROXY", ""), "127.0.0.1,localhost"]))
    env["no_proxy"] = env["NO_PROXY"]
    if args.gateway_proxy:
        # 真实上游要经 sing-box；探针进程本身被 env -u 清过代理，所以在这里显式给网关。
        for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
            env[key] = args.gateway_proxy
    for item in args.gateway_env or []:
        key, _, value = item.partition("=")
        env[key] = value
    if args.upstream:
        env["OPENCODE_ZEN_BASE"] = args.upstream
    log = open(log_path, "wb")
    proc = subprocess.Popen(
        [binary],
        stdout=log,
        stderr=subprocess.STDOUT,
        env=env,
        cwd=os.path.dirname(binary) or workdir,
    )
    proc._shape_log = log  # noqa: SLF001 — 收尾时关掉句柄
    return proc


def stop_gateway(proc):
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
    log = getattr(proc, "_shape_log", None)
    if log is not None:
        try:
            log.close()
        except Exception:  # noqa: BLE001
            pass


def tail(path, size=3000):
    try:
        with open(path, "rb") as handle:
            handle.seek(0, os.SEEK_END)
            length = handle.tell()
            handle.seek(max(0, length - size))
            return handle.read().decode("utf-8", "replace")
    except OSError:
        return ""


def post_chat(opener, port, body, timeout):
    """向网关发一发 chat completions，返回 (status, body_text)。"""
    data = json.dumps(body, ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(
        "http://127.0.0.1:%d/v1/chat/completions" % port,
        data=data,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": "Bearer acceptance",
            "User-Agent": "opencode/latest/0.0.0/cli",
        },
    )
    try:
        with opener.open(request, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    except Exception as exc:  # noqa: BLE001 — 连不上也要归成一条记录
        return 0, "transport: %s" % exc


def request_body(tools):
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "shape acceptance probe"}],
        "stream": False,
        "max_tokens": 16,
    }
    if tools:
        body["tools"] = tools
        body["tool_choice"] = "auto"
    return body


# --------------------------------------------------------------------------
# 主流程
# --------------------------------------------------------------------------


def probe_mode(args, binary, mode, scenarios, pristine_defs, rewritten_defs, official, workdir, opener):
    """跑一个 mode：起网关 → 逐场景一发 → 杀网关。返回记录列表。"""
    port = free_port()
    log_path = os.path.join(workdir, "gateway-%s.log" % mode)
    records = []
    proc = None
    try:
        proc = start_gateway(args, binary, port, mode, log_path, workdir)
        try:
            wait_health(opener, port, proc, args.timeout, "网关[%s]" % mode)
        except EnvError as exc:
            raise EnvError("%s\n--- gateway[%s] log ---\n%s"
                           % (exc, mode, tail(log_path))) from exc
        print("[%s] 网关就绪 http://127.0.0.1:%d（日志 %s）" % (mode, port, log_path), flush=True)

        quota_hit = False
        for scenario in scenarios:
            tools = scenario_tools(scenario, pristine_defs, rewritten_defs)
            expect = expected_branch(mode, tools, official)
            record = {
                "mode": mode,
                "scenario": scenario,
                "client_tools": len(tools),
                "client_tool_names": [t["function"]["name"] for t in tools],
                "gateway_branch": None,
                "gateway_branch_before": None,
                "gateway_branch_after": None,
                "expected_branch": expect,
                "branch_ok": False,
                "http": None,
                "upstream_error": None,
                "body_head": "",
                "latency_ms": 0,
                "quota_suspected": False,
                "skipped": None,
                "definitions": "rewritten" if scenario.endswith("_rewritten") else "pristine",
            }
            if quota_hit:
                record["skipped"] = "quota"
                record["body_head"] = "skipped: 本 mode 已经撞过额度，不再硬打"
                records.append(record)
                continue

            before = health_branch(opener, port)
            started = time.time()
            status, text = post_chat(opener, port, request_body(tools), args.http_timeout)
            latency = int((time.time() - started) * 1000)
            after = health_branch(opener, port)

            record["gateway_branch_before"] = before
            record["gateway_branch_after"] = after
            record["gateway_branch"] = after or before
            record["http"] = status
            record["body_head"] = body_head(text)
            record["latency_ms"] = latency
            record["quota_suspected"] = quota_suspected(status, text)
            record["upstream_error"] = upstream_error(status, text)
            record["branch_ok"] = record["gateway_branch"] == expect
            records.append(record)

            if record["quota_suspected"]:
                quota_hit = True

            print("[%s] %-13s branch=%-15s expect=%-15s http=%-4s %sms %s"
                  % (mode, scenario, record["gateway_branch"] or "-", expect, status, latency,
                     verdict(record)), flush=True)
            if status != 200 and not record["quota_suspected"]:
                print("      error: %s" % (record["upstream_error"] or "")[:200], flush=True)
            if record["quota_suspected"]:
                print("      撞额度：%s 后面的场景记 skipped"
                      % ("429/402" if status in QUOTA_STATUS else "body 命中额度指纹"), flush=True)
    finally:
        stop_gateway(proc)
    return records


def dry_run(args, scenarios, modes, pristine_defs, rewritten_defs, official, pristine_source):
    """打印请求矩阵，不发任何请求。"""
    print("[dry-run] 上游: %s" % (args.upstream or "(网关默认 https://opencode.ai/inference/v1)"))
    print("[dry-run] 原始定义: %s%s" % (
        pristine_source.get("path"),
        "（降级：下载失败，用的就是仓库改写版）" if pristine_source.get("fallback") else ""))
    print("[dry-run] 网关代理: %s" % (args.gateway_proxy or "-"))
    print()
    header = "%-8s %-14s %-6s %-16s %-9s %s" % (
        "mode", "scenario", "tools", "expected", "bytes", "names")
    print(header)
    print("-" * len(header))
    total = 0
    for mode in modes:
        for scenario in scenarios:
            tools = scenario_tools(scenario, pristine_defs, rewritten_defs)
            body = request_body(tools)
            expect = expected_branch(mode, tools, official)
            names = ",".join(t["function"]["name"] for t in tools[:3])
            if len(tools) > 3:
                names += ",…"
            print("%-8s %-14s %-6d %-16s %-9d %s" % (
                mode, scenario, len(tools), expect, len(json.dumps(body)), names))
            total += 1
    print()
    print("[dry-run] 共 %d 发（%d mode × %d 场景），未发任何请求" % (total, len(modes), len(scenarios)))
    return 0


def run(args):
    binary = os.path.abspath(args.bin)
    if not os.path.isfile(binary):
        raise EnvError("二进制不存在: %s" % binary)

    modes = [m.strip() for m in (args.modes or "").split(",") if m.strip()]
    if not modes:
        raise EnvError("--modes 不能为空")
    for mode in modes:
        if mode not in VALID_MODES:
            raise EnvError("未知 mode: %s（可选 %s）" % (mode, ",".join(VALID_MODES)))

    scenarios = [s.strip() for s in (args.only or "").split(",") if s.strip()] or list(VALID_SCENARIOS)
    for scenario in scenarios:
        if scenario not in VALID_SCENARIOS:
            raise EnvError("未知场景: %s（可选 %s）" % (scenario, ",".join(VALID_SCENARIOS)))

    rewritten_path = os.path.abspath(args.tools_file)
    if not os.path.isfile(rewritten_path):
        raise EnvError("改写版定义不存在: %s" % rewritten_path)
    rewritten_defs = load_definitions_file(rewritten_path)
    official = gateway_official_defs(rewritten_defs)

    workdir = args.workdir or tempfile.mkdtemp(prefix="zen-shape-")
    os.makedirs(workdir, exist_ok=True)
    if args.workdir:
        workdir = os.path.abspath(workdir)
    try:
        pristine_defs, pristine_source = resolve_pristine(args, workdir)
        if args.dry_run:
            return dry_run(args, scenarios, modes, pristine_defs, rewritten_defs,
                           official, pristine_source)

        opener = build_opener()
        records = []
        for mode in modes:
            records.extend(probe_mode(args, binary, mode, scenarios, pristine_defs,
                                      rewritten_defs, official, workdir, opener))
    finally:
        if not args.keep and not args.workdir:
            shutil.rmtree(workdir, ignore_errors=True)
        elif args.keep:
            print("[shape] 保留工作目录: %s" % workdir, flush=True)

    table = format_table(records)
    print()
    for line in table:
        print(line)
    print()
    lines = judgement(records)
    for line in lines:
        print("[判词] %s" % line)

    # 每条记录自带归类，报告与 CI 汇总表都用同一个判定，不在别处重算。
    for record in records:
        record["verdict"] = verdict(record)

    report = {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "bin": binary,
        "upstream": args.upstream or "https://opencode.ai/inference/v1",
        "gateway_proxy": args.gateway_proxy or "",
        "modes": modes,
        "scenarios": scenarios,
        "tool_file": rewritten_path,
        "pristine_source": pristine_source,
        "results": records,
        "table": table,
        "judgement": lines,
        "quota": sum(1 for r in records if verdict(r) == "quota"),
        "hits": sum(1 for r in records if verdict(r) == "hit"),
        "errors": sum(1 for r in records if verdict(r) == "error"),
        "ok": all(verdict(r) == "hit" for r in records) and bool(records),
    }
    out_dir = os.path.dirname(os.path.abspath(args.out))
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    print("[shape] hit=%d quota=%d error=%d -> %s"
          % (report["hits"], report["quota"], report["errors"], os.path.abspath(args.out)),
          flush=True)
    return exit_code(records)


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="用真实上游验 subset/content 透传判据（约 6~8 发，省额度）")
    parser.add_argument("--bin", required=True, help="网关可执行文件")
    parser.add_argument("--out", default="acceptance.json", help="报告输出路径")
    parser.add_argument("--pristine", default="", help="原始官方定义（默认从公共资产下载）")
    parser.add_argument("--tools-file", default=os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "tools", "opencode_tools.json"),
        help="仓库当前（改写版）工具定义")
    parser.add_argument("--modes", default="subset,content", help="逗号分隔：exact,subset,content")
    parser.add_argument("--only", default="", help="只跑指定场景，逗号分隔")
    parser.add_argument("--upstream", default="", help="上游 base（默认用网关内置值）")
    parser.add_argument("--gateway-proxy", default="", help="只给网关用的代理（如 http://127.0.0.1:18080）")
    parser.add_argument("--gateway-env", action="append", default=[], help="额外网关环境变量 KEY=VALUE，可重复")
    parser.add_argument("--timeout", type=float, default=60.0, help="网关 /health 就绪超时（秒）")
    parser.add_argument("--http-timeout", type=float, default=90.0, help="单发请求超时（秒）")
    parser.add_argument("--workdir", default="", help="工作目录（默认临时目录）")
    parser.add_argument("--keep", action="store_true", help="保留日志与下载的资产")
    parser.add_argument("--dry-run", action="store_true", help="只打印请求矩阵，不发请求")
    args = parser.parse_args(argv)

    try:
        return run(args)
    except EnvError as exc:
        print("[shape] 环境错误: %s" % exc, file=sys.stderr, flush=True)
        return 2
    except (OSError, ValueError) as exc:
        print("[shape] 环境错误: %s" % exc, file=sys.stderr, flush=True)
        return 2


if __name__ == "__main__":
    sys.exit(main())
