// convert_stream_test.go: P1a 流式正确性单测，纯离线（httptest + 假 SSE 流）。
package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// oneByteReader 逐字节返回，证明 pumpSSE 块解析不依赖读边界。
type oneByteReader struct{ s string }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

func collectEvents(t *testing.T, s string) [][2]string {
	t.Helper()
	var out [][2]string
	pumpSSE(&oneByteReader{s: s}, func(event, data string) {
		out = append(out, [2]string{event, data})
	})
	return out
}

func TestPumpSSEBlockJoin(t *testing.T) {
	// 多 data 行 join、注释跳过、\r\n、event 派发，全走单字节读。
	raw := ": ping line\r\n" +
		"event: message_start\r\n" +
		"data: {\"a\":1}\r\n" +
		"\r\n" +
		"event: content_block_delta\r\n" +
		"data: {\"x\":\"中\"}\r\n" +
		"data: {\"y\":\"文\"}\r\n" +
		"\n"
	evs := collectEvents(t, raw)
	if len(evs) != 2 {
		t.Fatalf("应 2 帧，得 %d: %v", len(evs), evs)
	}
	if evs[0][0] != "message_start" || evs[0][1] != `{"a":1}` {
		t.Errorf("首帧错: %v", evs[0])
	}
	if evs[1][0] != "content_block_delta" || evs[1][1] != "{\"x\":\"中\"}\n{\"y\":\"文\"}" {
		t.Errorf("多 data join 错: %q", evs[1][1])
	}
}

func TestEmitEventLines(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := newSSESink(rec)
	sink.emit(map[string]any{"type": "message_start"})
	sink.emit(chatChunk("chatcmpl-1", "m", map[string]any{}, ""))
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start\n") {
		t.Errorf("anthropic 事件缺 event 行:\n%s", body)
	}
	if strings.Contains(body, "event: chat.completion") {
		t.Errorf("chat chunk 不应有 event 行:\n%s", body)
	}
	// responses 事件名应原样透传为 event 行。
	rec2 := httptest.NewRecorder()
	sink2 := newSSESink(rec2)
	sink2.emit(map[string]any{"type": "response.output_text.delta"})
	if !strings.Contains(rec2.Body.String(), "event: response.output_text.delta\n") {
		t.Errorf("responses 事件缺 event 行:\n%s", rec2.Body.String())
	}
}

func TestStreamChatToResponsesLengthIncomplete(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("length 应标 incomplete:\n%s", body)
	}
	if strings.Contains(body, "response.failed") {
		t.Errorf("正常终态不应 failed:\n%s", body)
	}
}

func TestStreamEmptyEOF(t *testing.T) {
	// 空流：chat->responses 应 failed 而非 completed。
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(""), "mimo-v2.5-free")
	if body := rec.Body.String(); !strings.Contains(body, "response.failed") {
		t.Errorf("空流应 failed:\n%s", body)
	}
	// 空流：messages->chat 应 error 而非 stop。
	rec2 := httptest.NewRecorder()
	streamMessagesToChat(rec2, strings.NewReader(""), "claude-sonnet-4-5")
	if body := rec2.Body.String(); !strings.Contains(body, "stream_error") {
		t.Errorf("空流应 stream_error:\n%s", body)
	}
}

func TestStreamResponsesToChatFailed(t *testing.T) {
	up := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, "stream_error") {
		t.Errorf("failed 应转 error:\n%s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("failed 不应补成功尾:\n%s", body)
	}
}

func TestStreamResponsesToChatIncomplete(t *testing.T) {
	up := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "muse-spark-1.3")
	if body := rec.Body.String(); !strings.Contains(body, `"finish_reason":"length"`) {
		t.Errorf("incomplete 应标 length:\n%s", body)
	}
}

func TestStreamResponsesToMessagesFailed(t *testing.T) {
	up := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("failed 应转 anthropic error:\n%s", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("failed 不应补 end_turn:\n%s", body)
	}
}

func TestStreamResponsesToMessagesIncomplete(t *testing.T) {
	up := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	if body := rec.Body.String(); !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Errorf("incomplete 应标 max_tokens:\n%s", body)
	}
}

