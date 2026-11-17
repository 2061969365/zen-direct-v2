package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// NewRenameStreamReader wraps an SSE stream and restores wire tool names before
// downstream parsers see them. Chat frames go through the fragment-aware
// rewriter (a name may span several chunks); other protocols use the stateless
// exact rewrite. An empty rename set returns the reader unchanged.
func NewRenameStreamReader(reader io.Reader, protocol Format, renames *ToolRenames) io.Reader {
	if renames.Empty() && !channelModeOn() {
		return reader
	}
	return &renameStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		renamer: newStreamToolRenamer(protocol, renames),
	}
}

type renameStreamReader struct {
	buffer  *bufio.Reader
	renamer *streamToolRenamer
	pending []byte
	err     error
}

func (reader *renameStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		line, err := reader.buffer.ReadString('\n')
		if len(line) > 0 {
			reader.pending = reader.rewriteLine(line)
		}
		if err != nil {
			reader.err = err
			if errors.Is(err, io.EOF) {
				if final := reader.renamer.finish(); len(final) > 0 {
					if len(reader.pending) > 0 && !bytes.HasSuffix(reader.pending, []byte("\n\n")) && !bytes.HasSuffix(reader.pending, []byte("\r\n\r\n")) {
						if !bytes.HasSuffix(reader.pending, []byte("\n")) {
							reader.pending = append(reader.pending, '\n')
						}
						reader.pending = append(reader.pending, '\n')
					}
					reader.pending = append(reader.pending, final...)
				}
			}
			if len(reader.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

func (reader *renameStreamReader) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" {
		return []byte(line)
	}
	if raw == "[DONE]" {
		if final := reader.renamer.finish(); len(final) > 0 {
			return append(final, []byte(line)...)
		}
		return []byte(line)
	}
	rewritten := reader.renamer.rewriteFrame(raw)
	if string(rewritten) == raw {
		return []byte(line)
	}
	return []byte("data: " + string(rewritten) + terminator)
}

type streamToolRenameState struct {
	decided          bool
	renamed          bool
	pending          string
	pendingArguments strings.Builder
	callIndex        any
	choiceIndex      any
	modelID          string
	emitted          bool
}

type streamToolRenamer struct {
	protocol  Format
	renames   *ToolRenames
	tools     map[string]*streamToolRenameState
	order     []string
	channelOn bool
	// dsml 是 DeepSeek 文本工具调用恢复器：模型把 tool call 以 <|DSML|...>
	// 标记写进正文时，从 content 分片里增量提取并还原成结构化调用。
	// 仅在请求带客户端工具（renames 非空）的 chat 流上启用。
	dsml      textToolExtractor
	dsmlOn    bool
	dsmlCalls int
	// dsmlFlushed 防止终态冲刷执行两次（finish 帧与 [DONE] 都会走到）。
	dsmlFlushed bool
	// keyAlias 把同一 tool_call 的两种 key 形态（choice:index / choice:id）
	// 都指向 canonical key，解决"首帧只有 id、后续帧只有 index"的分叉。
	keyAlias map[string]string
	// channelArgs/channelNames 累积每个 tool call 的完整 arguments/name（跨 chunk）。
	channelArgs  map[string]*strings.Builder
	channelNames map[string]string
}

// callKey 计算 canonical key：choiceIndex 前缀 + index/id。已见过的别名
// 直接解析到原 canonical，保证跨帧一致。
func (renamer *streamToolRenamer) callKey(choiceIndex, index any, id string, fallback int) string {
	ck := fmt.Sprint(choiceIndex)
	candidates := []string{}
	if index != nil {
		candidates = append(candidates, ck+":i:"+fmt.Sprint(index))
	}
	if id != "" {
		candidates = append(candidates, ck+":id:"+id)
	}
	candidates = append(candidates, ck+":n:"+fmt.Sprint(fallback))
	for _, c := range candidates {
		if canonical, ok := renamer.keyAlias[c]; ok {
			return canonical
		}
	}
	canonical := candidates[0]
	for _, c := range candidates {
		renamer.keyAlias[c] = canonical
	}
	return canonical
}

// newStreamToolRenamer 构造流式改名器；channelOn 时启用通道选择器还原。
func newStreamToolRenamer(protocol Format, renames *ToolRenames) *streamToolRenamer {
	return &streamToolRenamer{
		protocol:     protocol,
		renames:      renames,
		tools:        map[string]*streamToolRenameState{},
		keyAlias:     map[string]string{},
		channelOn:    channelModeOn(),
		dsmlOn:       protocol == FmtChat && renames != nil && !renames.Empty(),
		channelArgs:  map[string]*strings.Builder{},
		channelNames: map[string]string{},
	}
}

func (renamer *streamToolRenamer) channelName(key, fragment string) {
	prev := renamer.channelNames[key]
	renamer.channelNames[key] = mergeToolName(prev, fragment)
}

// appendDSMLCalls 把 DSML 提取出的调用以 wire 名注入 message/delta 的
// tool_calls（index 顺延、id 现签）。wire 名由随后的逐行名字还原映射回
// 客户端名。返回是否发生改动。
func (renamer *streamToolRenamer) appendDSMLCalls(message map[string]any, calls ...textToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	existing := asArr(message["tool_calls"])
	base := len(existing)
	for i, call := range calls {
		existing = append(existing, map[string]any{
			"index": base + i,
			"id":    newID("call"),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	message["tool_calls"] = existing
	return true
}

func (renamer *streamToolRenamer) rewriteFrame(raw string) []byte {
	if renamer.protocol != FmtChat {
		// 通道模式：responses 流的 function_call 参数跨帧分段，无法逐帧还原；
		// response.completed 帧携带完整 output[]，在这里一次性还原。
		if renamer.channelOn && strings.Contains(raw, `"response.completed"`) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(raw), &payload); err == nil {
				if resp, ok := payload["response"].(map[string]any); ok && restoreChannelCalls(resp, renamer.renames) {
					payload["response"] = resp
					if enc, err := json.Marshal(payload); err == nil {
						return enc
					}
				}
			}
		}
		// 中间帧（output_item.added/done、function_call_arguments.done）里
		// name 仍是通道名：逐事件消费的客户端会先见到未还原的名字，在此还原。
		if renamer.channelOn && (strings.Contains(raw, `"function_call"`) || strings.Contains(raw, `"function_call_arguments.done"`)) && !strings.Contains(raw, `"response.completed"`) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(raw), &payload); err == nil {
				changed := false
				if item, ok := payload["item"].(map[string]any); ok {
					if n := asStr(item["name"]); n != "" && !renamer.renames.Declared(n) {
						if def := channelDefaultResolver(renamer.renames)(n); def != "" && def != n {
							item["name"] = def
							changed = true
						}
					}
				}
				if n := asStr(payload["name"]); n != "" && !renamer.renames.Declared(n) {
					if args := asStr(payload["arguments"]); args != "" {
						real, expanded := applyChannelArgsDefault(n, args, renamer.renames.Declared, channelDefaultResolver(renamer.renames))
						payload["name"] = real
						payload["arguments"] = expanded
						changed = true
					} else if def := channelDefaultResolver(renamer.renames)(n); def != "" && def != n {
						payload["name"] = def
						changed = true
					}
				}
				if changed {
					if enc, err := json.Marshal(payload); err == nil {
						return enc
					}
				}
			}
		}
		return RenameResponseTool([]byte(raw), renamer.renames)
	}
	if !strings.Contains(raw, `"tool_calls"`) && !strings.Contains(raw, `"finish_reason"`) && !renamer.dsmlOn {
		return []byte(raw)
	}
	return renamer.rewriteChatFrame(raw)
}

