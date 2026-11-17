package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildClientTools 从官方定义里挑出指定名字，拼成客户端 tools 数组。
// rewrite 为 nil 时原样复制；否则对每个工具调用一次改写。
func buildClientTools(names []string, rewrite func(name string, fn map[string]any)) []map[string]any {
	byName := map[string]map[string]any{}
	for _, item := range officialToolset(FmtChat) {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n := entryToolName(entry, FmtChat); n != "" {
			byName[n] = entry
		}
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		src, ok := byName[name]
		if !ok {
			continue
		}
		raw, _ := json.Marshal(src)
		var clone map[string]any
		_ = json.Unmarshal(raw, &clone)
		if rewrite != nil {
			if fn, ok := clone["function"].(map[string]any); ok {
				rewrite(name, fn)
			}
		}
		out = append(out, clone)
	}
	return out
}

func officialNamesWithout(exclude ...string) []string {
	skip := map[string]bool{}
	for _, n := range exclude {
		skip[n] = true
	}
	names := make([]string, 0, len(v2OfficialToolOrder))
	for _, n := range v2OfficialToolOrder {
		if !skip[n] {
			names = append(names, n)
		}
	}
	return names
}

func withShapeMode(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("ZEN_SHAPE_MODE", mode)
}

// 允许客户端只发声明的子集，是这次要验的假设之一。
func TestShapeModeSubsetAcceptsOfficialSubset(t *testing.T) {
	withShapeMode(t, "subset")
	subset := buildClientTools(officialNamesWithout("patch", "experimental_image_generate"), nil)
	if len(subset) != 12 {
		t.Fatalf("期望 12 个工具，得到 %d", len(subset))
	}
	if !shapePassThrough(subset, FmtChat) {
		t.Error("subset 档下 12 个官方名应当透传")
	}
}

