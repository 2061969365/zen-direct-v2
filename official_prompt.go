package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// officialSystemPrompt 是 OpenCode 官方 CLI 的系统提示词（2.x 正文）。
//
// 上游免费层要求 messages[0] 就是它（否则同样 429）。
//
//go:embed tools/official_system_prompt.txt
var officialSystemPrompt string

// officialPromptHead 用于判重：只要首条 system 内容以官方提示词开头，
// 就认为已经注入过（等价于 sidecar 的 80 字符前缀判断）。
const officialPromptHead = 80

// officialSkills 是 CLI 在环境块之后追加的内置 skill 列表。
const officialSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

var officialWorkDir string

func officialEnvironment(model string) string {
	if officialWorkDir == "" {
		dir := ""
		if current, err := os.Getwd(); err == nil {
			dir = filepath.Clean(strings.TrimSpace(current))
		}
		if dir == "" || dir == "." {
			dir = "/workspace"
		}
		officialWorkDir = dir
	}
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
		"  Working directory: " + officialWorkDir,
		"  Workspace root folder: " + officialWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

// officialPromptFor 拼出与官方 CLI 同形的完整系统提示词：正文 + 环境块 + skills。
func officialPromptFor(model string) string {
	base := strings.TrimSpace(officialSystemPrompt)
	if base == "" {
		return ""
	}
	return base + "\n" + officialEnvironment(model) + "\n" + officialSkills
}

// injectOfficialSystemPrompt 在 chat 形请求体前面插入官方 system 提示词。
// 返回是否真的插入（已存在或结构不认识时返回 false）。
func injectOfficialSystemPrompt(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	prompt := officialPromptFor(asStr(payload["model"]))
	if prompt == "" {
		return false
	}
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	head := prompt
	if len(head) > officialPromptHead {
		head = head[:officialPromptHead]
	}
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok {
			role, _ := first["role"].(string)
			content, _ := first["content"].(string)
			if role == "system" && strings.HasPrefix(content, head) {
				return false
			}
		}
	}
	msg := map[string]any{"role": "system", "content": prompt}
	payload["messages"] = append([]any{msg}, msgs...)
	return true
}
