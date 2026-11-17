"""test_tool_accuracy.py — 多轮工具准确率判据的单测（纯标准库，不打网络）。

跑法：
    python -m unittest tools.test_tool_accuracy -v

覆盖（假上游，注入 post_fn）：
    1. 第 1 轮 grep、第 2 轮 edit        -> hit, first_hit_turn=2, turns_used=2
    2. 全程不调工具                       -> no_tool_call，既不算 hit 也不算 miss
    3. 每轮发同一个调用                   -> stalled，且不跑满 max_turns
    4. 第 1 轮 429                        -> quota，不进分母
    5. 第 1 轮 shell（易混）、第 2 轮对   -> hit + confusion_tools_seen=["shell"]
    6. 只调易混工具                       -> confusion
    7. 中途不再调工具                     -> stalled
    8. 汇总键位/退出码语义                -> 0 全 hit / 1 有非 hit
    9. 多轮 messages 回灌形状             -> assistant(tool_calls) + tool(fixture)
"""

import json
import os
import sys
import unittest

try:
    from tools import tool_accuracy as ta
except ImportError:  # 直接 `python tools/test_tool_accuracy.py`
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    import tool_accuracy as ta


def _tool_call(name, args=None, cid=None):
    return {
        "id": cid or f"id_{name}",
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(args or {})},
    }


def _resp(*calls, content=""):
    msg = {"role": "assistant", "content": content}
    if calls:
        msg["tool_calls"] = list(calls)
    return {"choices": [{"index": 0, "message": msg, "finish_reason": "tool_calls" if calls else "stop"}]}


class FakePost:
    """按脚本逐次返回响应，并记录每次收到的 payload。

    脚本元素：dict 响应体 / ("status", code, body) 元组
    """

    def __init__(self, script):
        self.script = list(script)
        self.payloads = []
        self.calls = 0

    def __call__(self, base, payload, timeout):
        self.payloads.append(payload)
        self.calls += 1
        if not self.script:
            return 200, _resp()
        item = self.script.pop(0)
        if isinstance(item, tuple):
            return item[1], item[2]
        return 200, item


def task(**kw):
    base = {"id": "t5_edit", "prompt": "In main.go, replace the word foo with bar.",
            "expect": ["edit", "patch"], "confusion": ["write", "shell"], "max_turns": 4}
    base.update(kw)
    return base


# 用真实工具集校验任务集里引用的名字都存在（比硬编码一份假清单更可靠）
try:
    DEFS = ta.load_tools(ta.DEFAULT_TOOLS)
except Exception:  # 工具清单缺失时退化成最小集，保证判据单测仍能跑
    DEFS = {n: {} for n in ("read", "grep", "glob", "edit", "write", "patch",
                            "shell", "execute", "question", "subagent")}


