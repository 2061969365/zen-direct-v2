package main

// empty_content.go: 上游严格校验兜底。
//
// 部分上游（Anthropic 风格校验器）对 /chat/completions 报
// "[invalid_parameter_error] The content field is a required field."：
// 只要 messages[] 里出现 content 为 null / 缺失、且没有 tool_calls 的
// user/assistant 消息就整单 400，与消息是否可省略无关。
//
// 实测（严格节点 vs 宽松节点，chat 目标）：
//   - content:null 的 assistant（无 tool_calls）→ 丢整条，5/5 通过
//   - content:null 的 user → 置 ""，5/5 通过
//   - content:null 且带 tool_calls → 本来就 200，不动
//   - content:"" → 本来就 200，不动
//
// 因此只在上游目标为 FmtChat 时做归一化；responses 目标的 input[] 形状
// 不同，观察到的问题也全部来自 chat 路径，不去动它。

// normalizeEmptyMessageContent 就地修正 chat 请求体 messages[] 的空 content。
func normalizeEmptyMessageContent(payload map[string]any) {
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	kept := make([]any, 0, len(messages))
	var lastDropped map[string]any
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role := asStr(message["role"])
		if role != "user" && role != "assistant" {
			kept = append(kept, raw)
			continue
		}
		if message["content"] != nil {
			kept = append(kept, raw)
			continue
		}
		// content 为 null 或缺失。
		if role == "assistant" {
			if hasAnyToolCalls(message) {
				kept = append(kept, raw)
				continue
			}
			// 空 assistant（可能是纯 reasoning 残留）：置 "" 会被上游当作
			// 空消息拒绝的一部分，实测直接丢弃最稳；只有 reasoning 文本的
			// 说明该轮确有思考，补 "" 保住层级。
			if hasAnyText(message, "reasoning_content") || hasAnyText(message, "reasoning") {
				message["content"] = ""
				kept = append(kept, raw)
				continue
			}
			lastDropped = message
			continue
		}
		message["content"] = ""
		kept = append(kept, raw)
	}
	if len(kept) == 0 && lastDropped != nil {
		// 全被丢时保留最后一条空 assistant 并补 ""，避免发出空 messages。
		lastDropped["content"] = ""
		kept = append(kept, lastDropped)
	}
	payload["messages"] = kept
}

// hasAnyToolCalls 判断 assistant 消息是否带实际工具调用（含旧式 function_call）。
func hasAnyToolCalls(message map[string]any) bool {
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if call, ok := message["function_call"].(map[string]any); ok && len(call) > 0 {
		return true
	}
	return false
}

// hasAnyText 判断给定字段是否有非空文本。
func hasAnyText(message map[string]any, key string) bool {
	switch value := message[key].(type) {
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	}
	return false
}
