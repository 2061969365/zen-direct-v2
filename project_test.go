package main

import (
	"net/http"
	"strings"
	"testing"
)

// 没声明 project 时必须现签：两次调用拿到两个不同的 40 位 hex。
// 这是免费层额度分桶的前提，退化成常量就等于回到共用公共桶。
func TestUpstreamProjectIDFresh(t *testing.T) {
	a := upstreamProjectID("")
	b := upstreamProjectID("")
	if !gitCommitPattern.MatchString(a) || !gitCommitPattern.MatchString(b) {
		t.Fatalf("现签的 project id 形状不对：%q / %q", a, b)
	}
	if a == b {
		t.Fatalf("两次应现签不同 id，得到同一个 %q", a)
	}
}

// "global" 是所有人共用的桶，不能原样发出去。
func TestUpstreamProjectIDGlobalMintsFresh(t *testing.T) {
	got := upstreamProjectID("global")
	if got == "global" {
		t.Fatal("global 是公共桶，应改签新号")
	}
	if !gitCommitPattern.MatchString(got) {
		t.Fatalf("应签成 40 位 hex，得到 %q", got)
	}
	if again := upstreamProjectID("global"); again == got {
		t.Errorf("每次都应现签：%q 重复", got)
	}
}

// 真实 git root commit 必须原样保留（客户端自己的项目身份）。
func TestUpstreamProjectIDCommitVerbatim(t *testing.T) {
	const commit = "9f592410000000000000000000000000000000ab"
	if got := upstreamProjectID(commit); got != commit {
		t.Errorf("40 位 hex 应原样保留：got %q want %q", got, commit)
	}
}

// 其余值（尤其 prj_*）要哈希成 40 位 hex，且同一个值稳定映射。
func TestUpstreamProjectIDHashed(t *testing.T) {
	for _, in := range []string{"my-project", "prj_abc123", "Global", "a b"} {
		got := upstreamProjectID(in)
		if !gitCommitPattern.MatchString(got) {
			t.Fatalf("%q 应哈希成 40 位 hex，得到 %q", in, got)
		}
		if got == in {
			t.Errorf("%q 不应原样发出", in)
		}
		if again := upstreamProjectID(in); again != got {
			t.Errorf("%q 映射应稳定：%q != %q", in, got, again)
		}
	}
}

// 上游头必须带上归一后的 project；客户端声明的 prj_* 不能漏成原样。
func TestSetZenHeadersRendersProject(t *testing.T) {
	h := http.Header{}
	setZenHeaders(h, "public", "")
	got := h.Get("X-Opencode-Project")
	if !gitCommitPattern.MatchString(got) {
		t.Fatalf("未声明时应现签 40 位 hex，得到 %q", got)
	}
	if got == "global" {
		t.Fatal("不能发 global（公共桶）")
	}

	h2 := http.Header{}
	h2.Set("X-Opencode-Project", "prj_deadbeef")
	setZenHeaders(h2, "public", "")
	if v := h2.Get("X-Opencode-Project"); v == "prj_deadbeef" || !gitCommitPattern.MatchString(v) {
		t.Fatalf("prj_* 应被哈希成 40 位 hex，得到 %q", v)
	}

	const commit = "0123456789abcdef0123456789abcdef01234567"
	h3 := http.Header{}
	h3.Set("X-Opencode-Project", commit)
	setZenHeaders(h3, "public", "")
	if v := h3.Get("X-Opencode-Project"); v != commit {
		t.Errorf("客户端声明的真实 commit 应保留：got %q want %q", v, commit)
	}
}

// 显式配置了 OPENCODE_ZEN_PROJECT 时，没声明的客户端应该用配置值（哈希后稳定）。
func TestSetZenHeadersUsesConfiguredProject(t *testing.T) {
	h := http.Header{}
	setZenHeaders(h, "public", "team-bucket")
	v := h.Get("X-Opencode-Project")
	if !gitCommitPattern.MatchString(v) {
		t.Fatalf("配置值应哈希成 40 位 hex，得到 %q", v)
	}
	if v != upstreamProjectID("team-bucket") {
		t.Error("应与配置值的哈希一致")
	}
	if !strings.HasPrefix(v, upstreamProjectID("team-bucket")[:8]) {
		t.Error("哈希前缀应一致")
	}
}
