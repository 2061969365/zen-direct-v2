package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 上游按客户端版本放行：2.1.1 在 npm 发布 2.2.0 之后会被判 FreeUsageLimitError。
func TestDefaultUAVersionPassesGate(t *testing.T) {
	if !newerSameMajor("2.1.1", uaDefaultVersion) {
		t.Fatalf("uaDefaultVersion=%q，仍会被上游门禁拒绝（2.1.1 已过期）", uaDefaultVersion)
	}
}

// 起手必须同步拿到版本：云端第一条请求只给几秒，异步刷新来不及。
func TestPrimeUAVersionUsesRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":"2.2.1"}`)
	}))
	defer srv.Close()

	oldURL, oldVer := uaRefreshURL, uaVersion()
	uaRefreshURL = srv.URL
	defer func() { uaRefreshURL = oldURL; zenUAVersion.Store(oldVer) }()

	if !primeUAVersion(2 * time.Second) {
		t.Fatal("primeUAVersion 应同步升级到 2.2.1")
	}
	if got := uaVersion(); got != "2.2.1" {
		t.Fatalf("uaVersion = %q, want 2.2.1", got)
	}
	if got := uaString(); got != "opencode/latest/2.2.1/cli" {
		t.Fatalf("uaString = %q", got)
	}
}

func TestPrimeUAVersionGivesUpOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, `{"version":"2.9.9"}`)
	}))
	defer srv.Close()

	oldURL, oldVer := uaRefreshURL, uaVersion()
	uaRefreshURL = srv.URL
	defer func() { uaRefreshURL = oldURL; zenUAVersion.Store(oldVer) }()

	start := time.Now()
	if primeUAVersion(150 * time.Millisecond) {
		t.Fatal("超时不该升级")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("primeUAVersion 耗时 %v，应按 timeout 返回", elapsed)
	}
	if got := uaVersion(); got != oldVer {
		t.Fatalf("uaVersion = %q，超时后应保持 %q", got, oldVer)
	}
}

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"2.1.1", [3]int{2, 1, 1}, true},
		{"v2.2.0", [3]int{2, 2, 0}, true},
		{"2.2.0-beta.1", [3]int{2, 2, 0}, true},
		{"2.10.3", [3]int{2, 10, 3}, true},
		{"2", [3]int{}, false},
		{"", [3]int{}, false},
		{"x.y.z", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

// 只跟同一主版本，且必须真的更新。
func TestNewerSameMajor(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"2.1.1", "2.2.0", true},
		{"2.1.1", "2.1.2", true},
		{"2.1.1", "2.1.0", false},
		{"2.1.1", "2.1.1", false},
		{"2.1.1", "3.0.0", false}, // 跨主版本不自动跟
		{"2.1.1", "garbage", false},
	}
	for _, c := range cases {
		if got := newerSameMajor(c.current, c.candidate); got != c.want {
			t.Errorf("newerSameMajor(%q,%q)=%v want %v", c.current, c.candidate, got, c.want)
		}
	}
}

// UA 形状必须和官方 CLI 一致：opencode/latest/<ver>/cli
func TestUAStringShape(t *testing.T) {
	t.Setenv("DIRECT_UA", "")
	t.Setenv("DIRECT_UA_VERSION", "")
	got := uaString()
	if !strings.HasPrefix(got, "opencode/latest/") || !strings.HasSuffix(got, "/cli") {
		t.Fatalf("UA 形状不对：%q", got)
	}
	if strings.Contains(got, uaDefaultVersion) == false {
		t.Errorf("应包含默认版本 %s：%q", uaDefaultVersion, got)
	}
}

// 完整 UA 覆盖优先，其次是只钉版本。
func TestUAStringOverrides(t *testing.T) {
	t.Setenv("DIRECT_UA", "opencode/latest/9.9.9/cli")
	t.Setenv("DIRECT_UA_VERSION", "2.2.0")
	if got := uaString(); got != "opencode/latest/9.9.9/cli" {
		t.Errorf("DIRECT_UA 应覆盖整串，得到 %q", got)
	}

	t.Setenv("DIRECT_UA", "")
	t.Setenv("DIRECT_UA_VERSION", "2.2.0")
	if got := uaString(); got != "opencode/latest/2.2.0/cli" {
		t.Errorf("DIRECT_UA_VERSION 应只换版本，得到 %q", got)
	}
}

// 版本号本身就是门禁：默认值不能退回到被拒过的 2.0.29。
func TestUADefaultAboveRejectedVersion(t *testing.T) {
	cur, _ := parseSemver(uaDefaultVersion)
	rejected, _ := parseSemver("2.0.29")
	if !newerSameMajor("2.0.29", uaDefaultVersion) || cur[1] <= rejected[1] {
		t.Fatalf("默认版本 %s 应高于被拒的 2.0.29", uaDefaultVersion)
	}
}
