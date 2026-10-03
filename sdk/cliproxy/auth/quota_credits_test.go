package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func authWithSignals(id string, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: signals}}
}

func TestQuotaExhausted(t *testing.T) {
	t.Parallel()
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	cases := []struct {
		name    string
		signals map[string]string
		want    bool
	}{
		{"none", nil, false},
		{"5h under", map[string]string{QuotaSignalClaude5hUtilization: "0.97"}, false},
		{"5h full", map[string]string{QuotaSignalClaude5hUtilization: "1"}, true},
		{"7d full", map[string]string{QuotaSignalClaude7dUtilization: "1.0"}, true},
		{"status rejected", map[string]string{QuotaSignalClaude5hStatus: "rejected"}, true},
		{"status warning", map[string]string{QuotaSignalClaude5hStatus: "allowed_warning"}, false},
		{"full but reset passed", map[string]string{QuotaSignalClaude5hUtilization: "1", QuotaSignalClaude5hReset: past}, false},
		{"full and reset future", map[string]string{QuotaSignalClaude5hUtilization: "1", QuotaSignalClaude5hReset: future}, true},
		{"lowercase key", map[string]string{"anthropic-ratelimit-unified-7d-utilization": "1"}, true},
		{"codex primary", map[string]string{"X-Codex-Primary-Used-Percent": "100"}, true},
		{"codex secondary", map[string]string{"X-Codex-Secondary-Used-Percent": "100"}, true},
		{"codex under", map[string]string{"X-Codex-Primary-Used-Percent": "99.5"}, false},
	}
	for _, tc := range cases {
		if got := QuotaExhausted(authWithSignals("a", tc.signals)); got != tc.want {
			t.Errorf("%s: QuotaExhausted = %v, want %v", tc.name, got, tc.want)
		}
	}
	if QuotaExhausted(nil) {
		t.Error("QuotaExhausted(nil) = true")
	}
}

func TestUsageCreditsEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value         string
		enabled, know bool
	}{
		{"allowed", true, true},
		{"Rejected", false, true},
		{"", false, false},
		{"weird", false, false},
	}
	for _, tc := range cases {
		signals := map[string]string{}
		if tc.value != "" {
			signals[QuotaSignalClaudeOverageStatus] = tc.value
		}
		enabled, known := UsageCreditsEnabled(authWithSignals("a", signals))
		if enabled != tc.enabled || known != tc.know {
			t.Errorf("%q: got (%v,%v), want (%v,%v)", tc.value, enabled, known, tc.enabled, tc.know)
		}
	}
}

func TestQuotaUsageCreditsAndWindows(t *testing.T) {
	t.Parallel()
	a := authWithSignals("a", map[string]string{
		QuotaSignalClaudeOverageStatus:     "rejected",
		QuotaSignalClaudeOverageReason:     "out_of_credits",
		QuotaSignalUsageCreditsUsedCents:   "250",
		QuotaSignalUsageCreditsLimitCents:  "10000",
		QuotaSignalUsageCreditsEverEnabled: "true",
		QuotaSignalClaude5hUtilization:     "0.5",
	})
	info := QuotaUsageCredits(a)
	if info.Enabled || !info.Known || info.Reason != "out_of_credits" || info.UsedCents != 250 || info.LimitCents != 10000 || !info.EverEnabled {
		t.Fatalf("QuotaUsageCredits = %+v", info)
	}
	five, fiveOK, _, sevenOK := QuotaWindowPercents(a)
	if !fiveOK || five != 50 || sevenOK {
		t.Fatalf("QuotaWindowPercents = %v %v %v", five, fiveOK, sevenOK)
	}
}

func exhaustedAuth(id string, reset time.Time, credits string) *Auth {
	a := claudeAuthResettingAt(id, reset, 0)
	a.Quota.Signals[QuotaSignalClaude5hUtilization] = "1"
	if credits != "" {
		a.Quota.Signals[QuotaSignalClaudeOverageStatus] = credits
	}
	return a
}

func TestSoonestResetOrder_ExhaustedLast(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		exhaustedAuth("exhausted-soon", now.Add(time.Minute), "allowed"),
		exhaustedAuth("exhausted-later", now.Add(2*time.Hour), "allowed"),
		claudeAuthResettingAt("ok-late", now.Add(72*time.Hour), 0),
		claudeAuthResettingAt("ok-unknown", time.Time{}, 0),
		claudeAuthResettingAt("ok-soon", now.Add(10*time.Hour), 0),
	}
	got := soonestResetOrder(t, now, auths)
	want := []string{"ok-soon", "ok-late", "ok-unknown", "exhausted-soon", "exhausted-later"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSoonestResetPick_SpendUsageCredits(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{now: func() time.Time { return now }}
	t.Cleanup(func() { SetSpendUsageCredits(true) })
	pick := func(auths ...*Auth) (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	}

	SetSpendUsageCredits(true)
	got, err := pick(exhaustedAuth("billing", now.Add(time.Hour), "allowed"))
	if err != nil || got.ID != "billing" {
		t.Fatalf("spend=true: got %v, %v", got, err)
	}

	SetSpendUsageCredits(false)
	if _, err = pick(exhaustedAuth("billing", now.Add(time.Hour), "allowed")); err == nil {
		t.Fatal("spend=false: want error when only a credit-billing credential remains")
	}
	got, err = pick(exhaustedAuth("billing", now.Add(time.Hour), "allowed"), claudeAuthResettingAt("fresh", now.Add(90*time.Hour), 0))
	if err != nil || got.ID != "fresh" {
		t.Fatalf("spend=false with fresh: got %v, %v", got, err)
	}
	// Exhausted with credits rejected or unknown does not bill, so it stays eligible.
	for _, status := range []string{"rejected", ""} {
		got, err = pick(exhaustedAuth("no-credits", now.Add(time.Hour), status))
		if err != nil || got.ID != "no-credits" {
			t.Fatalf("spend=false credits=%q: got %v, %v", status, got, err)
		}
	}
}
