package main

import (
	"strings"
	"testing"
)

func TestSanitizeToolHistoryChat(t *testing.T) {
	in := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"read","description":"","parameters":{"type":"object"}}}],"messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c_bad","type":"function","function":{"name":"clint_f3316348dbadf","arguments":"{}"}},
			{"id":"c_read","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"x\"}"}}
		]},
		{"role":"tool","tool_call_id":"c_bad","content":"Error: unknown tool"},
		{"role":"tool","tool_call_id":"c_read","content":"ok"},
		{"role":"assistant","content":"done"}
	]}`)
	out, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	wire, ok := renames.Wire("read")
	if !ok || wire == "read" {
		t.Fatalf("read 未改名: %v", wire)
	}
	msgs := asArr(out["messages"])
	if len(msgs) != 3 {
		t.Fatalf("幽灵调用/结果应删除，剩余 %d 条: %s", len(msgs), canon(msgs))
	}
	calls := asArr(asMap(msgs[0])["tool_calls"])
	if len(calls) != 1 || asStr(asMap(asMap(calls[0])["function"])["name"]) != wire {
		t.Fatalf("保留的 read 调用应改写为 wire: %s", canon(msgs[0]))
	}
	if asStr(asMap(msgs[1])["tool_call_id"]) != "c_read" {
		t.Fatalf("配对的 read 结果应保留: %s", canon(msgs[1]))
	}
	// 客户端原始请求不得被改写（重试时还能从明文重建）。
	inCalls := asArr(asMap(asArr(in["messages"])[0])["tool_calls"])
	if len(inCalls) != 2 || asStr(asMap(asMap(inCalls[0])["function"])["name"]) != "clint_f3316348dbadf" {
		t.Fatalf("原始 in 被改写: %s", canon(in["messages"]))
	}
}

func TestSanitizeToolHistoryResponses(t *testing.T) {
	in := mustJSON(t, `{"model":"m","tools":[{"type":"function","name":"read","description":"","parameters":{"type":"object"}}],"input":[
		{"type":"function_call","call_id":"c_bad","name":"clint_f3316348dbadf","arguments":"{}"},
		{"type":"function_call_output","call_id":"c_bad","output":"err"},
		{"type":"function_call","call_id":"c_read","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c_read","output":"ok"}
	]}`)
	out, renames, err := buildUpstream(FmtResponses, FmtResponses, in)
	if err != nil {
		t.Fatal(err)
	}
	wire, ok := renames.Wire("read")
	if !ok {
		t.Fatal("read 未改名")
	}
	items := asArr(out["input"])
	if len(items) != 2 {
		t.Fatalf("幽灵 function_call/output 应删除，剩余 %d: %s", len(items), canon(items))
	}
	call := asMap(items[0])
	if asStr(call["name"]) != wire || asStr(call["call_id"]) != "c_read" {
		t.Fatalf("read 调用未正确改写: %s", canon(call))
	}
	if asStr(asMap(items[1])["call_id"]) != "c_read" {
		t.Fatalf("read 结果应保留: %s", canon(items[1]))
	}
}

func TestOfficialToolAliasBinding(t *testing.T) {
	in := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"ops_bash","description":"","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`)
	_, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := renames.Official("shell")
	if !ok || client != "ops_bash" {
		t.Fatalf("official shell 未绑定 ops_bash: %q %v", client, ok)
	}
	if got := renames.Restore("shell"); got != "ops_bash" {
		t.Fatalf("Restore(shell) = %q, want ops_bash", got)
	}
}

func TestBuildUpstreamDoesNotDoubleHash(t *testing.T) {
	in := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"read","description":"","parameters":{"type":"object"}}}],"messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	u1, r1, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	u2, r2, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	w1, ok1 := r1.Wire("read")
	w2, ok2 := r2.Wire("read")
	if !ok1 || !ok2 || w1 != w2 {
		t.Fatalf("两次构建 wire 不一致: %q %q", w1, w2)
	}
	names1 := toolNamesForLog(u1)
	names2 := toolNamesForLog(u2)
	if strings.Join(names1, ",") != strings.Join(names2, ",") {
		t.Fatalf("两次构建工具名不一致:\n%v\n%v", names1, names2)
	}
	for _, name := range names1 {
		if strings.HasPrefix(name, "clint_") && strings.Count(name, "clint_") > 1 {
			t.Fatalf("出现二次 hash: %s", name)
		}
	}
	// 原始 in 仍是客户端明文。
	if got := asStr(asMap(asMap(asArr(in["tools"])[0])["function"])["name"]); got != "read" {
		t.Fatalf("原始 in tools 被改写: %q", got)
	}
}

func TestSanitizeToolChoice(t *testing.T) {
	in := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"read","description":"","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"clint_f3316348dbadf"}},"messages":[{"role":"user","content":"hi"}]}`)
	out, _, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := out["tool_choice"]; exists {
		t.Fatalf("指向幽灵工具的 tool_choice 应删除: %s", canon(out["tool_choice"]))
	}

	in2 := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"read","description":"","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"read"}},"messages":[{"role":"user","content":"hi"}]}`)
	out2, r2, err := buildUpstream(FmtChat, FmtChat, in2)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := r2.Wire("read")
	choice := asMap(asMap(out2["tool_choice"])["function"])
	if asStr(choice["name"]) != wire {
		t.Fatalf("tool_choice 未改写为 wire: %s", canon(out2["tool_choice"]))
	}
}
