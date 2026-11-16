package main

import (
	"strings"
	"testing"
)

// chatTool 造一条 chat 形状的客户端工具定义。
func chatTool(name, description string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"arg1": map[string]any{"type": "string"}},
				"required":   []any{"arg1"},
			},
		},
	}
}

// 上游免费层按工具描述里的固定短语判定"非 OpenCode 客户端"。
//
// 2026-11-16 单节点实测（zen3.uflash.ccwu.cc，参数 schema、工具名、其余字段完全一致）：
//   - "Read a UTF-8 text file and return line-numbered content."  → 429
//   - "Read a text file and return line-numbered content."        → 200
//   - "Read a UTF-8 text file and return line-numbered"           → 200
//   - "Read a UTF-8 text file and return numbered content."       → 200
//   - "Process UTF-8 encoding."                                   → 200
//   - "Return line-numbered content."                             → 200
//   - "in the session skill catalog"                              → 429
//   - "in the catalog" / "in the available skills"                → 200
//
// 处理方式是把命中的短语就地替换掉，而不是整段丢弃描述——描述是模型判断工具用途的
// 唯一提示，抹掉会让模型不会用这个工具。
func TestSanitizeToolDescriptionRewritesPhrases(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantReplace bool
		wantAbsent  []string
	}{
		{
			name:        "DSH read",
			description: "Read a UTF-8 text file and return line-numbered content.",
			wantReplace: true,
			wantAbsent:  []string{"UTF-8 text file", "line-numbered content"},
		},
		{
			name:        "DSH edit",
			description: "Edit an existing UTF-8 text file by replacing literal text.",
			wantReplace: true,
			wantAbsent:  []string{"UTF-8 text file"},
		},
		{
			name:        "DSH write",
			description: "Create or fully replace a UTF-8 text file.",
			wantReplace: true,
			wantAbsent:  []string{"UTF-8 text file"},
		},
		{
			name:        "DSH skill",
			description: "Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog.",
			wantReplace: true,
			wantAbsent:  []string{"session skill catalog"},
		},
		{
			name:        "safe utf8",
			description: "Process UTF-8 encoding.",
			wantReplace: false,
		},
		{
			name:        "safe line-numbered",
			description: "Return line-numbered content.",
			wantReplace: false,
		},
		{
			name:        "safe official read",
			description: "Read the contents of a file or directory. Supports text files, images, and PDFs.",
			wantReplace: false,
		},
		{
			name:        "empty",
			description: "",
			wantReplace: false,
		},
		// 上游对指纹的大小写不敏感：小写 utf-8 同样触发 429，大写 Session
		// Skill Catalog 同样触发。精确匹配会漏掉客户端换大小写的写法。
		{
			name:        "lowercase utf-8",
			description: "Read a utf-8 text file and return line-numbered content.",
			wantReplace: true,
			wantAbsent:  []string{"utf-8 text file", "line-numbered content"},
		},
		{
			name:        "uppercase skill catalog",
			description: "Call it before acting on a task in the Session Skill Catalog.",
			wantReplace: true,
			wantAbsent:  []string{"Session Skill Catalog"},
		},
		{
			name:        "mixed case utf-8",
			description: "Edit an existing Utf-8 Text File by replacing literal text.",
			wantReplace: true,
			wantAbsent:  []string{"Utf-8 Text File"},
		},
		// 去掉连字符的 "UTF8" 实测不触发上游（200），不该误替换。
		{
			name:        "no hyphen utf8",
			description: "Read a UTF8 text file and return line-numbered content.",
			wantReplace: false,
		},
		{
			name:        "hyphenated skill catalog",
			description: "in the session-skill-catalog",
			wantReplace: false,
		},
	}
	for _, c := range cases {
		got, replaced := sanitizeToolDescription("clint_abc123", c.description)
		if replaced != c.wantReplace {
			t.Errorf("%s: replaced=%v, want %v（描述 %q）", c.name, replaced, c.wantReplace, c.description)
		}
		for _, absent := range c.wantAbsent {
			if strings.Contains(got, absent) {
				t.Errorf("%s: 处理后仍含指纹 %q：%q", c.name, absent, got)
			}
		}
		// 替换是就地改写，不能把描述清空——模型要靠它理解工具用途。
		if got == "" && c.description != "" {
			t.Errorf("%s: 处理后描述为空，模型将失去该工具的唯一提示", c.name)
		}
	}
}

// 整形后的出站请求体里不能残留封禁短语。这条直接钉住 DSH 429 的回归：
// 真实 43 工具请求体在修复前稳定 429，清理 read/edit/write/skill 后转 200。
//
// 注意工具名用非官方名（dsh_read…）：官方工具名现在走“原样透传”分支，
// 描述按官方原文出站、不做脱敏（见 renameAndCompleteTools 的说明），
// 这条用例要覆盖的是第三方客户端工具那条路径。
func TestOutboundHasNoBannedPhrases(t *testing.T) {
	tools := []any{
		chatTool("dsh_read", "Read a UTF-8 text file and return line-numbered content."),
		chatTool("dsh_edit", "Edit an existing UTF-8 text file by replacing literal text."),
		chatTool("dsh_write", "Create or fully replace a UTF-8 text file."),
		chatTool("dsh_skill", "Load the full instructions for a skill in the session skill catalog."),
		chatTool("grep", "Search file contents using regular expression syntax."),
	}
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"tools":    tools,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	entries, _ := up["tools"].([]any)
	for _, rule := range bannedDescriptionPhrases {
		for _, item := range entries {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := entry["function"].(map[string]any)
			if !ok {
				continue
			}
			description, _ := fn["description"].(string)
			if strings.Contains(description, rule.phrase) {
				t.Errorf("出站工具 %v 的描述仍含封禁短语 %q：%s",
					fn["name"], rule.phrase, description)
			}
		}
	}
}

// 官方工具集的描述是上游认可的原文，不能被脱敏误伤。
func TestOfficialDescriptionsSurvive(t *testing.T) {
	for _, name := range v2OfficialToolOrder {
		rendered := renderOfficialTool(FmtChat, name)
		fn, ok := rendered["function"].(map[string]any)
		if !ok {
			t.Fatalf("官方工具 %q 渲染异常", name)
		}
		description, _ := fn["description"].(string)
		if cleaned, replaced := sanitizeToolDescription(name, description); replaced {
			t.Errorf("官方工具 %q 的描述被误脱敏（%q → %q）", name, description, cleaned)
		}
	}
}
