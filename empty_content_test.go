package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func chatMessages(t *testing.T, raw string) []any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	normalizeEmptyMessageContent(payload)
	messages, _ := payload["messages"].([]any)
	return messages
}

func TestEmptyAssistantWithoutToolCallsIsDropped(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": null},
			{"role": "user", "content": "again"}
		]
	}`)
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3: %v", len(messages), messages)
	}
	if role := asStr(messages[2].(map[string]any)["role"]); role != "user" {
		t.Errorf("message 2 role = %q, want user", role)
	}
}

func TestEmptyUserContentBecomesEmptyString(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "user", "content": null},
			{"role": "assistant", "content": "ok"}
		]
	}`)
	message := messages[0].(map[string]any)
	content, ok := message["content"].(string)
	if !ok || content != "" {
		t.Errorf("content = %#v, want empty string", message["content"])
	}
	if _, exists := message["content"]; !exists {
		t.Error("content key was removed")
	}
}

func TestAssistantWithToolCallsIsKept(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "done"}
		]
	}`)
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	if messages[0].(map[string]any)["content"] != nil {
		t.Errorf("assistant content was rewritten: %#v", messages[0].(map[string]any)["content"])
	}
}

func TestAssistantWithFunctionCallIsKept(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "assistant", "content": null, "function_call": {"name": "f", "arguments": "{}"}}
		]
	}`)
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
}

func TestAssistantWithReasoningOnlyBecomesEmptyString(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "reasoning_content": "thinking"}
		]
	}`)
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	message := messages[1].(map[string]any)
	if content, _ := message["content"].(string); content != "" {
		t.Errorf("content = %#v, want empty string", message["content"])
	}
	if message["reasoning_content"] != "thinking" {
		t.Errorf("reasoning_content was dropped: %#v", message["reasoning_content"])
	}
}

func TestNonChatRolesAndStructuredContentAreUntouched(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "system", "content": null},
			{"role": "tool", "tool_call_id": "x", "content": null},
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		]
	}`)
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(messages))
	}
	if messages[0].(map[string]any)["content"] != nil {
		t.Error("system content was rewritten")
	}
	if messages[1].(map[string]any)["content"] != nil {
		t.Error("tool content was rewritten")
	}
	parts, ok := messages[2].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Errorf("user structured content was rewritten: %#v", messages[2].(map[string]any)["content"])
	}
}

func TestAllEmptyMessagesKeepsOneWithEmptyString(t *testing.T) {
	messages := chatMessages(t, `{
		"messages": [
			{"role": "assistant", "content": null},
			{"role": "assistant", "content": null}
		]
	}`)
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if content, _ := messages[0].(map[string]any)["content"].(string); content != "" {
		t.Errorf("content = %#v, want empty string", messages[0].(map[string]any)["content"])
	}
}

func TestPayloadWithoutMessagesIsUntouched(t *testing.T) {
	payload := map[string]any{"model": "x"}
	before := map[string]any{"model": "x"}
	normalizeEmptyMessageContent(payload)
	if !reflect.DeepEqual(payload, before) {
		t.Errorf("payload changed: %#v", payload)
	}
}
