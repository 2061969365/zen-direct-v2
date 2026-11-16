package main

// toolchannel.go: 45 个客户端工具通过 12 个官方桩名作为"通道"发给上游，
// 响应时按 _tool 选择器还原成真实工具名与参数。
//
// 背景：上游 free 层只放行官方工具名。客户端工具名（pwsh / team_task_* 等）
// 一律 429。既然出站只能用官方名，就把 12 个官方名当作 12 条复用通道，
// 在参数里加一个 _tool 字段告诉网关实际要调哪个工具。

import (
	"encoding/json"
	"os"
	"strings"
)

// channelSelector / channelArgs 是注入到每个官方通道参数里的选择器字段。
const (
	channelSelector = "_tool"
	channelArgs     = "_args"
)

// toolChannelMap 把客户端工具名映射到语义最接近的官方通道。
// 一个通道可承载多个客户端工具，靠 _tool 区分。
var toolChannelMap = map[string]string{
	// shell / 命令执行类 -> shell
	"pwsh": "shell", "shell": "shell", "execute_command": "shell",
	"job_kill": "shell", "schedule_create": "shell", "schedule_delete": "shell",
	"schedule_update": "shell", "schedule_list": "shell", "purge_apply": "shell",
	"purge_revert": "shell", "purge_status": "shell",

	// 文件读类 -> read
	"read": "read", "read_image": "read", "dsh_im_group_file": "read",

	// 文件写/改 -> write / edit
	"write": "write", "edit": "edit",

	// 路径查找 -> glob
	"glob": "glob",

	// 内容搜索 -> grep
	"grep": "grep",

	// 提问 -> question
	"ask_user_question": "question", "question": "question",

	// skill
	"skill": "skill",

	// agent / 多工具 -> subagent
	"subagent": "subagent", "subagent_fork": "subagent",
	"spawn_teammate": "subagent", "list_agents": "subagent",
	"team_task_create": "subagent", "team_task_get": "subagent",
	"team_task_list": "subagent", "team_task_update": "subagent",
	"send_message": "subagent", "interrupt_agent": "subagent",
	"wait_agent": "subagent", "create_goal": "subagent", "get_goal": "subagent",
	"update_goal": "subagent", "todo_write": "subagent", "workflow": "subagent",

	// web -> webfetch / websearch
	"webfetch": "webfetch", "web_fetch": "webfetch",
	"websearch": "websearch", "web_search": "websearch",
	"anysearch_search": "websearch", "anysearch_batch_search": "websearch",
	"anysearch_capabilities": "websearch",

	// 其他杂项 -> execute (通用代码执行通道)
	"present": "execute", "dsh_im_return_file": "execute",
	"dsh_im_group_history": "execute", "exit_plan_mode": "execute",
}

// channelForTool 返回客户端工具对应的官方通道名。
// 未登记的新工具不再全部落入 execute，而是按名字语义归类；
// 归不出的才进 execute 兜底。
func channelForTool(client string) string {
	if ch, ok := toolChannelMap[client]; ok {
		return ch
	}
	return classifyToolByName(client)
}

// classifyToolByName 关键词语义分类：让未来新增的、未登记进
// toolChannelMap 的工具自动落到语义最接近的通道，保持 _tool 选择器
// 的区分度。顺序即优先级（先具体后宽泛）。
func classifyToolByName(name string) string {
	n := strings.ToLower(name)
	match := func(keys ...string) bool {
		for _, k := range keys {
			if strings.Contains(n, k) {
				return true
			}
		}
		return false
	}
	switch {
	case match("skill"):
		return "skill"
	case match("question", "ask"):
		return "question"
	case match("search", "query"):
		return "websearch"
	case match("fetch", "url", "http", "web"):
		return "webfetch"
	case match("agent", "teammate", "spawn", "send", "notify", "message", "task", "todo", "goal", "wait", "interrupt"):
		return "subagent"
	case match("shell", "command", "exec", "bash", "pwsh", "run"):
		return "shell"
	case match("schedule", "cron"):
		return "shell"
	case match("grep"):
		return "grep"
	case match("glob", "find", "list"):
		return "glob"
	case match("image"):
		return "read"
	case match("read", "load"):
		return "read"
	case match("edit", "patch", "update", "replace"):
		return "edit"
	case match("write", "create", "save"):
		return "write"
	case match("delete", "remove", "purge", "kill", "stop"):
		return "execute"
	default:
		return "execute"
	}
}

