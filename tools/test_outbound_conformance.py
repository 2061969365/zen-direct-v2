"""test_outbound_conformance.py — 出站对拍器的纯函数单测（纯标准库，不起 Go 二进制）。

跑法：
    python -m unittest tools.test_outbound_conformance -v

覆盖：
    - params_deep_equal 的首个差异路径定位（嵌套 / 数组 / 缺键 / 类型不符）
    - desc_bytes_equal 的 UTF-8 字节语义（中文、多字节、同字节数不同内容）
    - UA 正则、order_equal、names_equal、passthrough、strict
    - 报告汇总与退出码映射（0 / 1 / 2）
    - 场景构造：s12 = 12、s14 = 14、s1 = 1、s0 = 0
"""

import json
import os
import sys
import tempfile
import unittest

try:
    from tools import outbound_conformance as oc
except ImportError:  # 直接 `python tools/test_outbound_conformance.py`
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    import outbound_conformance as oc


def tool(name, description="", parameters=None, strict=None):
    entry = {
        "type": "function",
        "function": {
            "name": name,
            "description": description,
            "parameters": parameters if parameters is not None else {"type": "object", "properties": {}},
        },
    }
    if strict is not None:
        entry["strict"] = strict
    return entry


class ParamsDeepEqualTest(unittest.TestCase):
    def test_nested_type_diff_points_at_leaf(self):
        client = {"type": "object", "properties": {"path": {"type": "string"}}}
        outbound = {"type": "object", "properties": {"path": {"type": "integer"}}}
        path, detail = oc.first_param_diff("read", client, outbound)
        self.assertEqual(path, "read.parameters.properties.path.type")
        self.assertIn('client "string"', detail)
        self.assertIn('outbound "integer"', detail)

    def test_array_element_diff(self):
        client = {"type": "string", "enum": ["a", "b"]}
        outbound = {"type": "string", "enum": ["a", "c"]}
        path, detail = oc.first_param_diff("webfetch", client, outbound)
        self.assertEqual(path, "webfetch.parameters.enum[1]")
        self.assertIn('"b"', detail)

    def test_array_length_diff(self):
        path, _ = oc.first_param_diff("glob", {"required": ["a"]}, {"required": ["a", "b"]})
        self.assertEqual(path, "glob.parameters.required.length")

    def test_missing_key_side(self):
        path, detail = oc.first_param_diff("edit", {"type": "object"}, {"type": "object", "additionalProperties": False})
        self.assertEqual(path, "edit.parameters.additionalProperties")
        self.assertIn("多出", detail)

    def test_kind_mismatch(self):
        path, detail = oc.first_param_diff("write", {"type": "object"}, [])
        self.assertEqual(path, "write.parameters")
        self.assertIn("object", detail)

    def test_equal_returns_none(self):
        payload = {"type": "object", "properties": {"n": {"type": "integer"}}, "required": ["n"]}
        self.assertEqual(oc.first_param_diff("shell", payload, json.loads(json.dumps(payload))), (None, None))

    def test_int_float_same_number(self):
        self.assertEqual(oc.first_param_diff("x", {"min": 1}, {"min": 1.0}), (None, None))

    def test_params_deep_equal_reports_first_tool(self):
        left = [tool("read", parameters={"type": "object"}), tool("glob", parameters={"type": "object"})]
        right = [tool("read", parameters={"type": "object"}), tool("glob", parameters={"type": "array"})]
        ok, detail = oc.params_deep_equal(oc.normalize_tools(left), oc.normalize_tools(right))
        self.assertFalse(ok)
        self.assertIn("tool[1]", detail)
        self.assertIn("glob.parameters", detail)

    def test_params_deep_equal_count_mismatch(self):
        ok, detail = oc.params_deep_equal(oc.normalize_tools([tool("read")]), [])
        self.assertFalse(ok)
        self.assertIn("条数不同", detail)

    def test_prefix_compared_before_count_mismatch(self):
        # 条数不同的场景里，仍然要先把公共前缀比完：改名/补全时这条才是有效信息
        left = oc.normalize_tools([tool("read", "读取文件", {"type": "object", "properties": {"a": {"type": "string"}}})])
        right = oc.normalize_tools(
            [
                tool("mcp_0123456789ab", "读取", {"type": "object", "properties": {"a": {"type": "integer"}}}),
                tool("edit", "官方桩"),
            ]
        )
        ok, detail = oc.params_deep_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("read.parameters.properties.a.type", detail)
        ok, detail = oc.desc_bytes_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("client 12B", detail)
        self.assertIn("outbound 6B", detail)


