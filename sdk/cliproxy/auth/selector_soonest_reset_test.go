package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeAuthResettingAt(id string, reset time.Time, priority int) *Auth {
	auth := &Auth{ID: id, Provider: "claude"}
	if !reset.IsZero() {
		auth.Quota = QuotaState{
			ObservedAt: reset.Add(-time.Hour),
			Signals:    map[string]string{QuotaSignalClaude7dReset: strconv.FormatInt(reset.Unix(), 10)},
		}
	}
	if priority != 0 {
		auth.Attributes = map[string]string{"priority": strconv.Itoa(priority)}
	}
	return auth
}

func soonestResetOrder(t *testing.T, now time.Time, auths []*Auth) []string {
	t.Helper()
	ordered := orderBySoonestReset(auths, now)
	ids := make([]string, len(ordered))
	for i, auth := range ordered {
		ids[i] = auth.ID
	}
	return ids
}

func TestSoonestResetOrder_KnownAscendingThenUnknownFillFirst(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		claudeAuthResettingAt("late", now.Add(72*time.Hour), 0),
		claudeAuthResettingAt("cold-b", time.Time{}, 0),
		claudeAuthResettingAt("soon", now.Add(2*time.Hour), 0),
		claudeAuthResettingAt("cold-a", time.Time{}, 0),
		claudeAuthResettingAt("stale", now.Add(-time.Hour), 0), // past reset counts as unknown
	}
	got := soonestResetOrder(t, now, auths)
	want := []string{"soon", "late", "cold-a", "cold-b", "stale"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSoonestResetSelectorPick_PrefersSoonestReset(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{now: func() time.Time { return now }}
	auths := []*Auth{
		claudeAuthResettingAt("a", now.Add(100*time.Hour), 0),
		claudeAuthResettingAt("b", now.Add(3*time.Hour), 0),
		claudeAuthResettingAt("c", time.Time{}, 0),
	}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want b", got.ID)
	}
}

func TestSoonestResetSelectorPick_NoResetInfoFallsBackToFillFirst(t *testing.T) {
	t.Parallel()
	selector := &SoonestResetSelector{}
	auths := []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "a" {
		t.Fatalf("Pick() = %q, want a", got.ID)
	}
}

func TestSoonestResetSelectorPick_PriorityIsHardTier(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{now: func() time.Time { return now }}
	auths := []*Auth{
		claudeAuthResettingAt("low-soon", now.Add(time.Hour), 0),
		claudeAuthResettingAt("high-late", now.Add(100*time.Hour), 10),
		claudeAuthResettingAt("high-cold", time.Time{}, 10),
	}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "high-late" {
		t.Fatalf("Pick() = %q, want high-late", got.ID)
	}
}

func TestSoonestResetSelectorPick_SkipsExhaustedCredential(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{now: func() time.Time { return now }}
	exhausted := claudeAuthResettingAt("exhausted", now.Add(time.Hour), 0)
	exhausted.Quota.Exceeded = true
	exhausted.Quota.Reason = "credential_quota"
	exhausted.Quota.NextRecoverAt = now.Add(time.Hour)
	disabled := claudeAuthResettingAt("disabled", now.Add(time.Minute), 0)
	disabled.Disabled = true
	auths := []*Auth{exhausted, disabled, claudeAuthResettingAt("ok", now.Add(50*time.Hour), 0)}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "ok" {
		t.Fatalf("Pick() = %q, want ok", got.ID)
	}
}

func TestSoonestResetSelectorPick_AllExhaustedReturnsCooldown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	selector := &SoonestResetSelector{}
	auth := claudeAuthResettingAt("only", now.Add(time.Hour), 0)
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "credential_quota"
	auth.Quota.NextRecoverAt = now.Add(time.Hour)
	if _, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{auth}); err == nil {
		t.Fatal("Pick() error = nil, want unavailable error")
	}
}

func TestSoonestResetSelector_SessionAffinityKeepsBindingAndRebindsToSoonest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()

	late := claudeAuthResettingAt("late", now.Add(100*time.Hour), 0)
	mid := claudeAuthResettingAt("mid", now.Add(10*time.Hour), 0)
	soon := claudeAuthResettingAt("soon", now.Add(2*time.Hour), 0)
	payload := []byte(`{"metadata":{"user_id":"user_xxx_account__session_soonest-reset-uuid"}}`)
	opts := cliproxyexecutor.Options{OriginalRequest: payload}

	first, err := selector.Pick(context.Background(), "claude", "claude-x", opts, []*Auth{late, mid})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if first.ID != "mid" {
		t.Fatalf("cold Pick() = %q, want mid", first.ID)
	}

	// A credential resetting even sooner appears; the bound session must stay put.
	again, err := selector.Pick(context.Background(), "claude", "claude-x", opts, []*Auth{late, mid, soon})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if again.ID != "mid" {
		t.Fatalf("bound Pick() = %q, want mid", again.ID)
	}

	// The bound credential becomes unavailable; failover rebinds to the soonest reset.
	failover, err := selector.Pick(context.Background(), "claude", "claude-x", opts, []*Auth{late, soon})
	if err != nil {
		t.Fatalf("failover Pick() error = %v", err)
	}
	if failover.ID != "soon" {
		t.Fatalf("failover Pick() = %q, want soon", failover.ID)
	}
	sticky, err := selector.Pick(context.Background(), "claude", "claude-x", opts, []*Auth{late, mid, soon})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if sticky.ID != "soon" {
		t.Fatalf("post-failover Pick() = %q, want soon", sticky.ID)
	}
}

func TestManagerApplyQuotaProbeSignals(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	manager := &Manager{auths: map[string]*Auth{"a": {ID: "a", Provider: "claude", Quota: QuotaState{
		Exceeded:   true,
		Reason:     "credential_quota",
		ObservedAt: now,
		Signals:    map[string]string{"Retry-After": "5"},
	}}}}

	if manager.ApplyQuotaProbeSignals("a", map[string]string{QuotaSignalClaude7dReset: "1"}, now.Add(-time.Minute)) {
		t.Fatal("older probe overwrote a fresher snapshot")
	}
	if manager.ApplyQuotaProbeSignals("missing", map[string]string{QuotaSignalClaude7dReset: "1"}, now) {
		t.Fatal("probe applied to unknown auth")
	}
	if !manager.ApplyQuotaProbeSignals("a", map[string]string{"anthropic-ratelimit-unified-7d-reset": "1800003600", "bad": "x\ny"}, now.Add(time.Minute)) {
		t.Fatal("fresh probe was not applied")
	}
	got := manager.auths["a"].Quota
	if got.Signals[QuotaSignalClaude7dReset] != "1800003600" || len(got.Signals) != 1 {
		t.Fatalf("signals = %v", got.Signals)
	}
	if !got.ObservedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("ObservedAt = %v", got.ObservedAt)
	}
	if !got.Exceeded || got.Reason != "credential_quota" {
		t.Fatal("probe modified cooldown fields")
	}
}
