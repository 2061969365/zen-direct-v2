package main

// 跨路径一致性检查。
//
// 背景：工具调用的状态管理在四条转换路径里各写一遍，历史上同一类错误反复出现——
// v0.9.19 修了 chat→responses 的 output_index 硬编码，v0.9.20 修了
// messages→responses 的共享 builder。这类 bug 的共同点是"状态没按工具 index 隔离"，
// 且只在多工具并行时才暴露。
//
// 这个文件用同一组语义等价的 fixture 跑遍所有路径，断言工具调用事件的结构不变量：
//   - 每个工具的参数互不污染
//   - 每个工具占独立的 output_index / content block index
//   - 工具名、call_id 完整不丢
//   - arguments 是合法 JSON（空值给 {}）
//
// 加新转换路径时，把它的 fixture 加进来即可复用这些断言。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// toolCallFact 是一条转换路径产出的单个工具调用事实，供跨路径比对。
type toolCallFact struct {
	name      string
	callID    string
	arguments string
	// slot 是这个工具在该协议里的位置标识：responses 的 output_index、
	// messages 的 content block index。同一次转换里必须互不相同。
	slot int
}

// collectResponsesToolCalls 从 responses 协议的事件流里提取工具调用事实。
//
// 参数来源优先级：function_call_arguments.done > output_item.done > response.completed
// 的 output。output_item.added 的 arguments 是空串（表示"开始"），不能当最终值。
func collectResponsesToolCalls(t *testing.T, body string) []toolCallFact {
	t.Helper()
	type acc struct {
		name      string
		arguments string
		slot      int
	}
	order := []string{}
	calls := map[string]*acc{}
	set := func(callID, name, arguments string, slot int) {
		if callID == "" {
			return
		}
		c, exists := calls[callID]
		if !exists {
			c = &acc{slot: slot}
			calls[callID] = c
			order = append(order, callID)
		}
		if name != "" {
			c.name = name
		}
		// 只接受非空参数；added 事件的空串不覆盖已有的完整值。
		if arguments != "" {
			c.arguments = arguments
		}
		if slot >= 0 {
			if c.slot >= 0 && c.slot != slot {
				t.Errorf("工具 %s 的 output_index 前后不一致：%d → %d", callID, c.slot, slot)
			}
			c.slot = slot
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch asStr(ev["type"]) {
		case "response.output_item.added", "response.output_item.done":
			item, ok := ev["item"].(map[string]any)
			if !ok || asStr(item["type"]) != "function_call" {
				continue
			}
			set(asStr(item["call_id"]), asStr(item["name"]),
				asStr(item["arguments"]), int(toFloat(ev["output_index"])))
		case "response.function_call_arguments.done":
			itemID := asStr(ev["item_id"])
			callID := strings.TrimPrefix(itemID, "fc_")
			set(callID, "", asStr(ev["arguments"]), int(toFloat(ev["output_index"])))
		case "response.completed", "response.incomplete":
			resp, ok := ev["response"].(map[string]any)
			if !ok {
				continue
			}
			for _, raw := range asArr(resp["output"]) {
				item := asMap(raw)
				if asStr(item["type"]) != "function_call" {
					continue
				}
				// output 是数组，下标不等于 output_index（后者把 message、
				// reasoning 也算进位置），这里只取名字与参数，不动 slot。
				set(asStr(item["call_id"]), asStr(item["name"]),
					asStr(item["arguments"]), -1)
			}
		}
	}
	facts := make([]toolCallFact, 0, len(order))
	for _, id := range order {
		c := calls[id]
		facts = append(facts, toolCallFact{
			name: c.name, callID: id, arguments: c.arguments, slot: c.slot,
		})
	}
	return facts
}

// collectMessagesToolCalls 从 messages 协议的事件流里提取工具调用事实。
func collectMessagesToolCalls(t *testing.T, body string) []toolCallFact {
	t.Helper()
	type acc struct {
		name string
		args strings.Builder
		slot int
	}
	order := []string{}
	calls := map[string]*acc{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch asStr(ev["type"]) {
		case "content_block_start":
			cb := asMap(ev["content_block"])
			if asStr(cb["type"]) != "tool_use" {
				continue
			}
			id := asStr(cb["id"])
			if id == "" {
				continue
			}
			if _, dup := calls[id]; !dup {
				calls[id] = &acc{name: asStr(cb["name"]), slot: int(toFloat(ev["index"]))}
				order = append(order, id)
			}
		case "content_block_delta":
			d := asMap(ev["delta"])
			if asStr(d["type"]) != "input_json_delta" {
				continue
			}
			// 按 content block index 找到对应工具。
			slot := int(toFloat(ev["index"]))
			for id, c := range calls {
				if c.slot == slot {
					c.args.WriteString(asStr(d["partial_json"]))
					_ = id
					break
				}
			}
		}
	}
	facts := make([]toolCallFact, 0, len(order))
	for _, id := range order {
		c := calls[id]
		facts = append(facts, toolCallFact{
			name: c.name, callID: id, arguments: c.args.String(), slot: c.slot,
		})
	}
	return facts
}

// assertToolCallInvariants 是跨路径共用的结构不变量。
func assertToolCallInvariants(t *testing.T, path string, facts []toolCallFact, wantNames []string) {
	t.Helper()
	if len(facts) != len(wantNames) {
		t.Fatalf("%s: 期望 %d 个工具调用，得到 %d 个：%+v", path, len(wantNames), len(facts), facts)
	}
	// slot 必须互不相同：共用 slot 会让客户端把结果关联到错误的工具。
	slots := map[int]string{}
	for _, f := range facts {
		if other, dup := slots[f.slot]; dup {
			t.Errorf("%s: 工具 %s 与 %s 共用 slot=%d", path, f.callID, other, f.slot)
		}
		slots[f.slot] = f.callID
	}
	// 名字必须齐全且与期望一致。
	gotNames := make([]string, 0, len(facts))
	for _, f := range facts {
		if f.name == "" {
			t.Errorf("%s: 工具 %s 没有名字", path, f.callID)
		}
		gotNames = append(gotNames, f.name)
	}
	for _, want := range wantNames {
		found := false
		for _, got := range gotNames {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 缺少工具 %q，实际 %v", path, want, gotNames)
		}
	}
	// 参数必须各自合法：不能是两个工具拼接后的脏 JSON。
	for _, f := range facts {
		if f.arguments == "" {
			t.Errorf("%s: 工具 %s 的参数为空，应为 {}", path, f.callID)
			continue
		}
		if !json.Valid([]byte(f.arguments)) {
			t.Errorf("%s: 工具 %s 的参数不是合法 JSON（可能是两个工具的参数拼接）：%q",
				path, f.callID, f.arguments)
		}
	}
}

// chat→responses：两个并行工具，参数分片交错到达。
func TestCrossPathChatToResponsesParallelTools(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c_a\",\"function\":{\"name\":\"pwsh\",\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c_b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"a.txt\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	facts := collectResponsesToolCalls(t, rec.Body.String())
	assertToolCallInvariants(t, "chat→responses", facts, []string{"pwsh", "read"})
}

// messages→responses：两个并行 tool_use 块，参数分片交错到达。
func TestCrossPathMessagesToResponsesParallelTools(t *testing.T) {
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
	facts := collectResponsesToolCalls(t, rec.Body.String())
	assertToolCallInvariants(t, "messages→responses", facts, []string{"pwsh", "read"})
}

// responses→chat：两个并行 function_call。
func TestCrossPathResponsesToChatParallelTools(t *testing.T) {
	up := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_a\",\"call_id\":\"c_a\",\"name\":\"pwsh\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_a\",\"output_index\":0,\"delta\":\"{\\\"cmd\\\":\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_b\",\"call_id\":\"c_b\",\"name\":\"read\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_b\",\"output_index\":1,\"delta\":\"{\\\"path\\\":\"}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_a\",\"output_index\":0,\"delta\":\"\\\"ls\\\"}\"}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_b\",\"output_index\":1,\"delta\":\"\\\"a.txt\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_a\",\"output_index\":0,\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_b\",\"output_index\":1,\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	// chat 协议：tool_calls 带 id/name/arguments，用 index 区分位置。
	var facts []toolCallFact
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		for _, raw := range asArr(chunk["choices"]) {
			cm := asMap(raw)
			delta := asMap(cm["delta"])
			for _, rawCall := range asArr(delta["tool_calls"]) {
				call := asMap(rawCall)
				id := asStr(call["id"])
				fn := asMap(call["function"])
				args := asStr(fn["arguments"])
				// 续传分片按 OpenAI 惯例只带 index + arguments（id 为 null、
				// name 被省略）。不能因为 id/name 都空就跳过整片，那会丢掉参数。
				if id == "" && asStr(fn["name"]) == "" && args == "" {
					continue
				}
				idx := int(toFloat(call["index"]))
				merged := false
				for i := range facts {
					if facts[i].slot == idx {
						// 续传分片的 id 可能是 JSON null 或字符串 "null"，
						// 不能拿它覆盖首次出现的真实 call_id。
						if id != "" && id != "null" {
							facts[i].callID = id
						}
						if n := asStr(fn["name"]); n != "" && n != "null" {
							facts[i].name = n
						}
						facts[i].arguments += args
						merged = true
						break
					}
				}
				if !merged {
					facts = append(facts, toolCallFact{
						name: asStr(fn["name"]), callID: id,
						arguments: args, slot: idx,
					})
				}
			}
		}
	}
	assertToolCallInvariants(t, "responses→chat", facts, []string{"pwsh", "read"})
}