func (renamer *streamToolRenamer) rewriteChatFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	changed := false
	for choicePosition, rawChoice := range asArr(payload["choices"]) {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := choice["index"]
		if choiceIndex == nil {
			choiceIndex = choicePosition
		}
		finishReason, hasFinishReason := choice["finish_reason"].(string)
		finished := hasFinishReason && finishReason != ""
		seen := map[string]bool{}
		for _, container := range []string{"delta", "message"} {
			message, ok := choice[container].(map[string]any)
			if !ok {
				continue
			}
			for callIndex, rawCall := range asArr(message["tool_calls"]) {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				function, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				// key 规范化：以 choiceIndex 为前缀避免多 choice 碰撞；同一调用
				// 的 index 形态与 id 形态两个别名都指向同一个 canonical key，
				// 解决"首帧只有 id、后续帧只有 index"的分叉。
				key := renamer.callKey(choiceIndex, call["index"], asStr(call["id"]), callIndex)
				state := renamer.toolState(key)
				if id := asStr(call["id"]); id != "" && state.modelID == "" {
					state.modelID = id
				}
				state.callIndex = firstAny(call["index"], callIndex)
				state.choiceIndex = choiceIndex
				seen[key] = true
				// 通道模式：吞掉所有 tool_call 分片，等 finish 帧发完整调用。
				if renamer.channelOn {
					if name, ok := function["name"].(string); ok && name != "" {
						renamer.channelName(key, name)
					}
					if frag, ok := function["arguments"].(string); ok && frag != "" {
						sb := renamer.channelArgs[key]
						if sb == nil {
							sb = &strings.Builder{}
							renamer.channelArgs[key] = sb
						}
						sb.WriteString(frag)
					}
					delete(message, "tool_calls")
					changed = true
					continue
				}
				if name, ok := function["name"].(string); ok && name != "" {
					if renamer.applyNameFragment(state, function, name) {
						changed = true
					}
				}
				if arguments, exists := function["arguments"]; exists && !state.decided {
					if renamer.renames.HasLongerPrefix(state.pending) {
						if fragment, ok := arguments.(string); ok {
							state.pendingArguments.WriteString(fragment)
							delete(function, "arguments")
							changed = true
						}
					} else {
						if state.pending != "" {
							function["name"] = state.pending
							changed = true
						}
						state.decided = true
					}
				}
				if state.decided && renamer.releasePendingArguments(state, function) {
					changed = true
				}
				if finished && !state.decided && (state.pending != "" || state.pendingArguments.Len() > 0) {
					if renamer.finishState(state, function) {
						changed = true
					}
				}
			}
			// DSML 文本工具调用恢复（mcp 模式）：content 分片喂增量提取器，
			// 提取出的调用以 wire 名注入 tool_calls 分片，随后的逐行名字还原
			// （pipeSSE restoreAliasNames）把它们映射回客户端名。
			if renamer.dsmlOn {
				if raw, ok := message["content"].(string); ok && raw != "" {
					visible, calls := renamer.dsml.Feed(raw)
					if visible != raw {
						message["content"] = visible
						changed = true
					}
					if len(calls) > 0 {
						renamer.dsmlCalls += len(calls)
						if renamer.appendDSMLCalls(message, calls...) {
							changed = true
						}
					}
				}
			}
		}
		if finished && renamer.dsmlOn && !renamer.dsmlFlushed {
			// 终态帧冲刷（整个流一次）：残留文本尾 + 未闭合块 best-effort。
			tail, calls := renamer.dsml.Flush()
			renamer.dsmlFlushed = true
			renamer.dsmlCalls += len(calls)
			if tail != "" || len(calls) > 0 {
				var target map[string]any
				for _, rawChoice2 := range asArr(payload["choices"]) {
					c2, ok2 := rawChoice2.(map[string]any)
					if !ok2 || fmt.Sprint(firstAny(c2["index"], -1)) != fmt.Sprint(choiceIndex) {
						continue
					}
					for _, container := range []string{"delta", "message"} {
						if message, ok := c2[container].(map[string]any); ok && target == nil {
							target = message
						}
					}
				}
				if target == nil {
					target = map[string]any{}
					choice["delta"] = target
				}
				if tail != "" {
					prev, _ := target["content"].(string)
					target["content"] = prev + tail
					changed = true
				}
				if len(calls) > 0 {
					if renamer.appendDSMLCalls(target, calls...) {
						changed = true
					}
				}
			}
			if renamer.dsmlCalls > 0 {
				if fr, ok := choice["finish_reason"].(string); ok && (fr == "" || fr == "stop" || fr == "end_turn") {
					choice["finish_reason"] = "tool_calls"
					changed = true
				}
			}
		}
		if finished {
			// 通道模式：finish 帧发一条完整的、已还原的 tool_call。
			if renamer.channelOn {
				var calls []any
				for _, key := range renamer.order {
					state := renamer.tools[key]
					if state == nil || state.emitted || fmt.Sprint(state.choiceIndex) != fmt.Sprint(choiceIndex) {
						continue
					}
					channel := renamer.channelNames[key]
					if channel == "" {
						continue
					}
					args := ""
					if sb := renamer.channelArgs[key]; sb != nil {
						args = sb.String()
					}
					real, expanded := applyChannelArgsDefault(channel, args, renamer.renames.Declared, channelDefaultResolver(renamer.renames))
					callID := state.modelID
					if callID == "" {
						// 上游真 id 缺失时才合成，保证客户端历史回放的 id 关联稳定。
						callID = "call_" + key
					}
					calls = append(calls, map[string]any{
						"index": state.callIndex,
						"id":    callID,
						"type":  "function",
						"function": map[string]any{
							"name":      real,
							"arguments": expanded,
						},
					})
					state.emitted = true
				}
				if len(calls) > 0 {
					delta, ok := choice["delta"].(map[string]any)
					if !ok {
						delta = map[string]any{}
						choice["delta"] = delta
					}
					delta["tool_calls"] = calls
					changed = true
				}
			} else if calls := renamer.pendingCalls(choiceIndex, seen); len(calls) > 0 {
				delta, ok := choice["delta"].(map[string]any)
				if !ok {
					delta = map[string]any{}
					choice["delta"] = delta
				}
				existing := asArr(delta["tool_calls"])
				delta["tool_calls"] = append(existing, calls...)
				changed = true
			}
		}
	}
	if !changed {
		return []byte(raw)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(raw)
	}
	return encoded
}

