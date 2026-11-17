#!/usr/bin/env python3
"""contract_probe.py -- 响应侧契约探针（不是"有没有 200"探针）。

asset_probe.py 只判 HTTP 200 + 有没有调工具，等于没验内容。本脚本验的是
模型 -> 网关 -> 客户端这条链路上的**响应契约**：

    args_schema  返回的 tool_calls.arguments 是否符合 opencode_tools.json 里
                 read 的 schema（必填在、类型对、没有多余字段）
    multiturn    把 role:"tool" 的结果回灌后还能不能继续，会不会把同一个调用
                 原样重发成死循环
    streaming    stream:true 时 tool_calls 是否以增量分片出现，碎片能不能拼回
                 完整的 name + 可解析的 arguments
    parallel     一次返回多个 tool_calls（read + glob）行不行（数量不够记 soft）
    errors       未知工具（tool_choice 指定）/ tool_choice=none /
                 tool_choice=required 三条错误路径
    endpoints    /v1/responses 与 /v1/messages 的最小请求是否可解释

固定接口（参数名不要改）：

    python tools/contract_probe.py --base http://127.0.0.1:8081 \
        --model deepseek-v4.1-flash-free --out verdict.json --timeout 120 \
        [--only args_schema,streaming] [--dry-run] [--tools-file PATH]

退出码：
    0  选中的 case 全部通过
    1  至少一个 case 失败（quota_suspected 也算失败：额度问题无法判定为通过）
    2  参数 / 环境错误（网关不可达、工具定义缺失或非法、报告写不出去）

行为约定：
    * 纯标准库（urllib / json / ssl），不依赖 requests；
    * 工具定义直接读 tools/opencode_tools.json（13 个真实定义），不自编 schema；
    * 不读任何凭证、不打印任何 token，鉴权由网关自己处理；
    * 每个 case 独立超时（Deadline），一个 case 挂了不影响后面；
    * 401/402/429 或响应体出现额度字样时单独标 quota_suspected，并把响应体
      前 300 字符留在 body_head 里，不当普通失败处理。
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

USER_AGENT = "zen-contract-probe/1.0"
DEFAULT_TIMEOUT = 120.0
HEALTH_TIMEOUT_CAP = 10.0
MAX_TOKENS = 512
BODY_CLIP = 500
QUOTA_BODY_CLIP = 300
ENDPOINT_BODY_CLIP = 200

CHAT_PATH = "/v1/chat/completions"
RESPONSES_PATH = "/v1/responses"
MESSAGES_PATH = "/v1/messages"
HEALTH_PATH = "/health"

CASE_NAMES = (
    "args_schema",
    "multiturn",
    "streaming",
    "parallel",
    "errors",
    "endpoints",
)
CASE_ALIASES = {f"case{index + 1}": name for index, name in enumerate(CASE_NAMES)}

REQUIRED_TOOLS = ("read", "glob")
UNKNOWN_TOOL = "probe_tool_that_does_not_exist"

QUOTA_STATUSES = (401, 402, 429)
QUOTA_MARKERS = ("freeusagelimit", "usage limit", "quota")
SHAPE_MARKERS = ("tool", "schema", "invalid", "unknown")

REASONABLE_FINISH = ("stop", "length", "tool_calls")


class ProbeError(Exception):
    """参数 / 环境错误，映射到退出码 2。"""


class CaseTimeout(Exception):
    """单个 case 超出独立超时。"""


# ---------------------------------------------------------------------------
# 小工具
# ---------------------------------------------------------------------------


def now_iso() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def clip(text, limit: int = BODY_CLIP) -> str:
    if not text:
        return ""
    return text[:limit]


def json_type_name(value) -> str:
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, str):
        return "string"
    if isinstance(value, int):
        return "integer"
    if isinstance(value, float):
        return "number"
    if isinstance(value, list):
        return "array"
    if isinstance(value, dict):
        return "object"
    if value is None:
        return "null"
    return type(value).__name__


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


def parse_json_body(text: str):
    """返回 (doc, error)；doc 一定是 dict。"""
    if not text or not text.strip():
        return None, "响应体为空"
    try:
        doc = json.loads(text)
    except json.JSONDecodeError as exc:
        return None, f"响应不是合法 JSON: {exc}"
    if not isinstance(doc, dict):
        return None, f"响应不是 JSON object，而是 {type(doc).__name__}"
    return doc, None


def chat_choice(doc):
    """取 choices[0] 这个 dict；拿不到就返回 {}。"""
    if not isinstance(doc, dict):
        return {}
    choices = doc.get("choices")
    if isinstance(choices, list) and choices and isinstance(choices[0], dict):
        return choices[0]
    return {}


def parse_arguments(raw):
    """OpenAI 的 function.arguments 是 JSON 字符串；宽容接受 dict。

    返回 (value, error)。空串按 {} 处理（零参数工具），但调用方要知道它是空的。
    """
    if isinstance(raw, dict):
        return raw, None
    if raw is None:
        return {}, None
    if isinstance(raw, str):
        if not raw.strip():
            return {}, None
        try:
            return json.loads(raw), None
        except json.JSONDecodeError as exc:
            return None, f"arguments 不是合法 JSON: {exc}"
    return None, f"arguments 类型不认识: {json_type_name(raw)}"


def extract_tool_calls(doc) -> list:
    """兼容 message.tool_calls 与 choice.tool_calls，返回统一结构。"""
    choice = chat_choice(doc)
    message = choice.get("message") if isinstance(choice.get("message"), dict) else {}
    calls = message.get("tool_calls")
    if not isinstance(calls, list) or not calls:
        calls = choice.get("tool_calls")
    out = []
    if not isinstance(calls, list):
        return out
    for call in calls:
        if not isinstance(call, dict):
            continue
        fn = call.get("function") if isinstance(call.get("function"), dict) else {}
        name = fn.get("name") or call.get("name") or ""
        raw_args = fn.get("arguments") if "arguments" in fn else call.get("arguments")
        parsed, error = parse_arguments(raw_args)
        if isinstance(raw_args, str):
            raw_text = raw_args
        elif raw_args is None:
            raw_text = ""
        else:
            raw_text = json.dumps(raw_args, ensure_ascii=False)
        out.append(
            {
                "id": call.get("id"),
                "name": name if isinstance(name, str) else "",
                "arguments": parsed,
                "arguments_raw": raw_text,
                "arguments_error": error,
            }
        )
    return out


def canonical_arguments(raw_text: str) -> str:
    """死循环判定用：能解析就按排序后的规范形式比，不能解析就比原文。"""
    try:
        doc = json.loads(raw_text) if raw_text and raw_text.strip() else {}
        return json.dumps(doc, ensure_ascii=False, sort_keys=True)
    except (json.JSONDecodeError, TypeError):
        return (raw_text or "").strip()


# ---------------------------------------------------------------------------
# 失败分类器（纯函数）
# ---------------------------------------------------------------------------


def classify_failure(http_status, body_text) -> str:
    """把一次失败归到 quota / shape / model_behavior / gateway / unknown。

    判据（按优先级）：
      quota           401/402/429，或 body 出现 FreeUsageLimit / usage limit / quota
      gateway         5xx
      shape           400/422 且 body 提到 tool / schema / invalid / unknown
      model_behavior  2xx 但没调工具、content 非空
      unknown         其余（含 2xx 空响应）
    """
    status = http_status if isinstance(http_status, int) else None
    text = body_text.lower() if isinstance(body_text, str) else ""

    if status in QUOTA_STATUSES or any(marker in text for marker in QUOTA_MARKERS):
        return "quota"
    if status is not None and 500 <= status <= 599:
        return "gateway"
    if status in (400, 422) and any(marker in text for marker in SHAPE_MARKERS):
        return "shape"
    if status is not None and 200 <= status < 300:
        try:
            doc = json.loads(body_text) if body_text and body_text.strip() else None
        except json.JSONDecodeError:
            return "shape"
        if not isinstance(doc, dict):
            return "shape"
        content = extract_text(
            (chat_choice(doc).get("message") or {}).get("content")
            if isinstance(chat_choice(doc).get("message"), dict)
            else ""
        )
        calls = extract_tool_calls(doc)
        if not calls:
            # 200 但没调工具：content 非空 -> 模型行为；两者都空 -> 响应形状问题
            return "model_behavior" if content.strip() else "shape"
    return "unknown"


# ---------------------------------------------------------------------------
# mini JSON-Schema 校验（够用即可：read/glob 的 schema 只用到这些关键字）
# ---------------------------------------------------------------------------


def _type_ok(value, expected: str) -> bool:
    if expected == "object":
        return isinstance(value, dict)
    if expected == "array":
        return isinstance(value, list)
    if expected == "string":
        return isinstance(value, str)
    if expected == "boolean":
        return isinstance(value, bool)
    if expected == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if expected == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    if expected == "null":
        return value is None
    return True


def _validate_node(value, schema, path: str, problems: list) -> None:
    if not isinstance(schema, dict):
        return

    expected = schema.get("type")
    if isinstance(expected, str) and not _type_ok(value, expected):
        problems.append(
            f"{path or 'arguments'} 类型应为 {expected}，实际 {json_type_name(value)}"
        )
        return

    enum = schema.get("enum")
    if isinstance(enum, list) and enum and value not in enum:
        problems.append(
            f"{path or 'arguments'} 取值 {value!r} 不在 enum {enum}"
        )

    if isinstance(value, dict):
        properties = schema.get("properties")
        properties = properties if isinstance(properties, dict) else {}
        required = schema.get("required")
        required = required if isinstance(required, list) else []
        for name in required:
            if name not in value:
                problems.append(
                    f"缺少必填字段 {path + '.' + name if path else name}"
                )
        extra_allowed = schema.get("additionalProperties", True)
        for key, sub_value in value.items():
            child = f"{path}.{key}" if path else key
            if key in properties:
                _validate_node(sub_value, properties[key], child, problems)
            elif extra_allowed is False:
                problems.append(f"出现 schema 未定义的字段 {child}")

    elif isinstance(value, list):
        items = schema.get("items")
        if isinstance(items, dict):
            for index, item in enumerate(value):
                _validate_node(item, items, f"{path}[{index}]", problems)
        min_items = schema.get("minItems")
        if isinstance(min_items, int) and len(value) < min_items:
            problems.append(
                f"{path or 'arguments'} 只有 {len(value)} 项，小于 minItems {min_items}"
            )

    if isinstance(value, str):
        min_length = schema.get("minLength")
        if isinstance(min_length, int) and len(value) < min_length:
            problems.append(
                f"{path or 'arguments'} 长度 {len(value)} 小于 minLength {min_length}"
            )

    if isinstance(value, (int, float)) and not isinstance(value, bool):
        minimum = schema.get("minimum")
        if isinstance(minimum, (int, float)) and value < minimum:
            problems.append(f"{path or 'arguments'} 值 {value} 小于 minimum {minimum}")
        maximum = schema.get("maximum")
        if isinstance(maximum, (int, float)) and value > maximum:
            problems.append(f"{path or 'arguments'} 值 {value} 大于 maximum {maximum}")
        exclusive_min = schema.get("exclusiveMinimum")
        if isinstance(exclusive_min, (int, float)) and value <= exclusive_min:
            problems.append(
                f"{path or 'arguments'} 值 {value} 不大于 exclusiveMinimum {exclusive_min}"
            )
        exclusive_max = schema.get("exclusiveMaximum")
        if isinstance(exclusive_max, (int, float)) and value >= exclusive_max:
            problems.append(
                f"{path or 'arguments'} 值 {value} 不小于 exclusiveMaximum {exclusive_max}"
            )


def validate_arguments(payload, schema) -> list:
    """按工具 schema 校验 arguments，返回具体到字段的问题列表。"""
    problems = []
    if not isinstance(payload, dict):
        return [
            f"arguments 顶层应为 object，实际 {json_type_name(payload)}"
        ]
    _validate_node(payload, schema, "", problems)
    return problems


# ---------------------------------------------------------------------------
# 工具定义
# ---------------------------------------------------------------------------


def default_tools_path() -> Path:
    here = Path(__file__).resolve().parent / "opencode_tools.json"
    if here.is_file():
        return here
    return Path.cwd() / "tools" / "opencode_tools.json"


def load_tool_definitions(path: Path) -> dict:
    """读 {name: {description, parameters}}，转成 OpenAI function 形状。"""
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

    missing = [name for name in REQUIRED_TOOLS if name not in definitions]
    if missing:
        raise ProbeError(
            f"工具定义 {path} 缺少契约探针必须的工具: " + ", ".join(missing)
        )
    return definitions


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


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


def _request_headers(accept: str = "application/json") -> dict:
    return {
        "Content-Type": "application/json",
        "Accept": accept,
        "User-Agent": USER_AGENT,
    }


def post_json(url: str, payload: dict, timeout: float) -> dict:
    """POST JSON，返回 {status,text,error,latency_ms,request_bytes}；永不抛异常。"""
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    started = time.perf_counter()
    result = {
        "status": None,
        "text": "",
        "error": None,
        "latency_ms": 0.0,
        "request_bytes": len(body),
    }
    try:
        request = urllib.request.Request(
            url, data=body, method="POST", headers=_request_headers()
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            result["status"] = response.getcode()
            raw = response.read()
    except urllib.error.HTTPError as exc:
        result["status"] = exc.code
        try:
            raw = exc.read()
        except OSError:
            raw = b""
    except (OSError, socket.timeout, ValueError) as exc:
        result["error"] = f"{type(exc).__name__}: {exc}"
        result["latency_ms"] = (time.perf_counter() - started) * 1000.0
        return result

    result["latency_ms"] = (time.perf_counter() - started) * 1000.0
    result["text"] = raw.decode("utf-8", "replace")
    return result


def read_health(base: str, timeout: float) -> dict:
    """GET {base}/health，永不抛异常；unreachable=True 表示连不上（退出码 2）。"""
    url = base + HEALTH_PATH
    entry = {
        "url": url,
        "http": None,
        "ok": False,
        "stats": None,
        "error": None,
        "unreachable": False,
    }
    try:
        request = urllib.request.Request(
            url,
            method="GET",
            headers={"Accept": "application/json", "User-Agent": USER_AGENT},
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            entry["http"] = response.getcode()
            raw = response.read()
    except urllib.error.HTTPError as exc:
        entry["http"] = exc.code
        try:
            raw = exc.read()
        except OSError:
            raw = b""
    except (OSError, socket.timeout, ValueError) as exc:
        entry["error"] = f"{type(exc).__name__}: {exc}"
        entry["unreachable"] = True
        return entry

    text = raw.decode("utf-8", "replace")
    try:
        doc = json.loads(text) if text.strip() else {}
    except json.JSONDecodeError:
        entry["error"] = "health 不是 JSON: " + clip(text, 200)
        entry["ok"] = entry["http"] == 200
        return entry

    if isinstance(doc, dict):
        stats = doc.get("stats")
        entry["stats"] = stats if isinstance(stats, dict) else None
        entry["gateway"] = {
            key: doc.get(key)
            for key in ("status", "mode", "version", "uptime_sec")
            if key in doc
        }
        status = doc.get("status")
        entry["ok"] = entry["http"] == 200 and (
            status is None or str(status).lower() == "ok"
        )
        if not entry["ok"]:
            entry["error"] = f"health status={status!r} http={entry['http']}"
        return entry

    entry["ok"] = entry["http"] == 200
    entry["error"] = "health 响应不是 JSON object"
    return entry


def stream_sse(url: str, payload: dict, timeout: float, deadline) -> dict:
    """发 stream:true 请求，把 SSE 的 data: 行收集起来。

    返回 {status,events,error,text,latency_ms,raw_lines,saw_done}。
    中途读断也保留已收到的事件，error 里说明断在哪。
    """
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    started = time.perf_counter()
    result = {
        "status": None,
        "events": [],
        "error": None,
        "text": "",
        "latency_ms": 0.0,
        "raw_lines": 0,
        "saw_done": False,
        "first_line": "",
    }
    try:
        request = urllib.request.Request(
            url,
            data=body,
            method="POST",
            headers=_request_headers(accept="text/event-stream"),
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            result["status"] = response.getcode()
            while True:
                if time.monotonic() > deadline.end:
                    result["error"] = (
                        f"流式读取超出 case 独立超时 {deadline.total:g}s"
                    )
                    break
                raw = response.readline()
                if not raw:
                    break
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                result["raw_lines"] += 1
                if line and not result["first_line"]:
                    result["first_line"] = clip(line, 200)
                if not line or line.startswith(":"):
                    continue
                if line.startswith("data:"):
                    data = line[5:].strip()
                    if not data:
                        continue
                    if data == "[DONE]":
                        result["saw_done"] = True
                        break
                    result["events"].append(data)
    except urllib.error.HTTPError as exc:
        result["status"] = exc.code
        try:
            raw = exc.read()
        except OSError:
            raw = b""
        result["text"] = raw.decode("utf-8", "replace")
        result["latency_ms"] = (time.perf_counter() - started) * 1000.0
        return result
    except (OSError, socket.timeout, ValueError) as exc:
        result["error"] = f"SSE 读取中断: {type(exc).__name__}: {exc}"
        result["latency_ms"] = (time.perf_counter() - started) * 1000.0
        return result

    result["latency_ms"] = (time.perf_counter() - started) * 1000.0
    return result


# ---------------------------------------------------------------------------
# 流式分片拼装
# ---------------------------------------------------------------------------


def _merge_name(current: str, incoming: str) -> str:
    """name 通常整块出现一次；有的网关会整块重发，也有的真的切片。"""
    if not current:
        return incoming
    if incoming == current or current.startswith(incoming):
        return current
    if incoming.startswith(current):
        return incoming
    return current + incoming


def assemble_stream_calls(events) -> tuple:
    """把 SSE delta 拼回 [call]；返回 (calls, text, malformed_event_indexes)。"""
    calls = {}
    text_parts = []
    malformed = []

    for position, data in enumerate(events):
        try:
            doc = json.loads(data)
        except json.JSONDecodeError:
            malformed.append(position)
            continue
        if not isinstance(doc, dict):
            malformed.append(position)
            continue

        choices = doc.get("choices")
        if not isinstance(choices, list) or not choices:
            continue
        choice = choices[0] if isinstance(choices[0], dict) else {}
        delta = choice.get("delta") if isinstance(choice.get("delta"), dict) else {}
        message = choice.get("message") if isinstance(choice.get("message"), dict) else {}

        chunks = delta.get("tool_calls")
        if not isinstance(chunks, list) or not chunks:
            chunks = message.get("tool_calls")
        if isinstance(chunks, list):
            for fallback_index, chunk in enumerate(chunks):
                if not isinstance(chunk, dict):
                    continue
                key = chunk.get("index", fallback_index)
                try:
                    key = int(key)
                except (TypeError, ValueError):
                    key = fallback_index
                slot = calls.setdefault(
                    key,
                    {
                        "id": None,
                        "name": "",
                        "name_parts": 0,
                        "arguments": "",
                        "argument_fragments": [],
                    },
                )
                if chunk.get("id"):
                    slot["id"] = chunk["id"]
                fn = chunk.get("function")
                if isinstance(fn, dict):
                    name = fn.get("name")
                    if isinstance(name, str) and name:
                        slot["name_parts"] += 1
                        slot["name"] = _merge_name(slot["name"], name)
                    arguments = fn.get("arguments")
                    if isinstance(arguments, (dict, list)):
                        arguments = json.dumps(arguments, ensure_ascii=False)
                    if isinstance(arguments, str) and arguments:
                        slot["arguments"] += arguments
                        slot["argument_fragments"].append(arguments)

        content = delta.get("content")
        if not isinstance(content, str):
            content = message.get("content")
        if isinstance(content, str) and content:
            text_parts.append(content)

    return calls, "".join(text_parts), malformed


def _fragment_index_for_offset(fragments, offset: int) -> int:
    """JSONDecodeError.pos 落在第几个 arguments 分片上（1-based）。"""
    consumed = 0
    for index, fragment in enumerate(fragments):
        consumed += len(fragment)
        if offset < consumed:
            return index + 1
    return max(1, len(fragments))


# ---------------------------------------------------------------------------
# 探针上下文 / case 基础设施
# ---------------------------------------------------------------------------


class Deadline:
    """一个 case 的独立超时预算。"""

    def __init__(self, total: float):
        self.total = total
        self.end = time.monotonic() + total

    def left(self) -> float:
        return self.end - time.monotonic()

    def timeout(self, cap: float) -> float:
        remaining = self.left()
        if remaining <= 0:
            raise CaseTimeout(f"case 独立超时 {self.total:g}s 已用完")
        return max(0.5, min(cap, remaining))


class Probe:
    def __init__(self, base: str, model: str, timeout: float, definitions: dict):
        self.base = base
        self.model = model
        self.timeout = timeout
        self.definitions = definitions

    def payload(self, prompt: str, tool_names=(), tool_choice="auto", **extra) -> dict:
        payload = {
            "model": self.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": MAX_TOKENS,
        }
        if tool_names:
            payload["tools"] = [self.definitions[name] for name in tool_names]
            if tool_choice is not None:
                payload["tool_choice"] = tool_choice
        payload.update(extra)
        return payload


def new_case(name: str) -> dict:
    return {
        "name": name,
        "ok": False,
        "soft": False,
        "http": None,
        "latency_ms": 0,
        "detail": "",
        "error": None,
        "failure_class": None,
        "quota_suspected": False,
        "body_head": None,
    }


def settle(record: dict, problems: list, text: str) -> dict:
    """把问题列表落成 case 结论，并按需打 quota 标记。"""
    record["ok"] = not problems
    record["error"] = "; ".join(problems) if problems else None
    if problems:
        failure_class = classify_failure(record["http"], text)
        record["failure_class"] = failure_class
        if failure_class == "quota":
            record["quota_suspected"] = True
            record["body_head"] = clip(text, QUOTA_BODY_CLIP)
    return record


def _subcase(name: str, resp: dict, problems: list, detail: str) -> dict:
    sub = {
        "name": name,
        "http": resp.get("status"),
        "ok": not problems,
        "detail": detail,
        "error": "; ".join(problems) if problems else None,
        "failure_class": None,
        "quota_suspected": False,
        "body_head": None,
    }
    if problems:
        failure_class = classify_failure(resp.get("status"), resp.get("text") or "")
        sub["failure_class"] = failure_class
        if failure_class == "quota":
            sub["quota_suspected"] = True
            sub["body_head"] = clip(resp.get("text") or "", QUOTA_BODY_CLIP)
    return sub


def aggregate(record: dict, subs: list, latency_ms: float) -> dict:
    """errors / endpoints 这类多子请求 case 的汇总。"""
    record["latency_ms"] = round(latency_ms)
    statuses = [sub["http"] for sub in subs]
    record["http"] = statuses[0] if len(set(statuses)) == 1 else None
    record["subcases"] = subs
    record["detail"] = " | ".join(f"{sub['name']}: {sub['detail']}" for sub in subs)

    failed = [sub for sub in subs if not sub["ok"]]
    record["ok"] = not failed
    if failed:
        record["error"] = "; ".join(f"{sub['name']}: {sub['error']}" for sub in failed)
        quota = next((sub for sub in subs if sub["quota_suspected"]), None)
        if quota is not None:
            record["quota_suspected"] = True
            record["body_head"] = quota["body_head"]
            record["failure_class"] = "quota"
        else:
            record["failure_class"] = failed[0]["failure_class"] or "unknown"
    return record


# ---------------------------------------------------------------------------
# case 1: args_schema
# ---------------------------------------------------------------------------


def case_args_schema(probe: Probe) -> dict:
    record = new_case("args_schema")
    deadline = Deadline(probe.timeout)
    schema = probe.definitions["read"]["function"]["parameters"]
    payload = probe.payload(
        "Call the `read` tool exactly once with path='/etc/hostname'. "
        "The tool call is the whole reply, do not answer in text.",
        tool_names=("read",),
        tool_choice="auto",
    )
    record["request"] = {"tools": ["read"], "tool_choice": "auto"}

    resp = post_json(probe.base + CHAT_PATH, payload, deadline.timeout(probe.timeout))
    record["http"] = resp["status"]
    record["latency_ms"] = round(resp["latency_ms"])
    text = resp["text"]
    problems = []

    if resp["error"]:
        problems.append(f"请求失败: {resp['error']}")
        record["detail"] = "没拿到响应，无法校验 read.arguments"
        return settle(record, problems, text)

    if resp["status"] != 200:
        problems.append(f"HTTP {resp['status']}")
        record["detail"] = f"HTTP {resp['status']}，响应体: {clip(text, 200)}"
        return settle(record, problems, text)

    doc, parse_error = parse_json_body(text)
    if parse_error:
        problems.append(parse_error)
        record["detail"] = parse_error
        return settle(record, problems, text)

    calls = extract_tool_calls(doc)
    if not calls:
        content = extract_text(chat_choice(doc).get("message", {}).get("content"))
        problems.append("没有返回 tool_calls，模型只回了文本")
        record["detail"] = (
            "没有 tool_calls；"
            + (f"content={clip(content, 120)!r}" if content.strip() else "content 也为空")
        )
        return settle(record, problems, text)

    target = next((call for call in calls if call["name"] == "read"), None)
    if target is None:
        names = ", ".join(call["name"] or "<empty>" for call in calls)
        problems.append(f"返回的工具不是 read: {names}")
        record["detail"] = problems[-1]
        return settle(record, problems, text)

    arguments = target["arguments"]
    schema_problems = []
    if target["arguments_error"]:
        problems.append(target["arguments_error"])
    elif isinstance(arguments, dict):
        schema_problems = validate_arguments(arguments, schema)
        problems.extend(schema_problems)
    else:
        problems.append(
            f"arguments 顶层应为 object，实际 {json_type_name(arguments)}"
        )

    record["tool"] = target["name"]
    record["arguments"] = clip(
        json.dumps(arguments, ensure_ascii=False)
        if isinstance(arguments, dict)
        else target["arguments_raw"],
        300,
    )
    record["schema_errors"] = schema_problems

    if problems:
        record["detail"] = "read.arguments 不符合 schema: " + "; ".join(problems)
    else:
        record["detail"] = (
            "read.arguments 的必填字段、类型、多余字段全部符合 "
            "opencode_tools.json#read"
        )
    return settle(record, problems, text)


# ---------------------------------------------------------------------------
# case 2: multiturn
# ---------------------------------------------------------------------------


def case_multiturn(probe: Probe) -> dict:
    record = new_case("multiturn")
    deadline = Deadline(probe.timeout)
    prompt = (
        "Use the `read` tool to read /etc/hostname, "
        "then tell me its first line."
    )
    payload = probe.payload(prompt, tool_names=("read",), tool_choice="auto")
    record["request"] = {"rounds": 2, "tools": ["read"], "tool_choice": "auto"}

    first = post_json(probe.base + CHAT_PATH, payload, deadline.timeout(probe.timeout))
    total_ms = first["latency_ms"]
    record["http"] = first["status"]
    text = first["text"]
    problems = []
    rounds = []

    if first["error"]:
        problems.append(f"第 1 轮请求失败: {first['error']}")
        record["detail"] = "第 1 轮没拿到响应"
        return settle(record, problems, text)

    doc1, parse_error = parse_json_body(text)
    calls1 = extract_tool_calls(doc1) if doc1 is not None else []
    rounds.append(
        {
            "round": 1,
            "http": first["status"],
            "finish_reason": chat_choice(doc1).get("finish_reason") if doc1 else None,
            "tool_calls": [call["name"] for call in calls1],
        }
    )
    if first["status"] != 200:
        problems.append(f"第 1 轮 HTTP {first['status']}")
        record["rounds"] = rounds
        record["detail"] = f"第 1 轮 HTTP {first['status']}，无法验证回灌"
        return settle(record, problems, text)

    if parse_error:
        problems.append("第 1 轮 " + parse_error)
        record["rounds"] = rounds
        record["detail"] = problems[-1]
        return settle(record, problems, text)

    if not calls1:
        problems.append("第 1 轮没有 tool_calls，无法验证多轮回灌")
        record["rounds"] = rounds
        record["detail"] = problems[-1]
        return settle(record, problems, text)

    call = calls1[0]
    call_id = call["id"] or "call_probe_1"
    arguments_raw = call["arguments_raw"] or "{}"
    messages = [
        {"role": "user", "content": prompt},
        {
            "role": "assistant",
            "content": None,
            "tool_calls": [
                {
                    "id": call_id,
                    "type": "function",
                    "function": {"name": call["name"], "arguments": arguments_raw},
                }
            ],
        },
        {
            "role": "tool",
            "tool_call_id": call_id,
            "name": call["name"],
            "content": "1: probe-host",
        },
    ]
    payload2 = {
        "model": probe.model,
        "messages": messages,
        "tools": [probe.definitions["read"]],
        "tool_choice": "auto",
        "max_tokens": MAX_TOKENS,
    }

    second = post_json(probe.base + CHAT_PATH, payload2, deadline.timeout(probe.timeout))
    total_ms += second["latency_ms"]
    record["http"] = second["status"]
    text = second["text"]

    if second["error"]:
        problems.append(f"第 2 轮请求失败: {second['error']}")
        record["rounds"] = rounds
        record["detail"] = "tool 结果回灌后的第 2 轮没拿到响应"
        return settle(record, problems, text)

    if second["status"] != 200:
        problems.append(f"第 2 轮 HTTP {second['status']}")
        record["rounds"] = rounds
        record["detail"] = f"回灌后 HTTP {second['status']}，链路断了"
        return settle(record, problems, text)

    doc2, parse_error = parse_json_body(text)
    if parse_error:
        problems.append("第 2 轮 " + parse_error)
        record["rounds"] = rounds
        record["detail"] = problems[-1]
        return settle(record, problems, text)

    choice = chat_choice(doc2)
    if not choice:
        problems.append("第 2 轮响应里没有 choices[0]")
        record["rounds"] = rounds
        record["detail"] = problems[-1]
        return settle(record, problems, text)

    finish = choice.get("finish_reason")
    calls2 = extract_tool_calls(doc2)
    rounds.append(
        {
            "round": 2,
            "http": second["status"],
            "finish_reason": finish,
            "tool_calls": [item["name"] for item in calls2],
        }
    )

    if finish not in REASONABLE_FINISH:
        problems.append(
            f"finish_reason={finish!r} 不合理（期望 stop/length/tool_calls）"
        )

    repeated = None
    for item in calls2:
        if item["name"] == call["name"] and canonical_arguments(
            item["arguments_raw"]
        ) == canonical_arguments(arguments_raw):
            repeated = item
            break
    if repeated is not None:
        problems.append(
            "第 2 轮把第 1 轮的调用原样重发（name+arguments 完全相同，疑似死循环）"
        )

    record["rounds"] = rounds
    record["finish_reason"] = finish
    record["round2_tool_calls"] = [item["name"] for item in calls2]
    record["latency_ms"] = round(total_ms)
    if problems:
        record["detail"] = (
            f"回灌后 finish_reason={finish!r}，tool_calls={len(calls2)}；"
            + "; ".join(problems)
        )
    else:
        record["detail"] = (
            f"tool 结果回灌后仍 200，finish_reason={finish!r}，"
            f"第 2 轮 tool_calls={len(calls2)}，没有重复第 1 轮的原样调用"
        )
    return settle(record, problems, text)


# ---------------------------------------------------------------------------
# case 3: streaming
# ---------------------------------------------------------------------------


def case_streaming(probe: Probe) -> dict:
    record = new_case("streaming")
    deadline = Deadline(probe.timeout)
    requested = ("read",)
    payload = probe.payload(
        "Call the `read` tool exactly once with path='/etc/hostname'. "
        "The tool call is the whole reply.",
        tool_names=requested,
        tool_choice="auto",
        stream=True,
    )
    record["request"] = {"tools": list(requested), "tool_choice": "auto", "stream": True}

    stream = stream_sse(
        probe.base + CHAT_PATH, payload, deadline.timeout(probe.timeout), deadline
    )
    record["http"] = stream["status"]
    record["latency_ms"] = round(stream["latency_ms"])
    text = stream["text"]

    if stream["error"]:
        record["detail"] = stream["error"]
        problems = [stream["error"]]
        if stream["events"]:
            record["detail"] += f"（已收到 {len(stream['events'])} 个 SSE 事件）"
        return settle(record, problems, text)

    if stream["status"] is None:
        problems = ["SSE 请求没有拿到响应"]
        record["detail"] = problems[0]
        return settle(record, problems, text)

    if stream["status"] != 200:
        problems = [f"HTTP {stream['status']}"]
        record["detail"] = (
            f"HTTP {stream['status']}，响应体: {clip(text, 200)}"
        )
        return settle(record, problems, text)

    events = stream["events"]
    record["sse_events"] = len(events)
    record["sse_raw_lines"] = stream["raw_lines"]
    record["sse_done"] = stream["saw_done"]

    if not events:
        first_line = stream.get("first_line") or ""
        problems = ["没有任何 SSE data: 事件"]
        record["detail"] = problems[0]
        if first_line and not first_line.startswith("data:"):
            record["detail"] += (
                f"；首行不是 SSE（stream:true 被当成非流式处理？）: {first_line!r}"
            )
        elif stream["raw_lines"] == 0:
            record["detail"] += "；连接建立后没有任何响应行"
        return settle(record, problems, text)

    calls, content, malformed = assemble_stream_calls(events)
    record["malformed_events"] = len(malformed)

    problems = []
    details = []

    if malformed:
        details.append(f"{len(malformed)} 个 SSE 事件不是合法 JSON")
        problems.append(
            f"{len(malformed)} 个 SSE 事件不是合法 JSON"
            f"（首个在事件 #{malformed[0] + 1}）"
        )

    if not calls:
        if content.strip():
            problems.append("SSE 里没有 tool_calls 增量（模型只回了文本）")
            details.append(f"content={clip(content, 120)!r}")
        else:
            problems.append("SSE 里既没有 tool_calls 增量也没有文本")
        record["detail"] = "; ".join(details or problems)
        return settle(record, problems, text)

    fragments_summary = []
    for index in sorted(calls):
        slot = calls[index]
        fragments = slot["argument_fragments"]
        fragments_summary.append(
            {
                "index": index,
                "name": slot["name"],
                "name_fragments": slot["name_parts"],
                "argument_fragments": len(fragments),
                "argument_bytes": len(slot["arguments"]),
            }
        )

        if not slot["name"]:
            problems.append(f"tool_calls[{index}] 拼不出 name")
            continue
        if slot["name"] not in requested:
            problems.append(
                f"tool_calls[{index}].name={slot['name']!r} 不在请求的工具里"
            )

        if not slot["arguments"].strip():
            problems.append(f"tool_calls[{index}]({slot['name']}) 的 arguments 为空")
            continue
        try:
            parsed = json.loads(slot["arguments"])
        except json.JSONDecodeError as exc:
            broken_at = _fragment_index_for_offset(fragments, exc.pos)
            lengths = "/".join(str(len(fragment)) for fragment in fragments) or "0"
            problems.append(
                f"tool_calls[{index}]({slot['name']}).arguments 分片拼接后不是合法 JSON: "
                f"{exc.msg}，断在第 {broken_at}/{len(fragments)} 个分片"
                f"（offset {exc.pos}，分片长度 {lengths}，末片={fragments[-1][-40:]!r}）"
            )
            continue

        if not isinstance(parsed, dict):
            problems.append(
                f"tool_calls[{index}].arguments 顶层不是 object，"
                f"而是 {json_type_name(parsed)}"
            )
            continue

        schema = probe.definitions.get(slot["name"], {}).get("function", {}).get(
            "parameters"
        )
        if isinstance(schema, dict):
            schema_problems = validate_arguments(parsed, schema)
            if schema_problems:
                details.append(
                    f"{slot['name']}.arguments 与 schema 不一致: "
                    + "; ".join(schema_problems)
                )

    record["stream_calls"] = fragments_summary

    if problems:
        record["detail"] = "; ".join(
            details
            + [
                "SSE 分片拼装失败: " + "; ".join(problems),
            ]
        )
        return settle(record, problems, text)

    fragment_counts = [
        item["argument_fragments"] for item in fragments_summary if item["argument_fragments"]
    ]
    if fragment_counts and min(fragment_counts) == 1:
        details.append("arguments 只出现 1 个分片（网关可能把增量合并了）")
    record["detail"] = (
        f"tool_calls 增量分片拼回 {len(calls)} 个完整调用，arguments JSON 可解析"
        + ("；" + "；".join(details) if details else "")
    )
    return settle(record, problems, text)


# ---------------------------------------------------------------------------
# case 4: parallel
# ---------------------------------------------------------------------------


def case_parallel(probe: Probe) -> dict:
    record = new_case("parallel")
    deadline = Deadline(probe.timeout)
    requested = ("read", "glob")
    payload = probe.payload(
        "同时调用 read 和 glob 两个工具：用 read 读 /etc/hostname，"
        "用 glob 列出当前目录下的 *.go 文件。"
        "两个工具必须在同一条回复里一起调用。",
        tool_names=requested,
        tool_choice="auto",
    )
    record["request"] = {"tools": list(requested), "tool_choice": "auto"}

    resp = post_json(probe.base + CHAT_PATH, payload, deadline.timeout(probe.timeout))
    record["http"] = resp["status"]
    record["latency_ms"] = round(resp["latency_ms"])
    text = resp["text"]
    problems = []

    if resp["error"]:
        problems.append(f"请求失败: {resp['error']}")
        record["detail"] = problems[0]
        return settle(record, problems, text)

    if resp["status"] != 200:
        problems.append(f"HTTP {resp['status']}")
        record["detail"] = f"HTTP {resp['status']}，响应体: {clip(text, 200)}"
        return settle(record, problems, text)

    doc, parse_error = parse_json_body(text)
    if parse_error:
        problems.append(parse_error)
        record["detail"] = parse_error
        return settle(record, problems, text)

    calls = extract_tool_calls(doc)
    record["tool_calls"] = [call["name"] for call in calls]
    record["tool_call_count"] = len(calls)

    notes = []
    if len(calls) < 2:
        record["soft"] = True
        problems.append(
            f"只返回 {len(calls)} 个 tool_calls，少于 2 个（模型可能只调了一个）"
        )

    for index, call in enumerate(calls):
        schema = probe.definitions.get(call["name"], {}).get("function", {}).get(
            "parameters"
        )
        if call["arguments_error"]:
            notes.append(f"tool_calls[{index}].arguments 解析失败: {call['arguments_error']}")
        elif isinstance(schema, dict) and isinstance(call["arguments"], dict):
            schema_problems = validate_arguments(call["arguments"], schema)
            if schema_problems:
                notes.append(
                    f"tool_calls[{index}]({call['name']}) 与 schema 不一致: "
                    + "; ".join(schema_problems)
                )

    record["schema_notes"] = notes
    if problems:
        record["detail"] = "; ".join(problems + notes)
    else:
        record["detail"] = (
            f"一条回复里返回 {len(calls)} 个 tool_calls: "
            + ", ".join(call["name"] or "<empty>" for call in calls)
        )
        if notes:
            record["detail"] += "；" + "；".join(notes)
    return settle(record, problems, text)


# ---------------------------------------------------------------------------
# case 5: errors（未知工具 / tool_choice=none / tool_choice=required）
# ---------------------------------------------------------------------------


def case_errors(probe: Probe) -> dict:
    record = new_case("errors")
    deadline = Deadline(probe.timeout)
    subs = []
    total_ms = 0.0

    # 5.1 tool_choice 指定不存在的工具
    resp = post_json(
        probe.base + CHAT_PATH,
        probe.payload(
            "Say hi in one word.",
            tool_names=("read",),
            tool_choice={"type": "function", "function": {"name": UNKNOWN_TOOL}},
        ),
        deadline.timeout(probe.timeout),
    )
    total_ms += resp["latency_ms"]
    problems = []
    if resp["error"]:
        problems.append(f"请求失败: {resp['error']}")
        detail = "未知工具请求没拿到响应"
    elif resp["status"] is None:
        problems.append("未知工具请求没有 HTTP 状态")
        detail = problems[-1]
    elif resp["status"] in (400, 404, 422):
        detail = f"网关拒绝未知工具 tool_choice（HTTP {resp['status']}）"
        body = clip(resp["text"], 160)
        if body:
            detail += f"，body={body!r}"
    elif resp["status"] == 200:
        doc, _ = parse_json_body(resp["text"])
        calls = extract_tool_calls(doc) if doc is not None else []
        bogus = [call for call in calls if call["name"] == UNKNOWN_TOOL]
        if bogus:
            problems.append(f"网关把不存在的工具 {UNKNOWN_TOOL} 当真实工具返回了")
            detail = problems[-1]
        else:
            detail = "网关放行未知工具名（HTTP 200），但没有产生伪造调用"
    else:
        problems.append(f"HTTP {resp['status']}")
        detail = f"未知工具请求 HTTP {resp['status']}，body={clip(resp['text'], 160)!r}"
    subs.append(_subcase("unknown_tool", resp, problems, detail))

    # 5.2 tool_choice="none" + 非空 tools
    resp = post_json(
        probe.base + CHAT_PATH,
        probe.payload(
            "Read /etc/hostname with the read tool.",
            tool_names=("read",),
            tool_choice="none",
        ),
        deadline.timeout(probe.timeout),
    )
    total_ms += resp["latency_ms"]
    problems = []
    if resp["error"]:
        problems.append(f"请求失败: {resp['error']}")
        detail = "tool_choice=none 请求没拿到响应"
    elif resp["status"] != 200:
        problems.append(f"HTTP {resp['status']}")
        detail = f"tool_choice=none HTTP {resp['status']}，body={clip(resp['text'], 160)!r}"
    else:
        doc, parse_error = parse_json_body(resp["text"])
        if parse_error:
            problems.append(parse_error)
            detail = parse_error
        else:
            calls = extract_tool_calls(doc)
            content = extract_text(chat_choice(doc).get("message", {}).get("content"))
            if calls:
                problems.append(
                    "tool_choice=none 仍然返回了 tool_calls: "
                    + ", ".join(call["name"] for call in calls)
                )
                detail = problems[-1]
            else:
                detail = (
                    "tool_choice=none 没有返回 tool_calls"
                    + (f"，content={clip(content, 60)!r}" if content.strip() else "")
                )
    subs.append(_subcase("tool_choice_none", resp, problems, detail))

    # 5.3 tool_choice="required"
    resp = post_json(
        probe.base + CHAT_PATH,
        probe.payload(
            "Say hi in one word.",
            tool_names=("read",),
            tool_choice="required",
        ),
        deadline.timeout(probe.timeout),
    )
    total_ms += resp["latency_ms"]
    problems = []
    if resp["error"]:
        problems.append(f"请求失败: {resp['error']}")
        detail = "tool_choice=required 请求没拿到响应"
    elif resp["status"] != 200:
        problems.append(f"HTTP {resp['status']}")
        detail = (
            f"tool_choice=required HTTP {resp['status']}，"
            f"body={clip(resp['text'], 160)!r}"
        )
    else:
        doc, parse_error = parse_json_body(resp["text"])
        if parse_error:
            problems.append(parse_error)
            detail = parse_error
        else:
            calls = extract_tool_calls(doc)
            if not calls:
                content = extract_text(chat_choice(doc).get("message", {}).get("content"))
                problems.append("tool_choice=required 却没有返回 tool_calls")
                detail = problems[-1] + (
                    f"，content={clip(content, 60)!r}" if content.strip() else ""
                )
            else:
                detail = "tool_choice=required 返回了 tool_calls: " + ", ".join(
                    call["name"] for call in calls
                )
    subs.append(_subcase("tool_choice_required", resp, problems, detail))

    return aggregate(record, subs, total_ms)


# ---------------------------------------------------------------------------
# case 6: endpoints（/v1/responses 与 /v1/messages）
# ---------------------------------------------------------------------------


def case_endpoints(probe: Probe) -> dict:
    record = new_case("endpoints")
    deadline = Deadline(probe.timeout)
    subs = []
    total_ms = 0.0

    requests = (
        (
            "responses",
            RESPONSES_PATH,
            {
                "model": probe.model,
                "input": "Say hi in one word.",
                "max_output_tokens": 16,
            },
        ),
        (
            "messages",
            MESSAGES_PATH,
            {
                "model": probe.model,
                "max_tokens": 16,
                "messages": [{"role": "user", "content": "Say hi in one word."}],
            },
        ),
    )

    for name, path, payload in requests:
        resp = post_json(probe.base + path, payload, deadline.timeout(probe.timeout))
        total_ms += resp["latency_ms"]
        status = resp["status"]
        problems = []
        if resp["error"]:
            problems.append(f"请求失败: {resp['error']}")
            detail = f"{path} 没拿到响应"
        elif status is None:
            problems.append("没有 HTTP 状态")
            detail = f"{path} 没有 HTTP 状态"
        elif status in QUOTA_STATUSES or classify_failure(
            status, resp["text"] or ""
        ) == "quota":
            problems.append(f"额度/鉴权受限（HTTP {status}），端点行为无法判定")
            detail = (
                f"HTTP {status}（额度/鉴权），body[0:200]="
                + repr(clip(resp["text"], ENDPOINT_BODY_CLIP))
            )
        elif 200 <= status < 300:
            detail = (
                f"HTTP {status}（2xx），body[0:200]="
                + repr(clip(resp["text"], ENDPOINT_BODY_CLIP))
            )
        elif 400 <= status < 500:
            detail = (
                f"HTTP {status}（明确的 4xx），body[0:200]="
                + repr(clip(resp["text"], ENDPOINT_BODY_CLIP))
            )
            if status in (404, 405):
                detail += "；404/405 通常表示该端点没有实现"
        else:
            problems.append(f"HTTP {status}（5xx）")
            detail = (
                f"HTTP {status}，body[0:200]="
                + repr(clip(resp["text"], ENDPOINT_BODY_CLIP))
            )

        sub = _subcase(name, resp, problems, detail)
        sub["path"] = path
        if not sub["body_head"]:  # quota 分支保留 300 字符，其余记 200 字符
            sub["body_head"] = clip(resp["text"], ENDPOINT_BODY_CLIP)
        subs.append(sub)

    return aggregate(record, subs, total_ms)


# ---------------------------------------------------------------------------
# 用例选择 / 报告
# ---------------------------------------------------------------------------


RUNNERS = {
    "args_schema": case_args_schema,
    "multiturn": case_multiturn,
    "streaming": case_streaming,
    "parallel": case_parallel,
    "errors": case_errors,
    "endpoints": case_endpoints,
}


def select_cases(only) -> list:
    if not only:
        return list(CASE_NAMES)
    wanted = []
    for raw in only.split(","):
        name = raw.strip().lower()
        if not name:
            continue
        name = CASE_ALIASES.get(name, name)
        if name not in CASE_NAMES:
            raise ProbeError(
                f"--only 里有未知 case {name!r}，可选: " + ", ".join(CASE_NAMES)
            )
        if name not in wanted:
            wanted.append(name)
    if not wanted:
        raise ProbeError("--only 没有选中任何 case，可选: " + ", ".join(CASE_NAMES))
    return [name for name in CASE_NAMES if name in wanted]


def summarize(record: dict) -> str:
    http = "-" if record["http"] is None else record["http"]
    verdict = "PASS" if record["ok"] else "FAIL"
    if record["soft"] and not record["ok"]:
        verdict = "FAIL(soft)"
    line = f"[{verdict}] {record['name']:<11} http={http} {record['latency_ms']}ms"
    if record["quota_suspected"]:
        line += " quota_suspected"
    if record["error"]:
        line += f"  error={record['error']}"
    return line


def print_dry_run(probe: Probe, specs, tools_path: Path) -> None:
    print(
        f"[dry-run] 目标: {probe.base}  model={probe.model}  "
        f"工具定义={tools_path}（{len(probe.definitions)} 个）"
    )
    for name in specs:
        if name == "args_schema":
            payload = probe.payload("...", tool_names=("read",), tool_choice="auto")
            size = len(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
            print(
                f"[dry-run] {name:<11} POST {CHAT_PATH}  tools=read  "
                f"tool_choice=auto  body={size}B  "
                f"断言: read.arguments 按 schema 校验（必填/类型/无多余字段）"
            )
        elif name == "multiturn":
            payload = probe.payload("...", tool_names=("read",), tool_choice="auto")
            size = len(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
            print(
                f"[dry-run] {name:<11} POST {CHAT_PATH} x2  tools=read  "
                f"body={size}B  "
                f"断言: role=tool 回灌后仍 200 / finish_reason 合理 / 不重复同一调用"
            )
        elif name == "streaming":
            payload = probe.payload(
                "...", tool_names=("read",), tool_choice="auto", stream=True
            )
            size = len(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
            print(
                f"[dry-run] {name:<11} POST {CHAT_PATH}  stream=true  tools=read  "
                f"body={size}B  断言: tool_calls 增量分片可拼回完整 name+arguments"
            )
        elif name == "parallel":
            payload = probe.payload(
                "...", tool_names=("read", "glob"), tool_choice="auto"
            )
            size = len(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
            print(
                f"[dry-run] {name:<11} POST {CHAT_PATH}  tools=read,glob  "
                f"tool_choice=auto  body={size}B  断言: tool_calls 数量 >= 2（soft）"
            )
        elif name == "errors":
            print(
                f"[dry-run] {name:<11} POST {CHAT_PATH} x3  "
                f"未知工具(tool_choice 指定) / tool_choice=none / tool_choice=required"
            )
        elif name == "endpoints":
            print(
                f"[dry-run] {name:<11} POST {RESPONSES_PATH} + POST {MESSAGES_PATH}  "
                f"断言: HTTP 200 或明确 4xx，记录状态码与 body 前 200 字符"
            )
    print("[dry-run] 不联网、不写报告，退出 0。")


def write_report(path: Path, report: dict) -> None:
    try:
        if str(path.parent):
            path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
    except OSError as exc:
        raise ProbeError(f"写不了报告 {path}: {exc}") from exc


def parse_args(argv) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="contract_probe.py",
        description=(
            "验 zen-direct 网关的响应侧工具契约（arguments schema、多轮回灌、"
            "流式分片、并行调用、错误路径、另两个协议端点）。"
        ),
        epilog="退出码: 0 全通过 / 1 有 case 失败 / 2 参数或环境错误。",
    )
    parser.add_argument(
        "--base", required=True, help="网关基址，如 http://127.0.0.1:8081"
    )
    parser.add_argument(
        "--model", required=True, help="模型名，如 deepseek-v4.1-flash-free"
    )
    parser.add_argument("--out", default=None, help="JSON 报告输出路径")
    parser.add_argument(
        "--timeout",
        type=float,
        default=DEFAULT_TIMEOUT,
        help=f"每个 case 的独立超时秒数（默认 {DEFAULT_TIMEOUT:.0f}）",
    )
    parser.add_argument(
        "--only",
        default=None,
        help="只跑子集，逗号分隔（可选: " + ",".join(CASE_NAMES) + "）",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="只打印将要发的请求摘要，不联网、不写报告",
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
    except SystemExit as exc:  # argparse: --help=0，参数错=2
        return int(exc.code or 0)

    try:
        if args.timeout <= 0:
            raise ProbeError(f"--timeout 必须 > 0，收到 {args.timeout}")
        if not (args.model or "").strip():
            raise ProbeError("--model 不能为空")
        base = normalize_base(args.base)
        model = args.model.strip()

        tools_path = (
            Path(args.tools_file).resolve() if args.tools_file else default_tools_path()
        )
        definitions = load_tool_definitions(tools_path)
        specs = select_cases(args.only)
        probe = Probe(base, model, args.timeout, definitions)

        if args.dry_run:
            print_dry_run(probe, specs, tools_path)
            return 0

        started_at = now_iso()
        started = time.perf_counter()

        health_before = read_health(base, health_timeout(args.timeout))
        if health_before.get("unreachable"):
            raise ProbeError(
                f"网关不可达: {health_before['url']} -> {health_before['error']}"
            )
        print(f"gateway: {base}  model={model}  tools={tools_path}")
        print(
            "health before: http={} ok={}".format(
                health_before["http"], health_before["ok"]
            )
        )

        cases = []
        for name in specs:
            try:
                record = RUNNERS[name](probe)
            except CaseTimeout as exc:
                record = new_case(name)
                record["detail"] = str(exc)
                record["error"] = str(exc)
                record["failure_class"] = "unknown"
            except Exception as exc:  # noqa: BLE001 - 一个 case 崩了不能带走整轮
                record = new_case(name)
                record["detail"] = f"case 内部异常: {type(exc).__name__}: {exc}"
                record["error"] = record["detail"]
                record["failure_class"] = "unknown"
            cases.append(record)
            print(summarize(record))

        health_after = read_health(base, health_timeout(args.timeout))

        passed = sum(1 for record in cases if record["ok"])
        failed = len(cases) - passed
        quota_suspected = any(record["quota_suspected"] for record in cases)

        report = {
            "base": base,
            "model": model,
            "started_at": started_at,
            "finished_at": now_iso(),
            "duration_ms": round((time.perf_counter() - started) * 1000),
            "cases": cases,
            "passed": passed,
            "failed": failed,
            "ok": failed == 0,
            "quota_suspected": quota_suspected,
            "health_before": health_before,
            "health_after": health_after,
            "tools_file": str(tools_path),
        }

        if args.out:
            out_path = Path(args.out)
            write_report(out_path, report)
            print(f"report: {out_path.resolve()}")

        print(f"VERDICT: {'PASS' if report['ok'] else 'FAIL'} {passed}/{len(cases)}")
        return 0 if report["ok"] else 1

    except ProbeError as exc:
        print(f"contract_probe: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
