package main

import (
	"strings"
	"testing"
)

// TestToolWireNameIsHashed 锁定线上名格式：**固定 mcp_<12位sha256> 哈希名**。
//
// 2026-11-18 实测（同节点同请求体，只改命名格式）：
//
//	哈希名 mcp_2d4f8285de96            → 200
//	可读名 mcp__anysearch_batch_search  → 429
//
// 可读名与官方 MCP 的 mcp__<server>__<tool> 同形，像 mcp__read / mcp__edit
// 会被上游判为冒充官方名并拒绝。所以这里断言名字必须是纯哈希，不含任何
// 客户端语义（不能出现原名、也不能出现归一化后的原名）。
func TestToolWireNameIsHashed(t *testing.T) {
	used := map[string]bool{}
	clients := []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash",
		"subagent_fork", "ops_bash", "anysearch_batch_search"}
	for _, client := range clients {
		wireName := ClientToolWireName(client, used)
		if !strings.HasPrefix(wireName, ClientToolPrefix()) {
			t.Errorf("wire name %q does not carry the client prefix", wireName)
		}
		body := strings.TrimPrefix(wireName, ClientToolPrefix())
		body = strings.TrimSuffix(body, "_2")
		body = strings.TrimSuffix(body, "_3")
		if len(body) != clientToolDigestLength {
			t.Errorf("wire name %q 主体长度 = %d, want %d（哈希名固定 12 位）",
				wireName, len(body), clientToolDigestLength)
		}
		for _, ch := range body {
			if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
				t.Errorf("wire name %q 主体含非 hex 字符 %q（说明不是哈希名）", wireName, ch)
				break
			}
		}
		want := ClientToolPrefix() + toolNameDigest(client, clientToolDigestLength)
		if wireName != want {
			t.Errorf("wire name %q != %q", wireName, want)
		}
		// 绝不能把客户端原名泄进线上名
		lower := strings.ToLower(client)
		if strings.Contains(strings.ToLower(body), lower) {
			t.Errorf("wire name %q 泄露了客户端原名 %q", wireName, client)
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
// 2026-11-18 起（v1.0）：
//   - 透传判据固定为 subset：官方名子集整份原样发出，不补桩（12 进 12 出）；
//   - 但透传前一律用 sanitizeRankedTools 洗掉工具级指纹短语，否则上游 429。
//
// 所以「逐字透传」现在指**名字与结构**逐字，描述里的指纹短语会被替换。
// 实测依据：shape-acceptance 44129660858 里「官方名 + 工具级被判原句」
// 未脱敏时 429，脱敏后 44130648925 转 200。
func TestOfficialToolsetPassesThroughWithSanitizedDescriptions(t *testing.T) {
	names := v2OfficialToolOrder
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, name := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		desc := "Tool " + name + "."
		if name == "read" {
			// 官方原文里的封禁短语：透传前必须被洗掉。
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
		if asStr(fn["name"]) != "read" {
			continue
		}
		// 描述同时经过短语黑名单与 NBSP 替换，比较前把 NBSP 折回空格。
		desc := strings.ReplaceAll(asStr(fn["description"]), "\u00a0", " ")
		if strings.Contains(desc, "line-numbered content") {
			t.Fatalf("指纹短语没洗掉: %q", desc)
		}
		if !strings.Contains(desc, "line-numbered output") {
			t.Fatalf("应当换成中性文本，实际 %q", desc)
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
