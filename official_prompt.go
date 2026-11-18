package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed tools/opencode_prompt_v2.txt
var opencodePromptV2 string

// ZEN_PROMPT_FILE 可覆盖注入的官方提示词正文（对照实验用：opencode2api
// v1.4.8 用的是 8.5KB 旧版，我们抓的是 CLI 2.2.5 的 95KB 原文）。
var officialSystemPrompt = opencodePromptV2

// promptFilePrompt 读取 ZEN_PROMPT_FILE 指定的提示词正文（未设置时为空）。
func promptFilePrompt() string {
	path := strings.TrimSpace(os.Getenv("ZEN_PROMPT_FILE"))
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("ZEN_PROMPT_FILE 读取失败 %s: %v", path, err)
		return ""
	}
	return string(data)
}

// opencodePromptMarker 是官方提示词的开头短语，测试与排障用来识别官方提示词。
//
// 注意：**不能**用它判定"客户端已经带了官方提示词"从而跳过注入。客户端的
// 自研提示词里也可能引用这句话；一旦据此跳过，上游免费层门禁必 429
// （2026-11-16 实测：DSH 全挂、Pi 正常，就是这条）。注入一律无条件执行。
//
// 2026-11-19：正文从 102KB 的 DSH system prompt 换成官方 CLI 原文（8.5KB，
// 来自 opencode2api 的 opencode_prompt_v2.txt），marker 随之改为官方首句。
// 原 102KB 版本留在 tools/opencode_prompt_v2.txt.bak-dsh-102k。
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
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	// 逐字对齐官方 CLI 发出的动态尾部（system prompt 的最后一段）。
	// 上游按完整 system prompt 指纹判定客户端身份，这段的措辞/空行/顺序
	// 与官方不一致就会 429。工作目录、模型名与日期是仅有的动态量。
	//
	// 格式取自官方 CLI 原文（与 opencode2api 的 opencodeEnvironment 一致）：
	// 首行声明模型身份，随后是 <env> 块。不再插入本机专属路径等自造内容——
	// 那不是官方措辞，只会让指纹偏离。
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
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
	// 静态正文（8.5KB 官方 CLI 原文，取自 opencode2api 的
	// opencode_prompt_v2.txt）与动态尾部、skills 段逐字拼接。
	//
	// 2026-11-19 之前正文误存为 DSH 自己的 system prompt（102KB，含
	// 「记忆自动写入协议」「REA reverse engineering」等本机规则），既浪费
	// 上下文又与出站工具集语义冲突；那份留在
	// tools/opencode_prompt_v2.txt.bak-dsh-102k。
	body := opencodePromptV2
	if override := promptFilePrompt(); override != "" {
		body = override
	}
	// skills 段单独追加：官方正文不含 <available_skills>，由 CLI 运行时补。
	return body + opencodeEnvironment(model) + "\n" + opencodeSkills
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
