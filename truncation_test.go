package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chat 入口的截断必须发 chat 形状错误帧：保留 choices 数组，带 error.code。
// 旧的 Responses 形状（无 choices）会让双协议 union 客户端直接报 schema 错。
func TestPipeSSEChatEntryTruncationUsesChatFrame(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	rec := httptest.NewRecorder()
	pipeSSEFor(rec, strings.NewReader(up), "req_chat_trunc", "m", 0, nil, FmtChat)
	body := rec.Body.String()
	if strings.Contains(body, "event: response.failed") {
		t.Fatalf("chat 入口不应发 Responses 事件:\n%s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("缺 chat chunk 形状:\n%s", body)
	}
	if !strings.Contains(body, `"choices":[]`) {
		t.Fatalf("缺 choices 数组（union 校验需要）:\n%s", body)
	}
	if !strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("缺 error.code:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("应以 [DONE] 收尾:\n%s", body)
	}
	if !strings.Contains(body, `"content":"Hi"`) {
		t.Fatalf("已收到的内容不应丢失:\n%s", body)
	}
}

// 带工具重命名时 reader 被 renameStreamReader 包装，超时分类仍须准确
// （旧实现用具体类型断言，带工具的真实请求会恒报 upstream_truncated）。
func TestPipeSSEIdleTimeoutSurvivesRenameWrapper(t *testing.T) {
	t.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", "3600")
	pr, pw := io.Pipe()
	ir := newIdleTimeoutBody(pr, 20*time.Millisecond)
	defer ir.Close()
	defer pw.Close()
	renames := NewToolRenames()
	renames.Add("my_tool", "clint_deadbeef0001")
	reader := NewRenameStreamReader(ir, FmtChat, renames)
	go func() {
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		time.Sleep(2 * time.Second)
	}()
	rec := httptest.NewRecorder()
	pipeSSEFor(rec, reader, "req_wrap", "m", 0, nil, FmtChat)
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_timeout"`) {
		t.Fatalf("空闲超时应报 upstream_timeout:\n%s", body)
	}
	if strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("超时不得误标为 truncated:\n%s", body)
	}
}

// 任何非 null finish_reason 都算终态，不能被误判成截断。
func TestPipeSSEFinishReasonVariantsAreTerminal(t *testing.T) {
	cases := []string{
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
		`{"choices":[{"delta":{},"finish_reason": "stop"}]}`,
		`{"choices":[{"delta":{},"finish_reason":"insufficient_system_resource"}]}`,
		`{"choices":[{"delta":{},"finish_reason":"function_call"}]}`,
	}
	for _, payload := range cases {
		rec := httptest.NewRecorder()
		pipeSSEFor(rec, strings.NewReader("data: "+payload+"\n\n"), "req_fr", "m", 0, nil, FmtChat)
		body := rec.Body.String()
		if strings.Contains(body, "upstream_truncated") {
			t.Errorf("finish_reason 已给却被判截断: %s\n%s", payload, body)
		}
		if !strings.Contains(body, "[DONE]") {
			t.Errorf("终态后应补 [DONE]: %s\n%s", payload, body)
		}
	}
}

// 上游自带的 error 帧就是终态：原样转发，不再追加第二帧。
func TestPipeSSEUpstreamErrorFrameNotDuplicated(t *testing.T) {
	up := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n"
	rec := httptest.NewRecorder()
	pipeSSEFor(rec, strings.NewReader(up), "req_err", "m", 0, nil, FmtChat)
	body := rec.Body.String()
	if strings.Contains(body, "upstream_truncated") {
		t.Fatalf("上游 error 后不应再追加伪造错误:\n%s", body)
	}
	if n := strings.Count(body, `"type":"error"`); n != 1 {
		t.Fatalf("error 帧应恰好一帧，实际 %d:\n%s", n, body)
	}
}

// responses 入口的 failed 帧要补规范必填字段与 code，严格 schema 客户端可解析。
func TestPipeSSEResponsesFailedFrameIsComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	pipeSSEFor(rec, strings.NewReader("data: {\"type\":\"response.output_text.delta\"}\n\n"), "req_resp", "m", 0, nil, FmtResponses)
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"response.failed"`,
		`"object":"response"`,
		`"created_at":`,
		`"output":[]`,
		`"usage":{}`,
		`"code":"upstream_truncated"`,
		`"id":"resp_`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("failed 帧缺 %s:\n%s", want, body)
		}
	}
}

// responses 入口（chat 上游）截断且已有输出：必须 failed，绝不 completed。
func TestStreamChatToResponsesTruncationIsFailedNotCompleted(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "deepseek-x")
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("无终态 EOF 应 failed:\n%s", body)
	}
	if strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("截断不得谎报 completed:\n%s", body)
	}
	if !strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("failed 应带 code:\n%s", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Fatalf("已生成内容应保留在 partial output:\n%s", body)
	}
}

// chat 入口（responses 上游）截断且已有输出：必须 chat error，不补 stop。
func TestStreamResponsesToChatTruncationIsError(t *testing.T) {
	up := "data: {\"type\":\"response.created\",\"response\":{\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "gpt-5")
	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("截断不得补 stop:\n%s", body)
	}
	if !strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("应发可重试 chat 错误:\n%s", body)
	}
	if !strings.Contains(body, `"choices":[]`) {
		t.Fatalf("chat 错误帧应保留 choices:\n%s", body)
	}
}
