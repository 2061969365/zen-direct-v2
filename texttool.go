package main

// texttool.go: DeepSeek DSML 文本工具调用恢复器。
// 移植自 opencode2iori internal/protocol/texttool.go（Apache-2.0），适配本仓
// 的命名与还原体系。
//
// 部分 upstream 模型（DeepSeek 系为主）把 tool call 以标记写进 assistant
// 正文而不是结构化 tool_calls 字段：
//
//	<|DSML| tool_calls>
//	<|DSML| invoke name="shell">
//	<|DSML| parameter name="cmd">ls -la</|DSML| parameter>
//	</|DSML| invoke>
//	</|DSML| calls>
//
// 只认结构化调用的客户端会把这块当普通文本渲染，回合结束、工具永不执行。
// 下面的提取器把标记还原成真正的 tool_calls，并让它不进入可见文本。
// 增量设计：只有可能是标记前缀的文本会被扣住，正常流式不被拖延。

import (
	"encoding/json"
	"regexp"
	"strings"
)

// dsmlToken opens every DSML tag.
const dsmlToken = "<|DSML|"

// dsmlBlockTags are the tags that open a tool-call block. The wrapper names are
// accepted because models vary between tool_calls, function_calls and calls; a
// bare invoke block is accepted too.
var dsmlBlockTags = map[string]bool{
	"tool_calls":     true,
	"function_calls": true,
	"calls":          true,
	"invoke":         true,
}

// dsmlWrapperClose lists the closing tags of a wrapped block, most specific
// first.
var dsmlWrapperClose = []string{
	"</|DSML| calls>",
	"</|DSML| tool_calls>",
	"</|DSML| function_calls>",
}

// dsmlInvokeClose closes a bare invoke block.
const dsmlInvokeClose = "</|DSML| invoke>"

// dsmlInvokePattern matches one invoke element and its name.
var dsmlInvokePattern = regexp.MustCompile(`(?is)<\|DSML\|\s*(?:invoke|function)\s+name\s*=\s*["']?([^"'>\s]+)["']?\s*>(.*?)</\|DSML\|\s*(?:invoke|function)\s*>`)

// dsmlParameterPattern matches one parameter element and its name.
var dsmlParameterPattern = regexp.MustCompile(`(?is)<\|DSML\|\s*(?:parameter|param)\s+name\s*=\s*["']?([^"'>\s]+)["']?\s*>(.*?)</\|DSML\|\s*(?:parameter|param)\s*>`)

// textToolCall is one call recovered from assistant text. Arguments is always a
// JSON object, because every downstream protocol expects the argument text in
// that form.
type textToolCall struct {
	Name      string
	Arguments string
}

// dsmlInvokeNamePattern reads the name attribute out of an opening tag.
var dsmlInvokeNamePattern = regexp.MustCompile(`(?i)name\s*=\s*["']?([^"'\s>]+)`)

// textToolExtractor is the incremental DSML scanner. buf holds text that has
// not been released yet: a possible partial marker outside a block, or the
// whole block while it is open. inBlock and wrapper describe the block being
// collected, and invokeName is the tool of a bare invoke block.
type textToolExtractor struct {
	buf        string
	inBlock    bool
	wrapper    bool
	invokeName string
	openTag    string
}

