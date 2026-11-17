package main

import (
	"strings"
	"testing"
)

// TestToolWireNameIsReadable 钉住新契约（2026-11-17 起）：线上名必须可读。
//
// 旧契约是"不透明"（mcp_<12hex>），模型只能靠描述猜工具，这是工具调用
// 准确率的最大损失源，所以名字改成 mcp__<归一化原名>。不透明性不再是
// 返回值，可逆性改由 ToolRenames 的映射表承担（见 toolshape_wirename_test.go）。
func TestToolWireNameIsReadable(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash", "subagent_fork", "ops_bash"} {
		wireName := ClientToolWireName(client, used)
		if !strings.HasPrefix(wireName, ClientToolPrefix()) {
			t.Errorf("wire name %q does not carry the client prefix", wireName)
		}
		if !strings.HasPrefix(wireName, ClientToolPrefix()+"_") {
			t.Errorf("wire name %q is missing the mcp__ separator (want %q)", wireName, ClientToolPrefix()+"_"+normalizeClientToolWireName(client))
		}
		if len(wireName) > toolWireNameLimit {
			t.Errorf("wire name %q is longer than %d bytes", wireName, toolWireNameLimit)
		}
		want := ClientToolPrefix() + "_" + normalizeClientToolWireName(client)
		if wireName != want {
			t.Errorf("wire name %q != %q (readable form expected)", wireName, want)
		}
	}
}

func TestToolRenamesRestoreOpaqueNames(t *testing.T) {
	renames := NewToolRenames()
	used := map[string]bool{}
	for _, client := range []string{"ReportFindings", "ScheduleWakeup", "subagent_fork"} {
		wireName := ClientToolWireName(client, used)
		if wireName == client {
			t.Fatalf("%q was not renamed", client)
		}
		renames.Add(client, wireName)
		if got := renames.Restore(wireName); got != client {
			t.Errorf("Restore(%q) = %q, want %q", wireName, got, client)
		}
	}
}

// TestOfficialToolsetPassesThroughVerbatim 钉住 2026-11-16 抓包得出的结论：
// 客户端（官方 CLI）已经自带**完整**官方工具集时，出站必须与原请求逐项一致——
// 不改名、不追加、不脱敏描述。
//
// 反例代价：改名+追加会让出站变成 26 个工具，同一份 body 从 200 掉到 429
// FreeUsageLimitError。所以这条一旦回归，网关会整体不可用。
//
// 2026-11-17 修正：透传条件从 allOfficialToolNames（官方名子集也算真）收紧为
// completeOfficialToolset。子集透传实测 0/8 全 429，见
// toolshape_official_subset_test.go 与 docs/head2head-zen-vs-opencode2api.md。
func TestOfficialToolsetPassesThroughVerbatim(t *testing.T) {
	names := v2OfficialToolOrder
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, name := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		desc := "Tool " + name + "."
		if name == "read" {
			// 官方原文里可能含“封禁短语”，但那是官方口径，透传时不能改。
			desc = "Read a UTF-8 text file and return line-numbered content."
		}
		sb.WriteString(`{"type":"function","function":{"name":"` + name + `","description":"` + desc + `","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`)
	}
	sb.WriteString(`]}`)
	in := mustJSON(t, sb.String())
	up, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	if renames.Count() != 0 {
		t.Fatalf("完整官方工具集不应改名，实际改了 %d 个: %v", renames.Count(), renames.toWire)
	}
	entries := asArr(up["tools"])
	if len(entries) != len(names) {
		t.Fatalf("完整官方工具集不应追加，出站 %d 个（应为 %d）", len(entries), len(names))
	}
	for i, name := range names {
		got := asStr(asMap(asMap(entries[i])["function"])["name"])
		if got != name {
			t.Fatalf("第 %d 个工具名被改动: got %q want %q", i, got, name)
		}
	}
	for _, raw := range entries {
		fn := asMap(asMap(raw)["function"])
		if asStr(fn["name"]) == "read" {
			desc := asStr(fn["description"])
			if !strings.Contains(desc, "line-numbered content") {
				t.Fatalf("官方描述被脱敏了: %q", desc)
			}
		}
	}
}

// TestThirdPartyToolsStillGetOfficialSet 确认第三方客户端工具仍走改名+补官方集：
// 上游按“名字集合是否为官方原样”放行，补集是这条路径能通的前提。
func TestThirdPartyToolsStillGetOfficialSet(t *testing.T) {
	in := mustJSON(t, `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"pwsh","description":"Run a command.","parameters":{"type":"object"}}}]}`)
	up, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	if renames.Count() == 0 {
		t.Fatal("第三方工具应被改名")
	}
	entries := asArr(up["tools"])
	if len(entries) <= 1 {
		t.Fatalf("第三方工具应补官方集，出站仅 %d 个", len(entries))
	}
	names := map[string]bool{}
	for _, raw := range entries {
		names[asStr(asMap(asMap(raw)["function"])["name"])] = true
	}
	if !names["read"] || !names["shell"] {
		t.Fatalf("官方集未补全: %v", names)
	}
	if names["pwsh"] {
		t.Fatal("第三方工具名应被改名为 clint_*")
	}
}
