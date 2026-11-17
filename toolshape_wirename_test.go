package main

import (
	"strings"
	"testing"
)

// wirePrefix 是当前生效前缀 + 分隔符（默认 mcp__）。测试不硬编码 mcp_，
// 因为 ZEN_TOOL_PREFIX 可以轮换（clint_ → mcp_ → …）。
func wirePrefix() string { return ClientToolPrefix() + clientToolNameSeparator }

func mustWire(t *testing.T, name string, used map[string]bool) string {
	t.Helper()
	wire := ClientToolWireName(name, used)
	if !strings.HasPrefix(wire, wirePrefix()) {
		t.Fatalf("ClientToolWireName(%q) = %q, want prefix %q", name, wire, wirePrefix())
	}
	if len(wire) > toolWireNameLimit {
		t.Fatalf("ClientToolWireName(%q) = %q, longer than %d bytes", name, wire, toolWireNameLimit)
	}
	return wire
}

// TestClientToolWireNameNormalization 钉住归一化规则：小写、非 [a-z0-9_] 换 _、
// 连续 _ 压一个、首尾 _ 去掉、空结果兜底 tool。
func TestClientToolWireNameNormalization(t *testing.T) {
	cases := []struct {
		name string
		want string // 归一化后的主体（不含前缀）
	}{
		{"read", "read"},
		{"Read", "read"},
		{"READ", "read"},
		{"github_create_issue", "github_create_issue"},
		{"My Tool!", "my_tool"},
		{"  spaced  out  ", "spaced_out"},
		{"read___write", "read_write"},
		{"__read__", "read"},
		{"_", "tool"},
		{"!!!", "tool"},
		{"", "tool"},
		{"搜索 工具", "tool"},   // 全非 ASCII：压成一个 _ 后被首尾去掉，兜底 tool
		{"read 文件", "read"}, // 中文尾巴被归一成 _ 后去掉
		{"工具 read", "read"}, // 中文前缀同理
		{"UPPER-case.9", "upper_case_9"},
		{"a-b.c d", "a_b_c_d"},
		{"ALLCAPS_AND.digits123", "allcaps_and_digits123"},
	}
	for _, c := range cases {
		if got := normalizeClientToolWireName(c.name); got != c.want {
			t.Errorf("normalizeClientToolWireName(%q) = %q, want %q", c.name, got, c.want)
		}
		wire := ClientToolWireName(c.name, map[string]bool{})
		if wire != wirePrefix()+c.want {
			t.Errorf("ClientToolWireName(%q) = %q, want %q", c.name, wire, wirePrefix()+c.want)
		}
	}
}

// TestClientToolWireNameCollision 钉住撞车策略：归一化后同名 → _2/_3 依次追加，
// 且每个名字都唯一（used 语义与旧实现一致：同一请求内先到先占）。
func TestClientToolWireNameCollision(t *testing.T) {
	used := map[string]bool{}
	first := mustWire(t, "Read", used)
	second := mustWire(t, "read", used)
	third := mustWire(t, "read!", used)
	fourth := mustWire(t, " read ", used)

	if want := wirePrefix() + "read"; first != want {
		t.Fatalf("first = %q, want %q", first, want)
	}
	if want := wirePrefix() + "read_2"; second != want {
		t.Fatalf("second = %q, want %q", second, want)
	}
	if want := wirePrefix() + "read_3"; third != want {
		t.Fatalf("third = %q, want %q", third, want)
	}
	if want := wirePrefix() + "read_4"; fourth != want {
		t.Fatalf("fourth = %q, want %q", fourth, want)
	}
	seen := map[string]bool{}
	for _, wire := range []string{first, second, third, fourth} {
		if seen[wire] {
			t.Fatalf("duplicate wire name %q", wire)
		}
		seen[wire] = true
	}
}

