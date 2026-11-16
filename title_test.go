package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestIsTitleGenerationRequest(t *testing.T) {
	v2 := mustJSON(t, `{"model":"m","messages":[
		{"role":"system","content":"You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else."},
		{"role":"user","content":"Fix the login bug"}]}`)
	if !isTitleGenerationRequest(v2) {
		t.Fatal("v2 标题请求未识别")
	}
	v1 := mustJSON(t, `{"model":"m","messages":[{"role":"user","content":"Generate a title for this conversation: user asked about login"}]}`)
	if !isTitleGenerationRequest(v1) {
		t.Fatal("v1 标题请求未识别")
	}
	probe := mustJSON(t, `{"model":"m","messages":[{"role":"system","content":"You are opencode, a coding agent. Answer concisely."},{"role":"user","content":"用一句话说明你能做什么。"}]}`)
	if isTitleGenerationRequest(probe) {
		t.Fatal("runner 探针被误判为标题")
	}
	plain := mustJSON(t, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	if isTitleGenerationRequest(plain) {
		t.Fatal("普通无工具请求被误判为标题")
	}
	withTools := mustJSON(t, `{"model":"m","tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"messages":[{"role":"system","content":"You generate a descriptive title for this conversation."},{"role":"user","content":"hi"}]}`)
	if isTitleGenerationRequest(withTools) {
		t.Fatal("带工具的请求不应判为标题")
	}
	withAssistant := mustJSON(t, `{"model":"m","messages":[{"role":"system","content":"You generate a descriptive title for this conversation."},{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`)
	if isTitleGenerationRequest(withAssistant) {
		t.Fatal("含 assistant 轮次的请求不应判为标题")
	}
}

func TestSynthesizeTitle(t *testing.T) {
	payload := mustJSON(t, `{"messages":[{"role":"system","content":"x"},{"role":"user","content":"Fix the login redirect bug in the auth middleware now"}]}`)
	title := synthesizeTitle(payload)
	if title == "" || len([]rune(title)) > 40 {
		t.Fatalf("标题长度异常: %q", title)
	}
	if len(strings.Fields(title)) > 6 {
		t.Fatalf("标题词数超过 6: %q", title)
	}
	if strings.HasSuffix(title, " in") || strings.HasSuffix(title, " the") {
		t.Fatalf("标题尾部停用词未去除: %q", title)
	}
	if got := synthesizeTitle(mustJSON(t, `{"messages":[{"role":"user","content":""}]}`)); got != "New Session" {
		t.Fatalf("空会话标题: %q", got)
	}
}

func TestWriteSynthesizedTitle(t *testing.T) {
	payload := mustJSON(t, `{"messages":[{"role":"user","content":"Fix the login bug"}]}`)
	rec := httptest.NewRecorder()
	writeSynthesizedTitle(rec, "m", false, payload)
	if rec.Code != 200 {
		t.Fatalf("http=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	content := asStr(asMap(asMap(asArr(body["choices"])[0])["message"])["content"])
	if content != "Fix the login bug" {
		t.Fatalf("title=%q", content)
	}
	streamRec := httptest.NewRecorder()
	writeSynthesizedTitle(streamRec, "m", true, payload)
	if !strings.Contains(streamRec.Body.String(), "[DONE]") || !strings.Contains(streamRec.Body.String(), "Fix the login bug") {
		t.Fatalf("SSE 标题不完整: %s", streamRec.Body.String())
	}
}

func TestTitleRequest429SynthesizesWithoutPoisoning(t *testing.T) {
	resetPoolForTest()
	freeLimitUntil.Store(0)
	var calls atomic.Int64
	var mu sync.Mutex
	upBody := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		upBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, freeLimitBody)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	body := `{"model":"m","stream":false,"messages":[
		{"role":"system","content":"You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else."},
		{"role":"user","content":"Fix the login bug"}]}`
	code, resp := postChat(t, "http://x", body)
	if code != 200 || !strings.Contains(resp, "Fix the login bug") {
		t.Fatalf("标题 429 应合成 200，得 %d %.200s", code, resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("标题请求不应重试，上游调用 %d 次", calls.Load())
	}
	mu.Lock()
	seen := upBody
	mu.Unlock()
	if strings.Contains(seen, `"tools"`) {
		t.Fatalf("标题请求不应注入 tools: %.400s", seen)
	}
	if _, blocked := freeLimitBlocked(); blocked {
		t.Fatal("标题 429 不应进入免费额度冷却")
	}
}

func TestNonTitleErrorIsNotSynthesized(t *testing.T) {
	resetPoolForTest()
	freeLimitUntil.Store(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	code, _ := postChat(t, "http://x", `{"model":"m","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	if code != 500 {
		t.Fatalf("普通请求应透传 500，得 %d", code)
	}
}
