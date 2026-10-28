package main

// ua.go: 上游 User-Agent。
//
// 2026-10-27 实测：上游免费层开始按客户端版本放行。同一 IP、同一份抓包 body、
// 同一个 transport：
//
//	UA opencode/latest/2.0.29/cli -> 429（140ms，FreeUsageLimitError）
//	UA opencode/latest/2.1.1/cli  -> 200（591ms，正常 SSE）
//
// 系统提示词换不换都过（我们的 v1 风格提示词配 2.1.1 就是 200），所以版本号
// 本身就是门禁的一部分，写死必然过期。这里用「已验证的版本 + 后台跟 npm 最新」
// 两层：拉不到就守住已验证值，拉到了且同主版本才升。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// uaDefaultVersion 是实测能过门禁的版本。不要随便降：2.0.29 已被拒，2.1.1 在
	// npm 发布 2.2.0（2026-10-27 08:59Z）之后也被拒。宁可高一点。
	uaDefaultVersion = "2.2.0"
	uaRefreshEvery   = 6 * time.Hour
	uaRefreshTimeout = 8 * time.Second
)

// uaRefreshURL 是测试可替换的变量（生产固定指向 npm latest）。
var uaRefreshURL = "https://registry.npmjs.org/@opencode/cli/latest"

// zenUAVersion 是当前使用的客户端版本，可被后台刷新提升。
var zenUAVersion atomic.Value // string

func init() { zenUAVersion.Store(uaDefaultVersion) }

// uaVersion 返回当前版本号。
func uaVersion() string {
	if v, ok := zenUAVersion.Load().(string); ok && v != "" {
		return v
	}
	return uaDefaultVersion
}

// uaString 渲染上游 UA："opencode/latest/<ver>/cli"。
//
// DIRECT_UA 覆盖整串（排障用）；DIRECT_UA_VERSION 只钉版本号。
func uaString() string {
	if v := strings.TrimSpace(os.Getenv("DIRECT_UA")); v != "" {
		return v
	}
	ver := uaVersion()
	if v := strings.TrimSpace(os.Getenv("DIRECT_UA_VERSION")); v != "" {
		ver = v
	}
	return "opencode/latest/" + ver + "/cli"
}

// startUAVersionRefresher 后台跟 npm 上的最新版本。
// 显式指定 UA / 版本、或 DIRECT_UA_REFRESH=0 时不启。
func startUAVersionRefresher() {
	if strings.TrimSpace(os.Getenv("DIRECT_UA")) != "" ||
		strings.TrimSpace(os.Getenv("DIRECT_UA_VERSION")) != "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIRECT_UA_REFRESH"))) {
	case "0", "off", "false", "no":
		return
	}
	go func() {
		refreshUAVersion()
		ticker := time.NewTicker(uaRefreshEvery)
		defer ticker.Stop()
		for range ticker.C {
			refreshUAVersion()
		}
	}()
}

// refreshUAVersion 拉 npm 的 latest 版本并（同主版本时）提升。
// 失败静默：拿不到就继续用已验证的版本，不能因为查询失败把网关带崩。
func refreshUAVersion() {
	refreshUAVersionWithin(uaRefreshTimeout, true)
}

// primeUAVersion 在开始监听前同步拉一次版本。起手探针只给几秒，
// 等后台 goroutine 慢慢刷新必然吃 429 门禁（云端曾因此无限交班）。
// 返回是否真的换了版本。
func primeUAVersion(timeout time.Duration) bool {
	return refreshUAVersionWithin(timeout, true)
}

func refreshUAVersionWithin(timeout time.Duration, verbose bool) bool {
	// 走环境里的代理：本机/国内直连 registry.npmjs.org 常常不通。
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
	resp, err := client.Get(uaRefreshURL)
	if err != nil {
		if verbose {
			log.Printf("ua: refresh failed (%v), keeping %s", err, uaVersion())
		}
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if verbose {
			log.Printf("ua: refresh status %d, keeping %s", resp.StatusCode, uaVersion())
		}
		return false
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false
	}
	latest := strings.TrimSpace(payload.Version)
	if !newerSameMajor(uaVersion(), latest) {
		if verbose {
			log.Printf("ua: npm latest %s, keeping %s", latest, uaVersion())
		}
		return false
	}
	zenUAVersion.Store(latest)
	log.Printf("ua: client version %s -> %s (npm latest)", uaDefaultVersion, latest)
	return true
}

// newerSameMajor 判断 candidate 是否比 current 新、且主版本相同。
// 跨主版本不自动跟：上游未必已经支持，宁可守住能过的版本。
func newerSameMajor(current, candidate string) bool {
	cur, ok1 := parseSemver(current)
	cand, ok2 := parseSemver(candidate)
	if !ok1 || !ok2 || cand[0] != cur[0] {
		return false
	}
	if cand[1] != cur[1] {
		return cand[1] > cur[1]
	}
	return cand[2] > cur[2]
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 {
		return out, false
	}
	for i := 0; i < len(parts) && i < 3; i++ {
		// 预发布后缀（2.2.0-beta.1）只取数字部分。
		digits := parts[i]
		if idx := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); idx >= 0 {
			digits = digits[:idx]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// uaDebug 输出当前 UA（diag 排查用）。
func uaDebug() string { return fmt.Sprintf("%s (default %s)", uaString(), uaDefaultVersion) }