func TestStreamMessagesToResponsesMaxTokens(t *testing.T) {
	up := "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("max_tokens 应标 incomplete:\n%s", body)
	}
	if !strings.Contains(body, "max_output_tokens") {
		t.Errorf("应带 incomplete_details:\n%s", body)
	}
}

// ---------- P2b：usage 常带 + 工具 id 保底 ----------

func TestP2bChatToResponsesUsageAlwaysPresent(t *testing.T) {
	// 上游全程无 usage，completed 仍须带零值 usage。
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"input_tokens":0`) || !strings.Contains(body, `"total_tokens":0`) {
		t.Errorf("completed 缺零值 usage:\n%s", body)
	}
}

func TestP2bMessagesToResponsesUsageAlwaysPresent(t *testing.T) {
	up := "event: message_start\ndata: {\"message\":{\"id\":\"m1\",\"model\":\"mm\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"input_tokens":0`) {
		t.Errorf("completed 缺零值 usage:\n%s", body)
	}
}

func TestP2bChatToMessagesDeltaUsageComplete(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"usage":{"input_tokens":0,"output_tokens":0}`) {
		t.Errorf("message_delta usage 形状不全:\n%s", body)
	}
}

func TestP2bResponsesToMessagesDeltaUsageComplete(t *testing.T) {
	up := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"usage":{"input_tokens":0,"output_tokens":0}`) {
		t.Errorf("message_delta usage 形状不全:\n%s", body)
	}
}

func TestP2bChatToMessagesToolFallbackID(t *testing.T) {
	// tool_calls 有名无 id：终态须 fallback 开块，不静默丢。
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"call_0"`) {
		t.Errorf("缺 fallback id:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("应 tool_use 收尾:\n%s", body)
	}
}

func TestP2bChatToMessagesToolDroppedFailed(t *testing.T) {
	// finish 宣称 tool_calls 却零 tool 增量：须 error，不伪造成功。
	up := "data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, "upstream tool call dropped") {
		t.Errorf("丢光应 failed:\n%s", body)
	}
	if strings.Contains(body, "event: message_delta") {
		t.Errorf("failed 不应补 message_delta:\n%s", body)
	}
}

func TestP2bResponsesToMessagesToolFallbackID(t *testing.T) {
	up := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"f\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"call_1"`) {
		t.Errorf("缺 fallback id:\n%s", body)
	}
	if strings.Contains(body, `"id":""`) {
		t.Errorf("不应有空 id:\n%s", body)
	}
}

func TestP2bChatToResponsesToolFallbackAndDrop(t *testing.T) {
	// a. 有名无 id：completed 输出须带 fallback call_id。
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"call_id":"call_2"`) {
		t.Errorf("缺 fallback call_id:\n%s", body)
	}
	// b. 宣称 tool 却零增量：failed。
	rec2 := httptest.NewRecorder()
	streamChatToResponses(rec2,
		strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"), "mimo-v2.5-free")
	if body := rec2.Body.String(); !strings.Contains(body, "upstream tool call dropped") {
		t.Errorf("丢光应 failed:\n%s", body)
	}
}

func TestP2bMessagesToResponsesToolFallbackID(t *testing.T) {
	up := "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"\",\"name\":\"f\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"call_id":"call_0"`) {
		t.Errorf("缺 fallback call_id:\n%s", body)
	}
	if strings.Contains(body, `"id":"fc_"`) {
		t.Errorf("不应有裸 fc_ 前缀:\n%s", body)
	}
}

