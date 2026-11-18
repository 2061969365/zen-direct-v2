package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 这些测试锁定「响应出口 function.name 必须是非空字符串」这条硬约束。
//
// 背景：客户端 SDK（OpenAI SDK、ZCode 等）解析 tool_calls 时做强类型校验，
// 拿到 null / 缺失 / 纯空白直接抛
// `Expected 'function.name' to be a string.`，整轮对话失败。
//
// 为什么必须在网关兜底：空名的来源是上游行为（断流、模型幻觉出未声明的
// 工具名、映射表查不到），网关无法从源头杜绝，只能在出口保证可解析。

func TestSanitizeResponseToolName(t *testing.T) {
	cases := []struct {
		name     string
		function map[string]any
		want     string
		wantFix  bool
	}{
		{"正常名字不动", map[string]any{"name": "read"}, "read", false},
		{"空串兜底", map[string]any{"name": ""}, responseToolNameFallback, true},
		{"纯空白兜底", map[string]any{"name": "   "}, responseToolNameFallback, true},
		{"制表符兜底", map[string]any{"name": "\t\n"}, responseToolNameFallback, true},
		{"null 兜底", map[string]any{"name": nil}, responseToolNameFallback, true},
		{"缺字段兜底", map[string]any{}, responseToolNameFallback, true},
		{"数字兜底", map[string]any{"name": 42}, responseToolNameFallback, true},
		{"对象兜底", map[string]any{"name": map[string]any{"x": 1}}, responseToolNameFallback, true},
		{"布尔兜底", map[string]any{"name": true}, responseToolNameFallback, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixed := sanitizeResponseToolName(c.function, "")
			if fixed != c.wantFix {
				t.Errorf("fixed = %v, want %v", fixed, c.wantFix)
			}
			got, ok := c.function["name"].(string)
			if !ok {
				t.Fatalf("兜底后 name 不是字符串: %#v", c.function["name"])
			}
			if got != c.want {
				t.Errorf("name = %q, want %q", got, c.want)
			}
			if strings.TrimSpace(got) == "" {
				t.Error("name 不能是空白")
			}
		})
	}
}

func TestSanitizeResponseBodyToolsChat(t *testing.T) {
	payload := map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{
					map[string]any{"id": "c1", "type": "function",
						"function": map[string]any{"name": "", "arguments": "{}"}},
					map[string]any{"id": "c2", "type": "function",
						"function": map[string]any{"arguments": "{}"}},
					map[string]any{"id": "c3", "type": "function",
						"function": map[string]any{"name": "read", "arguments": "{}"}},
				},
			},
		}},
	}
	if fixed := sanitizeResponseBodyTools(payload); fixed != 2 {
		t.Fatalf("fixed = %d, want 2", fixed)
	}
	msg := asMap(asArr(payload["choices"])[0].(map[string]any)["message"])
	calls := asArr(msg["tool_calls"])
	for i, raw := range calls {
		fn := asMap(asMap(raw)["function"])
		name, ok := fn["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			t.Errorf("第 %d 个 tool_call 的 name 仍非法: %#v", i, fn["name"])
		}
	}
	if got := asStr(asMap(asMap(calls[2])["function"])["name"]); got != "read" {
		t.Errorf("正常的 read 被改成了 %q", got)
	}
}

func TestSanitizeResponseBodyToolsResponses(t *testing.T) {
	payload := map[string]any{
		"output": []any{
			map[string]any{"type": "function_call", "name": "", "arguments": "{}"},
			map[string]any{"type": "function_call", "arguments": "{}"},
			map[string]any{"type": "function_call", "name": "read", "arguments": "{}"},
			map[string]any{"type": "message", "role": "assistant"},
		},
	}
	if fixed := sanitizeResponseBodyTools(payload); fixed != 2 {
		t.Fatalf("fixed = %d, want 2", fixed)
	}
	for i, raw := range asArr(payload["output"]) {
		item := asMap(raw)
		if asStr(item["type"]) != "function_call" {
			if _, exists := item["name"]; exists {
				t.Errorf("output[%d] 非 function_call 不该被塞 name", i)
			}
			continue
		}
		name, ok := item["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			t.Errorf("output[%d].name 仍非法: %#v", i, item["name"])
		}
	}
}

func TestSanitizeStreamFrame(t *testing.T) {
	t.Run("空名兜底", func(t *testing.T) {
		frame := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{}"}}]}}]}`)
		out := sanitizeStreamFrame(frame)
		if strings.Contains(string(out), `"name":""`) {
			t.Fatalf("空名字段没被兜底: %s", out)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("兜底后不是合法 JSON: %v", err)
		}
		delta := asMap(asArr(payload["choices"])[0].(map[string]any)["delta"])
		call := asMap(asArr(delta["tool_calls"])[0])
		if got := asStr(asMap(call["function"])["name"]); got != responseToolNameFallback {
			t.Fatalf("name = %q, want %q", got, responseToolNameFallback)
		}
	})

	t.Run("null兜底", func(t *testing.T) {
		frame := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null}}]}}]}`)
		if out := sanitizeStreamFrame(frame); strings.Contains(string(out), `"name":null`) {
			t.Fatalf("null 没被兜底: %s", out)
		}
	})

	t.Run("纯空白兜底", func(t *testing.T) {
		frame := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"  "}}]}}]}`)
		out := sanitizeStreamFrame(frame)
		if strings.Contains(string(out), `"name":"  "`) {
			t.Fatalf("纯空白没被兜底: %s", out)
		}
	})

	t.Run("name缺省的中间帧不动", func(t *testing.T) {
		// 分片流中间帧只带 arguments，客户端按 index 累积，不能补名字。
		frame := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`)
		if out := sanitizeStreamFrame(frame); string(out) != string(frame) {
			t.Fatalf("缺省 name 的中间帧不该改动:\n got %s\nwant %s", out, frame)
		}
	})

	t.Run("正常帧逐字节不变", func(t *testing.T) {
		frame := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read"}}]}}]}`)
		if out := sanitizeStreamFrame(frame); string(out) != string(frame) {
			t.Fatalf("正常帧被改动:\n got %s\nwant %s", out, frame)
		}
	})

	t.Run("无function字段直接返回", func(t *testing.T) {
		frame := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		if out := sanitizeStreamFrame(frame); string(out) != string(frame) {
			t.Fatal("无关帧被改动")
		}
	})
}

// 端到端：整形管道的流式出口也必须兜底。
func TestStreamPipelineSanitizesEmptyName(t *testing.T) {
	renamer := newStreamToolRenamer(FmtChat, NewToolRenames())
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":null}}]}}]}`,
	}
	for i, f := range frames {
		out := renamer.rewriteFrame(f)
		s := string(out)
		if strings.Contains(s, `"name":""`) || strings.Contains(s, `"name":null`) {
			t.Errorf("第 %d 帧仍有非法 name: %s", i, out)
		}
	}
}