// Feed consumes one text delta and returns the text that is safe to show plus
// any tool calls the delta completed.
func (extractor *textToolExtractor) Feed(text string) (string, []textToolCall) {
	if text == "" {
		return "", nil
	}
	extractor.buf += text
	var visible strings.Builder
	var calls []textToolCall
	for {
		if !extractor.inBlock {
			index := indexFold(extractor.buf, dsmlToken)
			if index < 0 {
				keep := partialTokenSuffix(extractor.buf)
				visible.WriteString(extractor.buf[:len(extractor.buf)-keep])
				extractor.buf = extractor.buf[len(extractor.buf)-keep:]
				break
			}
			tag, attrs, after, ok := readDSMLTag(extractor.buf, index+len(dsmlToken))
			if !ok {
				// The tag is still arriving. A buffer this long is not a tag
				// but prose that happens to contain the marker, so release it.
				if len(extractor.buf)-index > 64 {
					visible.WriteString(extractor.buf[:index+len(dsmlToken)])
					extractor.buf = extractor.buf[index+len(dsmlToken):]
					continue
				}
				visible.WriteString(extractor.buf[:index])
				extractor.buf = extractor.buf[index:]
				break
			}
			if !dsmlBlockTags[tag] || (tag == "invoke" && dsmlName(attrs) == "") {
				// A DSML tag that is not a tool call (some models use the
				// dialect for other blocks): show it unchanged.
				visible.WriteString(extractor.buf[:after])
				extractor.buf = extractor.buf[after:]
				continue
			}
			visible.WriteString(extractor.buf[:index])
			extractor.openTag = extractor.buf[index:after]
			extractor.buf = extractor.buf[after:]
			extractor.inBlock = true
			extractor.wrapper = tag != "invoke"
			extractor.invokeName = ""
			if tag == "invoke" {
				extractor.invokeName = dsmlName(attrs)
			}
			continue
		}
		if index, length, ok := findBlockClose(extractor.buf, extractor.wrapper); ok {
			content := extractor.buf[:index]
			closeTag := extractor.buf[index : index+length]
			extractor.buf = extractor.buf[index+length:]
			if extractor.wrapper {
				parsed := parseDSMLBlock(content)
				if len(parsed) == 0 {
					// A block that names no call is shown as it arrived rather
					// than swallowed.
					visible.WriteString(extractor.openTag + content + closeTag)
				} else {
					calls = append(calls, parsed...)
				}
			} else {
				calls = append(calls, bareTextToolCall(extractor.invokeName, content))
			}
			extractor.inBlock = false
			extractor.wrapper = false
			extractor.invokeName = ""
			extractor.openTag = ""
			continue
		}
		break
	}
	return visible.String(), calls
}

// Flush releases everything still held at the end of a stream. A block that
// never closed is parsed best-effort; when it holds no call its text is shown
// as it arrived, so a truncated answer loses nothing.
func (extractor *textToolExtractor) Flush() (string, []textToolCall) {
	rest := extractor.buf
	wasBlock, wrapper, invokeName, openTag := extractor.inBlock, extractor.wrapper, extractor.invokeName, extractor.openTag
	extractor.buf = ""
	extractor.inBlock = false
	extractor.wrapper = false
	extractor.invokeName = ""
	extractor.openTag = ""
	if rest == "" {
		return "", nil
	}
	if !wasBlock {
		return rest, nil
	}
	if wrapper {
		if calls := parseDSMLBlock(rest); len(calls) > 0 {
			return "", calls
		}
		return openTag + rest, nil
	}
	if call := bareTextToolCall(invokeName, rest); call.Name != "" {
		return "", []textToolCall{call}
	}
	return openTag + rest, nil
}

// readDSMLTag reads the tag name and attribute text that follow the token, up
// to the closing '>'. It reports false while the tag is still incomplete.
func readDSMLTag(text string, from int) (string, string, int, bool) {
	index := from
	for index < len(text) && (text[index] == ' ' || text[index] == '\t' || text[index] == '\r' || text[index] == '\n') {
		index++
	}
	start := index
	for index < len(text) && text[index] != '>' {
		index++
	}
	if index >= len(text) {
		return "", "", 0, false
	}
	tag := strings.TrimSpace(text[start:index])
	attrs := ""
	if space := strings.IndexAny(tag, " \t\r\n"); space >= 0 {
		attrs = tag[space+1:]
		tag = tag[:space]
	}
	return strings.ToLower(tag), attrs, index + 1, true
}

