#!/usr/bin/env python3
"""asset_probe.py — 对本地 zen-direct 网关发真实请求，判定工具调用链路是否正常。

固定接口（对接方按这个调用，参数名不要改）:

    python3 tools/asset_probe.py --base http://127.0.0.1:8081 \
        --model deepseek-v4.1-flash-free --out verdict.json --timeout 120

退出码:
    0  选中的 scenario 全部通过
    1  至少一个 scenario 失败
    2  参数或环境错误（工具定义缺失 / 网关没起来 / 报告写不出去）

行为:
    * 每个 scenario 发一次 POST {base}/v1/chat/completions（OpenAI 兼容、非流式）;
    * 每个 scenario 请求前后各读一次 GET {base}/health，比对 up429 / up5xx 增量;
    * 工具定义直接取自 tools/opencode_tools.json（13 个真实定义），不自己编 schema;
    * 不读任何凭证、不打印任何 token；鉴权由网关自己处理。

scenario:
    plain        无 tools，"Say hi in one word."
    single-tool  1 个工具 read，诱导读文件
    multi-tool   5 个工具 read/glob/grep/write/shell，诱导调用其中一个
    many-tools   12 个官方工具，tool_choice=auto，只要求 HTTP 200
"""

from __future__ import annotations

import argparse
import json
import socket
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

USER_AGENT = "zen-asset-probe/1.0"
DEFAULT_TIMEOUT = 120.0
HEALTH_TIMEOUT_CAP = 10.0
MAX_TOKENS = 512
BODY_CLIP = 500
STAT_KEYS = ("reqs", "up429", "up4xx", "up5xx", "circuit_open")
WIRE_PREFIXES = ("mcp_", "clint_")
CHAT_PATH = "/v1/chat/completions"
HEALTH_PATH = "/health"

# 官方工具名顺序（与网关 v2OfficialToolOrder 对齐；opencode_tools.json 里没有
# experimental_image_generate，所以只取前 12 个真实存在的定义）。
MANY_TOOL_NAMES = (
    "edit",
    "execute",
    "glob",
    "grep",
    "question",
    "read",
    "shell",
    "skill",
    "subagent",
    "webfetch",
    "websearch",
    "write",
)

SCENARIOS = (
    {
        "name": "plain",
        "tools": (),
        "tool_choice": None,
        "prompt": "Say hi in one word.",
    },
    {
        "name": "single-tool",
        "tools": ("read",),
        "tool_choice": "auto",
        "prompt": "Read the file /etc/hostname and tell me what it says.",
    },
    {
        "name": "multi-tool",
        "tools": ("read", "glob", "grep", "write", "shell"),
        "tool_choice": "auto",
        "prompt": (
            "Use the grep tool to find the string TODO in the files under the "
            "current directory, then tell me which file matched."
        ),
    },
    {
        "name": "many-tools",
        "tools": MANY_TOOL_NAMES,
        "tool_choice": "auto",
        "prompt": "Say hi in one word.",
    },
)

SCENARIO_NAMES = tuple(spec["name"] for spec in SCENARIOS)


class ProbeError(Exception):
    """参数 / 环境错误，映射到退出码 2。"""


# ---------------------------------------------------------------------------
# 工具定义
# ---------------------------------------------------------------------------


def default_tools_path() -> Path:
    """脚本同目录下的 opencode_tools.json，回退到 cwd/tools/。"""
    here = Path(__file__).resolve().parent / "opencode_tools.json"
    if here.is_file():
        return here
    return Path.cwd() / "tools" / "opencode_tools.json"


def load_tool_definitions(path: Path) -> dict:
    """读取 {name: {description, parameters}} 并转成 OpenAI function 形状。"""
    try:
        raw = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ProbeError(f"读不到工具定义 {path}: {exc}") from exc

    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ProbeError(f"工具定义不是合法 JSON {path}: {exc}") from exc

    if not isinstance(doc, dict) or not doc:
        raise ProbeError(
            f"工具定义格式不认识 {path}: 期望 {{name: {{description, parameters}}}}"
        )

    definitions = {}
    for name, body in doc.items():
        if not isinstance(name, str) or not isinstance(body, dict):
            raise ProbeError(f"工具定义 {path} 里 {name!r} 不是 object")
        parameters = body.get("parameters")
        if not isinstance(parameters, dict) or parameters.get("type") != "object":
            raise ProbeError(f"工具 {name} 的 parameters 不是 object schema")
        definitions[name] = {
            "type": "function",
            "function": {
                "name": name,
                "description": body.get("description") or "",
                "parameters": parameters,
            },
        }
    return definitions


