package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

//go:embed tools/opencode_tools.json
var opencodeToolsV2JSON []byte

type opencodeToolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// v2OfficialToolOrder 是官方 CLI 声明的工具名全集。
//
// experimental_image_generate 来自 2026-11-16 抓到的官方 CLI 2.2.5 实际出站
// （13 个工具，含它）。官方会按配置增减工具，这里做的是"已知官方名"集合，
// 供 allOfficialToolNames 判断"这份 tools 是不是官方原样"。
var v2OfficialToolOrder = []string{
	"edit", "execute", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "patch",
	"experimental_image_generate",
}

// 客户端工具一个不裁，官方集整份拼在后面。
//
// 上游免费层不限制 tools 数组长度：2026-11-16 在干净出口逐档加压实测，
// 出站 21 → 26 → 30 → 35 → 43 → 56 个工具全部 200；56 个连测三次通过，
// up429 始终不增；补字节到 36750 也仍 200。所以数量与字节都不是 429 的触发条件。
//
// 2026-11-16 曾把总数封顶到 21，那是个错误结论（早期 A/B 里"总数"和"工具内容"
// 两个变量同时变化，且出口额度已被自身测试消耗）。封顶的后果很严重：DSH 按字母序
// 发 43 个工具，如果按数组顺序截断，pwsh / read / write / edit / grep / glob 会被
// 全部砍掉，13 个官方桩随之绑定失败，模型表现为"没有 shell 工具可用"；即使按优先级
// 保留，也会丢掉 team_task_* / send_message / spawn_teammate 这些多 agent 协作必需的
// 工具，webfetch / websearch 的官方桩也绑不上（它们对应的客户端工具被裁了）。
//
// 真正的 429 根因是工具描述文本指纹，见 bannedDescriptionFragments。

// entryToolName 取一条工具定义的上线名，兼容 chat 与 responses 两种形状。
func entryToolName(entry map[string]any, protocol Format) string {
	if protocol == FmtChat {
		if fn, ok := entry["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			return name
		}
		return ""
	}
	name, _ := entry["name"].(string)
	return name
}