func (renamer *streamToolRenamer) releasePendingArguments(state *streamToolRenameState, function map[string]any) bool {
	if state.pendingArguments.Len() == 0 {
		return false
	}
	arguments := state.pendingArguments.String()
	if current, exists := function["arguments"]; exists {
		fragment, ok := current.(string)
		if !ok {
			return false
		}
		arguments += fragment
	}
	function["arguments"] = arguments
	state.pendingArguments.Reset()
	return true
}

func (renamer *streamToolRenamer) finishState(state *streamToolRenameState, function map[string]any) bool {
	changed := false
	if state.pending != "" {
		function["name"] = state.pending
		changed = true
	}
	if renamer.releasePendingArguments(state, function) {
		changed = true
	}
	state.decided, state.renamed = true, false
	return changed
}

func (renamer *streamToolRenamer) pendingCalls(choiceIndex any, skip map[string]bool) []any {
	var calls []any
	for _, key := range renamer.order {
		if skip[key] {
			continue
		}
		state := renamer.tools[key]
		if state == nil || state.decided || fmt.Sprint(state.choiceIndex) != fmt.Sprint(choiceIndex) {
			continue
		}
		if state.pending == "" && state.pendingArguments.Len() == 0 {
			continue
		}
		calls = append(calls, renamer.pendingCall(state))
	}
	return calls
}