class DescBytesTest(unittest.TestCase):
    def test_chinese_byte_length_not_char_length(self):
        # "中文" = 2 字符 / 6 字节；"abcdef" = 6 字符 / 6 字节 -> 字节数相同，内容不同
        left = oc.normalize_tools([tool("read", "中文")])
        right = oc.normalize_tools([tool("read", "abcdef")])
        ok, detail = oc.desc_bytes_equal(left, right)
        self.assertTrue(ok, detail)
        ok, detail = oc.desc_text_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("内容不同", detail)

    def test_truncated_chinese_description(self):
        left = oc.normalize_tools([tool("read", "读取文件")])
        right = oc.normalize_tools([tool("read", "读取")])
        ok, detail = oc.desc_bytes_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("client 12B", detail)
        self.assertIn("outbound 6B", detail)
        self.assertIn("'read'", detail)

    def test_identical_multibyte(self):
        text = "编辑文件内容（逐字替换，保留缩进）"
        left = oc.normalize_tools([tool("edit", text)])
        right = oc.normalize_tools([tool("edit", text)])
        self.assertTrue(oc.desc_bytes_equal(left, right)[0])
        self.assertTrue(oc.desc_text_equal(left, right)[0])

    def test_prefix_ok_but_counts_differ(self):
        left = oc.normalize_tools([tool("read", "读取")])
        right = oc.normalize_tools([tool("mcp_0123456789ab", "读取"), tool("edit", "官方桩")])
        ok, detail = oc.desc_bytes_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("条数不同", detail)
        self.assertIn("前 1 个", detail)


class UserAgentTest(unittest.TestCase):
    def test_official_shapes(self):
        for ua in ("opencode/latest/2.3.0/cli", "opencode/latest/2.2.5/cli", "opencode/latest/2.3.0-beta.1/cli"):
            ok, _ = oc.ua_official(ua)
            self.assertTrue(ok, ua)

    def test_rejects_non_official(self):
        for ua in (
            "",
            None,
            "opencode/latest/2.3.0",
            "opencode/latest/2.3.0/cli/extra",
            "opencode/latest//cli",
            "OpenCode/latest/2.3.0/cli",
            "opencode/2.3.0/cli",
            "opencode/latest/2.3.0/cli ",
            "python-requests/2.31",
        ):
            ok, _ = oc.ua_official(ua)
            self.assertFalse(ok, ua)


class OrderAndNamesTest(unittest.TestCase):
    def test_order_equal_true(self):
        tools = oc.normalize_tools([tool("read"), tool("edit")])
        self.assertTrue(oc.order_equal(tools, list(tools))[0])

    def test_order_equal_detects_swap(self):
        left = oc.normalize_tools([tool("read"), tool("edit")])
        right = oc.normalize_tools([tool("edit"), tool("read")])
        ok, detail = oc.order_equal(left, right)
        self.assertFalse(ok)
        self.assertIn("[0]", detail)

    def test_order_equal_detects_length(self):
        ok, detail = oc.order_equal(oc.normalize_tools([tool("read")]), [])
        self.assertFalse(ok)
        self.assertIn("长度不同", detail)

    def test_names_equal_ignores_order_but_not_membership(self):
        left = oc.normalize_tools([tool("read"), tool("edit")])
        right = oc.normalize_tools([tool("edit"), tool("read")])
        self.assertTrue(oc.names_equal(left, right)[0])
        renamed = oc.normalize_tools([tool("mcp_0123456789ab"), tool("edit")])
        ok, detail = oc.names_equal(left, renamed)
        self.assertFalse(ok)
        self.assertIn("mcp_0123456789ab", detail)


class PassthroughAndStrictTest(unittest.TestCase):
    def test_passthrough_counts(self):
        left = oc.normalize_tools([tool("read")] * 12)
        right = oc.normalize_tools([tool("read")] * 26)
        ok, detail = oc.passthrough(left, right)
        self.assertFalse(ok)
        self.assertIn("12", detail)
        self.assertIn("26", detail)
        self.assertIn("+14", detail)
        self.assertTrue(oc.passthrough(left, left)[0])

    def test_strict_absent_or_false(self):
        clean = oc.normalize_tools([tool("read"), tool("edit", strict=False)])
        self.assertTrue(oc.strict_absent_or_false(clean)[0])
        dirty = oc.normalize_tools([tool("read", strict=True)])
        ok, detail = oc.strict_absent_or_false(dirty)
        self.assertFalse(ok)
        self.assertIn("strict=True", detail)