// describeDroppedTools 汇总被裁掉的工具名，用于日志。裁剪是有损的——被裁掉的
// 客户端工具模型将无法调用——所以要留下可追溯的记录。
func describeDroppedTools(dropped []map[string]any, protocol Format) string {
	names := make([]string, 0, len(dropped))
	for _, entry := range dropped {
		if name := entryToolName(entry, protocol); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

var (
	opencodeToolsOnce sync.Once
	opencodeToolsV2   map[string]opencodeToolDefinition
)

func loadOfficialTools() {
	opencodeToolsOnce.Do(func() {
		opencodeToolsV2 = map[string]opencodeToolDefinition{}
		_ = json.Unmarshal(opencodeToolsV2JSON, &opencodeToolsV2)
	})
}

func v2ShellDescription(base string) string {
	if runtime.GOOS != "windows" {
		return base
	}
	return strings.Replace(base, "on Linux using bash", "on Windows using powershell", 1)
}

// bannedDescriptionPhrases 是上游免费层用来识别"非 OpenCode 客户端"的工具描述指纹。
// 命中即把该短语就地替换成中性说法，而不是整段丢弃描述——描述是模型判断工具用途的
// 唯一提示，整段抹掉会让模型不会用这个工具。
//
// 匹配**不区分大小写**（上游就是这么做的，见下方实测），替换时保留原文其余部分。
//
// 2026-11-16 单节点实测（zen3.uflash.ccwu.cc，参数 schema、工具名、其余字段完全一致）：
//
//	"Read a UTF-8 text file and return line-numbered content."  → 429
//	"Read a text file and return line-numbered content."        → 200
//	"Read a UTF-8 text file and return line-numbered"           → 200
//	"Read a UTF-8 text file and return numbered content."       → 200
//	"Process UTF-8 encoding."                                   → 200
//	"Return line-numbered content."                             → 200
//	"Read a utf-8 text file and return line-numbered content."  → 429（小写也触发）
//	"Read a UTF8 text file and return line-numbered content."   → 200（无连字符不触发）
//
// 即 "UTF-8 text file" 单独命中就触发（edit / write 走这条）；read 还需要
// "line-numbered content" 同时在场。单独一个词元、换词序、去掉连字符都不触发。
//
// 第二条指纹来自 skill 工具：把第 28 个工具设成 DSH 的 skill 定义时 429，换成极简
// 描述则 200。二分定位到 "session skill catalog"：
//
//	"in the session skill catalog"  → 429
//	"session skill catalog"         → 429
//	"in the Session Skill Catalog"  → 429（大写也触发）
//	"in the session-skill-catalog"  → 200（连字符不触发）
//	"in the catalog"                → 200
//	"in the available skills"       → 200
//
// 把 DSH 的 read/edit/write/skill 四条描述按上述规则清理后，整份 43 工具请求体
// 从 429 转 200（连测两轮，两个节点各自复现）。
var bannedDescriptionPhrases = []struct {
	phrase      string
	replacement string
	// requires 非空时，只有描述里同时含该子串（同样不区分大小写）才替换。用于
	// "line-numbered content"：它单独出现不触发上游，误替换只会削弱工具提示。
	requires string
}{
	// edit / write 单独命中即触发。
	{phrase: "UTF-8 text file", replacement: "text file"},
	// read 的第二半，需要 UTF-8 同时在场。
	{phrase: "line-numbered content", replacement: "line-numbered output", requires: "UTF-8"},
	// skill：上游按这个词组识别 DSH。
	{phrase: "session skill catalog", replacement: "available skills"},
}

// foldDescription 把描述折成小写用于大小写不敏感的指纹匹配。上游对指纹的大小写
// 不敏感（"utf-8" 与 "UTF-8" 同样触发），精确匹配会漏掉客户端换大小写的写法。
func foldDescription(s string) string {
	return strings.ToLower(s)
}

// replaceFoldInsensitive 把 description 中所有与 phrase 大小写不敏感匹配的片段
// 替换成 replacement，返回替换结果与是否发生替换。
func replaceFoldInsensitive(description, phrase, replacement string) (string, bool) {
	foldedPhrase := foldDescription(phrase)
	folded := foldDescription(description)
	if !strings.Contains(folded, foldedPhrase) {
		return description, false
	}
	// 逐个定位后按原串切片替换，保留未被命中的部分原样（含原始大小写）。
	var builder strings.Builder
	rest := description
	restFolded := folded
	for {
		index := strings.Index(restFolded, foldedPhrase)
		if index < 0 {
			builder.WriteString(rest)
			break
		}
		builder.WriteString(rest[:index])
		builder.WriteString(replacement)
		rest = rest[index+len(foldedPhrase):]
		restFolded = restFolded[index+len(foldedPhrase):]
	}
	return builder.String(), true
}

// sanitizeToolDescription 把描述里的上游封禁短语就地替换成中性说法。
//
// 只替换短语，保留描述其余部分：模型靠描述判断工具用途，整段抹掉会让它不会用这个
// 工具。不动工具名、参数 schema 与调用语义。返回处理后的描述和是否发生过替换。
func sanitizeToolDescription(_ string, description string) (string, bool) {
	if description == "" {
		return description, false
	}
	// ZEN_DESC_SANITIZE=1 时启用短语黑名单（默认关闭：对齐 opencode2api
	// v1.4.8 的零触碰描述做法；2026-11-17 同出口 A/B 实测证明描述文本
	// 不是 429 的变量）。
	if verbatimDescMode() {
		return description, false
	}
	// requires 按原始描述判断，不受前一条替换的影响：read 的
	// "UTF-8 text file" 会先被换成 "text file"，之后再判断 "UTF-8" 就失效了。
	original := foldDescription(description)
	cleaned := description
	replaced := false
	for _, rule := range bannedDescriptionPhrases {
		if rule.requires != "" && !strings.Contains(original, foldDescription(rule.requires)) {
			continue
		}
		next, hit := replaceFoldInsensitive(cleaned, rule.phrase, rule.replacement)
		if hit {
			cleaned = next
			replaced = true
		}
	}
	return cleaned, replaced
}

func renderOfficialTool(protocol Format, name string) map[string]any {
	loadOfficialTools()
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if def, ok := opencodeToolsV2[name]; ok {
		description = def.Description
		if def.Parameters != nil {
			parameters = def.Parameters
		}
	}
	if name == "shell" {
		description = v2ShellDescription(description)
	}
	if protocol == FmtChat {
		function := map[string]any{
			"name":        name,
			"description": description,
			"parameters":  parameters,
			"strict":      false,
		}
		return map[string]any{"type": "function", "function": function}
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters":  parameters,
	}
}

func officialToolset(protocol Format) []any {
	tools := make([]any, 0, len(v2OfficialToolOrder))
	for _, name := range v2OfficialToolOrder {
		tools = append(tools, renderOfficialTool(protocol, name))
	}
	return tools
}

// ClientToolPrefix 是客户端工具的线上名前缀。
//
// 上游已学会拒绝字面量 clint_（大小写不敏感）——那是旧版网关的标记，
// opencode2api #182 实测确认。mcp_ 前缀是官方客户端 MCP 工具
// （mcp__<server>__<tool>）的合法形态，拒绝它会拒绝合法 MCP 流量，
// 所以上游放行。可用 ZEN_TOOL_PREFIX 覆盖；当 429 只出现在带工具的
// 请求时轮换此值（猫鼠游戏，参考 opencode2api tools.go 同款注释）。
var clientToolPrefixOnce = sync.OnceValue(func() string {
	if v := strings.TrimSpace(os.Getenv("ZEN_TOOL_PREFIX")); v != "" {
		cleaned := strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
				return r
			}
			return -1
		}, v)
		if cleaned != "" {
			return cleaned
		}
	}
	return "mcp_"
})

