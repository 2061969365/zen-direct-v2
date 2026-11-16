package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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

var v2OfficialToolOrder = []string{
	"edit", "execute", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "patch",
}

// maxUpstreamTools 是上游免费层接受的 tools 数组长度上限（实测 21）。
//
// 2026-11-16 实测（同一节点、同一分钟交替发送，出站仅差一个工具）：
//   - 13 官方 + 8 客户端 = 21 → 200
//   - 13 官方 + 9 客户端 = 22 → 429 FreeUsageLimitError
//
// 触发条件与工具名无关：网关已把客户端工具全部改名为 clint_<digest>，出站没有
// 重名，纯粹是数组长度越界。DSH 一次请求带 43 个工具，撞上限后每次必 429。
//
// 因此整形后必须把总数压到 maxUpstreamTools 以内：客户端工具优先保留（它们才是
// 模型真正要调的），官方桩只补到能过门禁所需的最小集合。
const maxUpstreamTools = 21

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

const ClientToolPrefix = "clint_"
const clientToolDigestLength = 12
const toolWireNameLimit = 64

type ToolRenames struct {
	toWire   map[string]string
	toClient map[string]string
	official map[string]string
	byCase   map[string]string
	byShape  map[string]string
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
	if rest, ok := strings.CutPrefix(wire, ClientToolPrefix); ok && rest != "" {
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
	if rest, ok := strings.CutPrefix(wire, ClientToolPrefix); ok && rest != "" {
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
	if !strings.HasPrefix(wire, ClientToolPrefix) {
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
	max := toolWireNameLimit - len(ClientToolPrefix)
	if len(clean) > max {
		clean = clean[:max]
	}
	base := ClientToolPrefix + clean
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
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			shaped = append(shaped, item)
			continue
		}
		name := ""
		if protocol == FmtChat {
			if fn, ok := entry["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		} else {
			name, _ = entry["name"].(string)
		}
		if name == "" {
			shaped = append(shaped, item)
			continue
		}
		declaredNames = append(declaredNames, name)
		wireName := clientWireName(renames, used, name)
		if wireName != name {
			if protocol == FmtChat {
				if fn, ok := entry["function"].(map[string]any); ok {
					fn["name"] = wireName
				}
			} else {
				entry["name"] = wireName
			}
		}
		shaped = append(shaped, entry)
	}
	official := officialToolset(protocol)
	// 上游免费层对 tools 数组长度有硬上限（见 maxUpstreamTools）。客户端工具优先
	// 保留——它们才是模型真正要调的；官方集按顺序补到上限为止。被裁掉的客户端
	// 工具必须同时从重命名表里摘掉，否则历史改写会指向一个不存在的工具名。
	if over := len(shaped) + len(official) - maxUpstreamTools; over > 0 && protocol == FmtChat {
		shaped, declaredNames = trimClientTools(shaped, declaredNames, renames, used, over)
	}
	shaped = append(shaped, official...)
	if len(shaped) > maxUpstreamTools {
		// 极小概率：客户端工具本身就超过上限。截到上限，保证不再触发 429。
		shaped = shaped[:maxUpstreamTools]
	}
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

// trimClientTools 从 shaped 里去掉末尾 over 个客户端工具（连同它们的重命名登记），
// 返回裁剪后的工具列表与仍然保留的声明名。裁掉后重命名表里不再有这些客户端名，
// sanitizeToolHistory 就会把历史里对应的调用当作“未声明”一并清掉，保持前后一致。
func trimClientTools(shaped []any, declaredNames []string, renames *ToolRenames, used map[string]bool, over int) ([]any, []string) {
	if over <= 0 || over > len(shaped) {
		over = len(shaped)
	}
	keep := len(shaped) - over
	dropped := shaped[keep:]
	for _, item := range dropped {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		client, wire := "", ""
		if fn, ok := entry["function"].(map[string]any); ok {
			wire, _ = fn["name"].(string)
		}
		if wire == "" {
			wire, _ = entry["name"].(string)
		}
		if wire != "" {
			client, _ = renames.Client(wire)
		}
		renames.Remove(client, wire)
		if wire != "" {
			delete(used, wire)
		}
	}
	trimmed := shaped[:keep]
	keptNames := declaredNames
	if len(dropped) <= len(declaredNames) {
		keptNames = declaredNames[:len(declaredNames)-len(dropped)]
	}
	return trimmed, keptNames
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
	if !bytes.Contains(line, []byte(ClientToolPrefix)) {
		return line
	}
	for client, wire := range renames.toWire {
		line = bytes.ReplaceAll(line, []byte(`"name":"`+wire+`"`), []byte(`"name":"`+client+`"`))
		line = bytes.ReplaceAll(line, []byte(`"name": "`+wire+`"`), []byte(`"name": "`+client+`"`))
	}
	if !bytes.Contains(line, []byte(ClientToolPrefix)) {
		return line
	}
	return nameFieldPattern.ReplaceAllFunc(line, func(match []byte) []byte {
		sub := nameFieldPattern.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		wire := string(sub[1])
		if !strings.HasPrefix(wire, ClientToolPrefix) {
			return match
		}
		client, ok := renames.ResolveFuzzy(wire)
		if !ok || client == wire {
			return match
		}
		return bytes.Replace(match, sub[1], []byte(client), 1)
	})
}