func (renamer *streamToolRenamer) pendingCall(state *streamToolRenameState) any {
	function := map[string]any{}
	if state.pending != "" {
		function["name"] = state.pending
	}
	if state.pendingArguments.Len() > 0 {
		function["arguments"] = state.pendingArguments.String()
	}
	call := map[string]any{"index": state.callIndex, "function": function}
	state.decided, state.renamed = true, false
	state.pendingArguments.Reset()
	return call
}

func (renamer *streamToolRenamer) finish() []byte {
	if renamer.protocol != FmtChat {
		return nil
	}
	// 通道模式截断补发：上游在 finish_reason 之前断流（free 层常见）时，
	// 已被吞掉的 tool_call 分片在此补发完整调用，否则客户端本轮收不到任何调用。
	if renamer.channelOn {
		type choiceCalls struct {
			index any
			calls []any
		}
		var groups []*choiceCalls
		byIndex := map[string]*choiceCalls{}
		for _, key := range renamer.order {
			state := renamer.tools[key]
			if state == nil || state.emitted {
				continue
			}
			channel := renamer.channelNames[key]
			if channel == "" {
				continue
			}
			args := ""
			if sb := renamer.channelArgs[key]; sb != nil {
				args = sb.String()
			}
			real, expanded := applyChannelArgsDefault(channel, args, renamer.renames.Declared, channelDefaultResolver(renamer.renames))
			callID := state.modelID
			if callID == "" {
				callID = "call_" + key
			}
			index := state.choiceIndex
			if index == nil {
				index = 0
			}
			indexKey := fmt.Sprint(index)
			group := byIndex[indexKey]
			if group == nil {
				group = &choiceCalls{index: index}
				byIndex[indexKey] = group
				groups = append(groups, group)
			}
			group.calls = append(group.calls, map[string]any{
				"index": state.callIndex,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      real,
					"arguments": expanded,
				},
			})
			state.emitted = true
		}
		if len(groups) > 0 {
			choices := make([]any, 0, len(groups))
			for _, group := range groups {
				choices = append(choices, map[string]any{
					"index":         group.index,
					"delta":         map[string]any{"tool_calls": group.calls},
					"finish_reason": "tool_calls",
				})
			}
			if encoded, err := json.Marshal(map[string]any{"choices": choices}); err == nil {
				return append(append([]byte("data: "), encoded...), '\n', '\n')
			}
		}
	}
	type choiceCalls struct {
		index any
		calls []any
	}
	var groups []*choiceCalls
	byIndex := map[string]*choiceCalls{}
	for _, key := range renamer.order {
		state := renamer.tools[key]
		if state == nil || state.decided || state.pending == "" && state.pendingArguments.Len() == 0 {
			continue
		}
		index := state.choiceIndex
		if index == nil {
			index = 0
		}
		indexKey := fmt.Sprint(index)
		group := byIndex[indexKey]
		if group == nil {
			group = &choiceCalls{index: index}
			byIndex[indexKey] = group
			groups = append(groups, group)
		}
		group.calls = append(group.calls, renamer.pendingCall(state))
	}
	if len(groups) == 0 {
		return nil
	}
	choices := make([]any, 0, len(groups))
	for _, group := range groups {
		choices = append(choices, map[string]any{
			"index":         group.index,
			"delta":         map[string]any{"tool_calls": group.calls},
			"finish_reason": nil,
		})
	}
	encoded, err := json.Marshal(map[string]any{"choices": choices})
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), encoded...), '\n', '\n')
}