// TestClientToolWireNameTruncation 钉住超长处理：截断到 toolWireNameLimit，
// 尾巴带 8 位 digest，两个同前缀的超长名字仍然互不相同。
func TestClientToolWireNameTruncation(t *testing.T) {
	longA := strings.Repeat("a", 120)
	longB := strings.Repeat("a", 119) + "b"

	used := map[string]bool{}
	wireA := mustWire(t, longA, used)
	wireB := mustWire(t, longB, used)

	for _, wire := range []string{wireA, wireB} {
		if len(wire) != toolWireNameLimit {
			t.Errorf("truncated wire %q has length %d, want %d", wire, len(wire), toolWireNameLimit)
		}
		body := strings.TrimPrefix(wire, wirePrefix())
		if len(body) < clientToolDigestTailLength+1 {
			t.Fatalf("truncated body %q is too short to carry the digest tail", body)
		}
		tail := body[len(body)-clientToolDigestTailLength:]
		if strings.Trim(tail, "0123456789abcdef") != "" {
			t.Errorf("truncated body %q does not end with a hex digest tail (%q)", body, tail)
		}
		if cut := body[:len(body)-len(tail)-1]; strings.Trim(cut, "a") != "" {
			t.Errorf("truncated body %q keeps non-name bytes before the digest tail", body)
		}
	}
	if wireA == wireB {
		t.Fatalf("two long names truncated to the same wire name %q", wireA)
	}
	if want := toolNameDigest(longA, clientToolDigestTailLength); !strings.HasSuffix(wireA, "_"+want) {
		t.Errorf("wire %q does not end with digest %q of the original name", wireA, want)
	}
	// 稳定性：同一原名在不同请求（不同 used）里必须得到同一个名字，
	// 否则跨轮历史里的调用名对不上。
	if again := ClientToolWireName(longA, map[string]bool{}); again != wireA {
		t.Errorf("truncated wire name is not deterministic: %q vs %q", again, wireA)
	}
}

// TestClientToolWireNameReversible 钉住可逆性：映射表是唯一还原路径，
// Add(原名, wire) 之后 Client/Resolve/Restore 都必须回到原名——包括撞车名
// 和截断名。
func TestClientToolWireNameReversible(t *testing.T) {
	originals := []string{
		"read",
		"Read", // 与 read 归一化撞车
		"My Tool!",
		"github_create_issue",
		"搜索 工具",
		"",
		strings.Repeat("very-long-name-", 12),
	}
	renames := NewToolRenames()
	used := map[string]bool{}
	wantByWire := map[string]string{}
	bound := 0
	for _, original := range originals {
		wire := mustWire(t, original, used)
		if want := wantByWire[wire]; want != "" && want != original {
			t.Fatalf("wire %q already bound to %q, now %q", wire, want, original)
		}
		// Add 忽略空客户端名（与旧实现一致），空名只验证生成侧的可读性。
		if original == "" {
			if body := strings.TrimPrefix(wire, wirePrefix()); !strings.HasPrefix(body, clientToolNameFallback) {
				t.Fatalf("empty name wire = %q, want the %q fallback body", wire, clientToolNameFallback)
			}
			continue
		}
		wantByWire[wire] = original
		renames.Add(original, wire)
		bound++
	}
	if renames.Count() != bound {
		t.Fatalf("renames.Count() = %d, want %d", renames.Count(), bound)
	}
	for wire, original := range wantByWire {
		if got, ok := renames.Client(wire); !ok || got != original {
			t.Errorf("Client(%q) = %q,%v, want %q,true", wire, got, ok, original)
		}
		if got, ok := renames.Resolve(wire); !ok || got != original {
			t.Errorf("Resolve(%q) = %q,%v, want %q,true", wire, got, ok, original)
		}
		if got := renames.Restore(wire); got != original {
			t.Errorf("Restore(%q) = %q, want %q", wire, got, original)
		}
		if got, ok := renames.Wire(original); !ok || got != wire {
			t.Errorf("Wire(%q) = %q,%v, want %q,true", original, got, ok, wire)
		}
		if got, ok := renames.ResolveFuzzy(wire); !ok || got != original {
			t.Errorf("ResolveFuzzy(%q) = %q,%v, want %q,true", wire, got, ok, original)
		}
	}
}

