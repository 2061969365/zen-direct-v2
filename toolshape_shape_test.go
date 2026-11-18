package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 这组测试钉住 "整形决策可观测性" 的对外形状：
//   - 12 个官方名（官方 CLI 现状：patch 已下架）走改名+补全 → 26 个出站，
//     missing_official = patch / experimental_image_generate
//   - 14 个官方名（完整官方集）走透传 → 14 个出站，不改名不追加
//   - 第三方工具 → 改名+补全，extra_client 记下非官方名
//   - ZEN_TOOL_CHANNEL → 通道模式
//   - 没有任何客户端工具 → none（只补官方桩）
//
// 断言只看"决策记录"，不重复钉整形行为本身（那边由 toolshape_test.go /
// toolshape_official_subset_test.go 负责）。

// resetShapeState 把进程级决策状态清回初始态，让每条用例互不影响。
// 包里其它测试不并发跑（没有 t.Parallel），所以直接重置是安全的。
func resetShapeState(t *testing.T) {
	t.Helper()
	shapeState.mu.Lock()
	shapeState.last = ShapeDecision{}
	shapeState.hasLast = false
	shapeState.counts = nil
	shapeState.mu.Unlock()
}

// shapeTestPayload 造一份 chat 形状的请求，names 决定 tools 数组。
func shapeTestPayload(names []string) map[string]any {
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Tool " + name + ".",
				"parameters":  map[string]any{"type": "object"},
			},
		})
	}
	return map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    tools,
	}
}

func TestShapeDecisionEmptyStateIsSafe(t *testing.T) {
	resetShapeState(t)
	if _, ok := LastShapeDecision(); ok {
		t.Fatal("未发生过整形时 LastShapeDecision 必须返回 false")
	}
	counters := ShapeDecisionCounters()
	if len(counters) != 4 {
		t.Fatalf("counters 必须恒定给出四个模式，实际 %v", counters)
	}
	for _, mode := range []string{shapeModePassthrough, shapeModeChannel, shapeModeRenameComplete, shapeModeNone} {
		if counters[mode] != 0 {
			t.Fatalf("%s 初始计数应为 0，实际 %d", mode, counters[mode])
		}
	}
	var parsed shapeHealthNode
	if err := json.Unmarshal([]byte(ShapeHealthJSON()), &parsed); err != nil {
		t.Fatalf("/health 的 tool_shape 不是合法 JSON: %v", err)
	}
	if parsed.Last != nil {
		t.Fatalf("没有整形时 last 必须是 null，实际 %+v", parsed.Last)
	}
	if len(parsed.Counts) != 4 {
		t.Fatalf("counts 应为四个零值模式，实际 %v", parsed.Counts)
	}
}

// TestShapeDecisionOfficialSubsetTwelve 锁定 v1.0 的 subset 契约：
// 12 个官方名（官方 CLI 现在的实际形态，patch 已不在）走 passthrough，
// 出站 12 个，为 0。
func TestShapeDecisionOfficialSubsetTwelve(t *testing.T) {
	resetShapeState(t)
	clientNames := v2OfficialToolOrder[:12]
	payload := shapeTestPayload(clientNames)
	if _, shaped := renameAndCompleteTools(payload, FmtChat); shaped {
		t.Fatal("12 个官方名应当透传，不该改名+补全")
	}
	decision, ok := LastShapeDecision()
	if !ok {
		t.Fatal("决策没有被记录")
	}
	if decision.Mode != shapeModePassthrough {
		t.Fatalf("mode = %q, want %q", decision.Mode, shapeModePassthrough)
	}
	if decision.ClientTools != 12 {
		t.Fatalf("client_tools = %d, want 12", decision.ClientTools)
	}
	if decision.OutboundTools != 12 {
		t.Fatalf("outbound_tools = %d, want 12（透传不补桩）", decision.OutboundTools)
	}
	if decision.OfficialSetSize != len(v2OfficialToolOrder) {
		t.Fatalf("official_set_size = %d, want %d", decision.OfficialSetSize, len(v2OfficialToolOrder))
	}
	if len(decision.ExtraClient) != 0 {
		t.Fatalf("extra_client 应为空，实际 %v", decision.ExtraClient)
	}
	if decision.Reason == "" {
		t.Fatal("reason 不能为空")
	}
}

