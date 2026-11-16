package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed tools/opencode_prompt_v2.txt
var opencodePromptV2 string

var officialSystemPrompt = opencodePromptV2

// opencodePromptMarker 是官方提示词的开头短语，测试与排障用来识别官方提示词。
//
// 注意：**不能**用它判定"客户端已经带了官方提示词"从而跳过注入。DSH / Pi 等
// 客户端的自研提示词里也会引用这句话；一旦据此跳过，上游免费层门禁必 429
// （2026-11-16 实测：DSH 全挂、Pi 正常，就是这条）。注入一律无条件执行。
const opencodePromptMarker = "You are opencode, an interactive CLI tool"

const opencodeSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

var (
	opencodeEnvOnce sync.Once
	opencodeWorkDir string
)

func opencodeEnvironment(model string) string {
	opencodeEnvOnce.Do(func() {
		directory := ""
		if current, err := os.Getwd(); err == nil {
			directory = filepath.Clean(strings.TrimSpace(current))
		}
		if directory == "" || directory == "." {
			directory = "/workspace"
		}
		opencodeWorkDir = directory
	})
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + opencodeWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

func opencodeSystemPrompt(model string) string {
	return opencodePromptV2 + opencodeEnvironment(model) + "\n" + opencodeSkills
}

func injectOfficialSystemPrompt(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	model, _ := payload["model"].(string)
	prompt := opencodeSystemPrompt(model)
	return injectChatPrompt(payload, prompt)
}

// injectChatPrompt 把官方提示词装成 messages[0]。
//
// 设计要求：**无条件注入**，永不"因为客户端已经带了就跳过"。
// 2026-11-16 实测：客户端自带一份官方提示词正文时若跳过注入，出站比正确形状
// 少约 1.2KB（缺 env 段与 skills 段），上游免费层门禁同样回 429。也就是说门禁
// 认的是"网关拼出来的完整官方提示词"，不是"正文长得一样"。
//
// 为避免重复，客户端内容里已经包含的官方提示词正文会在拼接前剥掉（见
// stripOfficialPrompt），只保留它自己的额外指令。
func injectChatPrompt(payload map[string]any, prompt string) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		for _, part := range textFragments(message["content"]) {
			if part = stripOfficialPrompt(strings.TrimSpace(part)); part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	system := prompt
	if joined := strings.Join(instructions, "\n"); joined != "" {
		system += "\n\n" + joined
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// stripOfficialPrompt 从客户端内容里剥掉已经复制过来的官方提示词正文与 skills
// 段，避免"官方提示词 + 客户端的官方提示词副本"拼成两倍长度（浪费 token，也可能
// 撞上游长度限制）。剥完剩下的自定义指令仍然保留。
//
// 只剥常量部分：env 段带运行时工作目录/模型名，逐字比对不稳，且只有几百字节，
// 留一份副本无害。
func stripOfficialPrompt(text string) string {
	stripped := text
	if body := strings.TrimSpace(opencodePromptV2); body != "" {
		stripped = strings.ReplaceAll(stripped, body, "")
	}
	stripped = strings.ReplaceAll(stripped, opencodeSkills, "")
	return strings.TrimSpace(stripped)
}

func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}
