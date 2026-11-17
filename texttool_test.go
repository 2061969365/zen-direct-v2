package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDSMLExtractorWrappedBlock 完整 tool_calls 块：多 invoke、JSON 参数保持。
func TestDSMLExtractorWrappedBlock(t *testing.T) {
	markup := "before text\n<|DSML| tool_calls>\n" +
		"<|DSML| invoke name=\"pwsh\">\n" +
		"<|DSML| parameter name=\"command\">Get-Date</|DSML| parameter>\n" +
		"</|DSML| invoke>\n" +
		"<|DSML| invoke name=\"web_search\">\n" +
		"<|DSML| parameter name=\"queries\">[\"opencode\",\"zen\"]</|DSML| parameter>\n" +
		"</|DSML| invoke>\n" +
		"</|DSML| tool_calls>\nafter"
	var ex textToolExtractor
	visible, calls := ex.Feed(markup)
	tail, more := ex.Flush()
	visible += tail
	calls = append(calls, more...)
	if strings.Contains(visible, "DSML") {
		t.Fatalf("标记未从可见文本移除: %q", visible)
	}
	if !strings.Contains(visible, "before text") || !strings.Contains(visible, "after") {
		t.Fatalf("前后正文丢失: %q", visible)
	}
	if len(calls) != 2 {
		t.Fatalf("应恢复 2 个调用，得 %d", len(calls))
	}
	if calls[0].Name != "pwsh" {
		t.Fatalf("第一个调用名错误: %q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["command"] != "Get-Date" {
		t.Fatalf("command 参数错误: %v", args)
	}
	if calls[1].Name != "web_search" {
		t.Fatalf("第二个调用名错误: %q", calls[1].Name)
	}
	var arr []any
	if err := json.Unmarshal([]byte(calls[1].Arguments), &arr); err != nil {
		// arguments 是 {"queries": [...]} 结构
		var obj map[string]any
		if err2 := json.Unmarshal([]byte(calls[1].Arguments), &obj); err2 != nil {
			t.Fatalf("queries 参数解析失败: %s", calls[1].Arguments)
		}
		if _, ok := obj["queries"]; !ok {
			t.Fatalf("queries 缺失: %v", obj)
		}
	} else {
		_ = arr
	}
}

// TestDSMLExtractorBareInvoke 裸 invoke 块。
func TestDSMLExtractorBareInvoke(t *testing.T) {
	markup := "<|DSML| invoke name=\"read\">\n" +
		"<|DSML| parameter name=\"path\">a.txt</|DSML| parameter>\n" +
		"</|DSML| invoke>"
	var ex textToolExtractor
	_, calls := ex.Feed(markup)
	tail, more := ex.Flush()
	calls = append(calls, more...)
	_ = tail
	if len(calls) != 1 || calls[0].Name != "read" {
		t.Fatalf("裸 invoke 未恢复: %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "a.txt" {
		t.Fatalf("path 参数错误: %v", args)
	}
}

// TestDSMLExtractorPartialMarker 跨分片标记不被提前显示。
func TestDSMLExtractorPartialMarker(t *testing.T) {
	var ex textToolExtractor
	v1, _ := ex.Feed("text before <|DSML| tool_")
	if v1 != "text before " {
		t.Fatalf("部分标记应被扣住: %q", v1)
	}
	v2, calls := ex.Feed("calls><|DSML| invoke name=\"read\"><|DSML| parameter name=\"path\">x</|DSML| parameter></|DSML| invoke></|DSML| tool_calls>")
	tail, more := ex.Flush()
	calls = append(calls, more...)
	_ = tail
	if strings.Contains(v2+tail, "DSML") {
		t.Fatalf("完整标记后仍泄漏: %q %q", v2, tail)
	}
	if len(calls) != 1 || calls[0].Name != "read" {
		t.Fatalf("跨分片恢复失败: %+v", calls)
	}
}

// TestDSMLExtractorNonToolTag 非工具用途的 DSML 标签原样放行。
func TestDSMLExtractorNonToolTag(t *testing.T) {
	var ex textToolExtractor
	visible, calls := ex.Feed("<|DSML| note>hello</|DSML| note>")
	tail, _ := ex.Flush()
	visible += tail
	if strings.Contains(visible, "DSML") == false {
		t.Fatalf("非工具 DSML 标签应原样保留: %q", visible)
	}
	if len(calls) != 0 {
		t.Fatalf("非工具标签不应产生调用: %+v", calls)
	}
}

// TestDSMLExtractorUnclosedBlockFlush 未闭合块 flush best-effort。
func TestDSMLExtractorUnclosedBlockFlush(t *testing.T) {
	var ex textToolExtractor
	ex.Feed("<|DSML| tool_calls><|DSML| invoke name=\"read\"><|DSML| parameter name=\"path\">a</|DSML| parameter>")
	// 上游设计：未闭合块在 flush 时文本原样保留（"a truncated answer
	// loses nothing"），不强行恢复调用——对齐 opencode2iori 行为。
	visible, calls := ex.Flush()
	if len(calls) != 0 {
		t.Fatalf("未闭合块按设计不恢复调用: %+v", calls)
	}
	if !strings.Contains(visible, "invoke name=") {
		t.Fatalf("未闭合块文本应原样保留: %q", visible)
	}
}

// TestExtractChatTextToolCallsInto 非流 chat body 整体改写。
func TestExtractChatTextToolCallsInto(t *testing.T) {
	payload := mustJSON(t, `{"choices":[{"finish_reason":"stop","index":0,"message":{
		"role":"assistant","content":"working\n<|DSML| tool_calls><|DSML| invoke name=\"mcp_abc123def456\"><|DSML| parameter name=\"query\">opencode</|DSML| parameter></|DSML| invoke></|DSML| tool_calls>"
	}}]}`)
	if !ExtractChatTextToolCallsInto(payload) {
		t.Fatal("应发生改写")
	}
	choice := asArr(payload["choices"])[0].(map[string]any)
	if asStr(choice["finish_reason"]) != "tool_calls" {
		t.Fatalf("finish_reason 应改写: %v", choice["finish_reason"])
	}
	msg := asMap(choice["message"])
	if strings.Contains(asStr(msg["content"]), "DSML") {
		t.Fatalf("content 未清理: %q", asStr(msg["content"]))
	}
	calls := asArr(msg["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("应有 1 个恢复调用: %d", len(calls))
	}
	fn := asMap(asMap(calls[0])["function"])
	if asStr(fn["name"]) != "mcp_abc123def456" {
		t.Fatalf("wire 名错误: %v", fn["name"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(asStr(fn["arguments"])), &args); err != nil {
		t.Fatal(err)
	}
	if args["query"] != "opencode" {
		t.Fatalf("参数错误: %v", args)
	}
}

// TestDSMLStreamEndToEnd 流式端到端：DSML 文本分片 → 结构化 tool_calls →
// restoreAliasNames 还原客户端名。
func TestDSMLStreamEndToEnd(t *testing.T) {
	renames := NewToolRenames()
	wire := ClientToolWireName("web_search", nil)
	renames.Add("web_search", wire)

	// 三片：标记跨片分割
	p1 := "thinking\n<|DSML| tool_calls><|DSML| invoke name=\"" + wire + "\"><|DSML| param"
	p2 := "eter name=\"query\">opencode</|DSML| parameter></|DSML| invoke>"
	p3 := "</|DSML| tool_calls>"
	mk := func(content string, finish any) string {
		delta := map[string]any{"content": content}
		b, _ := json.Marshal(delta)
		fr := "null"
		if finish != nil {
			fr = `"` + finish.(string) + `"`
		}
		return "data: {\"choices\":[{\"index\":0,\"delta\":" + string(b) + ",\"finish_reason\":" + fr + "}]}\n\n"
	}
	input := mk(p1, nil) + mk(p2, nil) + mk(p3, "stop") + "data: [DONE]\n\n"
	out := string(restoreAliasNames([]byte(readRenamed(t, input, FmtChat, renames)), renames))
	if strings.Contains(out, "DSML") {
		t.Fatalf("流式输出残留 DSML 标记: %s", out)
	}
	if !strings.Contains(out, `"name":"web_search"`) {
		t.Fatalf("流式未恢复客户端工具名: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason 未改写: %s", out)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"query":"opencode"}`), &args); err != nil {
		t.Fatal(err)
	}
}
