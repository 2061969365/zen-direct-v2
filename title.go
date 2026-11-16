package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenCode v1 title prompt prefix in a user message.
const titlePromptLegacy = "generate a title for this conversation"

// OpenCode v2 title generation signatures in system / developer messages.
const (
	titlePromptV2Name    = "you name conversations so the user can find them later in a list of past sessions"
	titlePromptV2Desc    = "you generate a descriptive title for this conversation"
	titlePromptV2Auto    = "you decide whether the session title still matches the conversation"
	titlePromptV2Rule    = "reply with a title and nothing else"
	titlePromptV2RuleAlt = "reply with a title for it and nothing else"
)

// isTitleGenerationRequest reports whether payload is OpenCode's session-title
// generation call rather than agent traffic.
//
// OpenCode issues title calls with an EMPTY tool list. The upstream free-tier
// gate rejects the same call once an agent tool list is attached, so the title
// path must not inject the official tool set.
// isTitleGenerationRequest 判定这是不是 OpenCode 的会话标题请求。
//
// 判据是强签名，不是“形状像”：必须无工具、至多 3 条消息、无 assistant/tool
// 轮次，且 system 或 user 里带 OpenCode 标题提示词的签名（见 titlePromptV2*）。
//
// 为什么不按“无工具 + 短消息”兜底（opencode2api v1.4.7 是这么做的）：runner 探针、
// 普通无工具对话都长这样，误判会把一次正常对话替换成本地合成的标题——对用户
// 来说比 429 更糟（内容直接丢）。宁可漏判一个边角标题请求，也不能吃掉正常请求。
// 真正漏判时的代价只是那个标题请求可能 429，标题生成失败不影响主对话。
func isTitleGenerationRequest(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if tools := asArr(payload["tools"]); len(tools) > 0 {
		return false
	}
	if functions := asArr(payload["functions"]); len(functions) > 0 {
		return false
	}
	messages := asArr(payload["messages"])
	if len(messages) == 0 || len(messages) > 3 {
		return false
	}
	hasSystemSignature := false
	hasLegacyUserSignature := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(asStr(message["role"])))
		if role == "assistant" || role == "tool" || role == "function" {
			return false
		}
		text := strings.ToLower(strings.TrimSpace(textOfContent(message["content"])))
		if text == "" {
			continue
		}
		if role == "system" || role == "developer" {
			if strings.Contains(text, titlePromptV2Name) ||
				strings.Contains(text, titlePromptV2Desc) ||
				strings.Contains(text, titlePromptV2Auto) ||
				strings.Contains(text, titlePromptV2Rule) ||
				strings.Contains(text, titlePromptV2RuleAlt) {
				hasSystemSignature = true
			}
		} else if role == "user" {
			if strings.HasPrefix(text, titlePromptLegacy) {
				hasLegacyUserSignature = true
			}
		}
	}
	return hasSystemSignature || hasLegacyUserSignature
}

var titleStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "as": true, "at": true, "by": true,
	"for": true, "from": true, "in": true, "of": true, "on": true, "or": true,
	"the": true, "to": true, "with": true,
}

// synthesizeTitle extracts a compact title from the first user message.
func synthesizeTitle(payload map[string]any) string {
	if payload == nil {
		return "New Session"
	}
	for _, rawMessage := range asArr(payload["messages"]) {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(asStr(message["role"]))) != "user" {
			continue
		}
		text := strings.TrimSpace(textOfContent(message["content"]))
		if text == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(text), titlePromptLegacy) {
			text = text[len(titlePromptLegacy):]
		}
		text = strings.TrimSpace(strings.TrimLeft(text, " \t\r\n:：-—"))
		text = strings.Trim(text, "\"'`“”‘’")
		fields := strings.Fields(text)
		if len(fields) > 6 {
			fields = fields[:6]
		}
		for len(fields) > 0 {
			last := strings.TrimRight(fields[len(fields)-1], ".,:;!?，。：；！？-—")
			fields[len(fields)-1] = last
			if last == "" || titleStopWords[strings.ToLower(last)] {
				fields = fields[:len(fields)-1]
				continue
			}
			break
		}
		title := strings.Trim(strings.Join(fields, " "), "\"'`“”‘’")
		if runes := []rune(title); len(runes) > 40 {
			title = string(runes[:40])
		}
		if title != "" {
			return title
		}
	}
	return "New Session"
}

// writeSynthesizedTitle returns a Chat completion carrying a locally generated
// title. Title requests that hit a 429 or a transport failure must not poison
// the session pool or enter the free-limit cooldown.
func writeSynthesizedTitle(w http.ResponseWriter, model string, stream bool, payload map[string]any) {
	title := synthesizeTitle(payload)
	id := fmt.Sprintf("chatcmpl-title-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": title},
				"finish_reason": "stop",
			}},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	emit := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit(map[string]any{"role": "assistant"}, nil)
	emit(map[string]any{"content": title}, nil)
	emit(map[string]any{}, "stop")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
