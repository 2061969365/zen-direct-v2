package main

import (
	"strings"
	"testing"
)

func TestOfficialPromptInjected(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if !injectOfficialSystemPrompt(payload) {
		t.Fatal("首条不是官方提示词时应注入")
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("注入后应有两段消息，得到 %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("首条应为 system，得到 %v", first["role"])
	}
	if !strings.HasPrefix(first["content"].(string), "You are opencode, an interactive CLI tool") {
		t.Errorf("注入内容不像官方提示词：%.80q", first["content"])
	}
}

// 已注入过（前缀匹配）就不要再插。
func TestOfficialPromptIdempotent(t *testing.T) {
	prompt := strings.TrimSpace(officialSystemPrompt)
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": prompt + "\n\nextra"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if injectOfficialSystemPrompt(payload) {
		t.Fatal("已含官方提示词时不该重复注入")
	}
	if n := len(payload["messages"].([]any)); n != 2 {
		t.Errorf("消息数不该变，得到 %d", n)
	}
}

// 结构不认识（没有 messages 或类型不对）时安静返回 false。
func TestOfficialPromptShapeGuard(t *testing.T) {
	if injectOfficialSystemPrompt(nil) {
		t.Error("nil payload 应返回 false")
	}
	if injectOfficialSystemPrompt(map[string]any{"messages": "nope"}) {
		t.Error("messages 类型不对应返回 false")
	}
	if injectOfficialSystemPrompt(map[string]any{}) {
		t.Error("没有 messages 应返回 false")
	}
}

// 空 messages 也要能注入（首轮请求）。
func TestOfficialPromptEmptyHistory(t *testing.T) {
	payload := map[string]any{"messages": []any{}}
	if !injectOfficialSystemPrompt(payload) {
		t.Fatal("空 messages 应注入")
	}
	if n := len(payload["messages"].([]any)); n != 1 {
		t.Errorf("应只剩注入的一条，得到 %d", n)
	}
}
