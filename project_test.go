package main

import (
	"net/http"
	"testing"
)

// 客户端声明的 project 按形状归一：官方两种真实取值原样，其余哈希成 40 位 hex。
func TestRenderedProjectID(t *testing.T) {
	const commit = "9f592410000000000000000000000000000000ab"
	cases := []struct {
		in        string
		want      string
		wantHex40 bool
	}{
		{"", "", false},
		{"global", "global", false},
		{commit, commit, false},
		{"my-project", "", true},
		{"prj_abc123", "", true},
		{"Global", "", true},
	}
	for _, c := range cases {
		got := renderedProjectID(c.in)
		if c.wantHex40 {
			if !gitCommitPattern.MatchString(got) {
				t.Fatalf("%q 应哈希成 40 位 hex，得到 %q", c.in, got)
			}
			if again := renderedProjectID(c.in); again != got {
				t.Fatalf("%q 映射应稳定：%q != %q", c.in, got, again)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%q 应原样保留，得到 %q", c.in, got)
		}
	}
}

// 现签：两次必须不同，且都是 40 位小写 hex。
func TestRandomProjectIDFresh(t *testing.T) {
	a, b := randomProjectID(), randomProjectID()
	if !gitCommitPattern.MatchString(a) || !gitCommitPattern.MatchString(b) {
		t.Fatalf("形状不对：%q / %q", a, b)
	}
	if a == b {
		t.Fatalf("两次应不同，得到同一个 %q", a)
	}
}

// 未配置、客户端也没声明时：每次请求现签一个新号。
func TestSetZenHeadersMintsProject(t *testing.T) {
	h := http.Header{}
	setZenHeaders(h, "public", "")
	first := h.Get("X-Opencode-Project")
	if !gitCommitPattern.MatchString(first) {
		t.Fatalf("应现签 40 位 hex，得到 %q", first)
	}

	h2 := http.Header{}
	setZenHeaders(h2, "public", "")
	if second := h2.Get("X-Opencode-Project"); second == first {
		t.Fatalf("两次请求应换桶，得到同一个 %q", first)
	}
}

// 显式配置 OPENCODE_ZEN_PROJECT 时原样发送（保留回退到 global 的能力）。
func TestSetZenHeadersConfiguredVerbatim(t *testing.T) {
	h := http.Header{}
	setZenHeaders(h, "public", "global")
	if got := h.Get("X-Opencode-Project"); got != "global" {
		t.Errorf("显式配置应原样发送：got %q want global", got)
	}

	const fixed = "0123456789abcdef0123456789abcdef01234567"
	h2 := http.Header{}
	setZenHeaders(h2, "public", fixed)
	if got := h2.Get("X-Opencode-Project"); got != fixed {
		t.Errorf("显式配置的 40 位 hex 应原样发送：got %q", got)
	}
}

// 客户端声明的 prj_* 不能漏成原样（v2 门禁会 403）。
func TestSetZenHeadersNormalizesClientProject(t *testing.T) {
	h := http.Header{}
	h.Set("X-Opencode-Project", "prj_deadbeef")
	setZenHeaders(h, "public", "")
	if got := h.Get("X-Opencode-Project"); got == "prj_deadbeef" || !gitCommitPattern.MatchString(got) {
		t.Fatalf("prj_* 应哈希成 40 位 hex，得到 %q", got)
	}

	const commit = "0123456789abcdef0123456789abcdef01234567"
	h2 := http.Header{}
	h2.Set("X-Opencode-Project", commit)
	setZenHeaders(h2, "public", "")
	if got := h2.Get("X-Opencode-Project"); got != commit {
		t.Errorf("客户端真实 commit 应保留：got %q want %q", got, commit)
	}
}

// 客户端声明优先于显式配置。
func TestSetZenHeadersClientWins(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	h := http.Header{}
	h.Set("X-Opencode-Project", commit)
	setZenHeaders(h, "public", "global")
	if got := h.Get("X-Opencode-Project"); got != commit {
		t.Errorf("客户端声明应优先：got %q want %q", got, commit)
	}
}
