package auth

import (
	"testing"
	"time"
)

func TestQuotaResetInstant(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	observed := now.Add(-10 * time.Minute)
	weekly := now.Add(48 * time.Hour)
	short := now.Add(3 * time.Hour)

	cases := []struct {
		name    string
		signals map[string]string
		want    time.Time
		ok      bool
	}{
		{"no signals", nil, time.Time{}, false},
		{"claude weekly over 5h", map[string]string{
			QuotaSignalClaude5hReset: "1800010800",
			QuotaSignalClaude7dReset: "1800172800",
		}, weekly, true},
		{"claude 5h fallback", map[string]string{QuotaSignalClaude5hReset: "1800010800"}, short, true},
		{"claude past weekly falls back to 5h", map[string]string{
			QuotaSignalClaude7dReset: "1799990000",
			QuotaSignalClaude5hReset: "1800010800",
		}, short, true},
		{"claude rfc3339", map[string]string{QuotaSignalClaude7dReset: weekly.Format(time.RFC3339)}, weekly, true},
		{"claude rfc3339 nano", map[string]string{QuotaSignalClaude7dReset: "2027-01-15T08:00:00.123456789Z"}, time.Date(2027, 1, 15, 8, 0, 0, 123456789, time.UTC), true},
		{"claude unified", map[string]string{QuotaSignalClaudeUnifiedReset: "1800010800"}, short, true},
		{"lowercase keys", map[string]string{"anthropic-ratelimit-unified-7d-reset": "1800172800"}, weekly, true},
		{"unix milliseconds", map[string]string{QuotaSignalClaude7dReset: "1800172800000"}, weekly, true},
		{"garbage", map[string]string{QuotaSignalClaude7dReset: "soon"}, time.Time{}, false},
		{"codex secondary reset-at over primary", map[string]string{
			"X-Codex-Primary-Reset-At":   "1800010800",
			"X-Codex-Secondary-Reset-At": "1800172800",
		}, weekly, true},
		{"codex reset-after relative to observed", map[string]string{
			"X-Codex-Secondary-Reset-After-Seconds": "3600",
		}, observed.Add(time.Hour), true},
		{"codex reset-after already elapsed falls back to primary", map[string]string{
			"X-Codex-Secondary-Reset-After-Seconds": "60",
			"X-Codex-Primary-Reset-At":              "1800010800",
		}, short, true},
		{"codex window minutes decide weekly", map[string]string{
			"X-Codex-Primary-Window-Minutes":   "10080",
			"X-Codex-Primary-Reset-At":         "1800172800",
			"X-Codex-Secondary-Window-Minutes": "300",
			"X-Codex-Secondary-Reset-At":       "1800010800",
		}, weekly, true},
		{"codex primary only", map[string]string{
			"X-Codex-Primary-Window-Minutes": "300",
			"X-Codex-Primary-Reset-At":       "1800010800",
		}, short, true},
	}
	for _, tc := range cases {
		auth := &Auth{ID: "x", Quota: QuotaState{ObservedAt: observed, Signals: tc.signals}}
		got, ok := QuotaResetInstant(auth, now)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Fatalf("%s: QuotaResetInstant = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := QuotaResetInstant(nil, now); ok {
		t.Fatal("nil auth reported a reset")
	}
	noObserved := &Auth{Quota: QuotaState{Signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "3600"}}}
	if _, ok := QuotaResetInstant(noObserved, now); ok {
		t.Fatal("relative reset without ObservedAt reported a reset")
	}
}
