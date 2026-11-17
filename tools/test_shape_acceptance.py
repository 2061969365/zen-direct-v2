"""test_shape_acceptance.py — 透传判据验收探针的纯函数单测（零网络、零 Go 二进制）。

跑法：
    python -m unittest tools.test_shape_acceptance -v

覆盖：
    - 场景构造：s12 = 12 / s13 = 13 / s14 = 14 / s12_rewritten = 12，成员与顺序正确
    - 原始定义 vs 仓库改写定义的区分（以及它如何影响 content 档的预期分支）
    - 额度判定：429/402、200 但 body 含 FreeUsageLimitError、400 invalid tool
    - 三分类（hit / quota / error）与退出码（0 / 1 / 2）
    - 汇总表格式与判词
    - 定义装载的三种形状（资产 {order,tools}、仓库 dict、抓包 list）
"""

import contextlib
import io
import json
import os
import platform
import sys
import tempfile
import unittest

try:
    from tools import shape_acceptance as sa
except ImportError:  # 直接 `python tools/test_shape_acceptance.py`
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    import shape_acceptance as sa


def pristine_fixture():
    """模拟公共资产：13 个工具，描述带 [pristine] 标记。"""
    names = [n for n in sa.OFFICIAL_ORDER if n != "experimental_image_generate"]
    tools = {}
    for name in names:
        tools[name] = {
            "description": "[pristine] %s" % name,
            "parameters": {"type": "object", "properties": {"a": {"type": "string"}}},
            "strict": False,
        }
    return {"order": list(names), "tools": tools}


def rewritten_fixture():
    """模拟仓库 tools/opencode_tools.json：同样的名字，描述被覆盖层改过。"""
    defs = {}
    for name, item in pristine_fixture()["tools"].items():
        defs[name] = {
            "description": "[rewritten] %s" % name,
            "parameters": {"type": "object", "properties": {"b": {"type": "integer"}}},
        }
    return defs


class ScenarioConstructionTest(unittest.TestCase):
    def setUp(self):
        self.pristine = sa.load_definitions(pristine_fixture())
        self.rewritten = sa.load_definitions(rewritten_fixture())

    def test_s12_is_twelve_without_the_two_extras(self):
        names = [t["function"]["name"] for t in sa.scenario_tools("s12_pristine", self.pristine, self.rewritten)]
        self.assertEqual(len(names), 12)
        self.assertEqual(names, sa.CORE_NAMES)
        self.assertNotIn("patch", names)
        self.assertNotIn("experimental_image_generate", names)

    def test_s13_adds_only_experimental_image_generate(self):
        names = [t["function"]["name"] for t in sa.scenario_tools("s13_pristine", self.pristine, self.rewritten)]
        self.assertEqual(len(names), 13)
        self.assertIn("experimental_image_generate", names)
        self.assertNotIn("patch", names)
        self.assertEqual(names[:12], sa.CORE_NAMES)

    def test_s14_is_all_fourteen_in_official_order(self):
        names = [t["function"]["name"] for t in sa.scenario_tools("s14_pristine", self.pristine, self.rewritten)]
        self.assertEqual(names, sa.OFFICIAL_ORDER)
        self.assertEqual(len(names), 14)

    def test_s12_rewritten_uses_rewritten_definitions(self):
        tools = sa.scenario_tools("s12_rewritten", self.pristine, self.rewritten)
        self.assertEqual([t["function"]["name"] for t in tools], sa.CORE_NAMES)
        self.assertTrue(all(t["function"]["description"].startswith("[rewritten]") for t in tools))

    def test_tools_are_chat_shaped(self):
        tools = sa.scenario_tools("s12_pristine", self.pristine, self.rewritten)
        for entry in tools:
            self.assertEqual(entry["type"], "function")
            fn = entry["function"]
            self.assertEqual(sorted(fn), ["description", "name", "parameters"])
            self.assertIsInstance(fn["parameters"], dict)

    def test_missing_definition_falls_back_to_stub(self):
        # 资产里没有 experimental_image_generate：用网关同款兜底形状
        tools = sa.scenario_tools("s13_pristine", self.pristine, self.rewritten)
        entry = [t for t in tools if t["function"]["name"] == "experimental_image_generate"][0]
        self.assertEqual(entry["function"]["description"], "Agent tool experimental_image_generate")
        self.assertEqual(entry["function"]["parameters"], {"type": "object", "properties": {}})

    def test_pristine_and_rewritten_definitions_differ(self):
        pristine = sa.scenario_tools("s12_pristine", self.pristine, self.rewritten)
        rewritten = sa.scenario_tools("s12_rewritten", self.pristine, self.rewritten)
        self.assertEqual([t["function"]["name"] for t in pristine],
                         [t["function"]["name"] for t in rewritten])
        self.assertNotEqual(pristine[0]["function"], rewritten[0]["function"])

    def test_unknown_scenario_raises(self):
        with self.assertRaises(KeyError):
            sa.scenario_tools("s99", self.pristine, self.rewritten)


