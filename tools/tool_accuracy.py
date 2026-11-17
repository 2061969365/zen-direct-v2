"""工具调用准确率基准：同一批任务，看模型有没有选对工具。

为什么需要它：P0/P1 改的是「模型看到的名字和描述」，这种改动没法用
「HTTP 200」证明好坏。只能拿固定任务集去问，统计命中率。

判据只有三个：
  hit        调了期望的工具（任何一个都算）
  confusion  调了「易混清单」里的工具（edit 调成 patch 这种）
  miss       什么工具都没调，或者调了个无关的

用法：
  python tools/tool_accuracy.py --base http://127.0.0.1:8081 --model deepseek-v4.1-flash-free \
      --out acc.json [--trials 2] [--only t1,t3] [--dry-run]
退出码：0 全部命中 / 1 有未命中 / 2 环境或参数错误
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

# 任务集：prompt 用英文（工具描述是英文，避免引入语言变量）
# expect    期望调用的工具
# confusion 易混工具：调了它们说明选择错误，单独记一类而不是笼统计 miss
TASKS = [
    {
        "id": "t1_read",
        "prompt": "Show me the contents of the file /etc/hostname.",
        "expect": ["read"],
        "confusion": ["write", "edit", "patch", "shell"],
    },
    {
        "id": "t2_glob",
        "prompt": "List every .go file under the current directory tree.",
        "expect": ["glob"],
        "confusion": ["grep", "shell", "read"],
    },
    {
        "id": "t3_grep",
        "prompt": "Find every line containing TODO inside src/.",
        "expect": ["grep"],
        "confusion": ["glob", "read", "shell"],
    },
    {
        "id": "t4_write",
        "prompt": "Create a new file /tmp/notes.txt containing exactly: hello",
        "expect": ["write"],
        "confusion": ["edit", "patch", "shell"],
    },
    {
        "id": "t5_edit",
        "prompt": "In main.go, replace the word foo with bar.",
        "expect": ["edit", "patch"],   # 两者都算对，但要记下选了哪个
        "confusion": ["write", "shell"],
    },
    {
        "id": "t6_shell",
        "prompt": "Run the shell command `uname -s` and tell me its output.",
        "expect": ["shell"],
        "confusion": ["execute", "read"],
    },
]


def load_tools(path: str) -> dict:
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def build_payload(model: str, defs: dict, prompt: str) -> dict:
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
    return {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "tools": tools,
        "tool_choice": "auto",
        "max_tokens": 256,
    }


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


def called_names(data: dict) -> list[str]:
    out = []
    for ch in data.get("choices") or []:
        for tc in (ch.get("message") or {}).get("tool_calls") or []:
            fn = tc.get("function") or {}
            if fn.get("name"):
                out.append(fn["name"])
    return out


def is_quota(status: int, data: dict) -> bool:
    """429/402 或者 body 里点名额度，都不能算「选错了工具」。

    这条很关键：免费层按出口 IP 计量，跑十几发就会撞上 429。
    把它混进 miss 会让「改描述前后」的比较彻底失真。
    """
    if status in (401, 402, 429):
        return True
    text = json.dumps(data, ensure_ascii=False).lower()
    return any(k in text for k in ("freeusagelimit", "usage limit", "quota"))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8081")
    ap.add_argument("--model", default="deepseek-v4.1-flash-free")
    ap.add_argument("--out", default="accuracy.json")
    ap.add_argument("--tools-file", default=DEFAULT_TOOLS)
    ap.add_argument("--trials", type=int, default=2)
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--only", default="")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    if args.trials <= 0:
        print("asset: --trials 必须 > 0", file=sys.stderr)
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
        print(f"[dry-run] base={args.base} model={args.model} tools={len(defs)} trials={args.trials}")
        for t in tasks:
            p = build_payload(args.model, defs, t["prompt"])
            print(f"[dry-run] {t['id']:12} expect={'|'.join(t['expect']):12} "
                  f"confusion={'|'.join(t['confusion']):20} body={len(json.dumps(p))}B")
        return 0

    try:
        urllib.request.urlopen(args.base.rstrip("/") + "/health", timeout=10).read()
    except Exception as exc:
        print(f"asset: 网关不可达 {args.base}/health -> {exc}", file=sys.stderr)
        return 2

    results = []
    for task in tasks:
        for trial in range(1, args.trials + 1):
            payload = build_payload(args.model, defs, task["prompt"])
            started = time.time()
            status, data = post(args.base, payload, args.timeout)
            latency = int((time.time() - started) * 1000)
            names = called_names(data) if status == 200 else []
            if is_quota(status, data):
                verdict = "quota"
            elif any(n in task["expect"] for n in names):
                verdict = "hit"
            elif [n for n in names if n in task["confusion"]]:
                verdict = "confusion"
            else:
                verdict = "miss"
            results.append({
                "task": task["id"], "trial": trial, "http": status,
                "latency_ms": latency, "called": names,
                "expect": task["expect"], "verdict": verdict,
            })
            print(f"[{verdict:9}] {task['id']:12} trial={trial} http={status} "
                  f"called={','.join(names) or '-'} ({latency}ms)")

    hits = sum(1 for r in results if r["verdict"] == "hit")
    conf = sum(1 for r in results if r["verdict"] == "confusion")
    miss = sum(1 for r in results if r["verdict"] == "miss")
    quota = sum(1 for r in results if r["verdict"] == "quota")
    total = len(results)
    judged = total - quota          # 额度失败不进分母，否则比率没有意义
    report = {
        "base": args.base, "model": args.model, "trials": args.trials,
        "tools": len(defs), "results": results,
        "hits": hits, "confusions": conf, "misses": miss, "quota": quota,
        "total": total, "judged": judged,
        "hit_rate": round(hits / judged, 3) if judged else 0.0,
    }
    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=2)
    print(f"TOTAL hit={hits} confusion={conf} miss={miss} quota={quota} "
          f"judged={judged}/{total}  hit_rate={report['hit_rate']}  -> {args.out}")
    if quota and judged == 0:
        print("asset: 全部撞额度，本轮无有效样本", file=sys.stderr)
        return 2
    return 0 if miss == 0 and conf == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
