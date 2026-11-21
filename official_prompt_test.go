package main

import (
	"strings"
	"testing"
)

// 首条不是官方提示词的请求要注入。
func TestOfficialPromptInjected(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if !injectOfficialSystemPrompt(payload, clientEnvironment{}) {
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
	if !strings.HasPrefix(first["content"].(string), opencodePromptMarker) {
		t.Errorf("注入内容不像官方提示词：%.80q", first["content"])
	}
}

// 2026-11-16 事故：客户端自带"官方提示词副本"时，旧逻辑整单跳过注入，出站比正确
// 形状少了 env 段与 skills 段，上游免费层门禁回 429。
//
// 因此注入必须无条件执行：即使客户端已经带了一份官方提示词，也要由网关重新拼出
// messages[0]，同时把客户端那份副本剥掉，避免两倍长度。
func TestOfficialPromptAlwaysInjected(t *testing.T) {
	body := strings.TrimSpace(opencodePromptV2)
	cases := map[string]string{
		"完整官方正文":   body,
		"官方正文+自定义": body + "\n\nBe terse.",
	}
	for name, sys := range cases {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{
				"model": "deepseek-v4.1-flash-free",
				"messages": []any{
					map[string]any{"role": "system", "content": sys},
					map[string]any{"role": "user", "content": "hi"},
				},
			}
			if !injectOfficialSystemPrompt(payload, clientEnvironment{}) {
				t.Fatal("客户端自带官方提示词时仍须重新注入（跳过即 429）")
			}
			msgs := payload["messages"].([]any)
			if len(msgs) != 2 {
				t.Fatalf("应注成 1 条 system + 1 条 user，得到 %d 条", len(msgs))
			}
			got, _ := msgs[0].(map[string]any)["content"].(string)
			if !strings.HasPrefix(got, opencodePromptMarker) {
				t.Fatalf("注入内容必须以官方提示词开头，得到 %.80q", got)
			}
			if strings.Count(got, body) != 1 {
				t.Errorf("官方正文应恰好出现一次（副本要剥掉），实际出现 %d 次", strings.Count(got, body))
			}
			if strings.Contains(got, opencodeSkills) && strings.Count(got, opencodeSkills) != 1 {
				t.Error("skills 段应恰好出现一次")
			}
		})
	}
}

// 客户端只提到开头短语时，内容要原样拼在官方提示词之后，一个字都不能丢。
func TestOfficialPromptMarkerAloneStillInjects(t *testing.T) {
	cases := map[string]string{
		"只有开头短语": opencodePromptMarker,
		"埋在长文里":  "Some long agent preamble.\n\n" + opencodePromptMarker + " that helps users.\n\nMore rules follow.",
	}
	for name, sys := range cases {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{
				"model": "deepseek-v4.1-flash-free",
				"messages": []any{
					map[string]any{"role": "system", "content": sys},
					map[string]any{"role": "user", "content": "hi"},
				},
			}
			if !injectOfficialSystemPrompt(payload, clientEnvironment{}) {
				t.Fatal("只提到开头短语的客户端提示词仍须注入官方提示词")
			}
			msgs := payload["messages"].([]any)
			got, _ := msgs[0].(map[string]any)["content"].(string)
			if !strings.HasPrefix(got, opencodePromptMarker) {
				t.Fatalf("注入内容必须以官方提示词开头，得到 %.80q", got)
			}
			if !strings.Contains(got, sys) {
				t.Error("客户端原始提示词应拼在官方提示词之后，不能丢")
			}
		})
	}
}

// 结构不认识（没有 messages 或类型不对）时安静返回 false。
func TestOfficialPromptShapeGuard(t *testing.T) {
	if injectOfficialSystemPrompt(nil, clientEnvironment{}) {
		t.Error("nil payload 应返回 false")
	}
	if injectOfficialSystemPrompt(map[string]any{"messages": "nope"}, clientEnvironment{}) {
		t.Error("messages 类型不对应返回 false")
	}
	if injectOfficialSystemPrompt(map[string]any{}, clientEnvironment{}) {
		t.Error("没有 messages 应返回 false")
	}
}

// 空 messages 也要能注入（首轮请求）。
func TestOfficialPromptEmptyHistory(t *testing.T) {
	payload := map[string]any{"messages": []any{}}
	if !injectOfficialSystemPrompt(payload, clientEnvironment{}) {
		t.Fatal("空 messages 应注入")
	}
	if n := len(payload["messages"].([]any)); n != 1 {
		t.Errorf("应只剩注入的一条，得到 %d", n)
	}
}

// content 是分块数组时也要工作：既不能漏剥副本，也不能把自定义指令丢掉。
func TestOfficialPromptPartsArray(t *testing.T) {
	body := strings.TrimSpace(opencodePromptV2)
	payload := map[string]any{
		"model": "deepseek-v4.1-flash-free",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": body[:4000]},
				map[string]any{"type": "text", "text": body[4000:] + "\n\nCustom rule."},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if !injectOfficialSystemPrompt(payload, clientEnvironment{}) {
		t.Fatal("分块 system 也要注入")
	}
	got, _ := payload["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Count(got, body) != 1 {
		t.Errorf("分块副本应被剥成一份，实际 %d 份", strings.Count(got, body))
	}
	if !strings.Contains(got, "Custom rule.") {
		t.Error("自定义指令不能丢")
	}
}

// stripOfficialPrompt 的边界：没带官方正文时原样返回，不能误删客户端的字。
func TestStripOfficialPromptNoop(t *testing.T) {
	for _, in := range []string{
		"",
		"You are DeepSeek Harness.",
		opencodePromptMarker,
	} {
		if got := stripOfficialPrompt(in); got != strings.TrimSpace(in) {
			t.Errorf("stripOfficialPrompt(%.40q) = %.40q，应原样返回", in, got)
		}
	}
}