// chat→messages：两个并行工具。
func TestCrossPathChatToMessagesParallelTools(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c_a\",\"function\":{\"name\":\"pwsh\",\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c_b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"a.txt\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	facts := collectMessagesToolCalls(t, rec.Body.String())
	assertToolCallInvariants(t, "chat→messages", facts, []string{"pwsh", "read"})
}

// responses→messages：两个并行 function_call。
func TestCrossPathResponsesToMessagesParallelTools(t *testing.T) {
	up := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_a\",\"call_id\":\"c_a\",\"name\":\"pwsh\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_a\",\"output_index\":0,\"delta\":\"{\\\"cmd\\\":\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_b\",\"call_id\":\"c_b\",\"name\":\"read\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_b\",\"output_index\":1,\"delta\":\"{\\\"path\\\":\"}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_a\",\"output_index\":0,\"delta\":\"\\\"ls\\\"}\"}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_b\",\"output_index\":1,\"delta\":\"\\\"a.txt\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_a\",\"output_index\":0,\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_b\",\"output_index\":1,\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "muse-spark-1.3")
	facts := collectMessagesToolCalls(t, rec.Body.String())
	assertToolCallInvariants(t, "responses→messages", facts, []string{"pwsh", "read"})
}

// messages→chat：两个并行 tool_use。
func TestCrossPathMessagesToChatParallelTools(t *testing.T) {
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
	streamMessagesToChat(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	var facts []toolCallFact
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		for _, raw := range asArr(chunk["choices"]) {
			cm := asMap(raw)
			delta := asMap(cm["delta"])
			for _, rawCall := range asArr(delta["tool_calls"]) {
				call := asMap(rawCall)
				id := asStr(call["id"])
				fn := asMap(call["function"])
				args := asStr(fn["arguments"])
				// 续传分片按 OpenAI 惯例只带 index + arguments（id 为 null、
				// name 被省略）。不能因为 id/name 都空就跳过整片，那会丢掉参数。
				if id == "" && asStr(fn["name"]) == "" && args == "" {
					continue
				}
				idx := int(toFloat(call["index"]))
				merged := false
				for i := range facts {
					if facts[i].slot == idx {
						// 续传分片的 id 可能是 JSON null 或字符串 "null"，
						// 不能拿它覆盖首次出现的真实 call_id。
						if id != "" && id != "null" {
							facts[i].callID = id
						}
						if n := asStr(fn["name"]); n != "" && n != "null" {
							facts[i].name = n
						}
						facts[i].arguments += args
						merged = true
						break
					}
				}
				if !merged {
					facts = append(facts, toolCallFact{
						name: asStr(fn["name"]), callID: id,
						arguments: args, slot: idx,
					})
				}
			}
		}
	}
	assertToolCallInvariants(t, "messages→chat", facts, []string{"pwsh", "read"})
}



