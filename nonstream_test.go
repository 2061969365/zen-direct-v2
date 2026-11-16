package main

// 非流强制上游 stream:true + SSE 聚合回写单测。
import (
	"encoding/json"
	"strings"
	"testing"
)

// 客户端 stream:false：上游体必须 stream:true，下游回纯 JSON（非 SSE）。
func TestForcedUpstreamStreamTrue(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("http=%d body=%.300s", code, body)
	}
	_, upBody, _ := getLastUp()
	if !strings.Contains(upBody, `"stream":true`) {
		t.Fatalf("上游体应强制 stream:true: %.300s", upBody)
	}
	if strings.HasPrefix(strings.TrimSpace(body), "data:") {
		t.Fatalf("非流下游不应是 SSE: %.200s", body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("下游应为合法 JSON: %v %.200s", err, body)
	}
	if m["id"] != "r1" {
		t.Fatalf("聚合 response 丢失: %v", m)
	}
}

// 缺省 stream 字段同样强制上游流式。
func TestMissingStreamForcesTrue(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\",\"output\":[]}}\n\n")
	code, _ := postResponses(t, "http://x", `{"model":"m","input":"hi"}`)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	_, upBody, _ := getLastUp()
	if !strings.Contains(upBody, `"stream":true`) {
		t.Fatalf("缺省 stream 应被强制 true: %.300s", upBody)
	}
}

// 半截 SSE（无终态）→ 502，不吐残缺 JSON。
func TestNonStreamTruncatedSSE502(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 502 {
		t.Fatalf("半截流应502: %d %.300s", code, body)
	}
}

// chat 入口非流：聚合 chunk 为 chat.completion。
func TestNonStreamChatAssembles(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	withMockUpstream(t, sse)
	code, body := postChat(t, "http://x", `{"model":"mimo-v2.5-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("http=%d body=%.300s", code, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("应为 JSON: %v %.200s", err, body)
	}
	if m["object"] != "chat.completion" {
		t.Fatalf("object=%v", m["object"])
	}
	choices, _ := m["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", m["choices"])
	}
	msg := asMap(asMap(choices[0])["message"])
	if asStr(msg["content"]) != "hello" {
		t.Fatalf("content=%v", msg["content"])
	}
	if asStr(asMap(choices[0])["finish_reason"]) != "stop" {
		t.Fatalf("finish=%v", asMap(choices[0])["finish_reason"])
	}
}

// chat 非流半截（无 finish_reason）→ 502。
func TestNonStreamChatTruncated502(t *testing.T) {
	withMockUpstream(t, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
	code, _ := postChat(t, "http://x", `{"model":"mimo-v2.5-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 502 {
		t.Fatalf("无 finish 应502: %d", code)
	}
}

// 非 SSE 纯 JSON 上游体（单测 mock/兼容）仍走原 JSON 解析。
func TestNonStreamPlainJSONFallback(t *testing.T) {
	withMockUpstream(t, `{"id":"rj","status":"completed","output":[]}`)
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("http=%d body=%.300s", code, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("应为 JSON: %v", err)
	}
	if m["id"] != "rj" {
		t.Fatalf("id=%v", m["id"])
	}
}

// 非流 completed 回显 tools 仍剥离（聚合路径复用既有 strip）。
func TestNonStreamSSEStripsTools(t *testing.T) {
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r3\",\"status\":\"completed\",\"tools\":[{\"type\":\"function\",\"name\":\"bash\",\"description\":\"internal capability marker, never call\"}],\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\ndata: [DONE]\n\n"
	withMockUpstream(t, sse)
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	if strings.Contains(body, "never call") {
		t.Fatalf("聚合体工具回显未剥离: %.300s", body)
	}
	if !strings.Contains(body, "ok") {
		t.Fatalf("正文丢失: %.300s", body)
	}
}

// 非流聚合：上游不发 index、或多个工具共用同一个 index 时，两个工具不能合并。
//
// 2026-11-16 实测回归：聚合器直接用 index（缺省取数组下标，恒为 0）作槽位，
// 两个工具合并成一个，name 被后者覆盖，参数拼成 {"x":1}{"y":2}（非法 JSON）。
func TestNonStreamToolIndexCollision(t *testing.T) {
	cases := []struct {
		name string
		up   string
	}{
		{
			name: "完全不发 index",
			up: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c_a\",\"function\":{\"name\":\"a\",\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c_b\",\"function\":{\"name\":\"b\",\"arguments\":\"{\\\"y\\\":2}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
		},
		{
			name: "两个工具共用 index 0",
			up: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c_a\",\"function\":{\"name\":\"a\",\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c_b\",\"function\":{\"name\":\"b\",\"arguments\":\"{\\\"y\\\":2}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
		},
	}
	for _, c := range cases {
		out, err := sseToChatCompletion([]byte(c.up), "mimo-v2.5-free")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		msg := asMap(asArr(out["choices"])[0].(map[string]any)["message"])
		tcs := asArr(msg["tool_calls"])
		if len(tcs) != 2 {
			t.Errorf("%s: 期望 2 个工具，得到 %d 个", c.name, len(tcs))
			continue
		}
		got := map[string]string{}
		for _, raw := range tcs {
			fn := asMap(asMap(raw)["function"])
			got[asStr(fn["name"])] = asStr(fn["arguments"])
		}
		if got["a"] != `{"x":1}` {
			t.Errorf("%s: 工具 a 的参数是 %q", c.name, got["a"])
		}
		if got["b"] != `{"y":2}` {
			t.Errorf("%s: 工具 b 的参数是 %q", c.name, got["b"])
		}
	}
}

// 同一工具的续传分片仍归同一槽位。
func TestNonStreamToolFragmentsMerge(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c_a\",\"function\":{\"name\":\"a\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	out, err := sseToChatCompletion([]byte(up), "mimo-v2.5-free")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msg := asMap(asArr(out["choices"])[0].(map[string]any)["message"])
	tcs := asArr(msg["tool_calls"])
	if len(tcs) != 1 {
		t.Fatalf("续传分片被拆成 %d 个工具", len(tcs))
	}
	fn := asMap(asMap(tcs[0])["function"])
	if got := asStr(fn["arguments"]); got != `{"x":1}` {
		t.Errorf("参数拼接错误：%q", got)
	}
}