// TestClientToolWireNamePrefixStripShape 钉住反向解析的新形状：前缀 + 分隔符
// 能被剥掉（模型偶尔漏写一个下划线也要能还原），并且旧形状 mcp_<hex> 不会被
// 当成客户端工具名。
func TestClientToolWireNamePrefixStripShape(t *testing.T) {
	renames := NewToolRenames()
	used := map[string]bool{}
	wire := mustWire(t, "read", used)
	renames.Add("read", wire)

	// 正牌新形状。
	if got := renames.Restore(wire); got != "read" {
		t.Fatalf("Restore(%q) = %q, want read", wire, got)
	}
	// 少一个下划线的形状：前缀剥离后应等价。
	short := ClientToolPrefix() + strings.TrimPrefix(wire, wirePrefix())
	if short == wire {
		t.Fatalf("test setup: expected a distinct single-underscore form, got %q", short)
	}
	if got, ok := renames.ResolveFuzzy(short); !ok || got != "read" {
		t.Fatalf("ResolveFuzzy(%q) = %q,%v, want read,true", short, got, ok)
	}
	// 多一个下划线（模型手抖）同样能还原。
	if got, ok := renames.ResolveFuzzy(wirePrefix() + "_" + strings.TrimPrefix(wire, wirePrefix())); !ok || got != "read" {
		t.Fatalf("ResolveFuzzy(double separator) = %q,%v, want read,true", got, ok)
	}
	// 一个字符的拼写错误仍走保守修复。
	typo := wirePrefix() + "rea" + "d" + "x"
	if got, ok := renames.ResolveFuzzy(typo); !ok || got != "read" {
		t.Fatalf("ResolveFuzzy(%q) = %q,%v, want read,true", typo, got, ok)
	}
}

// TestLegacyDigestWireNamePassthrough 钉住旧形状兜底：历史消息/旧响应里残留的
// mcp_<12hex> 没有映射项时不得 panic、不得被剥成裸 hex 当客户端工具名，而是
// 原样透传（同时记一条 warn）。
func TestLegacyDigestWireNamePassthrough(t *testing.T) {
	legacy := ClientToolPrefix() + "262121c5372b"
	if !legacyClientToolWireName(legacy) {
		t.Fatalf("%q should be recognised as a legacy digest wire name", legacy)
	}

	renames := NewToolRenames()
	renames.Add("read", mustWire(t, "read", map[string]bool{}))

	if got := renames.Restore(legacy); got != legacy {
		t.Errorf("Restore(%q) = %q, want passthrough", legacy, got)
	}
	if got, ok := renames.Resolve(legacy); ok || got != legacy {
		t.Errorf("Resolve(%q) = %q,%v, want %q,false", legacy, got, ok, legacy)
	}
	if got, ok := renames.ResolveFuzzy(legacy); ok || got != legacy {
		t.Errorf("ResolveFuzzy(%q) = %q,%v, want %q,false", legacy, got, ok, legacy)
	}
	if _, ok := renames.Client(legacy); ok {
		t.Errorf("Client(%q) unexpectedly resolved", legacy)
	}
	// 旧形状不该被编辑距离修复成一个语义未知的可读名。
	if got := renames.Restore(ClientToolPrefix() + "262121c5372c"); got != ClientToolPrefix()+"262121c5372c" {
		t.Errorf("legacy name repaired into %q", got)
	}
	// 响应还原路径：不认识的旧名原样保留（RenameResponseTool 的字节替换 + ResolveFuzzy）。
	line := []byte(`{"name":"` + legacy + `","arguments":"{}"}`)
	if got := string(RenameResponseTool(line, renames)); got != string(line) {
		t.Errorf("RenameResponseTool rewrote an unmapped legacy name: %q", got)
	}
	// 历史消息路径：不丢弃、原样透传。
	if got, ok := historyToolWireName(legacy, map[string]string{"read": renames.Restore(mustWire(t, "read", map[string]bool{}))}, renames, officialToolNameSet()); !ok || got != legacy {
		t.Errorf("historyToolWireName(%q) = %q,%v, want passthrough", legacy, got, ok)
	}
	// 映射表里真有旧形状时优先按映射还原（升级前落盘的请求也可能带映射）。
	bound := NewToolRenames()
	bound.Add("read", legacy)
	if got, ok := bound.Resolve(legacy); !ok || got != "read" {
		t.Errorf("Resolve(mapped legacy %q) = %q,%v, want read,true", legacy, got, ok)
	}
	if got := bound.Restore(legacy); got != "read" {
		t.Errorf("Restore(mapped legacy %q) = %q, want read", legacy, got)
	}
}

