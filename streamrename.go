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
	emitted          bool
}

type streamToolRenamer struct {
	protocol  Format
	renames   *ToolRenames
	tools     map[string]*streamToolRenameState
	order     []string
	channelOn bool
	// channelArgs/channelNames 累积每个 tool call 的完整 arguments/name（跨 chunk）。
	channelArgs  map[string]*strings.Builder
	channelNames map[string]string
}

// newStreamToolRenamer 构造流式改名器；channelOn 时启用通道选择器还原。
func newStreamToolRenamer(protocol Format, renames *ToolRenames) *streamToolRenamer {
	return &streamToolRenamer{
		protocol:     protocol,
		renames:      renames,
		tools:        map[string]*streamToolRenameState{},
		channelOn:    channelModeOn(),
		channelArgs:  map[string]*strings.Builder{},
		channelNames: map[string]string{},
	}
}

func (renamer *streamToolRenamer) channelName(key, fragment string) {
	prev := renamer.channelNames[key]
	renamer.channelNames[key] = mergeToolName(prev, fragment)
}

func (renamer *streamToolRenamer) rewriteFrame(raw string) []byte {
	if renamer.protocol != FmtChat {
		return RenameResponseTool([]byte(raw), renamer.renames)
	}
	if !strings.Contains(raw, `"tool_calls"`) && !strings.Contains(raw, `"finish_reason"`) {
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
				key := fmt.Sprint(firstAny(call["index"], asStr(call["id"]), callIndex))
				state := renamer.toolState(key)
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
					real, expanded := applyChannelArgs(channel, args)
					calls = append(calls, map[string]any{
						"index": state.callIndex,
						"id":    "call_" + key,
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
