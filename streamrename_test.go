package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func testRenames(pairs ...string) *ToolRenames {
	renames := NewToolRenames()
	for i := 0; i+1 < len(pairs); i += 2 {
		renames.Add(pairs[i], pairs[i+1])
	}
	return renames
}

func chatDeltaFrame(t *testing.T, delta map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(payload) + "\n\n"
}

func toolCallDelta(t *testing.T, function map[string]any) string {
	t.Helper()
	return chatDeltaFrame(t, map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "function": function}},
	})
}

func readRenamed(t *testing.T, input string, protocol Format, renames *ToolRenames) string {
	t.Helper()
	out, err := io.ReadAll(NewRenameStreamReader(strings.NewReader(input), protocol, renames))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestStreamRenameSplitName(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	input := toolCallDelta(t, map[string]any{"name": wire[:7]}) +
		toolCallDelta(t, map[string]any{"name": wire[7:], "arguments": `{"file_path":`}) +
		toolCallDelta(t, map[string]any{"arguments": `"x"}`}) +
		chatDeltaFrame(t, map[string]any{}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	if strings.Contains(out, ClientToolPrefix()) {
		t.Fatalf("wire 名泄漏: %s", out)
	}
	if !strings.Contains(out, `"name":"read"`) {
		t.Fatalf("未还原为 read: %s", out)
	}
	if !strings.Contains(out, "file_path") || !strings.Contains(out, `"arguments"`) {
		t.Fatalf("arguments 丢失: %s", out)
	}
}

func TestStreamRenameHoldsArgumentsUntilNameCompletes(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	input := toolCallDelta(t, map[string]any{"name": wire[:7]}) +
		toolCallDelta(t, map[string]any{"arguments": `{"file_path":`}) +
		toolCallDelta(t, map[string]any{"name": wire[7:], "arguments": `"x"}`}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	t.Logf("input=%q", input)
	t.Logf("out=%q", out)
	if strings.Contains(out, ClientToolPrefix()) {
		t.Fatalf("wire 名泄漏: %s", out)
	}
	if !strings.Contains(out, `"name":"read"`) {
		t.Fatalf("分片名未还原: %s", out)
	}
	if !strings.Contains(out, "file_path") || !strings.Contains(out, `"arguments"`) {
		t.Fatalf("暂存的 arguments 未合并: %s", out)
	}
}

func TestStreamRenameDoneFlushesPartial(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	input := toolCallDelta(t, map[string]any{"name": wire[:7], "arguments": `{}`}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	if !strings.Contains(out, "arguments") {
		t.Fatalf("DONE 前未冲出暂存参数: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("DONE 丢失: %s", out)
	}
}

func TestStreamRenameEOF(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	input := toolCallDelta(t, map[string]any{"name": wire[:7], "arguments": `{}`})
	out := readRenamed(t, input, FmtChat, renames)
	if !strings.Contains(out, "arguments") {
		t.Fatalf("EOF 前未冲出暂存参数: %s", out)
	}
}

func TestStreamRenameConcurrentDifferentMaps(t *testing.T) {
	readWire := ClientToolWireName("read", nil)
	writeWire := ClientToolWireName("write", nil)
	makeInput := func(wire string) string {
		return toolCallDelta(t, map[string]any{"name": wire[:7], "arguments": `{}`}) +
			toolCallDelta(t, map[string]any{"name": wire[7:]}) +
			"data: [DONE]\n\n"
	}
	var wg sync.WaitGroup
	results := make([]string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = readRenamed(t, makeInput(readWire), FmtChat, testRenames("read", readWire))
	}()
	go func() {
		defer wg.Done()
		results[1] = readRenamed(t, makeInput(writeWire), FmtChat, testRenames("write", writeWire))
	}()
	wg.Wait()
	if !strings.Contains(results[0], `"name":"read"`) || strings.Contains(results[0], "clint_") {
		t.Fatalf("read 流串扰: %s", results[0])
	}
	if !strings.Contains(results[1], `"name":"write"`) || strings.Contains(results[1], "clint_") {
		t.Fatalf("write 流串扰: %s", results[1])
	}
}

func TestResolveFuzzyTypo(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	typo := ClientToolPrefix() + "f" + strings.TrimPrefix(wire, ClientToolPrefix())
	client, ok := renames.ResolveFuzzy(typo)
	if !ok || client != "read" {
		t.Fatalf("编辑距离 1 未修复: %q %v", client, ok)
	}
	if _, ok := renames.ResolveFuzzy(ClientToolPrefix() + "deadbeef0000"); ok {
		t.Fatal("不相关前缀名字不应被模糊匹配")
	}
	if _, ok := renames.ResolveFuzzy("unknown_plain"); ok {
		t.Fatal("非前缀明文不应被模糊匹配")
	}
}

func TestRenameResponseToolFuzzy(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	typo := ClientToolPrefix() + "f" + strings.TrimPrefix(wire, ClientToolPrefix())
	line := []byte(`{"name":"` + typo + `","arguments":"{}"}`)
	got := string(RenameResponseTool(line, renames))
	if !strings.Contains(got, `"name":"read"`) || strings.Contains(got, ClientToolPrefix()) {
		t.Fatalf("响应模糊还原失败: %s", got)
	}
}

func TestStreamRenameFuzzy(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	typo := ClientToolPrefix() + "f" + strings.TrimPrefix(wire, ClientToolPrefix())
	input := toolCallDelta(t, map[string]any{"name": typo, "arguments": `{}`}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	if !strings.Contains(out, `"name":"read"`) || strings.Contains(out, ClientToolPrefix()) {
		t.Fatalf("流式模糊还原失败: %s", out)
	}
}

func TestStreamRenameResponsesExact(t *testing.T) {
	wire := ClientToolWireName("read", nil)
	renames := testRenames("read", wire)
	frame, _ := json.Marshal(map[string]any{"name": wire})
	input := fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", frame)
	out := readRenamed(t, input, FmtResponses, renames)
	if !strings.Contains(out, `"name":"read"`) || strings.Contains(out, ClientToolPrefix()) {
		t.Fatalf("Responses 精确还原失败: %s", out)
	}
}
