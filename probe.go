package main

// 传输层自检：只发一条上游请求，验证「当前出口配置能不能过免费层门禁」。
//
// 用途：
//   - CI/云端回归：不碰本地 IP，用真实 CLI 抓包 body + 真 session 跑一发；
//   - 现场排障：确认代理/网络没变、指纹没被上游改规则挡住。
//
// 不经过网关的协议转换与会话池，避免把「传输层问题」和「请求体整形问题」混在一起。
//
// 环境变量：
//
//	ZEN_PROBE_BODY      必填，请求体文件（如 tools/cli_capture_body.json）
//	ZEN_PROBE_SESSION   X-Opencode-Session 等四个会话头（缺省不发）
//	ZEN_PROBE_URL       缺省 https://opencode.ai/inference/v1/chat/completions
//	ZEN_PROBE_UA        缺省 opencode/latest/2.0.29/cli
//	ZEN_TLS             沿用全局传输层开关（bun|go）

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// probeProjectID 是自检模式的 project 选择：显式配置优先（A/B 时要能钉死
// 一个值），没配就现签一个。
func probeProjectID() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_ZEN_PROJECT")); v != "" {
		return v
	}
	return randomProjectID()
}

func runProbe() {
	bodyPath := os.Getenv("ZEN_PROBE_BODY")
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		fmt.Printf("probe: read body %s: %v\n", bodyPath, err)
		os.Exit(2)
	}
	target := env("ZEN_PROBE_URL", "https://opencode.ai/inference/v1/chat/completions")
	session := strings.TrimSpace(os.Getenv("ZEN_PROBE_SESSION"))
	agent := env("ZEN_PROBE_UA", uaString())

	req, err := http.NewRequest("POST", target, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("probe: new request: %v\n", err)
		os.Exit(2)
	}
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agent)
	req.Header.Set("X-Opencode-Client", "cli")
	pid := probeProjectID()
	req.Header.Set("X-Opencode-Project", pid)
	if session != "" {
		for _, name := range []string{
			"X-Opencode-Session", "X-Opencode-Session-Id", "X-Session-Affinity", "X-Session-Id",
		} {
			req.Header.Set(name, session)
		}
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("probe tls=%s bytes=%d -> ERR %v (%dms)\n",
			zenTLS(), len(body), err, time.Since(start).Milliseconds())
		os.Exit(1)
	}
	defer resp.Body.Close()
	head := make([]byte, 200)
	n := 0
	if resp.Body != nil {
		n, _ = resp.Body.Read(head)
	}
	text := strings.ReplaceAll(strings.ToValidUTF8(string(head[:n]), "?"), "\n", " ")
	fmt.Printf("probe tls=%s proj=%s ses=%s bytes=%d -> %d %q retry-after=%q %dms\n",
		zenTLS(), pid, session, len(body), resp.StatusCode, text,
		resp.Header.Get("Retry-After"), time.Since(start).Milliseconds())
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