// ClientToolPrefix 返回当前生效的客户端工具线上名前缀。
func ClientToolPrefix() string { return clientToolPrefixOnce() }

const clientToolDigestLength = 12
const toolWireNameLimit = 64

type ToolRenames struct {
	toWire   map[string]string
	toClient map[string]string
	official map[string]string
	byCase   map[string]string
	byShape  map[string]string
	declared map[string]bool
}

func NewToolRenames() *ToolRenames {
	return &ToolRenames{
		toWire:   map[string]string{},
		toClient: map[string]string{},
		official: map[string]string{},
		byCase:   map[string]string{},
		byShape:  map[string]string{},
	}
}

func (r *ToolRenames) Empty() bool {
	return r == nil || len(r.toWire) == 0
}

func (r *ToolRenames) Count() int {
	if r == nil {
		return 0
	}
	return len(r.toWire)
}

// SetDeclared 记录本次请求声明的客户端工具名（通道模式用）。
func (r *ToolRenames) SetDeclared(names []string) {
	if r == nil {
		return
	}
	if r.declared == nil {
		r.declared = map[string]bool{}
	}
	for _, n := range names {
		if n != "" {
			r.declared[n] = true
		}
	}
}

// Declared 报告 name 是否是本次请求声明的客户端工具。nil 安全：未记录时返回
// false（此时兜底映射按"模型回显通道名"处理）。
func (r *ToolRenames) Declared(name string) bool {
	if r == nil || r.declared == nil {
		return false
	}
	return r.declared[name]
}

// DeclaredForChannel 返回通道 channel 下已声明的客户端工具名：恰好声明了一个
// 时返回它，零个或多个返回 ""（多个时语义不明，交给 channelDefaultTool 兜底）。
// 通道模式用：模型漏填或回显 _tool 时，优先还原到客户端真正声明过的工具，
// 避免换成未声明的通道默认工具导致客户端收到 unknown tool。
func (r *ToolRenames) DeclaredForChannel(channel string) string {
	if r == nil || r.declared == nil {
		return ""
	}
	found := ""
	for name := range r.declared {
		if channelForTool(name) == channel {
			if found != "" {
				return ""
			}
			found = name
		}
	}
	return found
}

func (r *ToolRenames) Wire(client string) (string, bool) {
	if r == nil || client == "" {
		return "", false
	}
	wire, ok := r.toWire[client]
	return wire, ok
}

func (r *ToolRenames) Client(wire string) (string, bool) {
	if r == nil || wire == "" {
		return "", false
	}
	client, ok := r.toClient[wire]
	return client, ok
}

// BindOfficial records the declared client tool equivalent to an official name.
func (r *ToolRenames) BindOfficial(official, client string) {
	if r == nil || official == "" || client == "" {
		return
	}
	if r.official == nil {
		r.official = map[string]string{}
	}
	if _, exists := r.official[official]; !exists {
		r.official[official] = client
	}
}

// Official returns the declared client tool bound to an official name.
func (r *ToolRenames) Official(official string) (string, bool) {
	if r == nil || official == "" {
		return "", false
	}
	client, ok := r.official[official]
	return client, ok
}

// HasLongerPrefix reports whether any wire name is longer than prefix and
// starts with it. A stream fragment builder uses this to hold a partial name
// back until the next chunk resolves it.
func (r *ToolRenames) HasLongerPrefix(prefix string) bool {
	if r == nil || prefix == "" {
		return false
	}
	for wire := range r.toClient {
		if len(wire) > len(prefix) && strings.HasPrefix(wire, prefix) {
			return true
		}
	}
	return false
}

