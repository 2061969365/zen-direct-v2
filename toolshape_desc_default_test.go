package main

import (
	"os"
	"strings"
	"testing"
)

// TestDescSanitizeDefaultOn 锁定"描述脱敏默认开启"。
//
// 2026-11-17 v0.9.36 把它改成默认关闭（依据一次无效的同出口 A/B：
// 出口被烧期间 A/B 都 429，误判"描述不是变量"），生产双节点立刻回归：
// real-multi-stream（真实 DSH 描述 + 流式）200 → 429。
// 恢复默认后同请求回到 200。
//
// 结论：描述脱敏是承重的。同出口 A/B 必须保证两次请求在同一出口、
// 且出口当时健康，否则样本无效。
func TestDescSanitizeDefaultOn(t *testing.T) {
	// 默认（未设置任何 env）：脱敏开启。
	os.Unsetenv("ZEN_DESC_VERBATIM")
	if verbatimDescMode() {
		t.Fatal("描述脱敏应默认开启（ZEN_DESC_VERBATIM 未设置时）")
	}
	// 显式逐字模式：脱敏关闭。
	t.Setenv("ZEN_DESC_VERBATIM", "1")
	if !verbatimDescMode() {
		t.Fatal("ZEN_DESC_VERBATIM=1 应关闭脱敏")
	}
}

// TestSanitizeOnRealDSHDescriptions 真实 DSH 描述在默认模式下必须被脱敏。
func TestSanitizeOnRealDSHDescriptions(t *testing.T) {
	os.Unsetenv("ZEN_DESC_VERBATIM")
	cases := []struct {
		name string
		in   string
		out  string
	}{
		{"read", "Read a UTF-8 text file and return line-numbered content.", "Read a text file and return line-numbered output."},
		{"edit", "Edit an existing UTF-8 text file by replacing literal text.", "Edit an existing text file by replacing literal text."},
		{"write", "Create or fully replace a UTF-8 text file.", "Create or fully replace a text file."},
		{"skill", "Load the full instructions for a skill in the session skill catalog.", "Load the full instructions for a skill in the available skills."},
	}
	for _, c := range cases {
		got, replaced := sanitizeToolDescription("mcp_test", c.in)
		if !replaced {
			t.Errorf("%s: 未触发脱敏", c.name)
		}
		if got != c.out {
			t.Errorf("%s: got %q want %q", c.name, got, c.out)
		}
		if strings.Contains(got, "UTF-8 text file") || strings.Contains(got, "session skill catalog") {
			t.Errorf("%s: 仍含封禁短语: %q", c.name, got)
		}
	}
}