def build_payload(model: str, spec: dict, definitions: dict) -> dict:
    """按接口约定拼最小请求体：只有 model/messages/tools?/tool_choice?/max_tokens。"""
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": spec["prompt"]}],
        "max_tokens": MAX_TOKENS,
    }
    if spec["tools"]:
        missing = [name for name in spec["tools"] if name not in definitions]
        if missing:
            raise ProbeError(
                "opencode_tools.json 缺少官方工具定义: " + ", ".join(missing)
            )
        payload["tools"] = [definitions[name] for name in spec["tools"]]
        if spec["tool_choice"]:
            payload["tool_choice"] = spec["tool_choice"]
    return payload


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


def clip(text: str, limit: int = BODY_CLIP) -> str:
    return text[:limit]


def now_iso() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def normalize_base(base: str) -> str:
    base = (base or "").strip().rstrip("/")
    if not base:
        raise ProbeError("--base 不能为空")
    if not (base.startswith("http://") or base.startswith("https://")):
        raise ProbeError(f"--base 必须是 http(s) URL，收到 {base!r}")
    if any(ch.isspace() for ch in base):
        raise ProbeError(f"--base 里不能有空白字符，收到 {base!r}")
    return base


def health_timeout(request_timeout: float) -> float:
    return min(HEALTH_TIMEOUT_CAP, max(1.0, request_timeout))


def normalize_stats(raw) -> dict:
    out = {key: None for key in STAT_KEYS}
    if isinstance(raw, dict):
        for key in STAT_KEYS:
            value = raw.get(key)
            out[key] = value if isinstance(value, int) and not isinstance(value, bool) else None
    return out


