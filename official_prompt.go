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
		content := message["content"]
		if carriesOpenCodePrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			if part = strings.TrimSpace(part); part != "" {
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

func carriesOpenCodePrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, opencodePromptMarker) {
			return true
		}
	}
	return false
}
