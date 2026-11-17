package main

import (
	"encoding/json"
	"strings"
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

// TestOfficialSubsetFallsThroughToRename 锁 exact 档的行为：官方名子集必须
// 改名+补全（2026-11-17 实测「官方名 + 非官方文本」整份透传会 429）。
// 默认档改成 subset 之后这条仍要能通过 ZEN_SHAPE_MODE=exact 复现。
func TestOfficialSubsetFallsThroughToRename(t *testing.T) {
	t.Setenv("ZEN_SHAPE_MODE", "exact")
	for _, subset := range [][]string{{"read"}, {"read", "edit"}, {"read", "edit", "write", "patch"}} {
		payload := officialSubsetPayload(t, subset)
		_, shaped := renameAndCompleteTools(payload, FmtChat)
		if !shaped {
			t.Fatalf("subset %v must be shaped, not passed through verbatim", subset)
		}
		out := asArr(payload["tools"])
		if len(out) != len(subset)+len(v2OfficialToolOrder) {
			t.Fatalf("subset %v: outbound tools = %d, want %d", subset, len(out), len(subset)+len(v2OfficialToolOrder))
		}
		for i := range subset {
			name := entryToolName(asMap(out[i]), FmtChat)
			if !strings.HasPrefix(name, ClientToolPrefix()) {
				t.Fatalf("subset %v: outbound tool %d = %q, want %q prefix", subset, i, name, ClientToolPrefix())
			}
		}
		if allOfficialToolNames(mapsFromArr(out), FmtChat) {
			t.Fatalf("subset %v: outbound must not look like a pure official set", subset)
		}
	}
}

func TestFullOfficialToolsetStaysVerbatim(t *testing.T) {
	payload := officialSubsetPayload(t, v2OfficialToolOrder)
	before, err := json.Marshal(payload["tools"])
	if err != nil {
		t.Fatal(err)
	}
	_, shaped := renameAndCompleteTools(payload, FmtChat)
	if shaped {
		t.Fatal("full official set must be passed through verbatim")
	}
	after, err := json.Marshal(payload["tools"])
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("full official set was modified:\nbefore %s\nafter  %s", before, after)
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
