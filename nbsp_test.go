package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const nbsp = "\u00a0"

// TestNbspReplacesAsciiSpaces 基本替换。
func TestNbspReplacesAsciiSpaces(t *testing.T) {
	got := nbspDescription("Read a file and return content.")
	if strings.Contains(got, " ") {
		t.Fatalf("仍残留 ASCII 空格: %q", got)
	}
	if !strings.Contains(got, nbsp) {
		t.Fatalf("没有 NBSP: %q", got)
	}
	if strings.ReplaceAll(got, nbsp, " ") != "Read a file and return content." {
		t.Fatalf("折回空格后内容变了: %q", got)
	}
}

// TestNbspIdempotent 重复调用不累积。
func TestNbspIdempotent(t *testing.T) {
	once := nbspDescription("a b c")
	twice := nbspDescription(once)
	if once != twice {
		t.Fatalf("不幂等: %q -> %q", once, twice)
	}
}

// TestNbspPreservesOtherWhitespace 换行、tab、已有 NBSP 原样保留。
func TestNbspPreservesOtherWhitespace(t *testing.T) {
	// 输入里有一个 ASCII 空格、一个换行、一个 tab、一个已有 NBSP。
	in := "a b\nc\td" + nbsp + "e"
	out := nbspDescription(in)
	if !strings.Contains(out, "\n") || !strings.Contains(out, "\t") {
		t.Fatalf("换行或 tab 被破坏: %q", out)
	}
	// 唯一的 ASCII 空格变成 NBSP，加上原有的那一共 2 个。
	if got := strings.Count(out, nbsp); got != 2 {
		t.Fatalf("NBSP 数量 want 2，got %d: %q", got, out)
	}
}

// TestNbspSkipsParamDescriptions 参数 schema 内的 description 不动。
func TestNbspSkipsParamDescriptions(t *testing.T) {
	payload := map[string]any{
		"model": "m",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "read_file",
			"description": "Read a UTF-8 text file and return line-numbered content.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{
						"type":        "string",
						"description": "Path to the file to read from disk",
					},
				},
				"required": []any{"file_path"},
			},
		}}},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	for _, raw := range asArr(up["tools"]) {
		fn := asMap(asMap(raw)["function"])
		if entryToolName(asMap(raw), FmtChat) != wireNameOf("read_file") {
			continue
		}
		params := asMap(fn["parameters"])
		paramDesc := asStr(asMap(asMap(params["properties"])["file_path"])["description"])
		if strings.Contains(paramDesc, nbsp) {
			t.Fatalf("参数级 description 被加了 NBSP: %q", paramDesc)
		}
		if paramDesc != "Path to the file to read from disk" {
			t.Fatalf("参数级 description 被改动: %q", paramDesc)
		}
		break
	}
}

// TestNbspOfficialStubsUntouched 14 个官方桩描述不含 NBSP。
func TestNbspOfficialStubsUntouched(t *testing.T) {
	payload := map[string]any{
		"model":    "m",
		"tools":    []any{chatTool("read_file", "Read a file.")},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	official := officialToolNameSet()
	for _, raw := range asArr(up["tools"]) {
		entry := asMap(raw)
		name := entryToolName(entry, FmtChat)
		if !official[name] {
			continue
		}
		desc := asStr(asMap(entry["function"])["description"])
		if strings.Contains(desc, nbsp) {
			t.Fatalf("官方桩 %q 的描述被加了 NBSP: %.80q", name, desc)
		}
	}
}

// TestNbspVerbatimModeSkips ZEN_DESC_VERBATIM=1 时描述逐字。
func TestNbspVerbatimModeSkips(t *testing.T) {
	t.Setenv("ZEN_DESC_VERBATIM", "1")
	payload := map[string]any{
		"model":    "m",
		"tools":    []any{chatTool("read_file", "Read a file now.")},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	for _, raw := range asArr(up["tools"]) {
		entry := asMap(raw)
		if entryToolName(entry, FmtChat) != wireNameOf("read_file") {
			continue
		}
		desc := asStr(asMap(entry["function"])["description"])
		if strings.Contains(desc, nbsp) {
			t.Fatalf("verbatim 档不该加 NBSP: %q", desc)
		}
		if desc != "Read a file now." {
			t.Fatalf("verbatim 档描述应逐字: %q", desc)
		}
		return
	}
	t.Fatal("没找到 read_file 的出站条目")
}

// TestOutboundClientDescriptionHasNoAsciiSpace 端到端：客户端工具描述出站后
// 不含 ASCII 空格（parameters 除外）。
func TestOutboundClientDescriptionHasNoAsciiSpace(t *testing.T) {
	payload := map[string]any{
		"model":    "m",
		"tools":    []any{chatTool("read_file", "Read a UTF-8 text file and return line-numbered content.")},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, payload)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	for _, raw := range asArr(up["tools"]) {
		entry := asMap(raw)
		if entryToolName(entry, FmtChat) != wireNameOf("read_file") {
			continue
		}
		desc := asStr(asMap(entry["function"])["description"])
		if strings.Contains(desc, " ") {
			t.Fatalf("客户端工具描述仍含 ASCII 空格: %q", desc)
		}
		if strings.Contains(strings.ReplaceAll(desc, nbsp, " "), "UTF-8 text file") {
			t.Fatalf("黑名单短语没洗掉: %q", desc)
		}
		return
	}
	t.Fatal("没找到 read_file 的出站条目")
}

// wireNameOf 返回某个客户端工具的线上名（测试辅助）。
func wireNameOf(client string) string {
	return ClientToolWireName(client, nil)
}

var _ = json.Marshal
