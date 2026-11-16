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
	name, expanded := applyChannelArgs("read", args, func(string) bool { return false })
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
	name, expanded := applyChannelArgs("read", args, func(string) bool { return false })
	if name != "read" || expanded != args {
		t.Fatalf("无选择器时应原样返回: %q %q", name, expanded)
	}
}

func TestRestoreChannelCalls(t *testing.T) {
	payload := mustJSON(t, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"websearch","arguments":"{\"query\":\"x\",\"_tool\":\"web_search\",\"_args\":{\"q\":\"y\"}}"}}
	]}}]}`)
	if !restoreChannelCalls(payload, nil) {
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

// TestApplyChannelArgsAllChannels 全通道还原矩阵：
// 1) 每个已知客户端工具经 _tool 选择器都还原回原名
// 2) 模型漏填 _tool 时兜底还原到通道默认工具（shell->pwsh 等）
// 3) 未知通道名不动
func TestApplyChannelArgsAllChannels(t *testing.T) {
	for client, channel := range toolChannelMap {
		got, expanded := applyChannelArgs(channel,
			`{"`+channelSelector+`":"`+client+`","`+channelArgs+`":{"k":"v"}}`,
			func(string) bool { return true })
		if got != client {
			t.Errorf("通道 %s 的 %s 还原失败: got %q", channel, client, got)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(expanded), &m); err != nil {
			t.Errorf("%s 参数不是合法 JSON: %s", client, expanded)
			continue
		}
		if m["k"] != "v" {
			t.Errorf("%s _args 未展开: %s", client, expanded)
		}
	}
	for channel, def := range channelDefaultTool {
		got, _ := applyChannelArgs(channel, `{"command":"ls"}`, func(string) bool { return false })
		if got != def {
			t.Errorf("通道 %s 缺选择器时应兜底到 %s: got %q", channel, def, got)
		}
	}
	if got, _ := applyChannelArgs("notachannel", `{"a":1}`, func(string) bool { return false }); got != "notachannel" {
		t.Errorf("未知通道名应原样保留: %q", got)
	}
}

// TestRewriteChannelToolChoice 通道模式 tool_choice 必须映射回通道名。
func TestRewriteChannelToolChoice(t *testing.T) {
	// 字符串形态
	p := mustJSON(t, `{"tool_choice":"web_search"}`)
	rewriteChannelToolChoice(p, FmtChat)
	if p["tool_choice"] != "websearch" {
		t.Fatalf("字符串 tool_choice 未映射: %v", p["tool_choice"])
	}
	// 对象形态
	p2 := mustJSON(t, `{"tool_choice":{"type":"function","function":{"name":"pwsh"}}}`)
	rewriteChannelToolChoice(p2, FmtChat)
	fn := asMap(asMap(p2["tool_choice"])["function"])
	if asStr(fn["name"]) != "shell" {
		t.Fatalf("对象 tool_choice 未映射: %v", fn["name"])
	}
	// 官方名不动
	p3 := mustJSON(t, `{"tool_choice":{"type":"function","function":{"name":"read"}}}`)
	rewriteChannelToolChoice(p3, FmtChat)
	fn3 := asMap(asMap(p3["tool_choice"])["function"])
	if asStr(fn3["name"]) != "read" {
		t.Fatalf("官方名不应改写: %v", fn3["name"])
	}
	// auto 不动
	p4 := mustJSON(t, `{"tool_choice":"auto"}`)
	rewriteChannelToolChoice(p4, FmtChat)
	if p4["tool_choice"] != "auto" {
		t.Fatalf("auto 不应改写: %v", p4["tool_choice"])
	}
}

// TestApplyChannelArgsSelectorVariants _tool 填官方名/通道名时的兜底。
func TestApplyChannelArgsSelectorVariants(t *testing.T) {
	// _tool=shell（官方名，客户端没有叫 shell 的工具）-> pwsh
	name, _ := applyChannelArgs("shell", `{"_tool":"shell","_args":{"command":"ls"}}`, func(string) bool { return false })
	if name != "pwsh" {
		t.Fatalf("_tool=shell 应兜底 pwsh: %q", name)
	}
	// _tool=websearch（通道名）-> web_search
	name2, _ := applyChannelArgs("websearch", `{"_tool":"websearch","_args":{"query":"x"}}`, func(string) bool { return false })
	if name2 != "web_search" {
		t.Fatalf("_tool=websearch 应兜底 web_search: %q", name2)
	}
	// _tool 是真客户端名 -> 原样
	name3, _ := applyChannelArgs("read", `{"_tool":"read","_args":{"path":"x"}}`, func(string) bool { return false })
	if name3 != "read" {
		t.Fatalf("_tool=客户端名应原样: %q", name3)
	}
}

// TestResponsesStreamChannelRestore responses 流 completed 帧的通道还原。
func TestResponsesStreamChannelRestore(t *testing.T) {
	t.Setenv("ZEN_TOOL_CHANNEL", "1")
	ren := newStreamToolRenamer(FmtResponses, NewToolRenames())
	frame := `{"type":"response.completed","response":{"output":[
		{"type":"function_call","name":"websearch","arguments":"{\"query\":\"x\",\"_tool\":\"web_search\",\"_args\":{\"q\":\"y\"}}"}
	]}}`
	out := ren.rewriteFrame(frame)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	item := asArr(asMap(payload["response"])["output"])[0]
	if asStr(item.(map[string]any)["name"]) != "web_search" {
		t.Fatalf("responses 流还原失败: %s", out)
	}
}
