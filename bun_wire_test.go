package main

import (
	"net/http"
	"strings"
	"testing"
)

func newHeadReq(t *testing.T, url string, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// fetch 的写法：WebKit 已知头按固定顺序与拼写在前，其余小写排序在后，
// 然后是 Connection / Host / Accept-Encoding / Content-Length。
func TestBunHeadOrder(t *testing.T) {
	req := newHeadReq(t, "https://opencode.ai/inference/v1/chat/completions", map[string]string{
		"Authorization":      "Bearer public",
		"Content-Type":       "application/json",
		"User-Agent":         "opencode/latest/2.0.29/cli",
		"X-Opencode-Session": "ses_abc",
		"X-Session-Id":       "ses_abc",
	})
	head, added, err := bunHead(req, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("bunHead: %v", err)
	}
	if !added {
		t.Error("没有 Accept-Encoding 时应由本层补，addedEncoding 应为 true")
	}
	got := string(head)
	lines := strings.Split(strings.TrimSuffix(got, "\r\n\r\n"), "\r\n")
	if len(lines) < 8 {
		t.Fatalf("头太短：%q", got)
	}
	if lines[0] != "POST /inference/v1/chat/completions HTTP/1.1" {
		t.Errorf("请求行 = %q", lines[0])
	}
	// 已知头的相对顺序：Authorization < Content-Length < Content-Type < User-Agent
	idx := func(prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		return -1
	}
	order := []string{"Authorization:", "Content-Length:", "Content-Type:", "User-Agent:"}
	last := -1
	for _, name := range order {
		i := idx(name)
		if i < 0 {
			t.Fatalf("缺少 %s：%q", name, got)
		}
		if i < last {
			t.Errorf("%s 位置不对（%d < %d）：%q", name, i, last, got)
		}
		last = i
	}
	// 其余头：小写 + 字典序。
	if i, j := idx("x-opencode-session:"), idx("x-session-id:"); i < 0 || j < 0 || i > j {
		t.Errorf("自定义头应为小写且按字典序：%q", got)
	}
	if !strings.Contains(got, "\r\nConnection: keep-alive\r\n") {
		t.Errorf("缺少 Connection: keep-alive：%q", got)
	}
	if !strings.Contains(got, "\r\nAccept-Encoding: gzip, deflate, br, zstd\r\n") {
		t.Errorf("缺少 fetch 默认 Accept-Encoding：%q", got)
	}
	if !strings.Contains(got, "\r\nHost: opencode.ai\r\n") {
		t.Errorf("Host 应去掉默认端口：%q", got)
	}
	if !strings.Contains(got, "\r\nAccept: */*\r\n") {
		t.Errorf("调用方没给 Accept 时应补默认值：%q", got)
	}
}

// 调用方自己给了 Accept-Encoding，就原样透传、不由本层解压。
func TestBunHeadKeepsCallerEncoding(t *testing.T) {
	req := newHeadReq(t, "https://opencode.ai/inference/v1/chat/completions", map[string]string{
		"Accept-Encoding": "gzip",
		"User-Agent":      "opencode/latest/2.0.29/cli",
		"Accept":          "*/*",
	})
	head, added, err := bunHead(req, nil)
	if err != nil {
		t.Fatalf("bunHead: %v", err)
	}
	if added {
		t.Error("调用方给了 Accept-Encoding，addedEncoding 应为 false")
	}
	got := string(head)
	if !strings.Contains(got, "\r\nAccept-Encoding: gzip\r\n") {
		t.Errorf("应保留调用方的值：%q", got)
	}
	if strings.Contains(got, "Content-Length") {
		t.Errorf("无 body 不该写 Content-Length：%q", got)
	}
}

// 默认 UA 只在调用方没给时补，且位置在 Connection 之后。
func TestBunHeadDefaultUserAgent(t *testing.T) {
	req := newHeadReq(t, "https://opencode.ai/x", nil)
	head, _, err := bunHead(req, nil)
	if err != nil {
		t.Fatalf("bunHead: %v", err)
	}
	got := string(head)
	conn := strings.Index(got, "Connection: keep-alive\r\n")
	ua := strings.Index(got, "User-Agent: "+bunDefaultUA+"\r\n")
	if ua < 0 || conn < 0 || ua < conn {
		t.Errorf("默认 UA 位置不对：%q", got)
	}
	if !strings.Contains(got, "\r\nAccept: */*\r\n") {
		t.Errorf("应补默认 Accept：%q", got)
	}
}

// 换行注入必须被拒。
func TestBunHeadRejectsInjection(t *testing.T) {
	req := newHeadReq(t, "https://opencode.ai/x", nil)
	req.Header["X-Bad"] = []string{"a\r\nX-Evil: 1"}
	if _, _, err := bunHead(req, nil); err == nil {
		t.Fatal("带 CRLF 的头值应被拒绝")
	}
}
