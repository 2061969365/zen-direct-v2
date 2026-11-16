package main

import (
	"strings"
	"testing"
)

func TestToolWireNameIsOpaque(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash", "subagent_fork", "ops_bash"} {
		wireName := ClientToolWireName(client, used)
		if !strings.HasPrefix(wireName, ClientToolPrefix) {
			t.Errorf("wire name %q does not carry the client prefix", wireName)
		}
		if len(wireName) > toolWireNameLimit {
			t.Errorf("wire name %q is longer than %d bytes", wireName, toolWireNameLimit)
		}
		if strings.Contains(strings.ToLower(wireName), strings.ToLower(client)) {
			t.Errorf("wire name %q still spells the client name %q", wireName, client)
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
// 客户端（官方 CLI）已经自带官方工具集时，出站必须与原请求逐项一致——
// 不改名、不追加、不脱敏描述。
//
// 反例代价：改名+追加会让出站变成 26 个工具，同一份 body 从 200 掉到 429
// FreeUsageLimitError。所以这条一旦回归，网关会整体不可用。
func TestOfficialToolsetPassesThroughVerbatim(t *testing.T) {
	in := mustJSON(t, `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"read","description":"Read a UTF-8 text file and return line-numbered content.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},
			{"type":"function","function":{"name":"edit","description":"Edit it.","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"experimental_image_generate","description":"Gen.","parameters":{"type":"object"}}}
		]}`)
	up, renames, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatal(err)
	}
	if renames.Count() != 0 {
		t.Fatalf("官方工具集不应改名，实际改了 %d 个: %v", renames.Count(), renames.toWire)
	}
	entries := asArr(up["tools"])
	if len(entries) != 3 {
		t.Fatalf("官方工具集不应追加，出站 %d 个（应为 3）", len(entries))
	}
	want := []string{"read", "edit", "experimental_image_generate"}
	for i, name := range want {
		got := asStr(asMap(asMap(entries[i])["function"])["name"])
		if got != name {
			t.Fatalf("第 %d 个工具名被改动: got %q want %q", i, got, name)
		}
	}
	// 描述也要原样保留（官方原文可能含“封禁短语”，但那是官方口径，不能改）。
	desc := asStr(asMap(asMap(entries[0])["function"])["description"])
	if !strings.Contains(desc, "line-numbered content") {
		t.Fatalf("官方描述被脱敏了: %q", desc)
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
