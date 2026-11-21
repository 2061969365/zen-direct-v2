package main

// 客户端环境声明：不再把网关主机的信息冒充成客户端环境。
//
// 背景（对齐 opencode2api v1.5.1 / commit 8f691a0）：
//
// 注入官方提示词时，尾部有一段 <env> 块，声明客户端的工作目录、工作区根
// 目录、是否 git 仓库、平台。原实现直接取网关自己的 os.Getwd() 与
// runtime.GOOS，等于对上游宣称"客户端在网关那台机器上"。这是冒充，
// 而且会在客户端真正报了自己的环境时给出矛盾信息。
//
// 现在的规则：
//   1. 优先读客户端自报的 <env> 块（system/developer 指令里）
//   2. 其次读独立 user 消息里的 <environment_context>（Codex 形态）
//   3. 顶层 client_environment 字段显式声明（网关内部扩展，读完即删）
//   4. 都没有就写 unknown —— 不猜、不回退网关主机信息
//
// 必须忽略的：Markdown 代码块里的示例、tool 结果、assistant 消息、
// 任意散文里的路径。只有格式明确的块才算声明。

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// maxClientEnvironmentValueLength 单个环境字段的字节上限。
const maxClientEnvironmentValueLength = 4096

// clientEnvironment 是一次请求的客户端环境快照。
//
// IsGitRepo 用指针区分「客户端没提供」与「客户端明确说是 false」：
// 前者写 unknown，后者写 no。
type clientEnvironment struct {
	WorkingDirectory string
	WorkspaceRoot    string
	Platform         string
	IsGitRepo        *bool
}

// empty 报告四个字段是否全未提供。
func (e clientEnvironment) empty() bool {
	return e.WorkingDirectory == "" && e.WorkspaceRoot == "" &&
		e.Platform == "" && e.IsGitRepo == nil
}

// 环境块里认识的标签（与官方 CLI 输出的 <env> 块逐字对齐）。
var environmentLabels = map[string]string{
	"Working directory":       "working_directory",
	"Workspace root folder":   "workspace_root",
	"Is directory a git repo": "is_git_repo",
	"Platform":                "platform",
}

// envFieldOrder 让 merge 的遍历顺序稳定，便于测试与排障。
var envFieldOrder = []string{"working_directory", "workspace_root", "is_git_repo", "platform"}

// environmentValue 取某字段的字符串形式；未提供的 git 仓库返回 ok=false。
func (e clientEnvironment) environmentValue(field string) (string, bool) {
	switch field {
	case "working_directory":
		return e.WorkingDirectory, e.WorkingDirectory != ""
	case "workspace_root":
		return e.WorkspaceRoot, e.WorkspaceRoot != ""
	case "platform":
		return e.Platform, e.Platform != ""
	case "is_git_repo":
		if e.IsGitRepo == nil {
			return "", false
		}
		if *e.IsGitRepo {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

// setField 写入单个字段；值非法时返回 false。
func (e *clientEnvironment) setField(field, value string) bool {
	value = strings.TrimSpace(value)
	switch field {
	case "working_directory":
		e.WorkingDirectory = value
	case "workspace_root":
		e.WorkspaceRoot = value
	case "platform":
		normalized, ok := normalizePlatform(value)
		if !ok {
			return false
		}
		e.Platform = normalized
	case "is_git_repo":
		switch strings.ToLower(value) {
		case "yes", "true":
			flag := true
			e.IsGitRepo = &flag
		case "no", "false":
			flag := false
			e.IsGitRepo = &flag
		case "unknown", "":
			e.IsGitRepo = nil
		default:
			return false
		}
	default:
		return false
	}
	return true
}

// normalizePlatform 把平台名归一成官方 CLI 的写法。
// 空值合法（表示未提供）；其余只接受四个已知值。
func normalizePlatform(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", true
	case "linux":
		return "linux", true
	case "darwin", "macos", "mac":
		return "darwin", true
	case "win32", "windows":
		return "win32", true
	case "unknown":
		return "", true
	default:
		return "", false
	}
}

// validate 按 opencode2api 的规则校验字段值。
func (e clientEnvironment) validate() error {
	for _, field := range envFieldOrder {
		value, ok := e.environmentValue(field)
		if !ok {
			continue
		}
		if len(value) > maxClientEnvironmentValueLength {
			return fmt.Errorf("client_environment.%s must not exceed %d bytes", field, maxClientEnvironmentValueLength)
		}
		for _, character := range value {
			if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' ||
				character == '<' || character == '>' {
				return fmt.Errorf("client_environment.%s must not contain control characters, line separators, or angle brackets", field)
			}
		}
	}
	return nil
}

// merge 用 candidate 的非空字段覆盖当前值。返回 false 表示出现冲突
// （同一字段两个块给出不同值），调用方应视为「环境不可信」。
func (e *clientEnvironment) merge(candidate clientEnvironment) bool {
	ok := true
	for _, field := range envFieldOrder {
		value, present := candidate.environmentValue(field)
		if !present {
			continue
		}
		if existing, exists := e.environmentValue(field); exists && existing != value {
			ok = false
			continue
		}
		if !e.setField(field, value) {
			ok = false
		}
	}
	return ok
}

// parseEnvironmentBlocks 从一段文本里提取 <env>...</env> 块。
//
// 返回 (环境, 是否出现块, 是否格式良好)。Markdown 代码块里的示例不算声明；
// 未闭合的块、重复的 <env>、无法解析的 git 值都算「格式不良」。
func parseEnvironmentBlocks(text string) (clientEnvironment, bool, bool) {
	var environment clientEnvironment
	found, inside, valid := false, false, true
	var fence rune
	fenceLength := 0

	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		// Markdown 围栏：进入围栏后所有行都被当作示例跳过。
		if !inside && len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
			length := 1
			for length < len(line) && rune(line[length]) == rune(line[0]) {
				length++
			}
			marker := rune(line[0])
			if fence == 0 && length >= 3 {
				fence, fenceLength = marker, length
				continue
			}
			if marker == fence && length >= fenceLength && strings.TrimSpace(line[length:]) == "" {
				fence = 0
				continue
			}
		}
		if fence != 0 {
			continue
		}

		switch line {
		case "<env>":
			if inside {
				valid = false
			}
			found, inside = true, true
			continue
		case "</env>":
			if !inside {
				valid = false
			}
			found, inside = true, false
			continue
		}
		if !inside {
			continue
		}
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field, known := environmentLabels[strings.TrimSpace(label)]
		if !known {
			continue
		}
		if !environment.setField(field, value) {
			valid = false
		}
	}
	if inside {
		valid = false
	}
	return environment, found, valid
}