func TestShapeModeSubsetRejectsForeignName(t *testing.T) {
	withShapeMode(t, "subset")
	tools := buildClientTools(officialNamesWithout(), nil)[:3]
	// 塞一个非官方名进去
	foreign := map[string]any{"type": "function", "function": map[string]any{
		"name": "mcp__github_create_issue", "description": "x",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	tools = append(tools, foreign)
	if shapePassThrough(tools, FmtChat) {
		t.Error("带非官方名时不应透传")
	}
}

func TestShapeModeSubsetRejectsEmpty(t *testing.T) {
	withShapeMode(t, "subset")
	if shapePassThrough(nil, FmtChat) {
		t.Error("一个工具都没有时不应透传")
	}
}

// content 档：名字对、文本也得对。
func TestShapeModeContentAcceptsExactDefinitions(t *testing.T) {
	withShapeMode(t, "content")
	tools := buildClientTools(officialNamesWithout("patch", "experimental_image_generate"), nil)
	if !shapePassThrough(tools, FmtChat) {
		t.Error("content 档下定义逐字一致应当透传")
	}
}

func TestShapeModeContentRejectsAlteredParameterDescription(t *testing.T) {
	withShapeMode(t, "content")
	tools := buildClientTools(officialNamesWithout("patch", "experimental_image_generate"),
		func(name string, fn map[string]any) {
			if name != "read" {
				return
			}
			params, ok := fn["parameters"].(map[string]any)
			if !ok {
				return
			}
			props, ok := params["properties"].(map[string]any)
			if !ok {
				return
			}
			path, ok := props["path"].(map[string]any)
			if !ok {
				return
			}
			// 这就是 2026-11-17 实测被判过的写法：官方名 + 非官方参数级描述
			path["description"] = "File to read, resolved by the filesystem backend"
		})
	if shapePassThrough(tools, FmtChat) {
		t.Error("参数级描述被改过时 content 档不应透传（上游会判「自称官方但 schema 不是原文」）")
	}
}

func TestShapeModeContentRejectsAlteredToolDescription(t *testing.T) {
	withShapeMode(t, "content")
	tools := buildClientTools(officialNamesWithout("patch", "experimental_image_generate"),
		func(name string, fn map[string]any) {
			if name == "shell" {
				fn["description"] = "run stuff"
			}
		})
	if shapePassThrough(tools, FmtChat) {
		t.Error("工具描述被改过时 content 档不应透传")
	}
}

// exact 档是现有生产行为，不能被这次改动碰坏。
func TestShapeModeExactUnchanged(t *testing.T) {
	withShapeMode(t, "exact")
	full := buildClientTools(officialNamesWithout(), nil)
	if !shapePassThrough(full, FmtChat) {
		t.Error("集齐 14 个时 exact 档应当透传")
	}
	subset := buildClientTools(officialNamesWithout("patch", "experimental_image_generate"), nil)
	if shapePassThrough(subset, FmtChat) {
		t.Error("只有 12 个时 exact 档不应透传（现有行为）")
	}
}

func TestShapeModeDefaultsToExact(t *testing.T) {
	t.Setenv("ZEN_SHAPE_MODE", "")
	if got := shapeMode(); got != "exact" {
		t.Errorf("未设置时应当回落到 exact，得到 %q", got)
	}
	t.Setenv("ZEN_SHAPE_MODE", "SUBSET")
	if got := shapeMode(); got != "subset" {
		t.Errorf("应当大小写不敏感，得到 %q", got)
	}
}

// subset 透传前必须洗掉工具描述里的指纹短语。
// 依据：真实上游实测（run 44129660858）官方名 + 工具级被判原句 → 429；
// 官方名 + 参数级文本 → 200。所以只洗工具级，且只在非 exact 档洗。
func TestShapeModeSubsetSanitizesBannedDescriptions(t *testing.T) {
	t.Setenv("ZEN_SHAPE_MODE", "subset")
	names := officialNamesWithout("patch", "experimental_image_generate")
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, name := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		desc := "Tool " + name + "."
		if name == "read" {
			desc = "Read a UTF-8 text file and return line-numbered content."
		}
		sb.WriteString(`{"type":"function","function":{"name":"` + name + `","description":"` + desc + `","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`)
	}
	sb.WriteString(`]}`)

	in := mustJSON(t, sb.String())
	up, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	if renames.Count() != 0 {
		t.Fatalf("subset 档下不该改名，实际改了 %d 个", renames.Count())
	}
	entries := asArr(up["tools"])
	if len(entries) != len(names) {
		t.Fatalf("subset 档下不该追加官方集：出站 %d 个（应为 %d）", len(entries), len(names))
	}
	for _, raw := range entries {
		fn := asMap(asMap(raw)["function"])
		if asStr(fn["name"]) != "read" {
			continue
		}
		desc := asStr(fn["description"])
		if strings.Contains(desc, "line-numbered content") {
			t.Fatalf("指纹短语没洗掉：%q", desc)
		}
		if !strings.Contains(desc, "line-numbered output") {
			t.Fatalf("应当换成中性文本，实际 %q", desc)
		}
	}
}

// exact 档不能跟着改：那是「客户端声明了完整官方集」，文本就是官方原文。
func TestShapeModeExactKeepsOfficialTextVerbatim(t *testing.T) {
	t.Setenv("ZEN_SHAPE_MODE", "exact")
	names := officialNamesWithout()
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, name := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		desc := "Tool " + name + "."
		if name == "read" {
			desc = "Read a UTF-8 text file and return line-numbered content."
		}
		sb.WriteString(`{"type":"function","function":{"name":"` + name + `","description":"` + desc + `","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`)
	}
	sb.WriteString(`]}`)

	in := mustJSON(t, sb.String())
	up, _, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range asArr(up["tools"]) {
		fn := asMap(asMap(raw)["function"])
		if asStr(fn["name"]) != "read" {
			continue
		}
		if !strings.Contains(asStr(fn["description"]), "line-numbered content") {
			t.Fatalf("exact 档不应脱敏，实际 %q", asStr(fn["description"]))
		}
	}
}
