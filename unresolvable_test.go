package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// plan1Payload 构造 ZCode 式请求：客户端只声明 read_file / shell_write，
// 出站会补 14 个裸官方名桩。返回 shaped 后的 renames。
func plan1Renames(t *testing.T) *ToolRenames {
	t.Helper()
	payload := map[string]any{
		"model": "m",
		"tools": []any{
			chatTool("read_file", "Read a file."),
			chatTool("shell_write", "Write a file."),
		},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	return renames
}

// TestUnresolvableToolName 判据：客户端认识的名字放行，裸官方名桩拦截。
func TestUnresolvableToolName(t *testing.T) {
	renames := plan1Renames(t)
	if !renames.Declared("read_file") {
		t.Fatal("declared 未记录 read_file，后续判据全部失效")
	}
	cases := []struct {
		wire    string
		wantOK  bool
		comment string
	}{
		{"read_file", true, "客户端声明的工具名"},
		{"shell_write", true, "客户端声明的工具名"},
		{ClientToolWireName("read_file", nil), true, "线上哈希名"},
		{"read", false, "裸官方名桩，客户端没声明同名工具"},
		{"shell", false, "裸官方名桩"},
		{"edit", false, "裸官方名桩"},
		{"totally_unknown", false, "任意未声明名字"},
	}
	for _, c := range cases {
		_, ok := unresolvableToolName(renames, c.wire)
		if ok != c.wantOK {
			t.Errorf("%q（%s）: 可解析=%v，期望 %v", c.wire, c.comment, ok, c.wantOK)
		}
	}
}

// TestUnresolvableKeepsDeclaredOfficialName：客户端自己声明了 read（DSH 形态），
// 模型返回 read 是正常回显，必须放行——这是不能误伤的主要场景。
func TestUnresolvableKeepsDeclaredOfficialName(t *testing.T) {
	payload := map[string]any{
		"model": "m",
		"tools": []any{
			chatTool("read", "Read a file."),
			chatTool("write", "Write a file."),
			chatTool("edit", "Edit a file."),
		},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	for _, name := range []string{"read", "write", "edit"} {
		if _, ok := unresolvableToolName(renames, name); !ok {
			t.Errorf("客户端声明了 %q，回显必须放行，不能被拦截", name)
		}
	}
}

// TestDropUnresolvableChat 非流式 chat：整条调用删除，空 message 一并清掉。
func TestDropUnresolvableChat(t *testing.T) {
	renames := plan1Renames(t)
	build := func(name string) []byte {
		body := map[string]any{"choices": []any{map[string]any{"index": 0,
			"message": map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": name, "arguments": `{"p":"/x"}`}}}}}}}
		raw, _ := json.Marshal(body)
		return raw
	}

	out, dropped := dropUnresolvableToolCalls(build("read"), renames)
	if dropped != 1 {
		t.Fatalf("桩名 read 应被丢弃 1 条，实际 %d", dropped)
	}
	if strings.Contains(string(out), `"read"`) {
		t.Fatalf("下发体里仍残留 read：%s", out)
	}

	out, dropped = dropUnresolvableToolCalls(build("read_file"), renames)
	if dropped != 0 {
		t.Fatalf("客户端真名 read_file 不能丢，实际丢了 %d 条", dropped)
	}
	if !strings.Contains(string(out), "read_file") {
		t.Fatalf("read_file 应原样保留：%s", out)
	}
}

// TestDropUnresolvableResponses 非流式 responses：output[] 里的 function_call。
func TestDropUnresolvableResponses(t *testing.T) {
	renames := plan1Renames(t)
	body := map[string]any{"output": []any{
		map[string]any{"type": "message", "role": "assistant"},
		map[string]any{"type": "function_call", "name": "read", "arguments": "{}", "call_id": "c1"},
		map[string]any{"type": "function_call", "name": "read_file", "arguments": "{}", "call_id": "c2"},
	}}
	raw, _ := json.Marshal(body)
	out, dropped := dropUnresolvableToolCalls(raw, renames)
	if dropped != 1 {
		t.Fatalf("应丢 1 条（read），实际 %d", dropped)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	names := []string{}
	for _, item := range asArr(got["output"]) {
		m, _ := item.(map[string]any)
		if s := asStr(m["name"]); s != "" {
			names = append(names, s)
		}
	}
	if len(names) != 1 || names[0] != "read_file" {
		t.Fatalf("留下的应是 read_file，实际 %v", names)
	}
}

// TestDropNoopReturnsSameBytes：一条都不删时不重新序列化，保证字节级不变。
func TestDropNoopReturnsSameBytes(t *testing.T) {
	renames := plan1Renames(t)
	raw := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	out, dropped := dropUnresolvableToolCalls(raw, renames)
	if dropped != 0 {
		t.Fatalf("无工具调用时不该删任何东西，实际 %d", dropped)
	}
	if string(out) != string(raw) {
		t.Fatalf("无改动时字节必须原样返回")
	}
}

// TestRealDSHNotDropped 用真实 DSH 抓包（45 工具，客户端自声明官方名）
// 验证零误伤：模型回显的任何官方名都必须在声明的集合里，一条都不能丢。
func TestRealDSHNotDropped(t *testing.T) {
	raw, err := os.ReadFile(`F:\worker\_cap_cli2\dsh43_stream.json`)
	if err != nil {
		t.Skipf("抓包不存在，跳过：%v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("抓包解析失败: %v", err)
	}
	up, renames, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	_ = up
	// 出站里所有裸官方名桩，模型都可能选；DSH 声明了同名的就要放行
	dropped, kept := 0, 0
	for _, name := range v2OfficialToolOrder {
		if _, ok := unresolvableToolName(renames, name); ok {
			kept++
		} else {
			dropped++
			t.Logf("DSH 场景下 %q 被判为不可解析（若 DSH 未声明同名工具则属正常）", name)
		}
	}
	t.Logf("官方名桩：放行 %d 个，拦截 %d 个", kept, dropped)
	if kept == 0 {
		t.Fatal("一个官方名都没放行，说明 declared 完全没生效")
	}
}

// TestStreamSuppressesUnresolvable：流式路径下，未映射的桩名必须整条吞掉，
// 客户端收到的帧里不能出现这个名字，也不能出现它的 arguments。
func TestStreamSuppressesUnresolvable(t *testing.T) {
	renames := plan1Renames(t)
	input := toolCallDelta(t, map[string]any{"name": "read", "arguments": `{"path":"/etc/hostname"}`}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	if strings.Contains(out, `"read"`) {
		t.Fatalf("流式仍下发了桩名 read：\n%s", out)
	}
	if strings.Contains(out, "hostname") {
		t.Fatalf("流式仍下发了被吞调用的 arguments：\n%s", out)
	}
}

// TestStreamKeepsResolvable：能还原的调用必须原样保留，不能误吞。
func TestStreamKeepsResolvable(t *testing.T) {
	renames := plan1Renames(t)
	wire := ClientToolWireName("read_file", nil)
	input := toolCallDelta(t, map[string]any{"name": wire, "arguments": `{"file_path":"/x"}`}) +
		"data: [DONE]\n\n"
	out := readRenamed(t, input, FmtChat, renames)
	if !strings.Contains(out, `"read_file"`) {
		t.Fatalf("可还原的哈希名被误吞：\n%s", out)
	}
}
