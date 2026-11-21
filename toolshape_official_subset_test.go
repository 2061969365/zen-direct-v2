package main

import (
	"encoding/json"
	"testing"
)

// 官方名子集不能走"原样透传"。
//
// 2026-11-17 GitHub Actions 多出口实测（24 个出口 IP / 48 发）：
//   - 只声明 {read}、且 read 的参数沿用 DSH 原文（"Path to edit, resolved by
//     the filesystem backend"）时，zen-direct 因为 allOfficialToolNames 为真而
//     整份透传，出站保留官方名 read → 上游 0/8 全 429；
//   - 同一份 body 交给 opencode2api（它无条件把客户端工具改名成 mcp_<digest>）
//     → 8/8 全 200。
//
// 结论：透传只允许"完整官方集"，子集必须走改名+补全。

func officialSubsetPayload(t *testing.T, names []string) map[string]any {
	t.Helper()
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Tool " + name + ".",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_path": map[string]any{
							"type":        "string",
							"description": "Path to edit, resolved by the filesystem backend",
						},
					},
					"required": []any{"file_path"},
				},
			},
		})
	}
	return map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    tools,
	}
}

func TestCompleteOfficialToolset(t *testing.T) {
	mk := func(names ...string) []map[string]any {
		out := make([]map[string]any, 0, len(names))
		for _, n := range names {
			out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": n}})
		}
		return out
	}

	cases := []struct {
		name string
		in   []map[string]any
		want bool
	}{
		{"empty", mk(), false},
		{"single official", mk("read"), false},
		{"two official", mk("read", "edit"), false},
		{"thirteen of fourteen", mk(v2OfficialToolOrder[:13]...), false},
		{"full official", mk(v2OfficialToolOrder...), true},
		{"full official reordered", mk(append([]string{}, reverseStrings(v2OfficialToolOrder)...)...), true},
		{"full official plus foreign", mk(append(append([]string{}, v2OfficialToolOrder...), "pwsh")...), false},
		{"only foreign", mk("pwsh"), false},
	}
	for _, tc := range cases {
		if got := completeOfficialToolset(tc.in, FmtChat); got != tc.want {
			t.Errorf("%s: completeOfficialToolset = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func reverseStrings(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// TestOfficialSubsetPassesThroughVerbatim 锁定 v1.0 的 subset 契约：
// 客户端工具名只要**全部落在官方集合内**，整份原样透传——不改名、不补桩。
//
// 依据：2026-11-18 shape-acceptance 在 vpngate 单节点实测 6/6 全 200
// （12 原始定义 / 13 / 14 / 12 改写 / 12 参数级 DSH 文本 / 12 工具级被判原句），
// 前提是透传前用 sanitizeRankedTools 洗掉工具级指纹短语。
//
// 注意：这是 v1.0 起**唯一**的透传判据，exact 档已移除；线上名也已是写死的
// 哈希名，所以透传时客户端工具名保持原样，不经过哈希改写。
func TestOfficialSubsetPassesThroughVerbatim(t *testing.T) {
	for _, subset := range [][]string{{"read"}, {"read", "edit"}, {"read", "edit", "write", "patch"}} {
		payload := officialSubsetPayload(t, subset)
		_, shaped := renameAndCompleteTools(payload, FmtChat)
		if shaped {
			t.Fatalf("subset %v 应当整份透传，不该走改名+补全", subset)
		}
		out := asArr(payload["tools"])
		if len(out) != len(subset) {
			t.Fatalf("subset %v: 出站 %d 个，want %d（不补桩）", subset, len(out), len(subset))
		}
		for i := range subset {
			name := entryToolName(asMap(out[i]), FmtChat)
			if name != subset[i] {
				t.Fatalf("subset %v: 出站第 %d 个名字 = %q, want %q（透传不改名）",
					subset, i, name, subset[i])
			}
		}
	}
}

// TestFullOfficialToolsetStaysVerbatim 验证完整官方集走透传：不改名、不补桩。
//
// 描述允许被脱敏（NBSP + 短语黑名单，见 shapeToolDescription），所以这里
// 只比对名字与参数 schema，不比对描述文本。2026-11-21 起 NBSP 会改写描述，
// 原先的「逐字节相同」断言已不适用。
func TestFullOfficialToolsetStaysVerbatim(t *testing.T) {
	payload := officialSubsetPayload(t, v2OfficialToolOrder)
	beforeNames := make([]string, 0, len(v2OfficialToolOrder))
	beforeParams := make([]string, 0, len(v2OfficialToolOrder))
	for _, raw := range asArr(payload["tools"]) {
		entry := asMap(raw)
		beforeNames = append(beforeNames, entryToolName(entry, FmtChat))
		fn := asMap(entry["function"])
		encoded, _ := json.Marshal(fn["parameters"])
		beforeParams = append(beforeParams, string(encoded))
	}

	_, shaped := renameAndCompleteTools(payload, FmtChat)
	if shaped {
		t.Fatal("full official set must be passed through verbatim")
	}

	out := asArr(payload["tools"])
	if len(out) != len(v2OfficialToolOrder) {
		t.Fatalf("透传不补桩：出站 %d 个，want %d", len(out), len(v2OfficialToolOrder))
	}
	for i, raw := range out {
		entry := asMap(raw)
		if name := entryToolName(entry, FmtChat); name != beforeNames[i] {
			t.Fatalf("第 %d 个名字被改：%q -> %q", i, beforeNames[i], name)
		}
		fn := asMap(entry["function"])
		encoded, _ := json.Marshal(fn["parameters"])
		if string(encoded) != beforeParams[i] {
			t.Fatalf("第 %d 个参数 schema 被改：\nbefore %s\nafter  %s", i, beforeParams[i], encoded)
		}
	}
}

func mapsFromArr(in []any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, item := range in {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