// channelDefaultTool 是模型没填 _tool 时的兜底还原目标：每个通道选一个
// 语义最直接的客户端工具。没有兜底时，客户端会收到通道原名（如 shell），
// 在 DSH 侧就是 unknown tool。
var channelDefaultTool = map[string]string{
	"shell":     "pwsh",
	"read":      "read",
	"write":     "write",
	"edit":      "edit",
	"glob":      "glob",
	"grep":      "grep",
	"question":  "ask_user_question",
	"skill":     "skill",
	"subagent":  "spawn_teammate",
	"webfetch":  "web_fetch",
	"websearch": "web_search",
	"execute":   "workflow",
}

// buildChannelTools 把客户端 tools 数组转换成 12 个官方通道工具。
// 每个通道的描述里列出它承载的客户端工具及其原始 schema，
// 参数里注入 _tool（枚举）与 _args（自由对象）。
func buildChannelTools(protocol Format, items []any) []any {
	// 按通道分组：channel -> 该通道承载的客户端工具
	groups := map[string][]map[string]any{}
	order := []string{}
	for _, raw := range items {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := entryToolName(entry, protocol)
		if name == "" {
			continue
		}
		ch := channelForTool(name)
		if _, seen := groups[ch]; !seen {
			order = append(order, ch)
		}
		groups[ch] = append(groups[ch], entry)
	}

	out := make([]any, 0, len(order))
	for _, ch := range order {
		tool := renderOfficialTool(protocol, ch)
		desc := channelDescription(ch, groups[ch], protocol)
		params := channelParameters(protocol, tool, groups[ch])
		if fn, ok := tool["function"].(map[string]any); ok {
			fn["description"] = desc
			fn["parameters"] = params
		} else {
			tool["description"] = desc
			tool["parameters"] = params
		}
		out = append(out, tool)
	}
	return out
}

// channelMemberDescCap 限制单个成员描述在通道描述里的长度：
// 通道成员多了之后描述会膨胀，撑大 token 也稀释模型注意力。
const channelMemberDescCap = 300