def read_health(base: str, timeout: float) -> dict:
    """GET {base}/health，返回 {url,http,ok,stats,error,...}；永不抛异常。"""
    url = base + HEALTH_PATH
    entry = {
        "url": url,
        "http": None,
        "ok": False,
        "stats": normalize_stats(None),
        "error": None,
    }
    try:
        request = urllib.request.Request(
            url,
            method="GET",
            headers={"Accept": "application/json", "User-Agent": USER_AGENT},
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            entry["http"] = response.getcode()
            body = response.read()
    except urllib.error.HTTPError as exc:
        entry["http"] = exc.code
        try:
            body = exc.read()
        except OSError:
            body = b""
    except (OSError, ValueError) as exc:
        entry["error"] = f"{type(exc).__name__}: {exc}"
        return entry

    text = body.decode("utf-8", "replace")
    try:
        doc = json.loads(text) if text.strip() else {}
    except json.JSONDecodeError:
        entry["error"] = "响应不是 JSON: " + clip(text, 200)
        return entry
    if not isinstance(doc, dict):
        entry["error"] = "响应不是 JSON object"
        return entry

    entry["stats"] = normalize_stats(doc.get("stats"))
    entry["gateway"] = {
        key: doc.get(key)
        for key in ("status", "mode", "version", "uptime_sec")
        if key in doc
    }
    if not isinstance(doc.get("stats"), dict):
        entry["error"] = "响应里没有 stats 字段"
    status = doc.get("status")
    entry["ok"] = entry["http"] == 200 and (status is None or str(status).lower() == "ok")
    if not entry["ok"] and not entry["error"]:
        entry["error"] = f"health status={status!r} http={entry['http']}"
    return entry


def post_json(url: str, body: bytes, timeout: float):
    """POST，返回 (status|None, body, error|None, elapsed_ms)。"""
    started = time.perf_counter()
    try:
        request = urllib.request.Request(
            url,
            data=body,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Accept": "application/json",
                "User-Agent": USER_AGENT,
            },
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status = response.getcode()
            payload = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        try:
            payload = exc.read()
        except OSError:
            payload = b""
    except (OSError, socket.timeout, ValueError) as exc:
        elapsed = (time.perf_counter() - started) * 1000.0
        return None, b"", f"{type(exc).__name__}: {exc}", elapsed
    elapsed = (time.perf_counter() - started) * 1000.0
    return status, payload, None, elapsed


# ---------------------------------------------------------------------------
# 响应判定
# ---------------------------------------------------------------------------


def extract_text(content) -> str:
    """content 可能是字符串，也可能是 [{type:text,text:...}] 片段数组。"""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for part in content:
            if isinstance(part, str):
                parts.append(part)
            elif isinstance(part, dict):
                text = part.get("text")
                if isinstance(text, str):
                    parts.append(text)
        return "".join(parts)
    return ""


def evaluate_response(status, body: bytes) -> dict:
    """把一次 chat/completions 响应拆成判定所需的字段，不做 scenario 级结论。"""
    result = {
        "finish_reason": None,
        "content": "",
        "tool_names": [],
        "problems": [],
        "body_head": None,
        "doc": None,
    }
    if status is None:
        result["problems"].append("请求没有拿到响应")
        return result

    text = body.decode("utf-8", "replace")
    if status != 200:
        result["problems"].append(f"HTTP {status}")
        result["body_head"] = clip(text)
        return result

    try:
        doc = json.loads(text) if text.strip() else {}
    except json.JSONDecodeError as exc:
        result["problems"].append(f"响应不是 JSON: {exc}")
        result["body_head"] = clip(text)
        return result
    if not isinstance(doc, dict):
        result["problems"].append("响应不是 JSON object")
        result["body_head"] = clip(text)
        return result
    result["doc"] = doc

    error = doc.get("error")
    if error:
        detail = json.dumps(error, ensure_ascii=False) if not isinstance(error, str) else error
        result["problems"].append("响应里带 error: " + clip(detail, 200))

    choices = doc.get("choices")
    choice = choices[0] if isinstance(choices, list) and choices and isinstance(choices[0], dict) else {}
    if not choice:
        result["problems"].append("响应里没有 choices[0]")
        return result

    result["finish_reason"] = choice.get("finish_reason")
    message = choice.get("message") if isinstance(choice.get("message"), dict) else {}
    result["content"] = extract_text(message.get("content"))

    calls = message.get("tool_calls")
    if not isinstance(calls, list) or not calls:
        calls = choice.get("tool_calls")
    if isinstance(calls, list):
        for call in calls:
            if not isinstance(call, dict):
                continue
            fn = call.get("function")
            name = fn.get("name") if isinstance(fn, dict) else call.get("name")
            if isinstance(name, str) and name:
                result["tool_names"].append(name)
    return result


def scenario_problems(name: str, ev: dict, requested) -> list:
    """scenario 级判定；返回问题列表（空 = 通过）。"""
    problems = list(ev["problems"])
    if ev["problems"]:
        # HTTP 层已经失败，内容判定不再叠加噪音
        return problems

    if name == "plain":
        if not ev["content"].strip():
            problems.append("message.content 为空")
        if ev["finish_reason"] not in ("stop", "length"):
            problems.append(
                f"finish_reason={ev['finish_reason']!r} 不是 stop/length"
            )
    elif name in ("single-tool", "multi-tool"):
        if not ev["tool_names"] and ev["finish_reason"] != "tool_calls":
            problems.append("没有 tool_calls，finish_reason 也不是 tool_calls")
        if name == "multi-tool":
            stray = [tool for tool in ev["tool_names"] if tool not in requested]
            if stray:
                problems.append("返回了未请求的工具名: " + ", ".join(stray))
    # many-tools: 只要求 HTTP 200（到这一步已经是 200 且是合法 JSON）
    return problems


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------


def select_scenarios(only: str | None):
    if not only:
        return list(SCENARIOS)
    wanted = []
    for raw in only.split(","):
        name = raw.strip().lower()
        if not name:
            continue
        if name not in SCENARIO_NAMES:
            raise ProbeError(
                f"--only 里有未知 scenario {name!r}，可选: " + ", ".join(SCENARIO_NAMES)
            )
        if name not in wanted:
            wanted.append(name)
    if not wanted:
        raise ProbeError("--only 没有选中任何 scenario，可选: " + ", ".join(SCENARIO_NAMES))
    return [spec for spec in SCENARIOS if spec["name"] in wanted]


def stats_delta(before: dict, after: dict) -> dict:
    delta = {}
    for key in STAT_KEYS:
        old, new = before.get(key), after.get(key)
        delta[key] = (new - old) if isinstance(old, int) and isinstance(new, int) else None
    return delta


def format_stats(stats: dict) -> str:
    return " ".join(
        f"{key}={'?' if stats.get(key) is None else stats[key]}" for key in STAT_KEYS
    )


def summarize(record: dict) -> str:
    http = "-" if record["http"] is None else record["http"]
    finish = record["finish_reason"] or "-"
    tools = ",".join(record["tool_names"]) or "-"
    verdict = "PASS" if record["ok"] else "FAIL"
    line = (
        f"[{verdict}] {record['name']:<12} http={http} "
        f"{record['latency_ms']}ms finish={finish} tools={tools}"
    )
    if record["error"]:
        line += f"  error={record['error']}"
    return line


def run_scenario(spec: dict, base: str, payload: dict, timeout: float) -> tuple:
    """跑一个 scenario，返回 (record, health_before, health_after)。"""
    name = spec["name"]
    requested = set(spec["tools"])
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")

    health_before = read_health(base, health_timeout(timeout))
    status, response_body, transport_error, elapsed_ms = post_json(
        base + CHAT_PATH, body, timeout
    )
    health_after = read_health(base, health_timeout(timeout))

    if transport_error:
        ev = {
            "finish_reason": None,
            "content": "",
            "tool_names": [],
            "problems": [f"请求失败: {transport_error}"],
            "body_head": None,
        }
        status = None
    else:
        ev = evaluate_response(status, response_body)

    record = {
        "name": name,
        "http": status,
        "latency_ms": round(elapsed_ms),
        "finish_reason": ev["finish_reason"],
        "tool_names": ev["tool_names"],
        "ok": False,
        "error": None,
        "requested_tools": list(spec["tools"]),
        "tool_choice": spec["tool_choice"],
        "payload_bytes": len(body),
    }
    if ev["body_head"] is not None:
        record["body_head"] = ev["body_head"]

    problems = scenario_problems(name, ev, requested)

    delta = stats_delta(health_before["stats"], health_after["stats"])
    record["health_delta"] = delta
    for key in ("up429", "up5xx"):
        try:
            grew = int(delta.get(key) or 0)
        except (TypeError, ValueError):
            grew = 0
        if grew > 0:
            problems.append(
                f"{key} +{grew}"
                f"（{health_before['stats'].get(key)} -> {health_after['stats'].get(key)}）"
            )

    if not health_after["ok"] and health_after["error"]:
        record["health_after_error"] = health_after["error"]

    record["ok"] = not problems
    record["error"] = "; ".join(problems) if problems else None
    return record, health_before, health_after


def leaked_names(records) -> list:
    """响应里出现线上名（mcp_/clint_ 前缀）就是还原失败的证据。"""
    seen = []
    for record in records:
        for name in record["tool_names"]:
            if name.startswith(WIRE_PREFIXES) and name not in seen:
                seen.append(name)
    return seen


def write_report(path: Path, report: dict) -> None:
    try:
        if str(path.parent):
            path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
    except OSError as exc:
        raise ProbeError(f"写不了报告 {path}: {exc}") from exc


def print_dry_run(base: str, model: str, definitions: dict, specs, tools_path: Path) -> None:
    total = len(definitions)
    print(f"[dry-run] 工具定义: {total} 个（{tools_path}）")
    print(f"[dry-run] 目标: POST {base}{CHAT_PATH}  model={model}  （不联网）")
    for spec in specs:
        payload = build_payload(model, spec, definitions)
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        names = ",".join(spec["tools"]) or "-"
        print(
            f"[dry-run] {spec['name']:<12} tools={len(spec['tools']):<2} "
            f"payload={len(body)} B  tool_choice={spec['tool_choice'] or '-'}  names={names}"
        )


def parse_args(argv) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="asset_probe.py",
        description="对本地 zen-direct 网关发真实请求，判定 HTTP 200 + 工具调用是否正常。",
        epilog=(
            "退出码: 0 全通过 / 1 有 scenario 失败 / 2 参数或环境错误。"
        ),
    )
    parser.add_argument("--base", required=True, help="网关基址，如 http://127.0.0.1:8081")
    parser.add_argument("--model", required=True, help="模型名，如 deepseek-v4.1-flash-free")
    parser.add_argument("--out", default=None, help="JSON 报告输出路径，如 verdict.json")
    parser.add_argument(
        "--timeout",
        type=float,
        default=DEFAULT_TIMEOUT,
        help=f"每个请求的超时秒数（默认 {DEFAULT_TIMEOUT:.0f}）",
    )
    parser.add_argument(
        "--only",
        default=None,
        help="只跑子集，逗号分隔，如 plain,multi-tool（可选: "
        + ",".join(SCENARIO_NAMES)
        + "）",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="只打印将要发的 payload 概要，不联网、不写报告",
    )
    parser.add_argument(
        "--tools-file",
        default=None,
        help="工具定义 JSON 路径（默认脚本同目录的 opencode_tools.json）",
    )
    return parser.parse_args(argv)


