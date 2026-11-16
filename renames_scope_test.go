package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 响应还原必须使用“本请求”的 renames；旧实现放在全局，并发请求会互相覆盖，
// 把 wire 名漏给客户端（unknown tool clint_xxx）。
func TestRestoreAliasNamesUsesRequestMap(t *testing.T) {
	a := NewToolRenames()
	a.Add("read", "clint_3316348dbadf")
	b := NewToolRenames()
	b.Add("pwsh", "clint_278a1a850e5c")

	lineA := []byte(`{"tool_calls":[{"function":{"name":"clint_3316348dbadf"}}]}`)
	lineB := []byte(`{"tool_calls":[{"function":{"name":"clint_278a1a850e5c"}}]}`)

	if got := string(restoreAliasNames(lineA, a)); !strings.Contains(got, `"name":"read"`) {
		t.Fatalf("A map did not restore read: %s", got)
	}
	if got := string(restoreAliasNames(lineB, b)); !strings.Contains(got, `"name":"pwsh"`) {
		t.Fatalf("B map did not restore pwsh: %s", got)
	}
	if got := string(restoreAliasNames(lineA, b)); got != string(lineA) {
		t.Fatalf("B map must not touch A line: %s", got)
	}
}

func TestConcurrentRestoreKeepsOwnMapping(t *testing.T) {
	a := NewToolRenames()
	a.Add("read", "clint_3316348dbadf")
	b := NewToolRenames()
	b.Add("pwsh", "clint_278a1a850e5c")

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			got := string(restoreAliasNames([]byte(`"name":"clint_3316348dbadf"`), a))
			if !strings.Contains(got, `"read"`) {
				t.Errorf("A cross-talk: %s", got)
			}
		}()
		go func() {
			defer wg.Done()
			got := string(restoreAliasNames([]byte(`"name":"clint_278a1a850e5c"`), b))
			if !strings.Contains(got, `"pwsh"`) {
				t.Errorf("B cross-talk: %s", got)
			}
		}()
	}
	wg.Wait()
}

func TestPipeSSERestoresWireName(t *testing.T) {
	r := NewToolRenames()
	r.Add("pwsh", "clint_278a1a850e5c")
	rec := httptest.NewRecorder()
	pipeSSE(rec, strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"clint_278a1a850e5c\",\"arguments\":\"\"}}]}}]}\n\n"), "req", "m", 0, r)
	body := rec.Body.String()
	if strings.Contains(body, "clint_278a1a850e5c") || !strings.Contains(body, `"name":"pwsh"`) {
		t.Fatalf("pipeSSE did not restore: %s", body)
	}
}