class ExpectedBranchTest(unittest.TestCase):
    def setUp(self):
        self.pristine = sa.load_definitions(pristine_fixture())
        self.rewritten = sa.load_definitions(rewritten_fixture())
        self.official = sa.gateway_official_defs(self.rewritten)

    def test_subset_accepts_any_official_subset(self):
        for scenario in sa.VALID_SCENARIOS:
            tools = sa.scenario_tools(scenario, self.pristine, self.rewritten)
            self.assertEqual(sa.expected_branch("subset", tools, self.official), "passthrough", scenario)

    def test_exact_needs_all_fourteen(self):
        twelve = sa.scenario_tools("s12_pristine", self.pristine, self.rewritten)
        fourteen = sa.scenario_tools("s14_pristine", self.pristine, self.rewritten)
        self.assertEqual(sa.expected_branch("exact", twelve, self.official), "rename_complete")
        self.assertEqual(sa.expected_branch("exact", fourteen, self.official), "passthrough")

    def test_content_passes_only_the_rewritten_definition(self):
        # 关键事实：content 档比的是「网关会发出的官方定义」，也就是仓库改写版；
        # 原始 13/14 个定义描述与参数都不同，所以预期走改名。
        for scenario in ("s12_pristine", "s13_pristine", "s14_pristine"):
            tools = sa.scenario_tools(scenario, self.pristine, self.rewritten)
            self.assertEqual(sa.expected_branch("content", tools, self.official), "rename_complete", scenario)
        tools = sa.scenario_tools("s12_rewritten", self.pristine, self.rewritten)
        self.assertEqual(sa.expected_branch("content", tools, self.official), "passthrough")

    def test_content_rejects_single_changed_parameter_description(self):
        tools = sa.scenario_tools("s12_rewritten", self.pristine, self.rewritten)
        params = tools[5]["function"]["parameters"]
        params["properties"]["b"]["description"] = "File to read, resolved by the filesystem backend"
        self.assertEqual(sa.expected_branch("content", tools, self.official), "rename_complete")

    def test_foreign_name_never_passes(self):
        tools = sa.scenario_tools("s12_rewritten", self.pristine, self.rewritten)
        tools = tools + [sa.chat_tool("mcp__github_create_issue", {"description": "x",
                                                                   "parameters": {"type": "object"}})]
        for mode in ("subset", "content"):
            self.assertEqual(sa.expected_branch(mode, tools, self.official), "rename_complete", mode)

    def test_empty_tools_is_none(self):
        self.assertEqual(sa.expected_branch("subset", [], self.official), "none")

    def test_name_only_criterion_accepts_client_text_but_content_rejects_it(self):
        """核心假设：只看名字会放行「官方名 + 客户端自有参数文本」，配文本校验才会拦下。

        这正是 s12_dsh 场景想验的那件事（2026-11-17 实测：官方名 + DSH 的参数级描述
        → 429）。上游接不接受是另一回事，判据本身必须能把它区分出来。
        """
        if not hasattr(sa, "apply_dsh_text"):
            self.skipTest("当前版本没有 apply_dsh_text（场景被移除）")
        base = {
            "description": "Read a file",
            "parameters": {"type": "object", "properties": {
                "path": {"type": "string", "description": "官方原文"}}},
        }
        official = {"read": json.loads(json.dumps(base))}
        tools = [sa.chat_tool("read", json.loads(json.dumps(base)))]
        tools = sa.apply_dsh_text(tools)
        self.assertEqual(sa.expected_branch("subset", tools, official), "passthrough")
        self.assertEqual(sa.expected_branch("content", tools, official), "rename_complete")

    def test_registered_scenarios_all_build(self):
        # VALID_SCENARIOS 是唯一事实来源：往里加场景就必须能构造出 tools
        for scenario in sa.VALID_SCENARIOS:
            tools = sa.scenario_tools(scenario, self.pristine, self.rewritten)
            self.assertTrue(tools, scenario)
            for entry in tools:
                self.assertTrue(entry["function"]["name"], scenario)

    def test_windows_shell_description_rewrite(self):
        defs = {"shell": {"description": "Run a command on Linux using bash", "parameters": {"type": "object"}}}
        official = sa.gateway_official_defs(defs)
        if platform.system() == "Windows":
            self.assertEqual(official["shell"]["description"], "Run a command on Windows using powershell")
        else:
            self.assertEqual(official["shell"]["description"], "Run a command on Linux using bash")