func TestShapeDecisionFullOfficialPassthrough(t *testing.T) {
	resetShapeState(t)
	payload := shapeTestPayload(v2OfficialToolOrder)
	if _, shaped := renameAndCompleteTools(payload, FmtChat); shaped {
		t.Fatal("完整官方集必须透传")
	}
	decision, ok := LastShapeDecision()
	if !ok {
		t.Fatal("透传也要记录决策（否则看不到走了哪条路）")
	}
	if decision.Mode != shapeModePassthrough {
		t.Fatalf("mode = %q, want %q", decision.Mode, shapeModePassthrough)
	}
	if decision.ClientTools != 14 || decision.OutboundTools != 14 {
		t.Fatalf("client_tools/outbound_tools = %d/%d, want 14/14", decision.ClientTools, decision.OutboundTools)
	}
	if len(decision.MissingOfficial) != 0 {
		t.Fatalf("完整官方集不该有 missing_official，实际 %v", decision.MissingOfficial)
	}
	if len(decision.ExtraClient) != 0 {
		t.Fatalf("完整官方集不该有 extra_client，实际 %v", decision.ExtraClient)
	}
	if counters := ShapeDecisionCounters(); counters[shapeModePassthrough] != 1 {
		t.Fatalf("passthrough 计数 = %d, want 1", counters[shapeModePassthrough])
	}
}

// 第三方工具：extra_client 必须点名它，missing_official 是全量官方集。
func TestShapeDecisionThirdPartyExtraClient(t *testing.T) {
	resetShapeState(t)
	payload := shapeTestPayload([]string{"pwsh", "team_task_create"})
	if _, shaped := renameAndCompleteTools(payload, FmtChat); !shaped {
		t.Fatal("第三方工具必须走改名+补全")
	}
	decision, ok := LastShapeDecision()
	if !ok {
		t.Fatal("决策没有被记录")
	}
	if decision.Mode != shapeModeRenameComplete {
		t.Fatalf("mode = %q, want %q", decision.Mode, shapeModeRenameComplete)
	}
	if got, want := strings.Join(decision.ExtraClient, ","), "pwsh,team_task_create"; got != want {
		t.Fatalf("extra_client = %q, want %q（保持客户端顺序）", got, want)
	}
	if got, want := len(decision.MissingOfficial), len(v2OfficialToolOrder); got != want {
		t.Fatalf("missing_official 长度 = %d, want %d", got, want)
	}
	if decision.OutboundTools != 2+len(v2OfficialToolOrder) {
		t.Fatalf("outbound_tools = %d, want %d", decision.OutboundTools, 2+len(v2OfficialToolOrder))
	}
}

// 通道模式：ZEN_TOOL_CHANNEL 生效时记为 channel，出站是收缩后的通道桩。
func TestShapeDecisionChannel(t *testing.T) {
	resetShapeState(t)
	t.Setenv("ZEN_TOOL_CHANNEL", "1")
	payload := shapeTestPayload([]string{"pwsh", "read_file", "web_search"})
	if _, shaped := renameAndCompleteTools(payload, FmtChat); !shaped {
		t.Fatal("通道模式必须返回已整形")
	}
	decision, ok := LastShapeDecision()
	if !ok {
		t.Fatal("决策没有被记录")
	}
	if decision.Mode != shapeModeChannel {
		t.Fatalf("mode = %q, want %q", decision.Mode, shapeModeChannel)
	}
	if decision.ClientTools != 3 {
		t.Fatalf("client_tools = %d, want 3", decision.ClientTools)
	}
	if decision.OutboundTools != toolCount(payload) {
		t.Fatalf("outbound_tools = %d, want %d（必须等于实际出站）", decision.OutboundTools, toolCount(payload))
	}
	if decision.OutboundTools > decision.ClientTools {
		t.Fatalf("通道模式是收缩：%d 个客户端工具不该变成 %d 个出站", decision.ClientTools, decision.OutboundTools)
	}
	if counters := ShapeDecisionCounters(); counters[shapeModeChannel] != 1 {
		t.Fatalf("channel 计数 = %d, want 1", counters[shapeModeChannel])
	}
}

// 一个客户端工具都没有时没有名字可改，单独记 none（只补官方桩）。
func TestShapeDecisionNoClientToolsIsNone(t *testing.T) {
	resetShapeState(t)
	payload := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	renameAndCompleteTools(payload, FmtChat)
	decision, ok := LastShapeDecision()
	if !ok {
		t.Fatal("决策没有被记录")
	}
	if decision.Mode != shapeModeNone {
		t.Fatalf("mode = %q, want %q", decision.Mode, shapeModeNone)
	}
	if decision.ClientTools != 0 {
		t.Fatalf("client_tools = %d, want 0", decision.ClientTools)
	}
	if decision.OutboundTools != toolCount(payload) {
		t.Fatalf("outbound_tools = %d, want %d", decision.OutboundTools, toolCount(payload))
	}
	if got, want := len(decision.MissingOfficial), shapeDecisionMissingCap; got != want {
		t.Fatalf("missing_official 长度 = %d, want %d（上限 14）", got, want)
	}
}

