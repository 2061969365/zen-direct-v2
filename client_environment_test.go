package main

import (
	"strings"
	"testing"
)

func envPayload(msgs ...map[string]any) map[string]any {
	list := make([]any, 0, len(msgs))
	for _, m := range msgs {
		list = append(list, m)
	}
	return map[string]any{"model": "m", "messages": list}
}

// TestEnvDefaultsToUnknown 客户端不报环境时四个字段全为 unknown，
// 且绝不出现网关主机信息。
func TestEnvDefaultsToUnknown(t *testing.T) {
	got := opencodeEnvironment("deepseek-v4.1-flash-free", clientEnvironment{})
	for _, want := range []string{
		"  Working directory: unknown",
		"  Workspace root folder: unknown",
		"  Is directory a git repo: unknown",
		"  Platform: unknown",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, got)
		}
	}
	// 不能泄漏网关主机路径或平台
	if strings.Contains(got, ":\\") || strings.Contains(got, "/worker") {
		t.Errorf("env 段泄漏了网关主机路径:\n%s", got)
	}
}

// TestEnvUsesClientDeclaration <env> 块里的值要进到出站 prompt。
func TestEnvUsesClientDeclaration(t *testing.T) {
	env, rejected, err := resolveClientEnvironment(envPayload(
		map[string]any{"role": "system", "content": "prefix\n<env>\n  Working directory: /srv/app\n  Workspace root folder: /srv\n  Is directory a git repo: no\n  Platform: linux\n</env>\n"},
	), FmtChat)
	if err != nil || rejected {
		t.Fatalf("不该拒绝: err=%v rejected=%v", err, rejected)
	}
	if env.WorkingDirectory != "/srv/app" || env.WorkspaceRoot != "/srv" {
		t.Fatalf("路径没解析对: %+v", env)
	}
	if env.Platform != "linux" {
		t.Fatalf("平台没解析对: %q", env.Platform)
	}
	if env.IsGitRepo == nil || *env.IsGitRepo {
		t.Fatalf("git 仓库应为 false: %+v", env.IsGitRepo)
	}
	out := opencodeEnvironment("m", env)
	for _, want := range []string{"/srv/app", "/srv", "Platform: linux", "git repo: no"} {
		if !strings.Contains(out, want) {
			t.Errorf("出站 env 缺 %q:\n%s", want, out)
		}
	}
}

// TestEnvIgnoresCodeFence 代码块里的示例不算声明。
func TestEnvIgnoresCodeFence(t *testing.T) {
	text := "example:\n```\n<env>\n  Working directory: /fake\n  Platform: linux\n</env>\n```\n"
	env, found, valid := parseEnvironmentBlocks(text)
	if found {
		t.Errorf("代码块内的 <env> 不该算声明，found=%v env=%+v", found, env)
	}
	if !valid {
		t.Errorf("代码块内容不该让解析判定为格式不良")
	}
}

// TestEnvIgnoresToolResults tool 消息里的 <env> 不算声明。
func TestEnvIgnoresToolResults(t *testing.T) {
	env, _, err := resolveClientEnvironment(envPayload(
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "tool", "tool_call_id": "c1",
			"content": "<env>\n  Working directory: /from-tool\n</env>"},
	), FmtChat)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if env.WorkingDirectory == "/from-tool" {
		t.Fatalf("tool 结果被当成声明了: %+v", env)
	}
}

// TestEnvConflictingBlocksTreatedUnknown 两个块给出不同值 → 整份不可信，
// 但不拒绝请求。
func TestEnvConflictingBlocksTreatedUnknown(t *testing.T) {
	env, rejected, err := resolveClientEnvironment(envPayload(
		map[string]any{"role": "system", "content": "<env>\n  Working directory: /a\n</env>"},
		map[string]any{"role": "developer", "content": "<env>\n  Working directory: /b\n</env>"},
	), FmtChat)
	if err != nil || rejected {
		t.Fatalf("冲突不该拒绝请求: err=%v rejected=%v", err, rejected)
	}
	if !env.empty() {
		t.Fatalf("冲突应整份作废，实际 %+v", env)
	}
}

// TestEnvTopLevelFieldConsumed 顶层 client_environment 读完即删，不转发上游。
func TestEnvTopLevelFieldConsumed(t *testing.T) {
	payload := envPayload(map[string]any{"role": "user", "content": "x"})
	payload["client_environment"] = map[string]any{
		"working_directory": "/app",
		"platform":          "linux",
		"is_git_repo":       true,
	}
	env, rejected, err := resolveClientEnvironment(payload, FmtChat)
	if err != nil || rejected {
		t.Fatalf("不该拒绝: err=%v rejected=%v", err, rejected)
	}
	if env.WorkingDirectory != "/app" || env.Platform != "linux" {
		t.Fatalf("字段没取到: %+v", env)
	}
	if _, exists := payload["client_environment"]; exists {
		t.Fatal("client_environment 必须从 payload 删除，不能转发上游")
	}
}

// TestEnvTopLevelFieldRejectsBadInput 类型错/未知字段 → 400。
func TestEnvTopLevelFieldRejectsBadInput(t *testing.T) {
	cases := []map[string]any{
		{"unknown_field": "x"},
		{"platform": 123},
		{"is_git_repo": "yes"},
		{"platform": "solaris"},
	}
	for _, bad := range cases {
		payload := envPayload(map[string]any{"role": "user", "content": "x"})
		payload["client_environment"] = bad
		_, rejected, err := resolveClientEnvironment(payload, FmtChat)
		if !rejected || err == nil {
			t.Errorf("%v 应被拒绝，实际 rejected=%v err=%v", bad, rejected, err)
		}
	}
}

// TestEnvPlatformNormalized 平台别名归一。
func TestEnvPlatformNormalized(t *testing.T) {
	cases := map[string]string{
		"windows": "win32",
		"win32":   "win32",
		"macos":   "darwin",
		"darwin":  "darwin",
		"linux":   "linux",
	}
	for in, want := range cases {
		got, ok := normalizePlatform(in)
		if !ok || got != want {
			t.Errorf("normalizePlatform(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := normalizePlatform("freebsd"); ok {
		t.Error("未知平台应被拒绝")
	}
}

// TestEnvDeploymentDefault 客户端没报时用 ZEN_PROMPT_ENV 兜底。
func TestEnvDeploymentDefault(t *testing.T) {
	t.Setenv("ZEN_PROMPT_ENV", "working_directory=/deploy;platform=linux;is_git_repo=true")
	effective := effectiveClientEnvironment(clientEnvironment{})
	if effective.WorkingDirectory != "/deploy" || effective.Platform != "linux" {
		t.Fatalf("部署默认没生效: %+v", effective)
	}
	if effective.IsGitRepo == nil || !*effective.IsGitRepo {
		t.Fatalf("is_git_repo 默认应 true: %+v", effective)
	}
}

// TestEnvClientOverridesDeployment 客户端声明优先于部署默认。
func TestEnvClientOverridesDeployment(t *testing.T) {
	t.Setenv("ZEN_PROMPT_ENV", "working_directory=/deploy;platform=linux")
	client := clientEnvironment{WorkingDirectory: "/client"}
	effective := effectiveClientEnvironment(client)
	if effective.WorkingDirectory != "/client" {
		t.Fatalf("客户端值应优先: %q", effective.WorkingDirectory)
	}
	// 客户端没给的字段用部署默认补
	if effective.Platform != "linux" {
		t.Fatalf("缺字段应由部署默认补: %q", effective.Platform)
	}
}
