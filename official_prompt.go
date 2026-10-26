package main

import (
	_ "embed"
	"strings"
)

// officialSystemPrompt 是 OpenCode 官方 CLI 的系统提示词。
//
// 上游免费层要求 messages[0] 就是它（否则同样 429），sidecar 时代由
// tools/zen_sidecar.js 注入；现在搬进网关，bun 只负责传输层。
//
//go:embed tools/official_system_prompt.txt
var officialSystemPrompt string

// officialPromptHead 用于判重：只要首条 system 内容以官方提示词开头，
// 就认为已经注入过（等价于 sidecar 的 80 字符前缀判断）。
const officialPromptHead = 80

// injectOfficialSystemPrompt 在 chat 形请求体前面插入官方 system 提示词。
// 返回是否真的插入（已存在或结构不认识时返回 false）。
func injectOfficialSystemPrompt(payload map[string]any) bool {
	prompt := strings.TrimSpace(officialSystemPrompt)
	if prompt == "" || payload == nil {
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
