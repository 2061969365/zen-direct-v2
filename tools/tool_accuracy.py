"""工具调用准确率基准（多轮版）：同一批任务，看模型有没有在允许的轮次内选对工具。

为什么需要它：P0/P1 改的是「模型看到的名字和描述」，这种改动没法用
「HTTP 200」证明好坏。只能拿固定任务集去问，统计命中率。

单轮判据的问题（实测）：任务「把 main.go 里的 foo 换成 bar」，模型先 grep
定位、下一轮再 edit —— 这是正确行为，但单轮只看第一轮，于是判成 miss。
指标把好行为算成坏行为，A/B 就废了。所以改成多轮：允许模型先用辅助工具
探路，只要在 max_turns 之内调到了期望工具就算命中。

判据（每条 task 一个 verdict）：
  hit          多轮里调到了 expect 里的工具（first_hit_turn 记录第几轮）
  confusion    全程没调过期望工具，但调过 confusion 清单里的邻近工具
  miss         全程没调期望工具，也没调易混工具（调了无关工具或空转到上限）
  stalled      第 1 轮之后不再调工具，或某轮 tool_calls 与上一轮完全相同
  no_tool_call 第 1 轮就一个工具都没调
  quota        429/402/额度耗尽，整条 task 作废，不进分母

多轮循环（每条 task）：
  messages = [user: prompt]
  for turn in 1..max_turns:
      resp = POST(messages, tools)
      if quota: verdict=quota, break
      calls = resp.tool_calls
      if not calls: turn==1 -> no_tool_call else stalled, break
      if expect 命中: hit, first_hit_turn=turn, break
      if calls 与上一轮完全相同: stalled, break        # 死循环保护
      回灌 assistant(tool_calls) + 每个 call 一条 fixture tool 结果
  工具结果一律是 FIXTURE_RESULT，不读写真实文件系统。

用法：
  python tools/tool_accuracy.py --base http://127.0.0.1:8081 --model deepseek-v4.1-flash-free \
      --out acc.json [--trials 2] [--only t1_read,t5_edit] [--max-turns 3] [--dry-run]
退出码：0 全部命中 / 1 有非命名中 / 2 环境或参数错误
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_TOOLS = os.path.join(HERE, "opencode_tools.json")

# 假结果：多轮回灌的 tool 消息内容。绝不触碰真实文件系统。
FIXTURE_RESULT = '{"ok": true, "note": "fixture: no real filesystem access"}'

DEFAULT_MAX_TURNS = 3

# 任务集：prompt 用英文（工具描述是英文，避免引入语言变量）
# expect    期望调用的工具（多轮内任意一轮出现即命中）
# confusion 易混工具：全程只调它们、始终没调到 expect，才算 confusion
# max_turns 允许的轮数：直接型 2，需要探路/搜索的给 3~4
# probe     探测型任务：工具集里没有标准答案（如删除），任何调用都算 hit，
#           只记录模型怎么绕，不判它错
TASKS = [
    {
        "id": "t1_read",
        "prompt": "Show me the contents of the file /etc/hostname.",
        "expect": ["read"],
        "confusion": ["write", "edit", "patch", "shell"],
        "max_turns": 2,
    },
    {
        "id": "t2_glob",
        "prompt": "List every .go file under the current directory tree.",
        "expect": ["glob"],
        "confusion": ["grep", "shell", "read"],
        "max_turns": 3,
    },
    {
        "id": "t3_grep",
        "prompt": "Find every line containing TODO inside src/.",
        "expect": ["grep"],
        "confusion": ["glob", "read", "shell"],
        "max_turns": 3,
    },
    {
        "id": "t4_write",
        "prompt": "Create a new file /tmp/notes.txt containing exactly: hello",
        "expect": ["write"],
        "confusion": ["edit", "patch", "shell"],
        "max_turns": 2,
    },
    {
        # 典型的「先探路再动手」：单轮判据在这里会误杀正确行为
        "id": "t5_edit",
        "prompt": "In main.go, replace the word foo with bar.",
        "expect": ["edit", "patch"],
        "confusion": ["write", "shell"],
        "max_turns": 4,
    },
    {
        "id": "t6_shell",
        "prompt": "Run the shell command `uname -s` and tell me its output.",
        "expect": ["shell"],
        "confusion": ["execute", "read"],
        "max_turns": 2,
    },
    {
        # 必须先读后改：read 是探路，edit 才是答案
        "id": "t7_read_then_edit",
        "prompt": "Read config.yaml, then change the line `version: 1.0.0` to `version: 1.1.0`.",
        "expect": ["edit", "patch"],
        "confusion": ["write", "shell"],
        "max_turns": 4,
    },
    {
        # 明确不该用 shell：正解是 read + limit
        "id": "t8_read_head",
        "prompt": "Show me only the first 10 lines of /var/log/app.log. Do not run a shell command.",
        "expect": ["read"],
        "confusion": ["shell", "grep"],
        "max_turns": 2,
    },
    {
        # 工具集里没有 delete：看模型怎么绕（shell rm / write 清空 / edit / 反问）
        # probe=True，不判错，只记录选择
        "id": "t9_delete_file",
        "prompt": "Delete the file /tmp/old.log entirely so that it no longer exists.",
        "expect": [],
        "confusion": [],
        "max_turns": 3,
        "probe": True,
        "note": "no delete tool in toolset; any call is recorded, not judged wrong",
    },
    {
        # grep vs glob 的边界：问「哪些文件含某内容」
        "id": "t10_grep_vs_glob",
        "prompt": "Which files under internal/ contain the string tool_shape? List the matching file names.",
        "expect": ["grep"],
        "confusion": ["glob", "shell", "read"],
        "max_turns": 4,
    },
]


def load_tools(path: str) -> dict:
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def tool_defs(defs: dict) -> list[dict]:
    tools = []
    for name, d in defs.items():
        tools.append({
            "type": "function",
            "function": {
                "name": name,
                "description": d.get("description", ""),
                "parameters": d.get("parameters", {"type": "object", "properties": {}}),
            },
        })
    return tools


def build_payload(model: str, defs: dict, messages) -> dict:
    """messages 可以是 prompt 字符串（单轮），也可以是完整的 messages 列表（多轮）。"""
    if isinstance(messages, str):
        messages = [{"role": "user", "content": messages}]
    return {
        "model": model,
        "messages": list(messages),
        "tools": tool_defs(defs),
        "tool_choice": "auto",
        "max_tokens": 256,
    }


def parser_add_common(p: argparse.ArgumentParser) -> None:
    p.add_argument("--base", default="http://127.0.0.1:8081")
    p.add_argument("--model", default="deepseek-v4.1-flash-free")
    p.add_argument("--out", default="accuracy.json")
    p.add_argument("--tools-file", default=DEFAULT_TOOLS)
    p.add_argument("--trials", type=int, default=2)
    p.add_argument("--timeout", type=float, default=120.0)
    p.add_argument("--only", default="")
    p.add_argument("--max-turns", type=int, default=0,
                   help="覆盖任务自带的 max_turns（0=用任务集里的值）")
    p.add_argument("--dry-run", action="store_true")


def post(base: str, payload: dict, timeout: float) -> tuple[int, dict]:
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"content-type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(body)
        except Exception:
            return exc.code, {"_raw": body[:500]}


def response_message(data: dict) -> dict:
    for ch in data.get("choices") or []:
        if ch.get("message"):
            return ch["message"]
    return {}


def response_calls(data: dict, turn: int = 0) -> list[dict]:
    """把一轮响应里的 tool_calls 规范成 [{id, type, function:{name, arguments}}]。"""
    out = []
    msg = response_message(data)
    for i, tc in enumerate(msg.get("tool_calls") or []):
        fn = tc.get("function") or {}
        name = fn.get("name") or ""
        if not name:
            continue
        args = fn.get("arguments")
        if args is None:
            args = "{}"
        elif not isinstance(args, str):
            args = json.dumps(args, ensure_ascii=False, sort_keys=True)
        out.append({
            "id": tc.get("id") or f"call_{turn}_{i}",
            "type": tc.get("type") or "function",
            "function": {"name": name, "arguments": args},
        })
    return out


def called_names(data: dict) -> list[str]:
    return [c["function"]["name"] for c in response_calls(data)]


def call_signature(calls: list[dict]) -> tuple:
    """名字+参数完全相同才算「同一轮重复调用」，用于死循环保护。"""
    parts = []
    for c in calls:
        raw = c["function"]["arguments"]
        try:
            norm = json.dumps(json.loads(raw), sort_keys=True, ensure_ascii=False)
        except Exception:
            norm = " ".join(str(raw).split())
        parts.append((c["function"]["name"], norm))
    return tuple(sorted(parts))


def is_quota(status: int, data: dict) -> bool:
    """429/402 或者 body 里点名额度，都不能算「选错了工具」。

    这条很关键：免费层按出口 IP 计量，跑十几发就会撞上 429。
    把它混进 miss 会让「改描述前后」的比较彻底失真。多轮里某一轮撞上，
    整条 task 记 quota（前面几轮的调用记录仍保留在 called_all 里）。
    """
    if status in (401, 402, 429):
        return True
    text = json.dumps(data, ensure_ascii=False).lower()
    return any(k in text for k in ("freeusagelimit", "usage limit", "quota"))


def assistant_turn(data: dict, calls: list[dict]) -> dict:
    return {
        "role": "assistant",
        "content": response_message(data).get("content") or "",
        "tool_calls": calls,
    }


def run_task(base: str, task: dict, model: str, defs: dict, timeout: float,
             post_fn=None, max_turns: int = 0) -> dict:
    """跑一条任务的多轮循环，返回结果字典。post_fn 可注入（单测用假上游）。"""
    post_fn = post_fn or post
    limit = max_turns or task.get("max_turns") or DEFAULT_MAX_TURNS
    expect = list(task.get("expect") or [])
    confusion = list(task.get("confusion") or [])
    probe = bool(task.get("probe"))

    messages: list[dict] = [{"role": "user", "content": task["prompt"]}]
    called_all: list[str] = []
    confusion_seen: list[str] = []
    turn_calls: list[list] = []
    turns_used = 0
    first_hit_turn = None
    stalled = False
    http_last = 0
    error = ""
    prev_sig = None
    verdict = ""

    started = time.time()
    for turn in range(1, limit + 1):
        payload = build_payload(model, defs, messages)
        status, data = post_fn(base, payload, timeout)
        turns_used = turn
        http_last = status

        if is_quota(status, data):
            verdict = "quota"
            break

        calls = response_calls(data, turn) if status == 200 else []
        for c in calls:
            name = c["function"]["name"]
            if name not in called_all:
                called_all.append(name)
            if name in confusion and name not in confusion_seen:
                confusion_seen.append(name)
        turn_calls.append([turn, [c["function"]["name"] for c in calls]])

        if status != 200:
            # 非额度错误（5xx/400）：不要伪装成「模型选错工具」，单独记 error
            error = f"http {status}: {json.dumps(data, ensure_ascii=False)[:200]}"
            verdict = "confusion" if confusion_seen else "miss"
            break

        if not calls:
            # 第 1 轮就不动手 = no_tool_call；之后突然不动 = stalled
            verdict = "no_tool_call" if turn == 1 else "stalled"
            stalled = verdict == "stalled"
            break

        hit = [c for c in called_all if c in expect]
        if hit or (probe and calls):
            verdict = "hit"
            first_hit_turn = turn
            break

        sig = call_signature(calls)
        if prev_sig is not None and sig == prev_sig:
            # 每轮发一模一样的调用：空转到上限没有信息量，提前收
            stalled = True
            verdict = "stalled"
            break
        prev_sig = sig

        # 原样回灌 assistant 消息 + 每个 tool_call 一条 fixture 结果
        messages.append(assistant_turn(data, calls))
        for c in calls:
            messages.append({
                "role": "tool",
                "tool_call_id": c["id"],
                "content": FIXTURE_RESULT,
            })

    if not verdict:  # 跑满 max_turns 还没命中
        verdict = "confusion" if confusion_seen else "miss"

    return {
        "task": task["id"],
        "verdict": verdict,
        "http": http_last,
        "latency_ms": int((time.time() - started) * 1000),
        "called": called_all,          # 兼容 CI：老键名指向去重后的全轮次调用
        "called_all": called_all,
        "confusion_tools_seen": confusion_seen,
        "first_hit_turn": first_hit_turn,
        "turns_used": turns_used,
        "max_turns": limit,
        "stalled": stalled,
        "expect": expect,
        "probe": probe,
        "turn_calls": turn_calls,
        "error": error,
    }


def summarize(results: list[dict], base: str, model: str, trials: int, tool_count: int,
              tasks: list[dict]) -> dict:
    hits = sum(1 for r in results if r["verdict"] == "hit")
    conf = sum(1 for r in results if r["verdict"] == "confusion")
    miss = sum(1 for r in results if r["verdict"] == "miss")
    quota = sum(1 for r in results if r["verdict"] == "quota")
    no_call = sum(1 for r in results if r["verdict"] == "no_tool_call")
    stalled = sum(1 for r in results if r["verdict"] == "stalled")
    errors = sum(1 for r in results if r.get("error"))
    total = len(results)
    judged = total - quota          # 额度失败不进分母，否则比率没有意义
    first_turn_hits = sum(1 for r in results if r["verdict"] == "hit" and r["first_hit_turn"] == 1)
    hit_turns = [r["turns_used"] for r in results if r["verdict"] == "hit"]
    avg_turns = round(sum(hit_turns) / len(hit_turns), 2) if hit_turns else 0.0
    probes = sum(1 for r in results if r.get("probe"))
    non_probe = judged - probes
    non_probe_hits = sum(1 for r in results
                         if r["verdict"] == "hit" and not r.get("probe"))
    return {
        "base": base, "model": model, "trials": trials,
        "tools": tool_count, "tasks": [t["id"] for t in tasks],
        "results": results,
        "hits": hits, "confusions": conf, "misses": miss, "quota": quota,
        "no_tool_call": no_call, "stalled": stalled, "errors": errors,
        "total": total, "judged": judged,
        "hit_rate": round(hits / judged, 3) if judged else 0.0,
        "first_turn_hits": first_turn_hits,
        "avg_turns": avg_turns,
        "probes": probes,
        "hit_rate_excl_probe": round(non_probe_hits / non_probe, 3) if non_probe else 0.0,
    }


def print_result(r: dict) -> None:
    trial = r.get("trial")
    prefix = f"{r['task']:16} trial={trial}"
    extra = ""
    if r["verdict"] == "hit" and r["first_hit_turn"]:
        extra = f" first_hit_turn={r['first_hit_turn']}"
    if r["confusion_tools_seen"]:
        extra += f" confusion_seen={','.join(r['confusion_tools_seen'])}"
    print(f"[{r['verdict']:11}] {prefix} turns={r['turns_used']}/{r['max_turns']}"
          f"{extra} called={','.join(r['called_all']) or '-'} ({r['latency_ms']}ms)")
    if r.get("error"):
        print(f"[error      ] {r['task']:16} {r['error']}")


def main() -> int:
    ap = argparse.ArgumentParser()
    parser_add_common(ap)
    args = ap.parse_args()

    if args.trials <= 0:
        print("asset: --trials 必须 > 0", file=sys.stderr)
        return 2
    if args.max_turns < 0:
        print("asset: --max-turns 不能为负", file=sys.stderr)
        return 2
    if not os.path.exists(args.tools_file):
        print(f"asset: 工具定义不存在 {args.tools_file}", file=sys.stderr)
        return 2

    defs = load_tools(args.tools_file)
    tasks = TASKS
    if args.only:
        want = {s.strip() for s in args.only.split(",") if s.strip()}
        tasks = [t for t in TASKS if t["id"] in want]
        if not tasks:
            print("asset: --only 没有匹配的任务", file=sys.stderr)
            return 2

    if args.dry_run:
        total_turns = 0
        print(f"[dry-run] base={args.base} model={args.model} tools={len(defs)} "
              f"trials={args.trials} tasks={len(tasks)}")
        for t in tasks:
            limit = args.max_turns or t.get("max_turns") or DEFAULT_MAX_TURNS
            total_turns += limit * args.trials
            p = build_payload(args.model, defs, t["prompt"])
            print(f"[dry-run] {t['id']:16} expect={'|'.join(t['expect']) or '-':16} "
                  f"confusion={'|'.join(t['confusion']) or '-':20} "
                  f"max_turns={limit} probe={1 if t.get('probe') else 0} "
                  f"body={len(json.dumps(p))}B")
        print(f"[dry-run] 最多 {total_turns} 次请求（{len(tasks)} 任务 x {args.trials} trials，"
              f"实际命中即提前退出）")
        return 0

    try:
        urllib.request.urlopen(args.base.rstrip("/") + "/health", timeout=10).read()
    except Exception as exc:
        print(f"asset: 网关不可达 {args.base}/health -> {exc}", file=sys.stderr)
        return 2

    results = []
    for task in tasks:
        for trial in range(1, args.trials + 1):
            r = run_task(args.base, task, args.model, defs, args.timeout,
                         max_turns=args.max_turns)
            r["trial"] = trial
            results.append(r)
            print_result(r)

    report = summarize(results, args.base, args.model, args.trials, len(defs), tasks)
    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=2)
    print(f"TOTAL hit={report['hits']} confusion={report['confusions']} "
          f"miss={report['misses']} quota={report['quota']} "
          f"no_tool_call={report['no_tool_call']} stalled={report['stalled']} "
          f"judged={report['judged']}/{report['total']} "
          f"hit_rate={report['hit_rate']} first_turn_hits={report['first_turn_hits']} "
          f"avg_turns={report['avg_turns']} -> {args.out}")
    if report["quota"] and report["judged"] == 0:
        print("asset: 全部撞额度，本轮无有效样本", file=sys.stderr)
        return 2
    # 0 = 全 hit；1 = 有任何非 hit（confusion/miss/stalled/no_tool_call/error）
    return 0 if report["hits"] == report["judged"] else 1


if __name__ == "__main__":
    sys.exit(main())