// parseCodexEnvironment 解析独立 user 消息里的 <environment_context> 块。
func parseCodexEnvironment(text string) (clientEnvironment, bool, bool) {
	var environment clientEnvironment
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<environment_context>") {
		return environment, false, true
	}
	if !strings.HasSuffix(text, "</environment_context>") {
		return environment, true, false
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	if _, err := decoder.Token(); err != nil {
		return environment, true, false
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return environment, true, false
		}
		switch element := token.(type) {
		case xml.CharData:
			_ = element
		case xml.StartElement:
			name := element.Name.Local
			var value string
			if err := decoder.DecodeElement(&value, &element); err != nil {
				return environment, true, false
			}
			field, known := codexEnvironmentField(name)
			if !known {
				continue
			}
			if !environment.setField(field, value) {
				return environment, true, false
			}
		case xml.EndElement:
			return environment, true, true
		}
	}
}

// codexEnvironmentField 把 Codex 的标签名映射到内部字段。
func codexEnvironmentField(name string) (string, bool) {
	switch name {
	case "cwd", "working_directory":
		return "working_directory", true
	case "workspace_root":
		return "workspace_root", true
	case "platform", "os":
		return "platform", true
	case "is_git_repo", "git_repo":
		return "is_git_repo", true
	}
	return "", false
}

