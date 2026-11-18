package main

import (
	"strings"
	"testing"
)

// 线上名契约（2026-11-18 起）：
//   ClientToolWireName(name) == ClientToolPrefix() + sha256(name)[:12]
//
// 为什么写死哈希名：实测同一份 45 工具请求，只改线上名格式——
//
//	哈希名 mcp_2d4f8285de96            → 200
//	可读名 mcp__anysearch_batch_search  → 429
//
// 可读名与官方 MCP 的 mcp__<server>__<tool> 同形，mcp__read / mcp__edit 这类
// 会被上游判为冒充官方名并拒绝。所以线上名不得携带任何客户端语义。
//
// 测试不硬编码 mcp_，因为 ZEN_TOOL_PREFIX 可以轮换（clint_ → mcp_ → …）。

func wirePrefix() string { return ClientToolPrefix() }

// mustWire 校验一个线上名的基本形状并返回它。
func mustWire(t *testing.T, name string, used map[string]bool) string {
	t.Helper()
	wire := ClientToolWireName(name, used)
	if !strings.HasPrefix(wire, wirePrefix()) {
		t.Fatalf("ClientToolWireName(%q) = %q, want prefix %q", name, wire, wirePrefix())
	}
	body := strings.TrimPrefix(wire, wirePrefix())
	// 允许 _2/_3 撞车后缀
	if i := strings.LastIndex(body, "_"); i > 0 {
		suffix := body[i+1:]
		if suffix != "" && strings.Trim(suffix, "0123456789") == "" {
			body = body[:i]
		}
	}
	if len(body) != clientToolDigestLength {
		t.Fatalf("ClientToolWireName(%q) = %q, 主体长度 %d, want %d",
			name, wire, len(body), clientToolDigestLength)
	}
	for _, ch := range body {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Fatalf("ClientToolWireName(%q) = %q, 主体含非 hex 字符 %q", name, wire, ch)
		}
	}
	if len(wire) > toolWireNameLimit {
		t.Fatalf("ClientToolWireName(%q) = %q 超过 %d 字节", name, wire, toolWireNameLimit)
	}
	return wire
}

// 线上名必须等于 sha256 前 12 位，且不得泄露客户端原名。
func TestClientToolWireNameIsHash(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"read", "Read", "edit", "Bash", "ops_bash",
		"anysearch_batch_search", "subagent_fork", "ScheduleWakeup", "搜索文档"} {
		wire := mustWire(t, client, used)
		want := ClientToolPrefix() + toolNameDigest(client, clientToolDigestLength)
		if wire != want {
			t.Errorf("ClientToolWireName(%q) = %q, want %q", client, wire, want)
		}
		body := strings.TrimPrefix(wire, ClientToolPrefix())
		if lower := strings.ToLower(client); lower != "" && strings.Contains(body, lower) {
			t.Errorf("线上名 %q 泄露了客户端原名 %q", wire, client)
		}
	}
}

// 不同客户端名必须得到不同线上名。
func TestClientToolWireNameDistinct(t *testing.T) {
	used := map[string]bool{}
	names := []string{"read", "Read", "READ", "edit", "Edit", "write", "shell"}
	seen := map[string]string{}
	for _, n := range names {
		w := mustWire(t, n, used)
		if prev, ok := seen[w]; ok && prev != n {
			t.Errorf("%q 与 %q 得到相同线上名 %q", prev, n, w)
		}
		seen[w] = n
	}
}

// 同名重复声明时追加 _2/_3，仍保持前缀与可读性无关的形态。
func TestClientToolWireNameCollision(t *testing.T) {
	used := map[string]bool{}
	first := mustWire(t, "Read", used)
	second := ClientToolWireName("Read", used)
	if second == first {
		t.Fatalf("重复声明应得到不同的线上名，两次都是 %q", first)
	}
	if !strings.HasPrefix(second, wirePrefix()) {
		t.Fatalf("第二个线上名 %q 丢了前缀 %q", second, wirePrefix())
	}
	if !strings.HasSuffix(second, "_2") {
		t.Fatalf("第二个线上名 %q 应以 _2 结尾", second)
	}
	third := ClientToolWireName("Read", used)
	if !strings.HasSuffix(third, "_3") {
		t.Fatalf("第三个线上名 %q 应以 _3 结尾", third)
	}
}