// dsmlName returns the name attribute of an opening tag.
func dsmlName(attrs string) string {
	match := dsmlInvokeNamePattern.FindStringSubmatch(attrs)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// bareTextToolCall builds the single call of a block opened by a bare invoke
// tag, whose parameters are the block body.
func bareTextToolCall(name, body string) textToolCall {
	if name == "" {
		return textToolCall{}
	}
	return textToolCall{Name: name, Arguments: dsmlArguments(body)}
}

// findBlockClose returns the position and length of the closing tag that ends
// the open block.
func findBlockClose(text string, wrapper bool) (int, int, bool) {
	if !wrapper {
		if index := indexFold(text, dsmlInvokeClose); index >= 0 {
			return index, len(dsmlInvokeClose), true
		}
		return 0, 0, false
	}
	best, length := -1, 0
	for _, close := range dsmlWrapperClose {
		if index := indexFold(text, close); index >= 0 && (best < 0 || index < best) {
			best, length = index, len(close)
		}
	}
	return best, length, best >= 0
}

// parseDSMLBlock turns the inside of a tool-call block into calls. Invokes are
// matched first; a block that only carries parameters is treated as one call
// named by its own "invoke" attribute when present.
func parseDSMLBlock(content string) []textToolCall {
	matches := dsmlInvokePattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	calls := make([]textToolCall, 0, len(matches))
	for _, match := range matches {
		name := strings.TrimSpace(match[1])
		if name == "" {
			continue
		}
		calls = append(calls, textToolCall{Name: name, Arguments: dsmlArguments(match[2])})
	}
	return calls
}

// dsmlArguments renders a block body's parameters as one JSON object.
func dsmlArguments(body string) string {
	arguments := map[string]any{}
	for _, match := range dsmlParameterPattern.FindAllStringSubmatch(body, -1) {
		name := strings.TrimSpace(match[1])
		if name == "" {
			continue
		}
		arguments[name] = dsmlValue(match[2])
	}
	if len(arguments) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// dsmlValue keeps a parameter that already is JSON (an object, array, literal
// or number) as JSON, and quotes everything else as a string.
func dsmlValue(raw string) any {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	switch value[0] {
	case '{', '[', '"':
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			return decoded
		}
	default:
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			switch decoded.(type) {
			case bool, float64, nil:
				return decoded
			}
		}
	}
	return raw
}

// indexFold is a case-insensitive strings.Index.
func indexFold(text, target string) int {
	if target == "" {
		return 0
	}
	return strings.Index(strings.ToLower(text), strings.ToLower(target))
}

// partialTokenSuffix returns the length of the longest suffix of text that is a
// proper prefix of the DSML token, so a marker split across deltas is not shown
// before it completes.
func partialTokenSuffix(text string) int {
	limit := len(dsmlToken) - 1
	if len(text) < limit {
		limit = len(text)
	}
	lower := strings.ToLower(text)
	for length := limit; length > 0; length-- {
		if strings.HasSuffix(lower, strings.ToLower(dsmlToken[:length])) {
			return length
		}
	}
	return 0
}

// ExtractChatTextToolCallsInto rewrites a complete (non-streaming) Chat
// completion payload so a tool call the model wrote as DSML text becomes a
// tool_calls entry. Recovered names keep the wire form they carry（mcp_<digest>
// 等），由调用方随后的名字还原统一映射回客户端名。返回是否有改动。
func ExtractChatTextToolCallsInto(payload map[string]any) bool {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		content, ok := message["content"].(string)
		if !ok || content == "" {
			continue
		}
		extractor := &textToolExtractor{}
		visible, calls := extractor.Feed(content)
		tail, more := extractor.Flush()
		visible += tail
		calls = append(calls, more...)
		if len(calls) == 0 {
			continue
		}
		changed = true
		message["content"] = visible
		existing, _ := message["tool_calls"].([]any)
		for _, call := range calls {
			existing = append(existing, map[string]any{
				"index": len(existing),
				"id":    newID("call"),
				"type":  "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			})
		}
		message["tool_calls"] = existing
		if reason, _ := choice["finish_reason"].(string); reason == "" || reason == "stop" {
			choice["finish_reason"] = "tool_calls"
		}
	}
	return changed
}