func TestP2bNonStreamToolFallbackIDs(t *testing.T) {
	// responses->chat：双空 id（返回 chat envelope，经 choices 导航）。
	out := responsesToChatResp(mustJSON(t, `{"output":[{"type":"function_call","name":"f","arguments":"{}"}]}`), "m")
	tc := asMap(asArr(asMap(asMap(asArr(out["choices"])[0])["message"])["tool_calls"])[0])
	if got := getStr(tc, "id"); got == "" {
		t.Errorf("responses->chat 空 id 未 fallback")
	}
	// chat->responses：空 id。
	out2 := chatToResponsesResp(mustJSON(t, `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), "m")
	if !strings.Contains(canon(out2), `"id":"fc_call_0"`) {
		t.Errorf("chat->responses 空 id 未 fallback: %s", canon(out2))
	}
	// chat->messages：空 id。
	out3 := chatToMessagesResp(mustJSON(t, `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), "m")
	blocks := asArr(out3["content"])
	if got := getStr(asMap(blocks[0]), "id"); got == "" {
		t.Errorf("chat->messages 空 id 未 fallback: %s", canon(blocks))
	}
	// messages->chat：空 id。
	out4 := messagesToChatResp(mustJSON(t, `{"content":[{"type":"tool_use","name":"f","input":{}}]}`), "m")
	mtc := asMap(asArr(asMap(asMap(asArr(out4["choices"])[0])["message"])["tool_calls"])[0])
	if got := getStr(mtc, "id"); got == "" {
		t.Errorf("messages->chat 空 id 未 fallback")
	}
	// responses->messages：双空 id。
	out5 := responsesToMessagesResp(mustJSON(t, `{"output":[{"type":"function_call","name":"f","arguments":"{}"}]}`), "m")
	if got := getStr(asMap(asArr(out5["content"])[0]), "id"); got == "" {
		t.Errorf("responses->messages 空 id 未 fallback")
	}
	// messages->responses：空 id。
	out6 := messagesToResponsesResp(mustJSON(t, `{"content":[{"type":"tool_use","name":"f","input":{}}]}`), "m")
	if !strings.Contains(canon(out6), `"call_id":"call_0"`) {
		t.Errorf("messages->responses 空 id 未 fallback: %s", canon(out6))
	}
}

// 多个并行工具调用时，每个 item 的 added / delta / done 必须报同一个
// output_index，否则客户端按 index 关联会串到别的工具上。
//
// 2026-11-16 实测回归：done 事件曾硬编码 output_index:0，两个工具调用时
// added 分别是 1/2，done 全是 0，客户端无法把结果对应回工具。
func TestChatToResponsesMultiToolOutputIndex(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"pwsh\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")

	// 每个 call_id 收集它在 added / arguments.delta / arguments.done /
	// output_item.done 里出现过的 output_index，必须唯一。
	seen := map[string]map[int]bool{}
	collect := func(callID string, index int) {
		if callID == "" {
			return
		}
		if seen[callID] == nil {
			seen[callID] = map[int]bool{}
		}
		seen[callID][index] = true
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		index := int(toFloat(ev["output_index"]))
		if item, ok := ev["item"].(map[string]any); ok {
			collect(asStr(item["call_id"]), index)
		}
		// function_call_arguments.* 只带 item_id（"fc_<call_id>"）。
		if id := asStr(ev["item_id"]); strings.HasPrefix(id, "fc_") {
			collect(strings.TrimPrefix(id, "fc_"), index)
		}
	}
	for _, callID := range []string{"call_a", "call_b"} {
		indices := seen[callID]
		if len(indices) == 0 {
			t.Errorf("%s 没有任何带 index 的事件", callID)
			continue
		}
		if len(indices) != 1 {
			t.Errorf("%s 的 output_index 不唯一：%v（客户端会串到别的工具）", callID, indices)
		}
	}
	// 两个工具必须占不同位置。
	if len(seen["call_a"]) == 1 && len(seen["call_b"]) == 1 {
		var a, b int
		for v := range seen["call_a"] {
			a = v
		}
		for v := range seen["call_b"] {
			b = v
		}
		if a == b {
			t.Errorf("两个工具占了同一个 output_index=%d", a)
		}
	}
}

// reasoning 的 output_index 必须稳定：它不能随工具数量变化。
func TestChatToResponsesReasoningIndexStable(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"b\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	indices := map[int]bool{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if asStr(ev["type"]) != "response.reasoning_summary_text.delta" {
			continue
		}
		indices[int(toFloat(ev["output_index"]))] = true
	}
	if len(indices) != 1 {
		t.Errorf("reasoning 的 output_index 不唯一：%v（工具到达前后应保持一致）", indices)
	}
}