def main(argv=None) -> int:
    try:
        args = parse_args(argv)
    except SystemExit as exc:  # argparse 自己的退出码（--help=0，参数错=2）
        return int(exc.code or 0)

    try:
        if args.timeout <= 0:
            raise ProbeError(f"--timeout 必须 > 0，收到 {args.timeout}")
        if not (args.model or "").strip():
            raise ProbeError("--model 不能为空")
        base = normalize_base(args.base)
        model = args.model.strip()

        tools_path = Path(args.tools_file).resolve() if args.tools_file else default_tools_path()
        definitions = load_tool_definitions(tools_path)
        specs = select_scenarios(args.only)

        if args.dry_run:
            print_dry_run(base, model, definitions, specs, tools_path)
            return 0

        started_at = now_iso()
        started = time.perf_counter()

        health_before = read_health(base, health_timeout(args.timeout))
        if not health_before["ok"]:
            reason = health_before["error"] or f"http={health_before['http']}"
            raise ProbeError(
                f"网关没起来或 /health 异常: {health_before['url']} -> {reason}"
            )

        print(f"gateway: {base}  model={model}  tools={tools_path}")
        print(f"health before: {format_stats(health_before['stats'])}")

        records = []
        health_after = health_before
        for spec in specs:
            payload = build_payload(model, spec, definitions)
            record, _, health_after = run_scenario(spec, base, payload, args.timeout)
            records.append(record)
            print(summarize(record))

        passed = sum(1 for record in records if record["ok"])
        failed = len(records) - passed
        leaked = leaked_names(records)

        print(f"health after : {format_stats(health_after['stats'])}")
        if leaked:
            print("leaked wire names: " + ", ".join(leaked))

        report = {
            "base": base,
            "model": model,
            "started_at": started_at,
            "finished_at": now_iso(),
            "duration_ms": round((time.perf_counter() - started) * 1000),
            "scenarios": records,
            "passed": passed,
            "failed": failed,
            "health_before": health_before,
            "health_after": health_after,
            "leaked_wire_names": leaked,
            "tools_file": str(tools_path),
            "ok": failed == 0,
        }

        if args.out:
            out_path = Path(args.out)
            write_report(out_path, report)
            print(f"report: {out_path.resolve()}")

        print(f"VERDICT: {'PASS' if report['ok'] else 'FAIL'} {passed}/{len(records)}")
        return 0 if report["ok"] else 1

    except ProbeError as exc:
        print(f"asset_probe: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
