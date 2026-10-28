package main

import "testing"

func TestStatsUp429CountedSeparately(t *testing.T) {
	statsReset()
	statsAddUp429()
	s := statsSnapshot()
	if s.Up429 != 1 {
		t.Fatalf("Up429 = %d, want 1", s.Up429)
	}
	// 429 分支不会走到 statsAddUpstream；万一以后走到，也不能重复计数。
	statsAddUpstream(429)
	s = statsSnapshot()
	if s.Up429 != 1 {
		t.Fatalf("statsAddUpstream(429) double counted: Up429 = %d", s.Up429)
	}
	if s.Up4xx != 1 {
		t.Fatalf("Up4xx = %d, want 1 (429 still counts as 4xx)", s.Up4xx)
	}
	statsAddUpstream(400)
	s = statsSnapshot()
	if s.Up429 != 1 || s.Up4xx != 2 {
		t.Fatalf("after 400: up429=%d up4xx=%d, want 1/2", s.Up429, s.Up4xx)
	}
	statsAddUpstream(503)
	s = statsSnapshot()
	if s.Up5xx != 1 || s.Up429 != 1 {
		t.Fatalf("after 503: up5xx=%d up429=%d, want 1/1", s.Up5xx, s.Up429)
	}
	statsReset()
}

func TestEgressCacheKeepsLastValue(t *testing.T) {
	setEgressIP("1.2.3.4")
	if got := cachedEgressIP(); got != "1.2.3.4" {
		t.Fatalf("cachedEgressIP = %q", got)
	}
	setEgressIP("")
	if got := cachedEgressIP(); got != "1.2.3.4" {
		t.Fatalf("empty update must not clear: %q", got)
	}
	setEgressIP("5.6.7.8")
	if got := cachedEgressIP(); got != "5.6.7.8" {
		t.Fatalf("cachedEgressIP = %q", got)
	}
}