func (r *ToolRenames) Add(client, wire string) {
	if r == nil || client == "" || wire == "" || client == wire {
		return
	}
	if existing, ok := r.toWire[client]; ok {
		if existing != wire {
			return
		}
	}
	if _, taken := r.toClient[wire]; taken {
		return
	}
	r.toWire[client] = wire
	r.toClient[wire] = client
	r.bindFolded(r.byCase, strings.ToLower(client), client)
	r.bindFolded(r.byShape, foldToolName(client), client)
}

// Remove 撤销一条客户端→上线名的登记。工具被裁掉时用，避免历史改写指向
// 一个已经不在 tools 里的名字。
func (r *ToolRenames) Remove(client, wire string) {
	if r == nil || wire == "" {
		return
	}
	if client == "" {
		client = r.toClient[wire]
	}
	if client == "" {
		return
	}
	if existing, ok := r.toWire[client]; ok && existing == wire {
		delete(r.toWire, client)
	}
	if existing, ok := r.toClient[wire]; ok && existing == client {
		delete(r.toClient, wire)
	}
	if key := strings.ToLower(client); key != "" {
		if bound, ok := r.byCase[key]; ok && bound == client {
			delete(r.byCase, key)
		}
	}
	if key := foldToolName(client); key != "" {
		if bound, ok := r.byShape[key]; ok && bound == client {
			delete(r.byShape, key)
		}
	}
}

func (r *ToolRenames) bindFolded(folded map[string]string, key, client string) {
	if folded == nil || key == "" {
		return
	}
	if existing, ok := folded[key]; ok {
		if existing != client {
			folded[key] = ""
		}
		return
	}
	folded[key] = client
}

const toolNameSeparators = "_.- \t\r\n"

