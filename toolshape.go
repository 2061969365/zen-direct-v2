package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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
	byCase   map[string]string
	byShape  map[string]string
}

func NewToolRenames() *ToolRenames {
	return &ToolRenames{
		toWire:   map[string]string{},
		toClient: map[string]string{},
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

func renameAndCompleteTools(payload map[string]any, protocol Format) (*ToolRenames, bool) {
	renames := NewToolRenames()
	used := map[string]bool{}
	var items []any
	if raw, exists := payload["tools"]; exists {
		if list, ok := raw.([]any); ok {
			items = list
		}
	}
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
	shaped = append(shaped, officialToolset(protocol)...)
	payload["tools"] = shaped
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
	return line
}
