package main

import (
	"strings"
	"testing"
)

func TestHoistChatToolImagesNoImage(t *testing.T) {
	in := mustJSON(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"plain text"}]}`)
	before := canon(in["messages"])
	if err := hoistChatToolImages(in); err != nil {
		t.Fatal(err)
	}
	if after := canon(in["messages"]); after != before {
		t.Fatalf("无图不应改动: %s", after)
	}
}

func TestHoistChatToolImagesMovesImageToUser(t *testing.T) {
	img := "data:image/png;base64,AAAA"
	in := mustJSON(t, `{"model":"m","messages":[
		{"role":"user","content":"look"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[
			{"type":"text","text":"shot"},
			{"type":"image_url","image_url":{"url":"`+img+`"}}]},
		{"role":"user","content":"what color"}]}`)
	if err := hoistChatToolImages(in); err != nil {
		t.Fatal(err)
	}
	msgs := asArr(in["messages"])
	if len(msgs) != 5 {
		t.Fatalf("应 5 条消息，得 %d: %s", len(msgs), canon(msgs))
	}
	tool := asMap(msgs[2])
	if got := asStr(tool["content"]); !strings.Contains(got, "[tool_image call_id=\"c1\" index=1]") || strings.Contains(got, "data:") {
		t.Fatalf("tool 消息应只留文本+标记: %v", tool["content"])
	}
	if asStr(tool["tool_call_id"]) != "c1" {
		t.Fatalf("tool_call_id 丢失: %v", tool)
	}
	att := asMap(msgs[3])
	if asStr(att["role"]) != "user" {
		t.Fatalf("图片应进 user 消息: %v", att)
	}
	parts := asArr(att["content"])
	if len(parts) != 2 {
		t.Fatalf("应 标记+图片 两块，得 %d", len(parts))
	}
	if !strings.Contains(asStr(asMap(parts[0])["text"]), "[tool_image call_id=\"c1\"") {
		t.Fatalf("缺标记块: %v", parts[0])
	}
	if got := asStr(asMap(asMap(parts[1])["image_url"])["url"]); got != img {
		t.Fatalf("图片应原样保留，得 %.40s", got)
	}
	if asStr(asMap(msgs[4])["content"]) != "what color" {
		t.Fatalf("用户消息顺序错: %v", msgs[4])
	}
}

func TestHoistChatToolImagesMergesGroup(t *testing.T) {
	in := mustJSON(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[{"type":"image_url","image_url":"data:image/png;base64,AA"}]},
		{"role":"tool","tool_call_id":"c2","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BB"}}]}]}`)
	if err := hoistChatToolImages(in); err != nil {
		t.Fatal(err)
	}
	msgs := asArr(in["messages"])
	if len(msgs) != 5 {
		t.Fatalf("应 assistant+2tool+2user = 5 条，得 %d: %s", len(msgs), canon(msgs))
	}
	if asStr(asMap(msgs[0])["role"]) != "assistant" {
		t.Fatalf("顺序错: %s", canon(msgs))
	}
	if asStr(asMap(msgs[1])["role"]) != "tool" || asStr(asMap(msgs[2])["role"]) != "tool" {
		t.Fatalf("tool 消息应连续: %s", canon(msgs))
	}
	for i := 3; i < 5; i++ {
		m := asMap(msgs[i])
		if asStr(m["role"]) != "user" {
			t.Fatalf("第 %d 条应为 user: %s", i, canon(m))
		}
		parts := asArr(m["content"])
		if len(parts) != 2 || !strings.Contains(asStr(asMap(parts[0])["text"]), "call_id=\"c") {
			t.Fatalf("图片消息缺标记: %s", canon(m))
		}
	}
}

func TestHoistChatToolImagesRejectsBadDataURI(t *testing.T) {
	in := mustJSON(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[
			{"type":"image_url","image_url":{"url":"data:text/plain,AAAA"}}]}]}`)
	if err := hoistChatToolImages(in); err == nil {
		t.Fatal("非图片 data URI 应报错")
	}
}

func TestBuildUpstreamHoistsChatToolImage(t *testing.T) {
	in := mustJSON(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[
			{"type":"text","text":"shot"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	out, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	msgs := asArr(out["messages"])
	var sawUser bool
	for _, raw := range msgs {
		m := asMap(raw)
		if asStr(m["role"]) == "tool" && strings.Contains(canon(m["content"]), "data:") {
			t.Fatalf("tool 消息不应再带 data URL: %s", canon(m["content"]))
		}
		if asStr(m["role"]) == "user" && strings.Contains(canon(m["content"]), "data:image/png") {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatalf("未抬升出 user 图片消息: %s", canon(msgs))
	}
}