class MultiTurnVerdictTest(unittest.TestCase):
    def run_task(self, script, **kw):
        fp = FakePost(script)
        r = ta.run_task("http://x", task(**kw), "m", DEFS, 1.0, post_fn=fp)
        return r, fp

    def test_grep_then_edit_is_hit_on_turn_2(self):
        r, fp = self.run_task([
            _resp(_tool_call("grep", {"pattern": "foo"})),
            _resp(_tool_call("edit", {"file": "main.go"})),
        ])
        self.assertEqual(r["verdict"], "hit")
        self.assertEqual(r["first_hit_turn"], 2)
        self.assertEqual(r["turns_used"], 2)
        self.assertEqual(r["called_all"], ["grep", "edit"])
        self.assertFalse(r["stalled"])
        self.assertEqual(fp.calls, 2)

    def test_no_tool_call_at_all(self):
        r, fp = self.run_task([_resp(content="I can't help with that.")])
        self.assertEqual(r["verdict"], "no_tool_call")
        self.assertIsNone(r["first_hit_turn"])
        self.assertEqual(r["turns_used"], 1)
        self.assertEqual(fp.calls, 1, "第 1 轮没调工具就该退出，不该空转")

    def test_identical_calls_every_turn_is_stalled_not_max_turns(self):
        same = _resp(_tool_call("read", {"path": "main.go"}))
        r, fp = self.run_task([same, same, same, same], max_turns=4)
        self.assertEqual(r["verdict"], "stalled")
        self.assertTrue(r["stalled"])
        self.assertEqual(r["turns_used"], 2, "第 2 轮发现重复就该退出")
        self.assertEqual(fp.calls, 2)

    def test_varying_but_useless_calls_exhaust_max_turns_as_miss(self):
        r, fp = self.run_task([
            _resp(_tool_call("read", {"path": "a"})),
            _resp(_tool_call("read", {"path": "b"})),
            _resp(_tool_call("read", {"path": "c"})),
            _resp(_tool_call("read", {"path": "d"})),
        ], max_turns=4)
        self.assertEqual(r["verdict"], "miss")
        self.assertEqual(r["turns_used"], 4)
        self.assertFalse(r["stalled"])

    def test_quota_on_first_turn(self):
        r, fp = self.run_task([
            ("status", 429, {"error": {"code": "FreeUsageLimit", "message": "quota"}}),
        ])
        self.assertEqual(r["verdict"], "quota")
        self.assertEqual(r["http"], 429)
        self.assertEqual(r["called_all"], [])

    def test_quota_mid_loop_discards_task(self):
        r, _ = self.run_task([
            _resp(_tool_call("grep", {"pattern": "foo"})),
            ("status", 429, {"error": {"message": "FreeUsageLimit"}}),
        ])
        self.assertEqual(r["verdict"], "quota")
        self.assertEqual(r["turns_used"], 2)
        self.assertEqual(r["called_all"], ["grep"], "已发生的调用仍要留证")

    def test_confusion_then_correct_is_hit_and_records_seen(self):
        r, _ = self.run_task([
            _resp(_tool_call("shell", {"cmd": "sed -i s/foo/bar/ main.go"})),
            _resp(_tool_call("edit", {"file": "main.go"})),
        ])
        self.assertEqual(r["verdict"], "hit")
        self.assertEqual(r["first_hit_turn"], 2)
        self.assertEqual(r["confusion_tools_seen"], ["shell"])

    def test_confusion_only(self):
        r, _ = self.run_task([
            _resp(_tool_call("shell", {"cmd": "sed -i s/foo/bar/ main.go"})),
            _resp(_tool_call("write", {"path": "main.go"})),
        ], max_turns=2)
        self.assertEqual(r["verdict"], "confusion")
        self.assertEqual(r["confusion_tools_seen"], ["shell", "write"])
        self.assertIsNone(r["first_hit_turn"])

    def test_tool_silence_after_first_turn_is_stalled(self):
        r, fp = self.run_task([
            _resp(_tool_call("grep", {"pattern": "foo"})),
            _resp(content="done, I already know the answer"),
        ])
        self.assertEqual(r["verdict"], "stalled")
        self.assertTrue(r["stalled"])
        self.assertEqual(fp.calls, 2)

    def test_http_error_is_flagged_not_silently_missed(self):
        r, _ = self.run_task([
            ("status", 500, {"_raw": "boom"}),
        ])
        self.assertEqual(r["verdict"], "miss")
        self.assertTrue(r["error"].startswith("http 500"))

    def test_probe_task_any_call_is_hit(self):
        r, _ = self.run_task([_resp(_tool_call("shell", {"cmd": "rm -f /tmp/old.log"}))],
                             id="t9_delete_file", expect=[], confusion=[], probe=True,
                             prompt="Delete /tmp/old.log")
        self.assertEqual(r["verdict"], "hit")
        self.assertEqual(r["called_all"], ["shell"])
        self.assertTrue(r["probe"])

    def test_max_turns_one_restores_single_turn_semantics(self):
        # --max-turns 1 用来复现旧的单轮判据：grep-then-edit 会重新变成 miss
        r, fp = self.run_task([
            _resp(_tool_call("grep", {"pattern": "foo"})),
            _resp(_tool_call("edit", {"file": "main.go"})),
        ], max_turns=1)
        self.assertEqual(r["verdict"], "miss")
        self.assertEqual(r["turns_used"], 1)
        self.assertEqual(fp.calls, 1)

    def test_multi_turn_messages_are_replayed_verbatim(self):
        _, fp = self.run_task([
            _resp(_tool_call("grep", {"pattern": "foo"}, cid="c1")),
            _resp(_tool_call("read", {"path": "main.go"})),
            _resp(_tool_call("edit", {"file": "main.go"})),
        ])
        second = fp.payloads[1]["messages"]
        self.assertEqual(second[0]["role"], "user")
        self.assertEqual(second[1]["role"], "assistant")
        self.assertEqual(second[1]["tool_calls"][0]["id"], "c1")
        self.assertEqual(second[1]["tool_calls"][0]["function"]["name"], "grep")
        self.assertEqual(second[2]["role"], "tool")
        self.assertEqual(second[2]["tool_call_id"], "c1")
        self.assertEqual(second[2]["content"], ta.FIXTURE_RESULT)
        self.assertTrue(json.loads(second[2]["content"])["ok"])
        # tools 每轮都要重新带上
        self.assertTrue(fp.payloads[1]["tools"])
        self.assertEqual(fp.payloads[1]["tool_choice"], "auto")


