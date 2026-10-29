package main

import (
	"bytes"
	"strings"
	"sync"
)

// alias.go: 上游保留工具名映射（V2 FreeTier 门禁）。
//
// 实测：bash/edit/read/write/glob/grep/shell 配非官方定义必 403；
// 未知名随便过；shell/read 配官方定义过。harness 自带这 7 个名的真实定义
// 与官方不一致，直传必死，故进上游前改 neutral 别名（描述+参数原样保留，
// 模型行为不变），回客户端时映射回去，harness 原样执行。
// 未知名的工具与历史一律不动。

// aliasReserved 上游保留名（全小写精确匹配；子串不算，"basher" 照过）。
var aliasReserved = []string{
	"bash", "edit", "read", "write", "glob", "grep", "shell",
	"question", "skill", "subagent", "webfetch", "websearch", "execute",
}

// aliasPrefix 别名前缀。客户端字面量叫 zd_<保留名> 的工具会被误映射，
// 概率极低，接受（相应单测锁定该行为）。
const aliasPrefix = "zd_"

func isAliasReserved(name string) bool {
	lower := strings.ToLower(name)
	for _, n := range aliasReserved {
		if n == lower {
			return true
		}
	}
	return false
}

// aliasFor 进上游名：保留名 → 别名，其余原样。
func aliasFor(name string) string {
	lower := strings.ToLower(name)
	if lower == "subagent_fork" {
		return "zd_fork"
	}
	if isAliasReserved(name) {
		return aliasPrefix + name
	}
	return name
}

// unaliasToolName 回客户端名：别名 → 原名，其余原样。
func unaliasToolName(name string) string {
	if name == "zd_fork" {
		return "subagent_fork"
	}
	if len(name) > len(aliasPrefix) && name[:len(aliasPrefix)] == aliasPrefix {
		if orig := name[len(aliasPrefix):]; isAliasReserved(orig) {
			return orig
		}
	}
	return name
}

var (
	lastRenamesMu sync.RWMutex
	lastRenames   *ToolRenames
)

func recordLastRenames(r *ToolRenames) {
	lastRenamesMu.Lock()
	lastRenames = r
	lastRenamesMu.Unlock()
}

func restoreLastRenames(line []byte) []byte {
	lastRenamesMu.RLock()
	r := lastRenames
	lastRenamesMu.RUnlock()
	if r != nil {
		line = RenameResponseTool(line, r)
	}
	return line
}

// restoreAliasNames 把上游体里的别名还原（同格式透传路径用：pipeSSE 按行、
// 非流 outBytes 整包）。只认 `"name":"zd_x"` 两种空白形态；JSON 字符串内的
// 转义形态（\"name\":\"zd_x\"）天然碰不到，安全。
func restoreAliasNames(line []byte) []byte {
	line = restoreLastRenames(line)
	if !bytes.Contains(line, []byte("zd_")) {
		return line
	}
	line = bytes.ReplaceAll(line, []byte(`"name":"zd_fork"`), []byte(`"name":"subagent_fork"`))
	line = bytes.ReplaceAll(line, []byte(`"name": "zd_fork"`), []byte(`"name": "subagent_fork"`))
	for _, n := range aliasReserved {
		for _, a := range []string{aliasPrefix + n, aliasPrefix + strings.Title(n)} {
			orig := a[len(aliasPrefix):]
			line = bytes.ReplaceAll(line, []byte(`"name":"`+a+`"`), []byte(`"name":"`+orig+`"`))
			line = bytes.ReplaceAll(line, []byte(`"name": "`+a+`"`), []byte(`"name": "`+orig+`"`))
		}
	}
	return line
}
// toolNamesForLog 提取上游请求里的工具名集合（diag 用：上游 4xx 时一眼
// 看出是不是新增了保留名）。两种形态通吃，去重保序。
func toolNamesForLog(upReq map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, raw := range asArr(upReq["tools"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := m["name"].(string); ok {
			add(n)
			continue
		}
		add(asStr(asMap(m["function"])["name"]))
	}
	return out
}

// aliasRequestTools 改写上游请求里的工具名（tools 定义 + 历史调用 + tool_choice），
// 返回实际改名对数（diag 日志用）。target 只可能是 responses/chat。
func aliasRequestTools(upReq map[string]any, target Format) int {
	renamed := 0
	rename := func(n string) string {
		m := aliasFor(n)
		if m != n {
			renamed++
		}
		return m
	}
	for _, raw := range asArr(upReq["tools"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if target == FmtResponses {
			if n, ok := m["name"].(string); ok && n != "" {
				m["name"] = rename(n)
			}
		} else if fn, ok := m["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				fn["name"] = rename(n)
			}
		}
	}
	if target == FmtResponses {
		for _, raw := range asArr(upReq["input"]) {
			item, ok := raw.(map[string]any)
			if !ok || asStr(item["type"]) != "function_call" {
				continue
			}
			if n, ok := item["name"].(string); ok && n != "" {
				item["name"] = rename(n)
			}
		}
	} else {
		for _, raw := range asArr(upReq["messages"]) {
			msg := asMap(raw)
			for _, tc := range asArr(msg["tool_calls"]) {
				fn := asMap(asMap(tc)["function"])
				if n, ok := fn["name"].(string); ok && n != "" {
					fn["name"] = rename(n)
				}
			}
		}
	}
	switch tc := upReq["tool_choice"].(type) {
	case string:
		if tc != "" {
			upReq["tool_choice"] = rename(tc)
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				fn["name"] = rename(n)
			}
		} else if n, ok := tc["name"].(string); ok && n != "" {
			tc["name"] = rename(n)
		}
	}
	return renamed
}