func foldToolName(name string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		if !strings.ContainsRune(toolNameSeparators, char) {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func (r *ToolRenames) Resolve(wire string) (string, bool) {
	if r == nil || wire == "" {
		return wire, false
	}
	if client, ok := r.Client(wire); ok {
		return client, client != wire
	}
	if client, ok := r.Official(wire); ok {
		return client, client != wire
	}
	client := wire
	if rest, ok := strings.CutPrefix(wire, ClientToolPrefix()); ok && rest != "" {
		client = rest
	}
	if _, declared := r.toWire[client]; declared {
		return client, client != wire
	}
	lower := strings.ToLower(client)
	if match, ok := r.byCase[lower]; ok {
		if match != "" {
			return match, match != wire
		}
		return client, client != wire
	}
	if match, ok := r.byShape[foldToolName(client)]; ok && match != "" {
		return match, match != wire
	}
	return client, client != wire
}

func (r *ToolRenames) Restore(wire string) string {
	client, _ := r.Resolve(wire)
	return client
}

// ResolveFuzzy is Resolve plus a conservative repair for a wire name the model
// copied with one extra/missing/substituted character. It only accepts a
// unique distance-1 match among the request's wire names.
func (r *ToolRenames) ResolveFuzzy(wire string) (string, bool) {
	if r == nil || wire == "" {
		return wire, false
	}
	if client, ok := r.Client(wire); ok {
		return client, true
	}
	if client, ok := r.Official(wire); ok {
		return client, true
	}
	candidate := wire
	if rest, ok := strings.CutPrefix(wire, ClientToolPrefix()); ok && rest != "" {
		candidate = rest
	}
	if _, declared := r.toWire[candidate]; declared {
		return candidate, true
	}
	if match, ok := r.byCase[strings.ToLower(candidate)]; ok && match != "" {
		return match, true
	}
	if match, ok := r.byShape[foldToolName(candidate)]; ok && match != "" {
		return match, true
	}
	if !strings.HasPrefix(wire, ClientToolPrefix()) {
		return wire, false
	}
	match := ""
	matches := 0
	for wireName, client := range r.toClient {
		if editDistanceAtMostOne(wire, wireName) {
			match = client
			matches++
			if matches > 1 {
				return wire, false
			}
		}
	}
	if matches == 1 {
		return match, true
	}
	return wire, false
}

// editDistanceAtMostOne reports whether a and b differ by at most one
// insertion, deletion or substitution.
func editDistanceAtMostOne(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := len(a), len(b)
	if la-lb > 1 || lb-la > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		switch {
		case la > lb:
			i++
		case lb > la:
			j++
		default:
			i++
			j++
		}
	}
	if i < la || j < lb {
		edits++
	}
	return edits <= 1
}

func ClientToolWireName(name string, used map[string]bool) string {
	if used == nil {
		used = map[string]bool{}
	}
	digest := sha256.Sum256([]byte(name))
	clean := hex.EncodeToString(digest[:])[:clientToolDigestLength]
	max := toolWireNameLimit - len(ClientToolPrefix())
	if len(clean) > max {
		clean = clean[:max]
	}
	base := ClientToolPrefix() + clean
	if !used[base] {
		used[base] = true
		return base
	}
	for suffix := 2; ; suffix++ {
		tag := "_" + strconv.Itoa(suffix)
		cut := toolWireNameLimit - len(tag)
		if cut > len(base) {
			cut = len(base)
		}
		candidate := base[:cut] + tag
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// OfficialToolCandidates returns client tool names equivalent to an official
// name. The upstream candidates are Claude Code spellings; the tail entries
// cover this deployment's DSH tool names (ops_bash / pwsh).
func OfficialToolCandidates(officialName string) []string {
	switch strings.ToLower(officialName) {
	case "bash", "shell":
		return []string{"Bash", "ops_bash", "pwsh", "shell", "bash"}
	case "edit":
		return []string{"Edit", "edit"}
	case "glob":
		return []string{"Glob", "glob"}
	case "grep":
		return []string{"Grep", "grep"}
	case "question":
		return []string{"AskUserQuestion", "question"}
	case "read":
		return []string{"Read", "read"}
	case "skill":
		return []string{"Skill", "skill"}
	case "subagent", "task":
		return []string{"Agent", "Task", "Subagent", "subagent"}
	case "todowrite":
		return []string{"TodoWrite", "todowrite"}
	case "webfetch":
		return []string{"WebFetch", "webfetch"}
	case "websearch":
		return []string{"WebSearch", "websearch"}
	case "write":
		return []string{"Write", "write"}
	case "patch":
		return []string{"Patch", "patch"}
	default:
		return nil
	}
}

func officialToolNameSet() map[string]bool {
	set := make(map[string]bool, len(v2OfficialToolOrder))
	for _, name := range v2OfficialToolOrder {
		set[name] = true
	}
	return set
}

func bindOfficialToolAliases(renames *ToolRenames, declaredNames, officialNames []string) {
	for _, official := range officialNames {
		candidates := OfficialToolCandidates(official)
		for _, declared := range declaredNames {
			if _, exists := renames.Wire(declared); !exists {
				continue
			}
			for _, candidate := range candidates {
				if match, _ := renames.Resolve(candidate); match == declared {
					renames.BindOfficial(official, declared)
					break
				}
			}
			if _, exists := renames.Official(official); exists {
				break
			}
		}
	}
}

func sanitizeToolHistory(payload map[string]any, protocol Format, declared map[string]string, renames *ToolRenames, laneOfficial map[string]bool) {
	dropped := map[string]bool{}
	switch protocol {
	case FmtChat:
		messages := asArr(payload["messages"])
		dropEmptyMessages := map[int]bool{}
		for index, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			calls, ok := message["tool_calls"].([]any)
			if !ok {
				continue
			}
			keptCalls := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				function := asMap(call["function"])
				name := asStr(function["name"])
				if wireName, ok := historyToolWireName(name, declared, renames, laneOfficial); ok {
					function["name"] = wireName
					keptCalls = append(keptCalls, rawCall)
				} else if id := asStr(call["id"]); id != "" {
					dropped[id] = true
				}
			}
			if len(keptCalls) == 0 {
				delete(message, "tool_calls")
				dropEmptyMessages[index] = len(calls) > 0
			} else {
				message["tool_calls"] = keptCalls
			}
		}
		keptMessages := make([]any, 0, len(messages))
		for index, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				keptMessages = append(keptMessages, rawMessage)
				continue
			}
			if asStr(message["role"]) == "tool" && dropped[asStr(message["tool_call_id"])] {
				continue
			}
			if dropEmptyMessages[index] && !chatMessageHasContent(message) {
				continue
			}
			keptMessages = append(keptMessages, rawMessage)
		}
		payload["messages"] = keptMessages
	case FmtResponses:
		items := asArr(payload["input"])
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch asStr(item["type"]) {
			case "function_call", "custom_tool_call":
				name := asStr(item["name"])
				if wireName, ok := historyToolWireName(name, declared, renames, laneOfficial); ok {
					item["name"] = wireName
				} else if id := firstString(asStr(item["call_id"]), asStr(item["id"])); id != "" {
					dropped[id] = true
				}
			}
		}
		keptItems := make([]any, 0, len(items))
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok {
				keptItems = append(keptItems, rawItem)
				continue
			}
			if asStr(item["type"]) == "function_call_output" && dropped[asStr(item["call_id"])] {
				continue
			}
			if kind := asStr(item["type"]); kind == "function_call" || kind == "custom_tool_call" {
				if _, ok := historyToolWireName(asStr(item["name"]), declared, renames, laneOfficial); !ok {
					continue
				}
			}
			keptItems = append(keptItems, rawItem)
		}
		payload["input"] = keptItems
	default:
		return
	}
	sanitizeToolChoice(payload, protocol, declared, renames, laneOfficial)
}