func (renamer *streamToolRenamer) applyNameFragment(state *streamToolRenameState, function map[string]any, fragment string) bool {
	if state.decided {
		if !state.renamed {
			return false
		}
		if _, exists := function["name"]; exists {
			delete(function, "name")
			return true
		}
		return false
	}
	state.pending = mergeToolName(state.pending, fragment)
	if client, ok := renamer.renames.Client(state.pending); ok {
		function["name"] = client
		state.decided, state.renamed = true, true
		return true
	}
	if renamer.renames.HasLongerPrefix(state.pending) {
		delete(function, "name")
		return true
	}
	client, renamed := renamer.renames.ResolveFuzzy(state.pending)
	function["name"] = client
	state.decided, state.renamed = true, renamed
	return true
}

func mergeToolName(pending, fragment string) string {
	if strings.HasPrefix(fragment, pending) {
		return fragment
	}
	if strings.HasPrefix(pending, fragment) {
		return pending
	}
	return pending + fragment
}

func (renamer *streamToolRenamer) toolState(key string) *streamToolRenameState {
	if state := renamer.tools[key]; state != nil {
		return state
	}
	state := &streamToolRenameState{}
	renamer.tools[key] = state
	renamer.order = append(renamer.order, key)
	return state
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		return value
	}
	return nil
}
