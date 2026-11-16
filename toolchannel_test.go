package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildChannelTools(t *testing.T) {
	in := []any{
		chatTool("pwsh", "Run a command."),
		chatTool("read", "Read a file."),
		chatTool("team_task_create", "Create a task."),
		chatTool("web_search", "Search the web."),
	}
	tools := buildChannelTools(FmtChat, in)
	if len(tools) == 0 || len(tools) > len(v2OfficialToolOrder) {
		t.Fatalf("通道数异常: %d", len(tools))
	}
	seen := map[string]bool{}
	for _, raw := range tools {
		name := asStr(asMap(asMap(raw)["function"])["name"])
		if !officialToolNameSet()[name] {
			t.Fatalf("出站工具 %q 不是官方名", name)
		}
		seen[name] = true
		// 每个通道都要带 _tool / _args
		props := asMap(asMap(asMap(asMap(raw)["function"])["parameters"])["properties"])
		if _, ok := props[channelSelector]; !ok {
			t.Fatalf("通道 %q 缺 %s", name, channelSelector)
		}
		if _, ok := props[channelArgs]; !ok {
			t.Fatalf("通道 %q 缺 %s", name, channelArgs)
		}
	}
	// pwsh 和 team_task_create 都映射到已有通道，不应新增非官方名。
	if seen["pwsh"] || seen["team_task_create"] {
		t.Fatalf("出站出现客户端原名: %v", seen)
	}
}

func TestChannelDescriptionListsMembers(t *testing.T) {
	in := []any{chatTool("web_search", "Search the web"), chatTool("anysearch_search", "Batch search")}
	tools := buildChannelTools(FmtChat, in)
	found := false
	for _, raw := range tools {
		desc := asStr(asMap(asMap(raw)["function"])["description"])
		if strings.Contains(desc, "web_search") && strings.Contains(desc, "anysearch_search") {
			found = true
		}
	}
	if !found {
		t.Fatal("通道描述未列出承载的真实工具")
	}
}

func TestApplyChannelArgs(t *testing.T) {
	args := `{"path":"a.txt","_tool":"read_image","_args":{"path":"pic.png","offset":2}}`
	name, expanded := applyChannelArgs("read", args)
	if name != "read_image" {
		t.Fatalf("真实工具名错误: %q", name)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(expanded), &m); err != nil {
		t.Fatal(err)
	}
	if m["path"] != "pic.png" || m["offset"] == nil {
		t.Fatalf("_args 未正确展开: %s", expanded)
	}
	if _, ok := m[channelSelector]; ok {
		t.Fatal("选择器字段未清除")
	}
	if _, ok := m[channelArgs]; ok {
		t.Fatal("_args 包装未清除")
	}
}

func TestApplyChannelArgsNoSelector(t *testing.T) {
	args := `{"path":"a.txt"}`
	name, expanded := applyChannelArgs("read", args)
	if name != "read" || expanded != args {
		t.Fatalf("无选择器时应原样返回: %q %q", name, expanded)
	}
}

func TestRestoreChannelCalls(t *testing.T) {
	payload := mustJSON(t, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"websearch","arguments":"{\"query\":\"x\",\"_tool\":\"web_search\",\"_args\":{\"q\":\"y\"}}"}}
	]}}]}`)
	if !restoreChannelCalls(payload) {
		t.Fatal("应当发生还原")
	}
	choice := asArr(payload["choices"])[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	call := asArr(msg["tool_calls"])[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if asStr(fn["name"]) != "web_search" {
		t.Fatalf("名称未还原: %v", fn["name"])
	}
	if !strings.Contains(asStr(fn["arguments"]), `"q":"y"`) {
		t.Fatalf("参数未展开: %s", asStr(fn["arguments"]))
	}
}

// TestRewriteChannelHistoryNoSelector 钉住多轮 429 的根因：
// 历史 tool_calls 的 arguments 必须是干净的官方参数，绝不能带 _tool/_args。
// 只有 name 映射回通道名，arguments 保持（或清理回）原样。
func TestRewriteChannelHistoryNoSelector(t *testing.T) {
	payload := mustJSON(t, `{"messages":[
		{"role":"user","content":"search"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"opencode\"}"}}
		]},
		{"role":"tool","tool_call_id":"c1","content":"result"}
	]}`)
	rewriteChannelHistory(payload, FmtChat)
	msgs := asArr(payload["messages"])
	call := asMap(asArr(asMap(msgs[1])["tool_calls"])[0])
	fn := asMap(call["function"])
	if asStr(fn["name"]) != "websearch" {
		t.Fatalf("历史 name 未映射回通道名: %v", fn["name"])
	}
	args := asStr(fn["arguments"])
	if strings.Contains(args, channelSelector) || strings.Contains(args, channelArgs) {
		t.Fatalf("历史 arguments 不得含选择器字段: %s", args)
	}
	if !strings.Contains(args, "opencode") {
		t.Fatalf("历史参数被清空: %s", args)
	}
}

// TestStripChannelSelector 验证回放上一轮响应时选择器字段被清理。
func TestStripChannelSelector(t *testing.T) {
	in := `{"query":"x","_tool":"web_search","_args":{"query":"y","limit":5}}`
	out := stripChannelSelector(in)
	if strings.Contains(out, channelSelector) || strings.Contains(out, channelArgs) {
		t.Fatalf("选择器未清除: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["query"] != "y" || m["limit"] == nil {
		t.Fatalf("_args 未提升为顶层参数: %s", out)
	}
}