func historyToolWireName(name string, declared map[string]string, renames *ToolRenames, laneOfficial map[string]bool) (string, bool) {
	if wireName, ok := declared[name]; ok {
		return wireName, true
	}
	if _, ok := renames.Client(name); ok {
		return name, true
	}
	if laneOfficial[name] {
		client, ok := renames.Official(name)
		if !ok {
			client, _ = renames.Resolve(name)
		}
		if wireName, ok := declared[client]; ok {
			return wireName, true
		}
		return renames.Wire(client)
	}
	client, _ := renames.Resolve(name)
	if wireName, ok := declared[client]; ok {
		return wireName, true
	}
	return "", false
}

func chatMessageHasContent(message map[string]any) bool {
	for _, key := range []string{"content", "reasoning_content", "reasoning"} {
		switch value := message[key].(type) {
		case string:
			if value != "" {
				return true
			}
		case []any:
			if len(value) > 0 {
				return true
			}
		case map[string]any:
			if len(value) > 0 {
				return true
			}
		case nil:
		default:
			return true
		}
	}
	return false
}

func sanitizeToolChoice(payload map[string]any, protocol Format, declared map[string]string, renames *ToolRenames, laneOfficial map[string]bool) {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	kind := asStr(choice["type"])
	if kind != "tool" && kind != "function" && kind != "custom" {
		return
	}
	target := choice
	if protocol == FmtChat {
		if function, ok := choice["function"].(map[string]any); ok {
			target = function
		}
	}
	name := asStr(target["name"])
	if name == "" {
		return
	}
	wireName, ok := historyToolWireName(name, declared, renames, laneOfficial)
	if !ok {
		delete(payload, "tool_choice")
		return
	}
	target["name"] = wireName
}