class QuotaAndVerdictTest(unittest.TestCase):
    def test_status_based(self):
        self.assertTrue(sa.quota_suspected(429, ""))
        self.assertTrue(sa.quota_suspected(402, ""))
        self.assertTrue(sa.quota_suspected(429, '{"error":{"type":"FreeUsageLimitError"}}'))

    def test_body_fingerprint_on_200(self):
        self.assertTrue(sa.quota_suspected(
            200, '{"error":{"type":"FreeUsageLimitError","message":"free usage limit reached"}}'))
        self.assertTrue(sa.quota_suspected(200, '{"message":"Usage Limit exceeded"}'))
        self.assertTrue(sa.quota_suspected(200, '{"error":"quota"}'))
        self.assertTrue(sa.quota_suspected(200, '{"error":"rate limit"}'))

    def test_plain_200_is_not_quota(self):
        self.assertFalse(sa.quota_suspected(200, '{"choices":[{"message":{"content":"ok"}}]}'))

    def test_upstream_400_invalid_tool_is_not_quota(self):
        body = '{"error":{"message":"invalid tool definition","type":"invalid_request_error"}}'
        self.assertFalse(sa.quota_suspected(400, body))
        self.assertEqual(sa.upstream_error(400, body), "invalid tool definition")

    def test_upstream_error_on_200_is_none(self):
        self.assertIsNone(sa.upstream_error(200, '{"choices":[]}'))

    def test_upstream_error_non_json(self):
        self.assertEqual(sa.upstream_error(502, "upstream unreachable"), "upstream unreachable")
        self.assertEqual(sa.upstream_error(502, ""), "http 502")

    def test_verdict_mapping(self):
        hit = {"http": 200, "branch_ok": True, "quota_suspected": False}
        self.assertEqual(sa.verdict(hit), "hit")
        self.assertEqual(sa.verdict({"http": 200, "branch_ok": False}), "error")
        self.assertEqual(sa.verdict({"http": 200, "branch_ok": True, "quota_suspected": True}), "quota")
        self.assertEqual(sa.verdict({"http": 429, "quota_suspected": True}), "quota")
        self.assertEqual(sa.verdict({"http": 502, "branch_ok": True}), "error")
        self.assertEqual(sa.verdict({"skipped": "quota"}), "quota")
        self.assertEqual(sa.verdict({"skipped": "error"}), "error")

    def test_exit_codes(self):
        ok = [{"http": 200, "branch_ok": True}]
        self.assertEqual(sa.exit_code(ok), 0)
        self.assertEqual(sa.exit_code(ok + [{"http": 429, "quota_suspected": True}]), 1)
        self.assertEqual(sa.exit_code(ok + [{"http": 502, "branch_ok": True}]), 1)
        self.assertEqual(sa.exit_code(ok, env_error=True), 2)
        self.assertEqual(sa.exit_code([]), 1)


class TableAndJudgementTest(unittest.TestCase):
    def records(self, subset_http=200, content_pristine="rename_complete", content_http=200):
        return [
            {"mode": "subset", "scenario": "s12_pristine", "gateway_branch": "passthrough",
             "expected_branch": "passthrough", "http": subset_http, "branch_ok": subset_http == 200,
             "quota_suspected": subset_http in (402, 429)},
            {"mode": "content", "scenario": "s14_pristine", "gateway_branch": content_pristine,
             "expected_branch": content_pristine, "http": content_http, "branch_ok": True,
             "quota_suspected": False},
            {"mode": "content", "scenario": "s12_rewritten", "gateway_branch": "passthrough",
             "expected_branch": "passthrough", "http": 200, "branch_ok": True, "quota_suspected": False},
        ]

    def test_table_header_and_rows(self):
        table = sa.format_table(self.records())
        self.assertIn("mode", table[0])
        self.assertIn("scenario", table[0])
        self.assertIn("verdict", table[0])
        self.assertEqual(len(table), 2 + 3)
        self.assertTrue(table[2].startswith("subset"))
        self.assertIn("passthrough", table[2])
        self.assertIn("hit", table[2])
        self.assertIn("content", table[3])

    def test_table_marks_quota_and_skip(self):
        records = self.records(subset_http=429)
        records[1]["skipped"] = "quota"
        records[1]["http"] = None
        records[1]["gateway_branch"] = None
        table = sa.format_table(records)
        self.assertIn("quota", table[2])
        self.assertIn("-", table[3])

    def test_judgement_subset_usable(self):
        lines = sa.judgement(self.records())
        self.assertIn("subset 可用", lines)

    def test_judgement_subset_rejected(self):
        lines = sa.judgement(self.records(subset_http=429))
        self.assertTrue(any("subset 不可用" in line for line in lines), lines)

    def test_judgement_reports_content_passthrough_and_not(self):
        lines = sa.judgement(self.records())
        self.assertTrue(any(line.startswith("content/s14_pristine 未透传") for line in lines), lines)
        self.assertTrue(any("content/s12_rewritten 透传且上游 200" in line for line in lines), lines)

    def test_judgement_without_samples(self):
        self.assertEqual(sa.judgement([]), ["没有可判定的样本"])