// channelDescription 生成通道描述：保留官方描述前缀，列出承载的真实工具。
func channelDescription(channel string, members []map[string]any, protocol Format) string {
	base := renderOfficialTool(protocol, channel)
	baseDesc := ""
	if fn, ok := base["function"].(map[string]any); ok {
		baseDesc, _ = fn["description"].(string)
	} else {
		baseDesc, _ = base["description"].(string)
	}
	var b strings.Builder
	b.WriteString(baseDesc)
	b.WriteString("\n\nThis tool dispatches to one of the following functions. ")
	b.WriteString("Set _tool to the function name and put its arguments in _args.\n")
	for _, m := range members {
		name := entryToolName(m, protocol)
		desc := ""
		if fn, ok := m["function"].(map[string]any); ok {
			desc, _ = fn["description"].(string)
		} else {
			desc, _ = m["description"].(string)
		}
		// 成员描述必须过脱敏：DSH 的 read/edit/skill 描述带上游封禁短语
		//（UTF-8 text file / line-numbered content 等），原样嵌入通道描述
		// 会把指纹原封不动送出去——单轮都 429，与多轮无关。
		if cleaned, replaced := sanitizeToolDescription(name, desc); replaced {
			desc = cleaned
		}
		// 截断超长成员描述，保持通道描述有界。
		if len(desc) > channelMemberDescCap {
			desc = desc[:channelMemberDescCap] + "…"
		}
		b.WriteString("- ")
		b.WriteString(name)
		if desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// channelParameters 在官方通道参数里注入 _tool 与 _args。
// _tool 带 enum（该通道承载的客户端工具全集），给模型硬约束提升选择准确率。
func channelParameters(protocol Format, tool map[string]any, members []map[string]any) map[string]any {
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	if fn, ok := tool["function"].(map[string]any); ok {
		if p, ok := fn["parameters"].(map[string]any); ok {
			params = p
		}
	} else if p, ok := tool["parameters"].(map[string]any); ok {
		params = p
	}
	props, _ := params["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	selector := map[string]any{
		"type":        "string",
		"description": "The actual function to dispatch to.",
	}
	if len(members) > 0 {
		enum := make([]any, 0, len(members))
		for _, m := range members {
			if n := entryToolName(m, protocol); n != "" {
				enum = append(enum, n)
			}
		}
		selector["enum"] = enum
	}
	props[channelSelector] = selector
	props[channelArgs] = map[string]any{
		"type":                  "object",
		"description":           "Arguments object for the actual function.",
		additionalPropertiesKey: true,
	}
	params["properties"] = props
	return params
}

const additionalPropertiesKey = "additionalProperties"

// applyChannelArgs 把模型返回的 _args 展开成真实参数，并去掉选择器字段。
// 返回 (真实工具名, 真实参数 JSON 字符串)。
// channelModeOn 报告是否启用了工具通道模式。
func channelModeOn() bool { return os.Getenv("ZEN_TOOL_CHANNEL") != "" }

// rewriteChannelHistory 把历史消息里真实工具名映射回通道名。
//
// 关键：参数必须原样保留，绝不能塞 _tool/_args。这两个字段是「模型 -> 网关」
// 方向的私有约定，只出现在新生成的 tool_calls 里；历史里的 tool_call 代表
// 「已经发生过的官方工具调用」，官方 CLI 的历史参数永远是干净的官方参数
// （websearch 就是 {"query":...}）。带选择器字段的历史会被上游按指纹门禁判为
// 第三方客户端，直接 429 FreeUsageLimitError。
//
// 这与 opencode2iori 的 sanitizeToolHistory 一致：只改 name，不动 arguments。
func rewriteChannelHistory(payload map[string]any, protocol Format) {
	messages := asArr(payload["messages"])
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		for _, rawCall := range asArr(message["tool_calls"]) {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			name := asStr(fn["name"])
			if name == "" {
				continue
			}
			// 已是通道名（官方名）则保持；否则映射回通道名。
			if !officialToolNameSet()[name] {
				fn["name"] = channelForTool(name)
			}
			// arguments 原样保留：去掉可能残留的选择器字段（幂等）。
			if args := asStr(fn["arguments"]); args != "" {
				fn["arguments"] = stripChannelSelector(args)
			}
		}
	}
	// responses 扁平形态
	for _, rawItem := range asArr(payload["input"]) {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		name := asStr(item["name"])
		if name == "" {
			continue
		}
		if !officialToolNameSet()[name] {
			item["name"] = channelForTool(name)
		}
		if args := asStr(item["arguments"]); args != "" {
			item["arguments"] = stripChannelSelector(args)
		}
	}
}

// rewriteChannelToolChoice 把 tool_choice 里的客户端工具名映射回通道名。
// 字符串形态（"web_search"）与对象形态（{function:{name:...}}）都处理；
// 非 function 值（auto/required/none）不动。legacy function_call 同步改写。
func rewriteChannelToolChoice(payload map[string]any, protocol Format) {
	mapName := func(name string) string {
		// 只映射已知客户端工具名；auto/required/none 等控制值原样通过。
		if ch, ok := toolChannelMap[name]; ok && ch != name {
			return ch
		}
		if _, isClient := toolChannelMap[name]; isClient {
			return name
		}
		return name
	}
	if tc, ok := payload["tool_choice"].(string); ok {
		payload["tool_choice"] = mapName(tc)
		return
	}
	if tc, ok := payload["tool_choice"].(map[string]any); ok {
		if fn, ok := tc["function"].(map[string]any); ok {
			if n := asStr(fn["name"]); n != "" {
				fn["name"] = mapName(n)
			}
		}
		// responses 扁平形态：{"type":"function","name":"web_search"}
		if n := asStr(tc["name"]); n != "" {
			tc["name"] = mapName(n)
		}
		return
	}
	if fc, ok := payload["function_call"].(map[string]any); ok {
		if n := asStr(fc["name"]); n != "" {
			fc["name"] = mapName(n)
		}
	} else if fc, ok := payload["function_call"].(string); ok {
		payload["function_call"] = mapName(fc)
	}
}

// stripChannelSelector 从 arguments 里移除 _tool/_args，只保留真实参数。
// 历史调用里若被客户端自己带上这两个字段（例如把上一轮响应原样回放），
// 也要清掉，否则同样触发指纹门禁。
func stripChannelSelector(arguments string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return arguments
	}
	_, hasSelector := m[channelSelector]
	argsObj, hasArgs := m[channelArgs].(map[string]any)
	if !hasSelector && !hasArgs {
		return arguments
	}
	out := map[string]any{}
	// 若存在 _args，以其为权威参数来源。
	if hasArgs {
		for k, v := range argsObj {
			out[k] = v
		}
	}
	// 顶层非选择器字段补充。
	for k, v := range m {
		if k == channelSelector || k == channelArgs || k == "_" {
			continue
		}
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return arguments
	}
	return string(b)
}

