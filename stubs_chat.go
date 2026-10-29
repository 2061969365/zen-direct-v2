package main

import (
	"strings"
)

// chatStubToolItem chat functions 形桩。
func chatStubToolItem(name string) map[string]any {
	initOfficialTools()
	if t, ok := officialToolsMap[name]; ok {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		}
	}
	desc, params, req := stubSpec(name)
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": params,
			"required": []any{req}, "additionalProperties": false},
	}}
}

// mergeChatStubTools chat 形注桩：补齐 2.2.0 官方工具（客户端工具排前，按名称不区分大小写去重，总数严格限额 <=42）。
func mergeChatStubTools(in map[string]any) {
	initOfficialTools()
	toolName := func(raw any) string {
		m, ok := raw.(map[string]any)
		if !ok {
			return ""
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				return n
			}
		}
		if n, ok := m["name"].(string); ok {
			return n
		}
		return ""
	}
	have := map[string]bool{}
	items, _ := in["tools"].([]any)
	var deduped []any
	for _, raw := range items {
		n := toolName(raw)
		if n == "" {
			continue
		}
		lower := strings.ToLower(n)
		if have[lower] {
			continue // 客户端工具自身按小写去重
		}
		have[lower] = true
		deduped = append(deduped, raw)
	}
	// 仅在未达到 42 上限且该名（及 zd_ 别名）未被客户端占用时增补官方桩
	for _, t := range officialToolsList {
		if len(deduped) >= 42 {
			break
		}
		lower := strings.ToLower(t.Name)
		if !have[lower] && !have["zd_"+lower] {
			have[lower] = true
			deduped = append(deduped, chatStubToolItem(t.Name))
		}
	}
	in["tools"] = deduped
}