class LoadDefinitionsTest(unittest.TestCase):
    def _write(self, payload):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        json.dump(payload, handle, ensure_ascii=False)
        handle.close()
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def test_asset_shape_order_plus_tools(self):
        path = self._write(pristine_fixture())
        defs = sa.load_definitions_file(path)
        self.assertEqual(sorted(defs), sorted(pristine_fixture()["tools"]))
        self.assertEqual(defs["read"]["description"], "[pristine] read")
        self.assertIn("properties", defs["read"]["parameters"])

    def test_repo_dict_shape(self):
        path = self._write({"read": {"description": "d", "parameters": {"type": "object"}}})
        defs = sa.load_definitions_file(path)
        self.assertEqual(list(defs), ["read"])
        self.assertEqual(defs["read"]["parameters"], {"type": "object"})

    def test_capture_list_shape(self):
        path = self._write([
            {"type": "function", "function": {"name": "glob", "description": "g",
                                              "parameters": {"type": "object"}}},
            {"name": "grep", "description": "h"},
        ])
        defs = sa.load_definitions_file(path)
        self.assertEqual(sorted(defs), ["glob", "grep"])
        self.assertEqual(defs["grep"]["parameters"], {"type": "object", "properties": {}})

    def test_metadata_keys_are_ignored(self):
        path = self._write({
            "cli_version": "2.2.5",
            "captured_at": "2026-11-17T07:39:47Z",
            "source": {"workflow": "bootstrap"},
            "order": ["read"],
            "tools": {"read": {"description": "d", "parameters": {"type": "object"}}},
        })
        self.assertEqual(list(sa.load_definitions_file(path)), ["read"])

    def test_bad_shape_raises(self):
        path = self._write("nope")
        with self.assertRaises(ValueError):
            sa.load_definitions_file(path)

    def test_definition_for_fallback(self):
        stub = sa.definition_for("nope", {})
        self.assertEqual(stub["description"], "Agent tool nope")
        self.assertEqual(stub["parameters"], {"type": "object", "properties": {}})

    def test_resolve_pristine_local_wins(self):
        path = self._write(pristine_fixture())
        args = type("A", (), {"pristine": path, "tools_file": path})()
        defs, source = sa.resolve_pristine(args, tempfile.gettempdir())
        self.assertEqual(source["origin"], "local")
        self.assertFalse(source["fallback"])
        self.assertEqual(len(defs), 13)

    def test_resolve_pristine_missing_file_is_env_error(self):
        args = type("A", (), {"pristine": os.path.join(tempfile.gettempdir(), "definitely-missing.json"),
                              "tools_file": os.path.join(tempfile.gettempdir(), "definitely-missing.json")})()
        with self.assertRaises(sa.EnvError):
            sa.resolve_pristine(args, tempfile.gettempdir())

    def test_resolve_pristine_download_failure_degrades(self):
        # 把 URL 指向不可达地址，验证降级到仓库改写版并标记 fallback
        path = self._write({"read": {"description": "d", "parameters": {"type": "object"}}})
        workdir = tempfile.mkdtemp(prefix="shape-test-")
        self.addCleanup(lambda: __import__("shutil").rmtree(workdir, ignore_errors=True))
        args = type("A", (), {"pristine": "", "tools_file": path})()
        old_url = sa.ASSET_URL
        sa.ASSET_URL = "http://127.0.0.1:9/nope.json"
        buffer = io.StringIO()
        try:
            with contextlib.redirect_stdout(buffer):
                defs, source = sa.resolve_pristine(args, workdir)
        finally:
            sa.ASSET_URL = old_url
        self.assertTrue(source["fallback"])
        self.assertEqual(source["origin"], "fallback")
        self.assertIn("read", defs)
        # 降级必须留下 GitHub annotation，否则 CI 里没人知道这轮 pretine 是假的
        self.assertIn("::warning::", buffer.getvalue())


class BodyTest(unittest.TestCase):
    def test_body_head_truncates_and_flattens(self):
        head = sa.body_head("a" * 150 + "\n" + "b" * 150)
        self.assertEqual(len(head), 200)
        self.assertNotIn("\n", head)

    def test_body_head_short(self):
        self.assertEqual(sa.body_head("{}"), "{}")


if __name__ == "__main__":
    unittest.main(verbosity=2)
