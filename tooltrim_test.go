package main

import (
	"encoding/json"
	"strings"
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

// 2026-11-16 实测：上游免费层对 tools 数组长度有硬上限 21。
//   - 13 官方 + 8 客户端 = 21 → 200
//   - 13 官方 + 9 客户端 = 22 → 429 FreeUsageLimitError
//
// DSH 一次请求带 43 个工具，撞上限后每次必 429。整形后必须把总数压到 21。
func TestToolsTrimmedToUpstreamCap(t *testing.T) {
	for _, clientCount := range []int{1, 8, 9, 12, 30, 43, 60} {
		payload := map[string]any{
			"model": "deepseek-v4.1-flash-free",
			"tools": buildChatTools("clintz_", clientCount),
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		names := outboundToolNames(t, payload)
		if len(names) > maxUpstreamTools {
			t.Errorf("客户端 %d 个工具时出站 %d 个，超过上限 %d：%v",
				clientCount, len(names), maxUpstreamTools, names)
		}
		if clientCount <= 8 && len(names) != clientCount+len(v2OfficialToolOrder) {
			t.Errorf("客户端 %d 个工具时不该丢工具，出站 %d 个：%v",
				clientCount, len(names), names)
		}
	}
}

// 客户端工具优先保留：超出上限时被裁掉的是末尾的客户端工具，官方集始终完整。
func TestClientToolsKeptBeforeOfficialOverflow(t *testing.T) {
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"tools":    buildChatTools("keep_", 12),
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	names := outboundToolNames(t, payload)
	if len(names) != maxUpstreamTools {
		t.Fatalf("出站应为 %d 个，得到 %d：%v", maxUpstreamTools, len(names), names)
	}
	// 官方 13 个必须全在。
	present := map[string]bool{}
	for _, name := range names {
		present[name] = true
	}
	for _, official := range v2OfficialToolOrder {
		if !present[official] {
			t.Errorf("官方工具 %q 被裁掉了", official)
		}
	}
	// 客户端工具保留前面的 8 个（上限 21 - 官方 13）。
	clientKept := 0
	for _, name := range names {
		if len(name) >= len(ClientToolPrefix) && name[:len(ClientToolPrefix)] == ClientToolPrefix {
			clientKept++
		}
	}
	if clientKept != maxUpstreamTools-len(v2OfficialToolOrder) {
		t.Errorf("应保留 %d 个客户端工具，实际 %d：%v",
			maxUpstreamTools-len(v2OfficialToolOrder), clientKept, names)
	}
}

// 被裁掉的工具必须从重命名表里消失，否则响应映射与历史改写会指向不存在的名字。
func TestTrimmedToolsLeaveNoRenameResidue(t *testing.T) {
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"tools":    buildChatTools("gone_", 20),
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	up, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	tools, _ := up["tools"].([]any)
	wireNames := map[string]bool{}
	for _, item := range tools {
		if entry, ok := item.(map[string]any); ok {
			if fn, ok := entry["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					wireNames[name] = true
				}
			}
		}
	}
	for i := 0; i < 20; i++ {
		client := "gone_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		wire, ok := renames.Wire(client)
		if !ok {
			continue // 已被裁掉且登记已撤销，符合预期
		}
		if !wireNames[wire] {
			t.Errorf("客户端工具 %q 的重命名 %q 仍留在表里，但它不在出站 tools 中", client, wire)
		}
	}
}

// 历史里引用了被裁掉的工具时，该调用连同配对结果应被清掉，不能把不存在的名字
// 发给上游（那正是 opencode2api v1.4.7 修掉的“幽灵调用”）。
func TestHistoryGhostCallsDroppedAfterTrim(t *testing.T) {
	tools := buildChatTools("ghost_", 20)
	// 最后一个工具会被裁掉；历史里引用它。
	droppedTool := "ghost_" + string(rune('a'+19%26)) + string(rune('a'+19/26))
	payload := map[string]any{
		"model": "deepseek-v4.1-flash-free",
		"tools": tools,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id":   "call_ghost",
					"type": "function",
					"function": map[string]any{
						"name":      droppedTool,
						"arguments": "{}",
					},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_ghost", "content": "result"},
			map[string]any{"role": "user", "content": "again"},
		},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	raw, _ := json.Marshal(up)
	if strings.Contains(string(raw), droppedTool) {
		t.Errorf("出站 body 里仍有被裁掉的工具名 %q：%.400s", droppedTool, raw)
	}
}
