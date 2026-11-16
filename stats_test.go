package main

import (
	"testing"
	"time"
)

func withStatsClock(t *testing.T, now time.Time) {
	t.Helper()
	old := statsClock
	statsClock = func() time.Time { return now }
	t.Cleanup(func() { statsClock = old })
}

func TestStatsTTFBWindowEvictsOldSamples(t *testing.T) {
	statsReset()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	withStatsClock(t, now)

	statsAddTTFBAt(50, now.Add(-6*time.Minute))
	statsAddTTFBAt(9999, now.Add(-5*time.Minute-time.Second))
	statsAddTTFBAt(100, now.Add(-5*time.Minute))
	statsAddTTFBAt(200, now.Add(-4*time.Minute))
	statsAddTTFBAt(300, now)

	s := statsSnapshot()
	if s.Samples != 3 {
		t.Fatalf("Samples = %d, want 3 (5m 边界内)", s.Samples)
	}
	if s.TTFBP50 != 200 || s.TTFBP95 != 300 {
		t.Fatalf("p50/p95 = %d/%d, want 200/300", s.TTFBP50, s.TTFBP95)
	}
	if s.LastTTFBMs != 300 {
		t.Fatalf("LastTTFBMs = %d, want 300", s.LastTTFBMs)
	}
}

func TestStatsTTFBPercentilesIgnoreOldSamples(t *testing.T) {
	statsReset()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	withStatsClock(t, now)

	statsAddTTFBAt(100000, now.Add(-10*time.Minute))
	for i, ms := range []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100} {
		statsAddTTFBAt(ms, now.Add(-time.Duration(10-i)*time.Second))
	}

	s := statsSnapshot()
	if s.Samples != 10 {
		t.Fatalf("Samples = %d, want 10", s.Samples)
	}
	if s.TTFBP50 != 60 {
		t.Fatalf("TTFBP50 = %d, want 60", s.TTFBP50)
	}
	if s.TTFBP95 != 100 {
		t.Fatalf("TTFBP95 = %d, want 100（旧样本 100000 不得参与）", s.TTFBP95)
	}
}

func TestStatsTTFBRingCapKeepsNewest(t *testing.T) {
	statsReset()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	withStatsClock(t, now)

	for i := int64(1); i <= 300; i++ {
		statsAddTTFBAt(i, now)
	}
	s := statsSnapshot()
	if s.Samples != ttfbRingCap {
		t.Fatalf("Samples = %d, want ring cap %d", s.Samples, ttfbRingCap)
	}
	if s.LastTTFBMs != 300 {
		t.Fatalf("LastTTFBMs = %d, want 300", s.LastTTFBMs)
	}
	// 仅保留 45..300；p50 索引 128 -> 173，p95 索引 243 -> 288。
	if s.TTFBP50 != 173 || s.TTFBP95 != 288 {
		t.Fatalf("p50/p95 = %d/%d, want 173/288", s.TTFBP50, s.TTFBP95)
	}
}

func TestStatsTTFBResetClearsWindow(t *testing.T) {
	statsReset()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	withStatsClock(t, now)

	statsAddTTFBAt(123, now)
	statsReset()

	s := statsSnapshot()
	if s.Samples != 0 || s.TTFBP50 != 0 || s.TTFBP95 != 0 || s.LastTTFBMs != 0 {
		t.Fatalf("reset 后仍有窗口数据: %+v", s)
	}
}