// environmentTexts 取一段 content 里的纯文本声明。
//
// 只认 string 与 []text-block（Anthropic 的 text/input_text）；图片、tool
// 结果等混合块一律不算声明。
func environmentTexts(content any) []string {
	if text, ok := content.(string); ok {
		return []string{text}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	texts := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := block["type"].(string)
		if kind != "text" && kind != "input_text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

// decodeClientEnvironment 解析顶层 client_environment 对象。
// 出现未知字段或类型不符一律报错（对应 400）。
func decodeClientEnvironment(raw any) (clientEnvironment, error) {
	var environment clientEnvironment
	fields, ok := raw.(map[string]any)
	if !ok {
		return environment, errors.New("client_environment must be an object")
	}
	for name, value := range fields {
		switch name {
		case "working_directory", "workspace_root", "platform":
			text, ok := value.(string)
			if !ok {
				return environment, fmt.Errorf("client_environment.%s must be a string", name)
			}
			if !environment.setField(name, text) {
				return environment, fmt.Errorf("client_environment.%s has an unsupported value", name)
			}
		case "is_git_repo":
			flag, ok := value.(bool)
			if !ok {
				return environment, errors.New("client_environment.is_git_repo must be a boolean")
			}
			environment.IsGitRepo = &flag
		default:
			return environment, errors.New("client_environment contains an unsupported field")
		}
	}
	return environment, environment.validate()
}

// resolveClientEnvironment 从请求里取出客户端环境声明。
//
// 来源优先级（后面的只在前面未提供该字段时生效，冲突则整份视为不可信）：
//  1. system/developer 指令里的 <env> 块
//  2. 独立 user 消息里的 <environment_context>（最后一次覆盖之前的）
//  3. 顶层 client_environment 对象
//
// 顶层 client_environment 会被**从 payload 删除**，不转发上游。
// 返回 (环境, 是否需要拒绝请求, 错误)。
func resolveClientEnvironment(payload map[string]any, protocol Format) (clientEnvironment, bool, error) {
	var environment clientEnvironment
	if payload == nil {
		return environment, false, nil
	}

	if raw, exists := payload["client_environment"]; exists {
		decoded, err := decodeClientEnvironment(raw)
		delete(payload, "client_environment")
		if err != nil {
			return clientEnvironment{}, true, err
		}
		if !decoded.empty() {
			environment.merge(decoded)
		}
	}

	found, valid := false, true
	mergeBlocks := func(content any) {
		for _, text := range environmentTexts(content) {
			candidate, present, wellFormed := parseEnvironmentBlocks(text)
			if !present {
				continue
			}
			found = true
			if !wellFormed || !environment.merge(candidate) {
				valid = false
			}
		}
	}

	var latest clientEnvironment
	updated := false
	addUser := func(content any) {
		texts := environmentTexts(content)
		if blocks, ok := content.([]any); ok && len(blocks) != len(texts) {
			return // 图文/工具结果混合，不是独立环境声明
		}
		update, present, wellFormed := parseCodexEnvironment(strings.Join(texts, "\n"))
		if !present {
			return
		}
		// 后来的独立更新整体替换之前的（包括它没提供的字段）。
		latest, updated = clientEnvironment{}, true
		if wellFormed {
			latest = update
		}
	}

	field := "messages"
	switch protocol {
	case FmtChat:
	case FmtMessages:
		mergeBlocks(payload["system"])
		if messages, ok := payload["messages"].([]any); ok {
			for _, raw := range messages {
				message, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if role, _ := message["role"].(string); role == "user" {
					addUser(message["content"])
				}
			}
		}
	case FmtResponses:
		mergeBlocks(payload["instructions"])
		field = "input"
		if input, ok := payload[field].(string); ok {
			addUser(input)
		}
	default:
		return environment, false, nil
	}

	if field == "messages" {
		if messages, ok := payload["messages"].([]any); ok {
			for _, raw := range messages {
				message, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				kind, _ := message["type"].(string)
				if kind != "" && kind != "message" {
					continue
				}
				switch role, _ := message["role"].(string); role {
				case "system", "developer":
					mergeBlocks(message["content"])
				case "user":
					addUser(message["content"])
				}
			}
		}
	}

	if updated {
		environment.merge(latest)
		return environment, false, nil
	}
	if !valid {
		// 声明互相矛盾或格式错误：整份环境不可信，按 unknown 处理，
		// 但不拒绝请求（客户端不该因为环境块写歪就整轮失败）。
		return clientEnvironment{}, false, nil
	}
	if !found && environment.empty() {
		return environment, false, nil
	}
	return environment, false, nil
}

// deploymentEnvironment 读运维在 ZEN_PROMPT_ENV 里给的部署级默认值。
//
// 形如：working_directory=/app;platform=linux;is_git_repo=true
// 只在客户端没提供该字段时兜底。
func deploymentEnvironment() clientEnvironment {
	var environment clientEnvironment
	raw := strings.TrimSpace(os.Getenv("ZEN_PROMPT_ENV"))
	if raw == "" {
		return environment
	}
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		field, known := environmentFieldFromConfig(strings.TrimSpace(key))
		if !known {
			continue
		}
		environment.setField(field, value)
	}
	return environment
}

// environmentFieldFromConfig 把配置键映射到内部字段名。
func environmentFieldFromConfig(key string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "working_directory", "workingdirectory":
		return "working_directory", true
	case "workspace_root", "workspaceroot":
		return "workspace_root", true
	case "platform":
		return "platform", true
	case "is_git_repo", "isgitrepo", "git_repo":
		return "is_git_repo", true
	}
	return "", false
}

// effectiveClientEnvironment 合并客户端声明与部署默认值：客户端优先，
// 缺字段用部署默认补。两者都没有的字段保持零值（渲染成 unknown）。
func effectiveClientEnvironment(client clientEnvironment) clientEnvironment {
	if client.empty() {
		return deploymentEnvironment()
	}
	defaults := deploymentEnvironment()
	merged := client
	for _, field := range envFieldOrder {
		if _, present := merged.environmentValue(field); present {
			continue
		}
		value, present := defaults.environmentValue(field)
		if !present {
			continue
		}
		merged.setField(field, value)
	}
	return merged
}