class ScenarioBuildTest(unittest.TestCase):
    def synthetic_defs(self):
        names = [
            "edit", "execute", "glob", "grep", "question", "read", "shell",
            "skill", "subagent", "webfetch", "websearch", "write", "patch",
        ]
        return {name: {"description": "d-" + name, "parameters": {"type": "object", "properties": {}}} for name in names}

    def test_counts(self):
        scenarios = dict(oc.build_scenarios(self.synthetic_defs()))
        self.assertEqual(len(scenarios["s12"]), 12)
        self.assertEqual(len(scenarios["s14"]), 14)
        self.assertEqual(len(scenarios["s1"]), 1)
        self.assertEqual(len(scenarios["s0"]), 0)

    def test_s14_member_names(self):
        names = [entry["function"]["name"] for entry in dict(oc.build_scenarios(self.synthetic_defs()))["s14"]]
        self.assertEqual(sorted(names[:12]), sorted(oc.core_twelve(self.synthetic_defs())))
        self.assertIn("patch", names)
        self.assertIn("experimental_image_generate", names)
        self.assertEqual(names[0:12], oc.core_twelve(self.synthetic_defs()))

    def test_missing_definition_gets_synthetic_schema(self):
        scenarios = dict(oc.build_scenarios(self.synthetic_defs()))
        entry = [e for e in scenarios["s14"] if e["function"]["name"] == "experimental_image_generate"][0]
        self.assertEqual(entry["function"]["parameters"], {"type": "object", "properties": {}})


class LoadDefinitionsTest(unittest.TestCase):
    def _write(self, payload):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        json.dump(payload, handle, ensure_ascii=False)
        handle.close()
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def test_dict_shape(self):
        path = self._write({"read": {"description": "读取", "parameters": {"type": "object"}}})
        defs = oc.load_tool_definitions(path)
        self.assertEqual(list(defs), ["read"])
        self.assertEqual(defs["read"]["description"], "读取")

    def test_list_shape_with_function_wrapper(self):
        path = self._write([{"type": "function", "function": {"name": "glob", "description": "g", "parameters": {"type": "object"}}}])
        defs = oc.load_tool_definitions(path)
        self.assertEqual(list(defs), ["glob"])
        self.assertEqual(defs["glob"]["parameters"], {"type": "object"})


class EvaluateAndExitTest(unittest.TestCase):
    def record(self, tools, ua="opencode/latest/2.3.0/cli"):
        return {"body": {"tools": tools}, "headers_lower": {"user-agent": ua}}

    def test_passthrough_scenario_all_green(self):
        client = [tool("read", "读取文件"), tool("edit", "编辑文件")]
        outbound = json.loads(json.dumps(client))
        assertions, _, outbound_norm = oc.evaluate(client, self.record(outbound))
        self.assertEqual(len(outbound_norm), 2)
        self.assertTrue(all(item["ok"] for item in assertions.values()), assertions)

    def test_renamed_and_completed_scenario_fails(self):
        client = [tool("read", "读取文件")]
        outbound = [tool("mcp_0123456789ab", "读取文件"), tool("edit", "官方桩")]
        assertions, _, outbound_norm = oc.evaluate(client, self.record(outbound))
        self.assertFalse(assertions["passthrough"]["ok"])
        self.assertFalse(assertions["names_equal"]["ok"])
        self.assertFalse(assertions["order_equal"]["ok"])
        self.assertTrue(assertions["strict_absent_or_false"]["ok"])
        self.assertEqual(len(outbound_norm), 2)
        passed, failed = oc.summarize(assertions)
        self.assertGreater(failed, 0)
        self.assertEqual(passed + failed, len(oc.ASSERTION_ORDER))

    def test_missing_ua_fails_only_that_assertion(self):
        client = [tool("read", "d")]
        assertions, _, _ = oc.evaluate(client, self.record(client, ua=""))
        self.assertFalse(assertions["ua_official"]["ok"])
        self.assertTrue(assertions["passthrough"]["ok"])

    def test_exit_code_mapping(self):
        self.assertEqual(oc.exit_code(8, 0), 0)
        self.assertEqual(oc.exit_code(7, 1), 1)
        self.assertEqual(oc.exit_code(0, 8), 1)
        self.assertEqual(oc.exit_code(8, 0, env_error=True), 2)
        self.assertEqual(oc.exit_code(0, 0), 1)  # 一个断言都没跑不能算通过


if __name__ == "__main__":
    unittest.main(verbosity=2)
