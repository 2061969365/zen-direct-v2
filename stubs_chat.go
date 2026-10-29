package main

// chatStubToolItem chat functions 形桩。
func chatStubToolItem(name string) map[string]any {
	initOfficialTools()
	if t, ok := officialToolsMap[name]; ok {
		desc := t.Description
		// 为 shell 和 read 保留禁调标记
		if name == "shell" || name == "read" {
			desc += stubNeverCall
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": desc,
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

// mergeChatStubTools chat 形注桩：补齐 2.2.0 官方全部 12 个工具（客户端工具排前，按 function.name 去重）。
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
	for _, raw := range items {
		if n := toolName(raw); n != "" {
			have[n] = true
		}
	}
	for _, t := range officialToolsList {
		if !have[t.Name] {
			items = append(items, chatStubToolItem(t.Name))
		}
	}
	in["tools"] = items
}
