package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestProbeOutboundToolNames 把真实抓包 body 过一遍 buildUpstream，打印出站工具名，
// 用来核对“客户端工具与官方集重名”时网关到底发了什么。
//
// 默认跳过；用 ZEN_PROBE_BODY=<file> 指定输入即可运行。
func TestProbeOutboundToolNames(t *testing.T) {
	path := os.Getenv("ZEN_PROBE_FILE")
	if path == "" {
		t.Skip("set ZEN_PROBE_FILE to a captured request body")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	up, _, err := buildUpstream(FmtChat, FmtChat, in)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	tools, _ := up["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, item := range tools {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := entry["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	body, _ := json.Marshal(up)
	t.Logf("%s -> outTools=%d outBytes=%d", path, len(names), len(body))
	t.Logf("names=%v", names)
}