class TaskSetTest(unittest.TestCase):
    def test_ten_tasks_with_max_turns(self):
        self.assertEqual(len(ta.TASKS), 10)
        ids = [t["id"] for t in ta.TASKS]
        self.assertEqual(len(ids), len(set(ids)), "任务 id 不能重复")
        for t in ta.TASKS:
            self.assertTrue(t["prompt"])
            self.assertIn("max_turns", t)
            self.assertGreaterEqual(t["max_turns"], 2)
            self.assertIsInstance(t["expect"], list)
            self.assertIsInstance(t["confusion"], list)
            # expect 与 confusion 不该重叠，否则同一调用既是命中又是混淆
            self.assertFalse(set(t["expect"]) & set(t["confusion"]), t["id"])
            for name in t["expect"] + t["confusion"]:
                self.assertIn(name, DEFS, f"{t['id']} 引用了不存在的工具 {name}")

    def test_exploratory_tasks_have_more_turns(self):
        by_id = {t["id"]: t for t in ta.TASKS}
        self.assertGreaterEqual(by_id["t5_edit"]["max_turns"], 4)
        self.assertGreaterEqual(by_id["t7_read_then_edit"]["max_turns"], 4)
        self.assertGreaterEqual(by_id["t10_grep_vs_glob"]["max_turns"], 4)

    def test_read_head_forbids_shell(self):
        by_id = {t["id"]: t for t in ta.TASKS}
        self.assertIn("shell", by_id["t8_read_head"]["confusion"])
        self.assertEqual(by_id["t8_read_head"]["expect"], ["read"])

    def test_delete_task_is_probe_only(self):
        by_id = {t["id"]: t for t in ta.TASKS}
        self.assertTrue(by_id["t9_delete_file"]["probe"])
        self.assertEqual(by_id["t9_delete_file"]["expect"], [])
        # 官方工具集确实没有删除工具；哪天加了，这条任务就该改成 expect=["delete"]
        self.assertNotIn("delete", DEFS)
        self.assertNotIn("remove", DEFS)


class SummaryTest(unittest.TestCase):
    def test_summary_keys_and_math(self):
        results = [
            {"task": "a", "verdict": "hit", "first_hit_turn": 1, "turns_used": 1,
             "confusion_tools_seen": [], "probe": False, "error": ""},
            {"task": "b", "verdict": "hit", "first_hit_turn": 3, "turns_used": 3,
             "confusion_tools_seen": ["shell"], "probe": False, "error": ""},
            {"task": "c", "verdict": "confusion", "first_hit_turn": None, "turns_used": 3,
             "confusion_tools_seen": ["glob"], "probe": False, "error": ""},
            {"task": "d", "verdict": "miss", "first_hit_turn": None, "turns_used": 4,
             "confusion_tools_seen": [], "probe": False, "error": ""},
            {"task": "e", "verdict": "stalled", "first_hit_turn": None, "turns_used": 2,
             "confusion_tools_seen": [], "probe": False, "error": ""},
            {"task": "f", "verdict": "no_tool_call", "first_hit_turn": None, "turns_used": 1,
             "confusion_tools_seen": [], "probe": False, "error": ""},
            {"task": "g", "verdict": "quota", "first_hit_turn": None, "turns_used": 1,
             "confusion_tools_seen": [], "probe": False, "error": ""},
            {"task": "h", "verdict": "hit", "first_hit_turn": 1, "turns_used": 1,
             "confusion_tools_seen": [], "probe": True, "error": ""},
        ]
        s = ta.summarize(results, "http://x", "m", 1, 13, ta.TASKS)
        for key in ("hits", "confusions", "misses", "quota", "judged", "hit_rate",
                    "first_turn_hits", "avg_turns", "stalled", "no_tool_call",
                    "total", "tools", "tasks", "results"):
            self.assertIn(key, s, key)
        self.assertEqual(s["hits"], 3)
        self.assertEqual(s["confusions"], 1)
        self.assertEqual(s["misses"], 1)
        self.assertEqual(s["quota"], 1)
        self.assertEqual(s["stalled"], 1)
        self.assertEqual(s["no_tool_call"], 1)
        self.assertEqual(s["judged"], 7, "quota 不进分母")
        self.assertEqual(s["hits"] + s["confusions"] + s["misses"] + s["stalled"]
                         + s["no_tool_call"], s["judged"])
        self.assertEqual(s["hit_rate"], round(3 / 7, 3))
        self.assertEqual(s["first_turn_hits"], 2)
        self.assertEqual(s["avg_turns"], round((1 + 3 + 1) / 3, 2))
        self.assertEqual(s["probes"], 1)
        self.assertEqual(s["hit_rate_excl_probe"], round(2 / 6, 3))


class ExitCodeTest(unittest.TestCase):
    def exit_for(self, results):
        s = ta.summarize(results, "http://x", "m", 1, 13, ta.TASKS)
        return 0 if s["hits"] == s["judged"] else 1

    def test_all_hit_is_zero(self):
        rs = [{"task": "a", "verdict": "hit", "first_hit_turn": 1, "turns_used": 1,
               "confusion_tools_seen": [], "probe": False, "error": ""}]
        self.assertEqual(self.exit_for(rs), 0)

    def test_non_hit_is_one(self):
        for v in ("confusion", "miss", "stalled", "no_tool_call"):
            rs = [{"task": "a", "verdict": v, "first_hit_turn": None, "turns_used": 2,
                   "confusion_tools_seen": [], "probe": False, "error": ""}]
            self.assertEqual(self.exit_for(rs), 1, v)

    def test_quota_only_is_environment_error(self):
        rs = [{"task": "a", "verdict": "quota", "first_hit_turn": None, "turns_used": 1,
               "confusion_tools_seen": [], "probe": False, "error": ""}]
        s = ta.summarize(rs, "http://x", "m", 1, 13, ta.TASKS)
        self.assertEqual(s["judged"], 0)
        self.assertEqual(s["hit_rate"], 0.0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
