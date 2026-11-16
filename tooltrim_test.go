package main

import (
	"encoding/json"
	"testing"
)

// buildChatTools 造 count 个客户端工具，名字带前缀避免与官方集重名。
func buildChatTools(prefix string, count int) []any {
	tools := make([]any, 0, count)
	for i := 0; i < count; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        prefix + string(rune('a'+i%26)) + string(rune('a'+i/26)),
				"description": "test tool",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"a": map[string]any{"type": "string"}},
				},
			},
		})
	}
	return tools
}

func outboundToolNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	tools, _ := up["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, item := range tools {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := entry["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// 客户端工具一个不裁。
//
// 2026-11-16 在干净出口逐档加压实测：出站 21 → 26 → 30 → 35 → 43 → 56 个工具全部
// 200；56 个连测三次通过，up429 始终不增；补字节到 36750 也仍 200。所以数量与字节
// 都不是 429 的触发条件，触发点是工具描述文本指纹（见 bannedDescriptionFragments）。
//
// 曾把总数封顶到 21，后果是丢掉 team_task_* / send_message / spawn_teammate 这些多
// agent 协作必需的工具，webfetch / websearch 的官方桩也绑不上。
func TestNoClientToolTrimmed(t *testing.T) {
	for _, clientCount := range []int{1, 8, 9, 12, 30, 43, 60} {
		payload := map[string]any{
			"model": "deepseek-v4.1-flash-free",
			"tools": buildChatTools("clintz_", clientCount),
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		names := outboundToolNames(t, payload)
		want := clientCount + len(v2OfficialToolOrder)
		if len(names) != want {
			t.Errorf("客户端 %d 个工具时出站应 %d 个（客户端 + 官方 %d），得到 %d",
				clientCount, want, len(v2OfficialToolOrder), len(names))
		}
	}
}

// 真实 DSH 的 43 个工具必须全部出站，一个都不能少。
// 这条直接钉住"工具被裁掉导致模型没法用"的回归。
func TestDSHCoreToolsAllSurvive(t *testing.T) {
	// DSH 实际携带的工具，覆盖执行、读写、检索、协作、委托、任务板、调度、联网。
	names := []string{
		"anysearch_batch_search", "anysearch_capabilities", "anysearch_search",
		"ask_user_question", "create_goal", "dsh_im_group_file", "dsh_im_group_history",
		"dsh_im_return_file", "edit", "exit_plan_mode", "get_goal", "glob", "grep",
		"interrupt_agent", "job_kill", "job_list", "job_output", "list_agents", "present",
		"pwsh", "read", "read_image", "schedule_create", "schedule_delete", "schedule_list",
		"schedule_update", "send_message", "skill", "spawn_teammate", "subagent",
		"subagent_fork", "team_task_create", "team_task_get", "team_task_list",
		"team_task_update", "todo_write", "update_goal", "wait_agent", "web_fetch",
		"web_search", "workflow", "working_directory", "write",
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, chatTool(name, "Tool "+name))
	}
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"tools":    tools,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	up, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	entries, _ := up["tools"].([]any)
	if len(entries) != len(names)+len(v2OfficialToolOrder) {
		t.Fatalf("出站应为 %d 个工具，得到 %d", len(names)+len(v2OfficialToolOrder), len(entries))
	}
	for _, name := range names {
		if _, ok := renames.Wire(name); !ok {
			t.Errorf("客户端工具 %q 没有登记重命名，说明整形丢了它", name)
		}
	}
}

// 官方桩必须绑定到客户端真实工具，否则模型看到的是调不动的空壳。
// 其中 shell -> pwsh 就是用户最初报告的"网关 shell 工具没有了"。
func TestOfficialStubsBindToClientTools(t *testing.T) {
	want := map[string]string{
		"shell": "pwsh", "read": "read", "write": "write", "edit": "edit",
		"grep": "grep", "glob": "glob", "subagent": "subagent", "skill": "skill",
		"webfetch": "web_fetch", "websearch": "web_search",
	}
	tools := make([]any, 0, len(want))
	seen := map[string]bool{}
	for _, client := range want {
		if seen[client] {
			continue
		}
		seen[client] = true
		tools = append(tools, chatTool(client, "Tool "+client))
	}
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"tools":    tools,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	_, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	for official, client := range want {
		bound, ok := renames.Official(official)
		if !ok {
			t.Errorf("官方桩 %q 没有绑定，模型调用它会落空", official)
			continue
		}
		if bound != client {
			t.Errorf("官方桩 %q 绑到 %q，期望 %q", official, bound, client)
		}
	}
}

// 历史里的工具调用必须跟着重命名走，不能被误清。
func TestHistoryKeepsDeclaredToolCalls(t *testing.T) {
	payload := map[string]any{
		"model": "deepseek-v4.1-flash-free",
		"tools": buildChatTools("keep_", 30),
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id":   "call_keep",
					"type": "function",
					"function": map[string]any{
						"name":      "keep_aa",
						"arguments": "{}",
					},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_keep", "content": "result"},
			map[string]any{"role": "user", "content": "again"},
		},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	raw, _ := json.Marshal(up)
	if !json.Valid(raw) {
		t.Fatalf("出站 body 不是合法 JSON")
	}
	messages, _ := up["messages"].([]any)
	sawToolCall := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Errorf("声明过的工具的调用被误清了")
	}
}