// messages 上游的多个并行 tool_use 块必须各自独立累积参数。
//
// 2026-11-16 实测回归：参数曾用单个 strings.Builder + 单个 itemID 保存，两个
// 并行 tool_use 时第二个拿到拼接后的脏参数（{"path":"ls"}"a.txt"}），第一个
// 完全消失，且两个块的 output_index 都是 0。
func TestMessagesToResponsesParallelToolArgs(t *testing.T) {
	up := "event: message_start\ndata: {\"message\":{\"model\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_a\",\"name\":\"pwsh\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\"}}\n\n" +
		"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_b\",\"name\":\"read\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"ls\\\"}\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.txt\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"index\":0}\n\n" +
		"event: content_block_stop\ndata: {\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")

	args := map[string]string{}
	indices := map[string]int{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		item, ok := ev["item"].(map[string]any)
		if !ok {
			continue
		}
		if asStr(item["type"]) != "function_call" {
			continue
		}
		callID := asStr(item["call_id"])
		if callID == "" {
			continue
		}
		if a := asStr(item["arguments"]); a != "" {
			args[callID] = a
		}
		if _, seen := indices[callID]; !seen {
			indices[callID] = int(toFloat(ev["output_index"]))
		}
	}
	want := map[string]string{"tu_a": `{"cmd":"ls"}`, "tu_b": `{"path":"a.txt"}`}
	for callID, expected := range want {
		if args[callID] != expected {
			t.Errorf("%s 的参数是 %q，期望 %q", callID, args[callID], expected)
		}
	}
	if indices["tu_a"] == indices["tu_b"] {
		t.Errorf("两个工具占了同一个 output_index=%d", indices["tu_a"])
	}
}

// 单个 tool_use 时参数与 item 信息仍须完整（不能因为改成 map 就丢字段）。
func TestMessagesToResponsesSingleToolComplete(t *testing.T) {
	up := "event: message_start\ndata: {\"message\":{\"model\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_x\",\"name\":\"grep\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `{\"q\":\"x\"}`) {
		t.Errorf("参数丢失:\n%s", body)
	}
	if !strings.Contains(body, `"name":"grep"`) {
		t.Errorf("工具名丢失:\n%s", body)
	}
	if !strings.Contains(body, `"call_id":"tu_x"`) {
		t.Errorf("call_id 丢失:\n%s", body)
	}
}


// 上游不发 arguments（无参工具）时，function_call.arguments 必须给 "{}"。
// 空串会让部分客户端 JSON 解析失败。
func TestChatToResponsesEmptyArgumentsBecomeObject(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	// added 事件的空 arguments 表示"开始"，是正确语义；只要求 done 与最终 output 给 {}。
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "output_item.added") {
			continue
		}
		if strings.Contains(line, `"arguments":""`) {
			t.Errorf("非 added 事件里出现空 arguments，应为 {}：%s", line)
		}
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Errorf("缺少 arguments:{}：\n%s", body)
	}
}

// 工具缺 name / 缺 id 时，流式阶段仍要发出 added/done 事件。
// 否则客户端在流式过程中看不到这个工具，只能从最终 output 里发现，已错过调用时机。
func TestChatToResponsesMissingNameOrIDStillEmits(t *testing.T) {
	cases := []struct {
		name      string
		up        string
		wantName  string
		wantCall  string
	}{
		{
			name:     "缺 name",
			up:       "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
			wantName: "unknown_tool",
			wantCall: "c1",
		},
		{
			name:     "缺 id",
			up:       "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
			wantName: "f",
			wantCall: "call_0",
		},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		streamChatToResponses(rec, strings.NewReader(c.up), "mimo-v2.5-free")
		body := rec.Body.String()
		if !strings.Contains(body, `"type":"response.output_item.added"`) {
			t.Errorf("%s: 缺 output_item.added：\n%s", c.name, body)
		}
		if !strings.Contains(body, `"type":"response.output_item.done"`) {
			t.Errorf("%s: 缺 output_item.done：\n%s", c.name, body)
		}
		if !strings.Contains(body, `"name":"`+c.wantName+`"`) {
			t.Errorf("%s: 缺 name=%s：\n%s", c.name, c.wantName, body)
		}
		if !strings.Contains(body, `"call_id":"`+c.wantCall+`"`) {
			t.Errorf("%s: 缺 call_id=%s：\n%s", c.name, c.wantCall, body)
		}
	}
}