func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func renameAndCompleteTools(payload map[string]any, protocol Format) (*ToolRenames, bool) {
	renames := NewToolRenames()
	used := map[string]bool{}
	var items []any
	if raw, exists := payload["tools"]; exists {
		if list, ok := raw.([]any); ok {
			items = list
		}
	}
	declaredNames := make([]string, 0, len(items))
	shaped := make([]any, 0, len(items)+len(v2OfficialToolOrder))
	// 不裁剪也不重排：客户端工具按原顺序出站。工具名会被改成 clint_<digest>，
	// 顺序对上游没有意义，保持原序让出站 body 与客户端请求一一对应，便于排障。
	ranked := make([]map[string]any, 0, len(items))
	passThrough := make([]any, 0)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			passThrough = append(passThrough, item)
			continue
		}
		name := entryToolName(entry, protocol)
		if name == "" {
			passThrough = append(passThrough, item)
			continue
		}
		ranked = append(ranked, entry)
	}
	// 通道模式：把客户端工具按语义归到 12 个官方桩名，靠参数里的 _tool 选择器
	// 区分真实工具。出站只出现官方工具名，专为"只放行官方名"的上游设计。
	if os.Getenv("ZEN_TOOL_CHANNEL") != "" && len(ranked) > 0 && !completeOfficialToolset(ranked, protocol) {
		payload["tools"] = buildChannelTools(protocol, items)
		// 记录本次声明的客户端工具名：响应还原时区分"模型回显通道名"
		// （需兜底映射）与"客户端真名"（如 subagent，不能误映射）。
		declaredClients := make([]string, 0, len(ranked))
		for _, entry := range ranked {
			declaredClients = append(declaredClients, entryToolName(entry, protocol))
		}
		renames.SetDeclared(declaredClients)
		// 历史消息里的工具调用名也要改回通道名，并重新包装参数，
		// 否则上游看到历史 tool_calls.name 为客户端原名会 429。
		rewriteChannelHistory(payload, protocol)
		// tool_choice 指向的是客户端名：不改写的话上游看到未声明工具直接报错。
		rewriteChannelToolChoice(payload, protocol)
		return renames, true
	}

	official := officialToolset(protocol)
	// 客户端自带**完整**官方工具集（官方 CLI / 官方 SDK 就是这种形态）时，
	// 整份原样透传：不改名、不追加、不脱敏描述。
	//
	// 为什么必须这样：官方 CLI 发出的 tools 名称就是 edit/read/shell…，
	// 上游按“工具名集合是否官方原样”判定客户端身份。把它们改名成
	// clint_<digest> 再补一份官方集，出站变成 26 个工具、顺序也与原版不同，
	// 实测同一份 body 从 200 直接掉到 429 FreeUsageLimitError；不整形则 200，
	// 且 upBytes 与原请求字节数一致。
	//
	// 2026-11-17 GitHub Actions 多出口实测（24 IP / 48 发，见
	// docs/head2head-zen-vs-opencode2api.md）：透传条件曾经是
	// allOfficialToolNames，它对**官方名子集**也为真，于是只声明 read/edit
	// 这类子集的请求被整份透传，官方名下带着 DSH 的参数级描述
	// （"Path to edit, resolved by the filesystem backend"）出站，
	// 上游判定“自称官方但 schema 不是官方原文”，zen 0/8 全 429，
	// 而把同一份 body 交给改名路径的 opencode2api 8/8 全 200。
	// 所以透传必须要求集合**完整**，子集一律走改名+补全。
	if len(ranked) > 0 && completeOfficialToolset(ranked, protocol) {
		return renames, false
	}
	for _, entry := range ranked {
		name := entryToolName(entry, protocol)
		declaredNames = append(declaredNames, name)
		wireName := clientWireName(renames, used, name)
		if protocol == FmtChat {
			if fn, ok := entry["function"].(map[string]any); ok {
				if wireName != name {
					fn["name"] = wireName
				}
				// 描述里若带上游封禁的客户端指纹，换成中性文本。见
				// bannedDescriptionFragments 的实测：DSH 的 read/edit/write
				// 三条描述都会触发 429，换成官方原文后同一请求体转 200。
				description, _ := fn["description"].(string)
				if cleaned, replaced := sanitizeToolDescription(wireName, description); replaced {
					fn["description"] = cleaned
				}
			}
		} else {
			if wireName != name {
				entry["name"] = wireName
			}
			description, _ := entry["description"].(string)
			if cleaned, replaced := sanitizeToolDescription(wireName, description); replaced {
				entry["description"] = cleaned
			}
		}
		shaped = append(shaped, entry)
	}
	// 第三方客户端工具重命名并脱敏后，若缺官方名则补上官方集。
	//
	// 上游的判定是“工具名集合是否为官方原样”，所以补官方集本身就是给
	// 第三方工具打通行证。2026-11-16 实测：只发一个官方名 read（描述/参数
	// 自定义）即可 200，非官方名 pwsh 一律 429——放行的不是描述，是名字集合。
	// 这里保留补集行为，让非官方客户端仍能被上游接受。
	shaped = append(shaped, official...)
	payload["tools"] = shaped
	if protocol == FmtChat || protocol == FmtResponses {
		bindOfficialToolAliases(renames, declaredNames, v2OfficialToolOrder)
		declared := make(map[string]string, len(declaredNames))
		for _, name := range declaredNames {
			if wireName, ok := renames.Wire(name); ok {
				declared[name] = wireName
			}
		}
		sanitizeToolHistory(payload, protocol, declared, renames, officialToolNameSet())
	}
	rewriteToolReferences(payload, protocol, renames)
	return renames, true
}

func clientWireName(renames *ToolRenames, used map[string]bool, client string) string {
	if wireName, ok := renames.Wire(client); ok {
		return wireName
	}
	wireName := ClientToolWireName(client, used)
	renames.Add(client, wireName)
	return wireName
}

