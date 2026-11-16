package main

// toolchannel_adversarial_test.go: 工具通道的对抗性边界测试。
// 每个场景先推演"正确行为"再断言；判定依据写在各测试的注释里。

import (
	"encoding/json"
	"strings"
	"testing"
)

// marshalForCompare 深拷贝成 JSON 字符串，用于比较两次改写结果。
func marshalForCompare(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 场景 1：rewriteChannelHistory 幂等性。
// 正确行为：第一遍把客户端名映射为通道名、清掉选择器字段；第二遍看到的是
// 官方名（officialToolNameSet 命中）与干净参数，必须原样通过。映射不能二次
// 叠加（anysearch_search→websearch 之后不能再变），参数也不能被二次处理。
func TestRewriteChannelHistoryIdempotent(t *testing.T) {
	payload := mustJSON(t, `{"messages":[
		{"role":"user","content":"search"},
		{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"anysearch_search","arguments":"{\"query\":\"x\",\"_tool\":\"web_search\",\"_args\":{\"query\":\"y\"}}"}},
			{"id":"c2","type":"function","function":{"name":"pwsh","arguments":"{\"command\":\"ls\"}"}}
		]}
	],"input":[
		{"type":"function_call","name":"web_search","arguments":"{\"query\":\"x\"}"},
		{"type":"function_call","call_id":"c9","name":"some_unknown_tool","arguments":"{\"k\":1}"}
	]}`)
	rewriteChannelHistory(payload, FmtChat)
	first := marshalForCompare(t, payload)
	// 第一遍结果钉住：客户端名→通道名，选择器清掉。
	msgs := asArr(payload["messages"])
	call1 := asMap(asMap(asArr(asMap(msgs[1])["tool_calls"])[0])["function"])
	if asStr(call1["name"]) != "websearch" {
		t.Fatalf("anysearch_search 应映射为 websearch: %v", call1["name"])
	}
	if strings.Contains(asStr(call1["arguments"]), channelSelector) {
		t.Fatalf("历史 arguments 选择器未清除: %s", call1["arguments"])
	}
	rewriteChannelHistory(payload, FmtChat)
	second := marshalForCompare(t, payload)
	if first != second {
		t.Fatalf("rewriteChannelHistory 不幂等:\nfirst : %s\nsecond: %s", first, second)
	}
}

// 场景 2：客户端只声明 execute_command，模型以 shell 通道调用且 _tool 回显
// "shell"（或漏填）。正确行为：还原为客户端已声明的 execute_command。
// 修复前的行为是兜底到 channelDefaultTool["shell"]="pwsh"——客户端从没声明过
// pwsh，会收到 unknown tool，客户端名被弄丢。修复：通道下恰好声明了一个客户端
// 工具时优先还原到它（applyChannelArgsDefault + channelDefaultResolver）。
func TestApplyChannelArgsEchoedChannelPrefersDeclaredTool(t *testing.T) {
	ren := NewToolRenames()
	ren.SetDeclared([]string{"execute_command"})
	resolver := channelDefaultResolver(ren)
	// _tool 回显通道名
	name, _ := applyChannelArgsDefault("shell", `{"_tool":"shell","_args":{"command":"ls"}}`, ren.Declared, resolver)
	if name != "execute_command" {
		t.Fatalf("_tool=shell 且客户端仅声明 execute_command 时应还原为 execute_command: %q", name)
	}
	// 漏填选择器
	name2, _ := applyChannelArgsDefault("shell", `{"command":"ls"}`, ren.Declared, resolver)
	if name2 != "execute_command" {
		t.Fatalf("漏填 _tool 时应还原为已声明的 execute_command: %q", name2)
	}
	// restoreChannelCalls 全链路
	payload := mustJSON(t, `{"choices":[{"index":0,"message":{"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"_tool\":\"shell\",\"_args\":{\"command\":\"ls\"}}"}}
	]}}]}`)
	if !restoreChannelCalls(payload, ren) {
		t.Fatal("应当发生还原")
	}
	fn := asMap(asMap(asArr(asMap(asMap(asArr(payload["choices"])[0])["message"])["tool_calls"])[0])["function"])
	if asStr(fn["name"]) != "execute_command" {
		t.Fatalf("restoreChannelCalls 应还原为 execute_command: %v", fn["name"])
	}
	// 通道下声明了多个客户端工具时语义不明，仍走通道默认（pwsh）。
	ren2 := NewToolRenames()
	ren2.SetDeclared([]string{"execute_command", "pwsh"})
	name3, _ := applyChannelArgsDefault("shell", `{"_tool":"shell"}`, ren2.Declared, channelDefaultResolver(ren2))
	if name3 != "pwsh" {
		t.Fatalf("多声明时应兜底通道默认 pwsh: %q", name3)
	}
	// 未声明任何工具时保持原兜底。
	name4, _ := applyChannelArgsDefault("shell", `{"_tool":"shell"}`, func(string) bool { return false }, channelDefaultResolver(nil))
	if name4 != "pwsh" {
		t.Fatalf("无声明时应兜底通道默认 pwsh: %q", name4)
	}
}

// 场景 3：applyChannelArgs 对畸形 arguments 的健壮性。
// 正确行为：非法 JSON / 空串 / 标量 / 数组都不 panic、参数原样返回；名字只有
// 在能解析出选择器时才变。
func TestApplyChannelArgsMalformedArguments(t *testing.T) {
	declared := func(string) bool { return false }
	cases := []struct{ args, wantName, wantArgs string }{
		{"not json", "shell", "not json"},        // 非法 JSON：原样
		{"", "shell", ""},                        // 空串：原样
		{"42", "shell", "42"},                    // 纯数字：原样
		{"[1,2]", "shell", "[1,2]"},              // JSON 数组：原样
		{"null", "pwsh", "null"},                 // null 可解析为 nil map：无选择器，走通道默认，参数原样
		{"\"str\"", "shell", "\"str\""},          // JSON 字符串：原样
	}
	for _, c := range cases {
		name, args := applyChannelArgs("shell", c.args, declared)
		if name != c.wantName || args != c.wantArgs {
			t.Errorf("arguments=%q: got (%q,%q) want (%q,%q)", c.args, name, args, c.wantName, c.wantArgs)
		}
	}
}

// 场景 4：responses 形态 input[] 里的 function_call_output。
// 正确行为：output 项没有 name/arguments 字段，不得被改写、不得被加上 name；
// call_id 与 output 内容原样保留。
func TestRewriteChannelHistoryFunctionCallOutputUntouched(t *testing.T) {
	payload := mustJSON(t, `{"input":[
		{"type":"function_call","name":"pwsh","arguments":"{\"command\":\"ls\"}","call_id":"c1"},
		{"type":"function_call_output","call_id":"c1","output":"total 0"},
		{"type":"message","role":"user","content":"hi"}
	]}`)
	before := marshalForCompare(t, payload)
	rewriteChannelHistory(payload, FmtChat)
	items := asArr(payload["input"])
	fnItem := items[0].(map[string]any)
	if asStr(fnItem["name"]) != "shell" {
		t.Fatalf("function_call name 应映射为 shell: %v", fnItem["name"])
	}
	outItem := items[1].(map[string]any)
	if _, has := outItem["name"]; has {
		t.Fatal("function_call_output 不应被加上 name 字段")
	}
	if asStr(outItem["output"]) != "total 0" || asStr(outItem["call_id"]) != "c1" {
		t.Fatalf("function_call_output 被误改: %v", outItem)
	}
	// 除 function_call 的 name 外，其余项逐字节不变。
	fnItem["name"] = "pwsh"
	if before != marshalForCompare(t, payload) {
		t.Fatal("function_call_output/message 项被误改")
	}
}

// 场景 5：restoreChannelCalls 对流式 delta.tool_calls 的安全性。
// 正确行为：arguments 为空字符串（流式起始分片）或缺 arguments 字段时不改写、
// 不 panic、changed=false——空参数没有选择器可还原。
func TestRestoreChannelCallsDeltaEmptyArguments(t *testing.T) {
	payload := mustJSON(t, `{"choices":[{"index":0,"delta":{"tool_calls":[
		{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":""}},
		{"index":1,"id":"c2","type":"function","function":{"name":"websearch"}}
	]}}]}`)
	before := marshalForCompare(t, payload)
	if restoreChannelCalls(payload, nil) {
		t.Fatal("空 arguments 的 delta 不应触发还原")
	}
	if before != marshalForCompare(t, payload) {
		t.Fatal("空 arguments 的 delta 被误改")
	}
}

// 场景 6：tool_choice 未知形态 {"type":"allowed_tools","config":[...]}。
// 正确行为：不 panic、不改写（代码只处理 string / {function:{name}} / legacy
// function_call 三种形态；config 里嵌的客户端名不是已支持的形态，原样通过）。
func TestRewriteChannelToolChoiceUnknownShape(t *testing.T) {
	payload := mustJSON(t, `{"tool_choice":{"type":"allowed_tools","config":[
		{"type":"function","name":"pwsh"}
	]}}`)
	before := marshalForCompare(t, payload)
	rewriteChannelToolChoice(payload, FmtChat)
	if before != marshalForCompare(t, payload) {
		t.Fatalf("未知 tool_choice 形态被改写: %v", payload["tool_choice"])
	}
	// tool_choice 为非 string 非 map（数字）也不 panic。
	p2 := mustJSON(t, `{"tool_choice":3}`)
	rewriteChannelToolChoice(p2, FmtChat)
	if p2["tool_choice"] != float64(3) {
		t.Fatalf("数字 tool_choice 被改写: %v", p2["tool_choice"])
	}
}

// 场景 7：stripChannelSelector 的两个边界。
// 正确行为：只有 _tool 没有参数 → 清掉选择器后剩空对象 "{}"；只有空 _args →
// _args 提升后是空对象 "{}"。两者都不 panic。
func TestStripChannelSelectorEdges(t *testing.T) {
	if out := stripChannelSelector(`{"_tool":"x"}`); out != "{}" {
		t.Fatalf("只有 _tool 应得 {}: %q", out)
	}
	if out := stripChannelSelector(`{"_args":{}}`); out != "{}" {
		t.Fatalf("只有空 _args 应得 {}: %q", out)
	}
	if out := stripChannelSelector(`{"_tool":"x","_args":{}}`); out != "{}" {
		t.Fatalf("选择器+空 _args 应得 {}: %q", out)
	}
}

// 场景 8：buildChannelTools 混入非 function 类型条目（内置工具声明
// {"type":"web_search"}、非 map 条目）。
// 判定（当前行为=正确行为）：entryToolName 返回空后这些项被跳过，不会出现在
// 出站通道工具里——这是有意的：通道模式下上游只放行 12 个官方桩名，内置工具
// 声明发出去只会破坏"名字集合全官方"的门禁；客户端侧这些内置能力本来也不经
// 这条链路。会丢失的是"声明"，不是"功能"。断言不 panic、剩余通道仍完整、
// 非法条目不产生通道。
func TestBuildChannelToolsSkipsNonFunctionEntries(t *testing.T) {
	in := []any{
		chatTool("pwsh", "Run a command."),
		map[string]any{"type": "web_search"}, // 内置工具声明，无 function/name
		"not-a-map",                          // 非 map 条目
		nil,
	}
	tools := buildChannelTools(FmtChat, in)
	if len(tools) != 1 {
		t.Fatalf("只有 pwsh 应产生 1 个通道: %d", len(tools))
	}
	name := asStr(asMap(asMap(tools[0])["function"])["name"])
	if name != "shell" {
		t.Fatalf("pwsh 应映射为 shell: %q", name)
	}
}

// 场景 9：channelDescription 对成员描述含换行/反引号/超长文本（10KB）。
// 正确行为：输出仍是合法可编码的字符串——JSON 序列化往返后与原文一致，
// 成员名列出，长度可控（不加协议外字符），不 panic。
func TestChannelDescriptionHostileMemberDescription(t *testing.T) {
	hostile := "line1\nline2 `backticks` {\"json\":\"inside\"}\n" + strings.Repeat("x", 10*1024)
	members := []map[string]any{
		{"function": map[string]any{"name": "pwsh", "description": hostile}},
	}
	desc := channelDescription("shell", members, FmtChat)
	if !strings.Contains(desc, "pwsh") {
		t.Fatal("通道描述未列出成员名")
	}
	// 合法性：能被 encoding/json 无损编解码。
	enc, err := json.Marshal(map[string]string{"description": desc})
	if err != nil {
		t.Fatalf("描述 JSON 编码失败: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatalf("描述 JSON 解码失败: %v", err)
	}
	if back["description"] != desc {
		t.Fatal("描述 JSON 往返不一致")
	}
	if !strings.Contains(desc, strings.Repeat("x", 1024)) {
		t.Fatal("超长描述被截断")
	}
}

// 场景 10：历史 tool_calls 的 arguments 里嵌套转义的 _tool 字样（客户端内容
// 碰巧带这个字段）。
// 正确行为：stripChannelSelector 只动顶层键；嵌套在字符串值里的 `_tool` 是
// 客户端数据，必须原样保留。顶层没有选择器时整个 arguments 逐字节不变。
func TestRewriteChannelHistoryNestedSelectorStringPreserved(t *testing.T) {
	// 顶层干净，嵌套字符串里带 _tool
	nested := `{"command":"echo {\"_tool\":\"x\"}"}`
	payload := mustJSON(t, `{"messages":[{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"pwsh","arguments":"{\"command\":\"echo {\\\"_tool\\\":\\\"x\\\"}\"}"}}
	]}]}`)
	rewriteChannelHistory(payload, FmtChat)
	fn := asMap(asMap(asArr(asMap(asArr(payload["messages"])[0])["tool_calls"])[0])["function"])
	if asStr(fn["name"]) != "shell" {
		t.Fatalf("name 应映射为 shell: %v", fn["name"])
	}
	if asStr(fn["arguments"]) != nested {
		t.Fatalf("嵌套 _tool 字样所在参数被误改:\n got : %s\nwant : %s", fn["arguments"], nested)
	}
	// 顶层带选择器 + _args 里嵌套 _tool 字样：清顶层选择器，嵌套内容保留。
	payload2 := mustJSON(t, `{"messages":[{"role":"assistant","tool_calls":[
		{"id":"c2","type":"function","function":{"name":"pwsh","arguments":"{\"_tool\":\"pwsh\",\"_args\":{\"command\":\"echo {\\\"_tool\\\":\\\"nested\\\"}\"}}"}}
	]}]}`)
	rewriteChannelHistory(payload2, FmtChat)
	fn2 := asMap(asMap(asArr(asMap(asArr(payload2["messages"])[0])["tool_calls"])[0])["function"])
	var m map[string]any
	if err := json.Unmarshal([]byte(asStr(fn2["arguments"])), &m); err != nil {
		t.Fatalf("清理后的参数不是合法 JSON: %v", fn2["arguments"])
	}
	if _, has := m[channelSelector]; has {
		t.Fatal("顶层选择器未清除")
	}
	if m["command"] != `echo {"_tool":"nested"}` {
		t.Fatalf("嵌套 _tool 字符串被误改: %v", m["command"])
	}
}