// restoreChannelCalls 把响应里的通道工具调用还原成真实客户端工具调用：
// 从 arguments 的 _tool 取真实工具名，把 _args 展开为顶层参数。
// 支持 chat（choices[].message/delta.tool_calls）与 responses（output[]）。
func restoreChannelCalls(payload map[string]any, renames *ToolRenames) bool {
	changed := false
	rewriteFn := func(fn map[string]any) {
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if name == "" || args == "" {
			return
		}
		real, expanded := applyChannelArgsDefault(name, args, renames.Declared, channelDefaultResolver(renames))
		if real != name || expanded != args {
			fn["name"] = real
			fn["arguments"] = expanded
			changed = true
		}
	}

	for _, rawChoice := range asArr(payload["choices"]) {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		for _, container := range []string{"message", "delta"} {
			msg, ok := choice[container].(map[string]any)
			if !ok {
				continue
			}
			for _, rawCall := range asArr(msg["tool_calls"]) {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				if fn, ok := call["function"].(map[string]any); ok {
					rewriteFn(fn)
				}
			}
		}
	}
	// responses 扁平形态
	for _, rawItem := range asArr(payload["output"]) {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := item["function_call"].(map[string]any); ok {
			rewriteFn(fn)
		}
		if name, ok := item["name"].(string); ok && name != "" {
			if args, ok := item["arguments"].(string); ok && args != "" {
				real, expanded := applyChannelArgsDefault(name, args, renames.Declared, channelDefaultResolver(renames))
				if real != name || expanded != args {
					item["name"] = real
					item["arguments"] = expanded
					changed = true
				}
			}
		}
	}
	return changed
}

// applyChannelArgs 把模型返回的 _tool 选择器还原成真实客户端工具名，
// 并把 _args 展开成顶层参数。declared 用于区分"客户端真名"与"模型回显的
// 通道名"——subagent 这类既是客户端真名又是官方名的值不能误映射。
func applyChannelArgs(toolName, arguments string, declared func(string) bool) (string, string) {
	return applyChannelArgsDefault(toolName, arguments, declared, nil)
}

// channelDefaultResolver 生成通道缺省还原函数：该通道下恰好声明了一个客户端
// 工具时优先还原到它（漏填或回显 _tool 时避免换成未声明的通道默认工具，
// 导致客户端收到 unknown tool）；否则退回 channelDefaultTool。nil renames 安全。
func channelDefaultResolver(renames *ToolRenames) func(string) string {
	return func(channel string) string {
		if only := renames.DeclaredForChannel(channel); only != "" {
			return only
		}
		return channelDefaultTool[channel]
	}
}

// applyChannelArgsDefault 同 applyChannelArgs；defaultFor 允许调用方按已声明
// 工具集合决定通道缺省还原目标（nil 时用 channelDefaultTool）。
func applyChannelArgsDefault(toolName, arguments string, declared func(string) bool, defaultFor func(string) string) (string, string) {
	if defaultFor == nil {
		defaultFor = func(channel string) string { return channelDefaultTool[channel] }
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		// 不可解析的 JSON 无法判断模型意图（无参还是截断）：原样保留名字与
		// 参数。名字泄漏（通道名）在此场景是已知限制——客户端报 unknown tool
		// 时参数也在，重试可自愈。
		return toolName, arguments
	}
	real, _ := m[channelSelector].(string)
	if real == "" {
		// 没填选择器：模型直接以通道名调用（常见于它沿用官方 shell 语义）。
		// 若通道名本身是客户端真名（如 subagent）则保留；否则兜底到通道默认
		// 客户端工具（优先该通道下已声明的工具），避免客户端收到未知工具名
		// （shell）报 unknown tool。
		if !declared(toolName) {
			if def := defaultFor(toolName); def != "" && def != toolName {
				return def, arguments
			}
		}
		return toolName, arguments
	}
	// _tool 填了通道名本身（回显官方语义）：映射到该通道默认客户端工具
	// （优先该通道下已声明的工具）。客户端真名（declared）或任意其他值原样保留。
	if real == toolName && !declared(real) {
		if def := defaultFor(toolName); def != "" && def != toolName {
			real = def
		}
	}
	out := map[string]any{}
	if args, ok := m[channelArgs].(map[string]any); ok {
		for k, v := range args {
			out[k] = v
		}
	}
	// 保留非选择器字段，避免模型把参数直接放在顶层时丢失。
	for k, v := range m {
		if k == channelSelector || k == channelArgs || k == "_" {
			continue
		}
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return real, "{}"
	}
	return real, string(b)
}