// missing_official 的上限是 14：官方集不可能超，但代码要自己封顶，别依赖调用方。
func TestShapeDecisionMissingCapFallsBack(t *testing.T) {
	decision := summarizeShapeDecision(shapeModeRenameComplete, 0, nil, 0, "cap check")
	if len(decision.MissingOfficial) != len(v2OfficialToolOrder) {
		t.Fatalf("missing_official 长度 = %d, want %d", len(decision.MissingOfficial), len(v2OfficialToolOrder))
	}
	if len(decision.MissingOfficial) > shapeDecisionMissingCap {
		t.Fatalf("missing_official 超过上限 %d", shapeDecisionMissingCap)
	}
}

// 并发安全：整形 + 读取同时跑。
//
// -race 需要 cgo + C 编译器，本机（无 gcc）跑不了，所以这里用不变量兜底：
// 每条读到的快照必须自洽（mode 与 client/outbound 数量互相匹配），计次总和
// 必须等于落盘次数。真机上有编译器时再跑一次
// `CGO_ENABLED=1 go test -race ./...` 收口。
// TestShapeDecisionConcurrentRecording 只验并发安全，不验某一档的判定结果。
// v1.0 起档位固定，这里不再设置环境变量；断言只看快照自洽性。
func TestShapeDecisionConcurrentRecording(t *testing.T) {
	resetShapeState(t)
	var wg sync.WaitGroup
	const workers = 64
	errs := make(chan string, workers*32)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names := []string{fmt.Sprintf("tool_%d", i)}
			if i%2 == 0 {
				names = v2OfficialToolOrder[:12]
			}
			payload := shapeTestPayload(names)
			renameAndCompleteTools(payload, FmtChat)
			for round := 0; round < 8; round++ {
				decision, ok := LastShapeDecision()
				if !ok {
					errs <- fmt.Sprintf("worker %d: 决策缺失", i)
					return
				}
				if decision.Reason == "" {
					errs <- fmt.Sprintf("worker %d: 决策缺少 reason", i)
				}
				// 快照自洽性：读到的必须是一条完整决策，不能是半更新状态。
				switch decision.Mode {
				case shapeModePassthrough:
					// subset 档的 passthrough 允许 missing_official 非空
					// （客户端只声明了官方集的一部分），但不允许有 extra_client。
					if decision.ClientTools != decision.OutboundTools {
						errs <- fmt.Sprintf("worker %d: passthrough %d/%d", i, decision.ClientTools, decision.OutboundTools)
					}
					if len(decision.ExtraClient) != 0 {
						errs <- fmt.Sprintf("worker %d: passthrough 带了 extra_client", i)
					}
				case shapeModeRenameComplete:
					if decision.OutboundTools <= decision.ClientTools {
						errs <- fmt.Sprintf("worker %d: rename_complete 出站 %d <= 客户端 %d", i, decision.OutboundTools, decision.ClientTools)
					}
					if len(decision.MissingOfficial) == 0 {
						errs <- fmt.Sprintf("worker %d: rename_complete 却无 missing_official", i)
					}
				default:
					errs <- fmt.Sprintf("worker %d: 意外 mode %q", i, decision.Mode)
				}
				// 读侧拿到的切片必须是独立副本：改动它不应污染进程状态。
				for idx := range decision.MissingOfficial {
					decision.MissingOfficial[idx] = "poisoned"
				}
				_ = ShapeDecisionCounters()
				_ = ShapeHealthJSON()
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	total := 0
	for _, count := range ShapeDecisionCounters() {
		total += count
	}
	if total != workers {
		t.Fatalf("计次总和 = %d, want %d", total, workers)
	}
	// 污染检查：最后一次决策的 missing_official 不能带上 "poisoned"。
	if last, ok := LastShapeDecision(); ok {
		for _, name := range last.MissingOfficial {
			if name == "poisoned" {
				t.Fatal("LastShapeDecision 返回的切片与内部状态共享底层数组")
			}
		}
	}
}