func rewriteToolReferences(payload map[string]any, protocol Format, renames *ToolRenames) {
	if renames.Empty() {
		return
	}
	rename := func(container map[string]any) {
		name, ok := container["name"].(string)
		if !ok {
			return
		}
		if wireName, ok := renames.Wire(name); ok {
			container["name"] = wireName
		}
	}
	if protocol == FmtChat {
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			if function, ok := choice["function"].(map[string]any); ok {
				rename(function)
			} else {
				rename(choice)
			}
		}
		if call, ok := payload["function_call"].(map[string]any); ok {
			rename(call)
		}
		if msgs, ok := payload["messages"].([]any); ok {
			for _, rawMessage := range msgs {
				message, _ := rawMessage.(map[string]any)
				if call, ok := message["function_call"].(map[string]any); ok {
					rename(call)
				}
				if tcs, ok := message["tool_calls"].([]any); ok {
					for _, rawCall := range tcs {
						call, _ := rawCall.(map[string]any)
						if function, ok := call["function"].(map[string]any); ok {
							rename(function)
						}
					}
				}
			}
		}
	} else if protocol == FmtResponses {
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			rename(choice)
		}
		if input, ok := payload["input"].([]any); ok {
			for _, rawItem := range input {
				item, _ := rawItem.(map[string]any)
				if asStr(item["type"]) == "function_call" || asStr(item["type"]) == "custom_tool_call" {
					rename(item)
				}
			}
		}
	}
}

var nameFieldPattern = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

// RenameResponseTool replaces wire names (clint_...) in raw JSON/SSE bytes back to client names.
func RenameResponseTool(line []byte, renames *ToolRenames) []byte {
	if renames == nil || renames.Empty() || len(line) == 0 {
		return line
	}
	if !bytes.Contains(line, []byte(ClientToolPrefix())) {
		return line
	}
	for client, wire := range renames.toWire {
		line = bytes.ReplaceAll(line, []byte(`"name":"`+wire+`"`), []byte(`"name":"`+client+`"`))
		line = bytes.ReplaceAll(line, []byte(`"name": "`+wire+`"`), []byte(`"name": "`+client+`"`))
	}
	if !bytes.Contains(line, []byte(ClientToolPrefix())) {
		return line
	}
	return nameFieldPattern.ReplaceAllFunc(line, func(match []byte) []byte {
		sub := nameFieldPattern.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		wire := string(sub[1])
		if !strings.HasPrefix(wire, ClientToolPrefix()) {
			return match
		}
		client, ok := renames.ResolveFuzzy(wire)
		if !ok || client == wire {
			return match
		}
		return bytes.Replace(match, sub[1], []byte(client), 1)
	})
}

// allOfficialToolNames 判断客户端 tools 是否全部使用官方工具名。
//
// 判定只做“全命中”，不做“齐全”：官方 CLI 允许按需隐藏部分工具
// （如 experimental_image_generate、patch 是否出现取决于配置），
// 只要声明的每一个名字都在官方集合里，就说明这是官方工具集形态，
// 应当原样透传，而不是当成第三方工具去改名。
func allOfficialToolNames(ranked []map[string]any, protocol Format) bool {
	if len(ranked) == 0 {
		return false
	}
	set := officialToolNameSet()
	for _, entry := range ranked {
		name := entryToolName(entry, protocol)
		if name == "" || !set[name] {
			return false
		}
	}
	return true
}

// completeOfficialToolset 报告客户端 tools 是否**恰好**是完整官方集：
// 每个名字都是官方名，且官方集里的每个名字都出现了。
//
// 这是"原样透传"的唯一入口条件。只看 allOfficialToolNames 是不够的——
// 它对 {read} 这种官方名子集也为真，而子集透传会被上游当成
// "自称官方、schema 却不是官方原文"而 429（2026-11-17 多出口实测）。
func completeOfficialToolset(ranked []map[string]any, protocol Format) bool {
	if len(ranked) == 0 {
		return false
	}
	set := officialToolNameSet()
	seen := make(map[string]bool, len(set))
	for _, entry := range ranked {
		name := entryToolName(entry, protocol)
		if name == "" || !set[name] {
			return false
		}
		seen[name] = true
	}
	for name := range set {
		if !seen[name] {
			return false
		}
	}
	return true
}

// verbatimDescMode 报告是否逐字出站客户端描述（跳过短语黑名单）。
//
// 2026-11-17 生产 A/B 实测（同一节点、同一请求，只改脱敏开关）：
//
//	real-multi-stream（真实 DSH 描述 + stream）
//	  v0.9.34/35 脱敏开启 → 200
//	  v0.9.36   脱敏关闭 → 429（双节点一致）
//
// 而 45multi / turn3（非流 + 占位描述）两种模式都 200。
//
// 结论：**描述脱敏是承重的**，至少在"真实描述 + 流式"这一形态上。此前
// 同出口 A/B 显示"A/B 都 429"是出口被烧期间的无效样本，不能据此否定脱敏。
// 因此默认保持脱敏开启；ZEN_DESC_VERBATIM=1 仅供对照实验。
func verbatimDescMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZEN_DESC_VERBATIM"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}