// TestLegacyDigestDetectionShape 钉住旧形状识别边界：可读名不算旧形状。
func TestLegacyDigestDetectionShape(t *testing.T) {
	cases := []struct {
		wire string
		want bool
	}{
		{ClientToolPrefix() + "262121c5372b", true},
		{ClientToolPrefix() + "262121c5372b0f4d", true},
		{wirePrefix() + "262121c5372b", true}, // 分隔符可有可无
		{wirePrefix() + "read", false},
		{wirePrefix() + "read_2", false},
		{wirePrefix() + "deadbeef", false}, // 8 位 hex 太短：更像可读名
		{"read", false},
		{"", false},
	}
	for _, c := range cases {
		if got := legacyClientToolWireName(c.wire); got != c.want {
			t.Errorf("legacyClientToolWireName(%q) = %v, want %v", c.wire, got, c.want)
		}
	}
}

// TestWireNameThroughShapePipeline 端到端的"整形 → 还原"：非官方工具集出站后
// 名字可读，客户端工具与官方桩共存，历史调用按新形状改写并可逆。
func TestWireNameThroughShapePipeline(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "Read", "description": "read a file", "parameters": map[string]any{"type": "object"},
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "duplicate after normalisation", "parameters": map[string]any{"type": "object"},
			}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{
					"name": "read", "arguments": "{}",
				}},
			}},
		},
	}
	renames, shaped := renameAndCompleteTools(payload, FmtChat)
	if !shaped {
		t.Fatal("non-official toolset should be shaped")
	}
	readWire, ok := renames.Wire("Read")
	if !ok || readWire != wirePrefix()+"read" {
		t.Fatalf("Read wire = %q,%v, want %q", readWire, ok, wirePrefix()+"read")
	}
	dupWire, ok := renames.Wire("read")
	if !ok || dupWire != wirePrefix()+"read_2" {
		t.Fatalf("read wire = %q,%v, want %q", dupWire, ok, wirePrefix()+"read_2")
	}
	outbound := map[string]bool{}
	for _, raw := range asArr(payload["tools"]) {
		if name := asStr(asMap(asMap(raw)["function"])["name"]); name != "" {
			outbound[name] = true
		} else if name := asStr(asMap(raw)["name"]); name != "" {
			outbound[name] = true
		}
	}
	if !outbound[readWire] || !outbound[dupWire] {
		t.Fatalf("outbound tools missing readable wire names: %v", outbound)
	}
	if outbound[ClientToolPrefix()+"262121c5372b"] {
		t.Fatal("outbound still contains a legacy digest wire name")
	}
	// 历史里的客户端名被改写成与 tools 一致的上线名。
	calls := asArr(asMap(asArr(payload["messages"])[1])["tool_calls"])
	if got := asStr(asMap(asMap(calls[0])["function"])["name"]); got != dupWire {
		t.Fatalf("history call name = %q, want %q", got, dupWire)
	}
	// 响应还原：模型回显上线名，客户端看到的必须是原名。
	restored := string(RenameResponseTool([]byte(`{"name":"`+dupWire+`"}`), renames))
	if !strings.Contains(restored, `"name":"read"`) {
		t.Fatalf("response restore = %q, want client name read", restored)
	}
}