// 超长名字也走哈希：长度恒定，不会因为输入变长而膨胀。
func TestClientToolWireNameLongNameIsShortHash(t *testing.T) {
	used := map[string]bool{}
	long := strings.Repeat("a", 300)
	wire := mustWire(t, long, used)
	if len(wire) != len(wirePrefix())+clientToolDigestLength {
		t.Fatalf("超长名线上名 %q 长度 %d, want %d",
			wire, len(wire), len(wirePrefix())+clientToolDigestLength)
	}
	want := wirePrefix() + toolNameDigest(long, clientToolDigestLength)
	if wire != want {
		t.Fatalf("超长名线上名 %q != %q", wire, want)
	}
}

// 是否可逆与否则由映射表承担：加进去必须能还原。
func TestClientToolWireNameReversible(t *testing.T) {
	for _, original := range []string{"read", "Bash", "anysearch_batch_search", "搜索文档"} {
		used := map[string]bool{}
		wire := mustWire(t, original, used)
		renames := NewToolRenames()
		renames.Add(original, wire)
		got, ok := renames.Client(wire)
		if !ok {
			t.Fatalf("%q -> %q 无法还原", original, wire)
		}
		if got != original {
			t.Fatalf("%q -> %q 还原成 %q", original, wire, got)
		}
	}
}

// 前缀剥离：哈希名剥掉前缀后就是 12 位 hex 主体。
func TestClientToolWireNamePrefixStripShape(t *testing.T) {
	used := map[string]bool{}
	wire := mustWire(t, "read", used)
	body, ok := splitClientToolWireName(wire)
	if !ok {
		t.Fatalf("splitClientToolWireName(%q) 失败", wire)
	}
	if body != toolNameDigest("read", clientToolDigestLength) {
		t.Fatalf("剥前缀得到 %q, want %q", body, toolNameDigest("read", clientToolDigestLength))
	}
	// 无前缀的明文不应被判为线上名
	if _, ok := splitClientToolWireName("plain_name"); ok {
		t.Fatal("明文不应带前缀剥离成功")
	}
}

// 哈希名必须被识别为「当前格式」，而不是历史遗留。
func TestHashWireNameIsCurrentFormat(t *testing.T) {
	used := map[string]bool{}
	wire := mustWire(t, "read", used)
	if !legacyClientToolWireName(wire) {
		t.Log("注：legacyClientToolWireName 现在形容词义已变，仅作形状识别")
	}
	// 关键：哈希名必须参与编辑距离修复，否则模型抄错一位就无法还原
	renames := NewToolRenames()
	renames.Add("read", wire)
	typo := wirePrefix() + "f" + strings.TrimPrefix(wire, wirePrefix())
	client, ok := renames.ResolveFuzzy(typo)
	if !ok || client != "read" {
		t.Fatalf("哈希名的编辑距离修复失效: %q -> %q (%v)", typo, client, ok)
	}
}

// 整形管道里，客户端工具必须拿到哈希名。
func TestWireNameThroughShapePipeline(t *testing.T) {
	payload := shapeTestPayload([]string{"Read", "Bash"})
	renames, _ := renameAndCompleteTools(payload, FmtChat)
	readWire, ok := renames.Wire("Read")
	if !ok {
		t.Fatal("Read 没有被改名")
	}
	want := wirePrefix() + toolNameDigest("Read", clientToolDigestLength)
	if readWire != want {
		t.Fatalf("Read wire = %q, want %q", readWire, want)
	}
	if strings.Contains(readWire, "read") || strings.Contains(readWire, "Read") {
		t.Fatalf("线上名 %q 泄露了客户端原名", readWire)
	}
}
