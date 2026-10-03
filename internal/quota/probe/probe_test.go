package probe

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

type recordingSink struct {
	mu      sync.Mutex
	applied map[string]map[string]string
	at      map[string]time.Time
}

func (s *recordingSink) ApplyQuotaProbeSignals(authID string, signals map[string]string, observedAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied == nil {
		s.applied = map[string]map[string]string{}
		s.at = map[string]time.Time{}
	}
	s.applied[authID] = signals
	s.at[authID] = observedAt
	return true
}

const claudeUsageBody = `{
  "five_hour": {"utilization": 42.5, "resets_at": "2027-01-15T08:00:00.123456+00:00"},
  "seven_day": {"utilization": 100, "resets_at": "2027-01-20T00:00:00Z"},
  "seven_day_opus": {"utilization": 10, "resets_at": null},
  "seven_day_sonnet": null,
  "extra_usage": {"is_enabled": false}
}`

const codexUsageBody = `{
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1800003600},
    "secondary_window": {"used_percent": "55.5", "limit_window_seconds": 604800, "reset_after_seconds": 86400, "reset_at": 1800086400}
  }
}`

func TestParseClaudeUsage(t *testing.T) {
	signals := ParseClaudeUsage([]byte(claudeUsageBody))
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":      "0.425",
		"Anthropic-Ratelimit-Unified-5h-Status":           "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization":      "1",
		"Anthropic-Ratelimit-Unified-7d-Status":           "rejected",
		"Anthropic-Ratelimit-Unified-7d_opus-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-7d_opus-Status":      "allowed",
	}
	want["Anthropic-Ratelimit-Unified-5h-Reset"] = unixString(time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC))
	want["Anthropic-Ratelimit-Unified-7d-Reset"] = unixString(time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC))
	if len(signals) != len(want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	for key, value := range want {
		if signals[key] != value {
			t.Fatalf("signals[%q] = %q, want %q (all: %v)", key, signals[key], value, signals)
		}
	}

	auth := &coreauth.Auth{Quota: coreauth.QuotaState{Signals: signals, ObservedAt: time.Date(2027, 1, 15, 7, 0, 0, 0, time.UTC)}}
	resetAt, ok := coreauth.QuotaResetInstant(auth, time.Date(2027, 1, 15, 7, 0, 0, 0, time.UTC))
	if !ok || !resetAt.Equal(time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("QuotaResetInstant = %v, %v; want weekly reset", resetAt, ok)
	}

	if ParseClaudeUsage([]byte(`not json`)) != nil || ParseClaudeUsage([]byte(`{"error":{"type":"x"}}`)) != nil {
		t.Fatal("invalid payloads produced signals")
	}
}

func TestParseCodexUsage(t *testing.T) {
	signals := ParseCodexUsage([]byte(codexUsageBody))
	want := map[string]string{
		"X-Codex-Plan-Type":                     "pro",
		"X-Codex-Allowed":                       "true",
		"X-Codex-Limit-Reached":                 "false",
		"X-Codex-Primary-Used-Percent":          "12",
		"X-Codex-Primary-Window-Minutes":        "300",
		"X-Codex-Primary-Reset-After-Seconds":   "3600",
		"X-Codex-Primary-Reset-At":              "1800003600",
		"X-Codex-Secondary-Used-Percent":        "55.5",
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-After-Seconds": "86400",
		"X-Codex-Secondary-Reset-At":            "1800086400",
	}
	if len(signals) != len(want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	for key, value := range want {
		if signals[key] != value {
			t.Fatalf("signals[%q] = %q, want %q", key, signals[key], value)
		}
	}
	now := time.Unix(1_800_000_000, 0)
	resetAt, ok := coreauth.QuotaResetInstant(&coreauth.Auth{Quota: coreauth.QuotaState{Signals: signals, ObservedAt: now}}, now)
	if !ok || resetAt.Unix() != 1800086400 {
		t.Fatalf("QuotaResetInstant = %v, %v; want weekly reset", resetAt, ok)
	}
	if ParseCodexUsage([]byte(`{"plan_type":"free"}`)) != nil {
		t.Fatal("payload without windows produced signals")
	}
}

func TestProbeAll_StubbedHTTP(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	var requests []*http.Request
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		switch {
		case req.URL.String() == ClaudeUsageURL && req.Header.Get("Authorization") == "Bearer claude-bad":
			return jsonResponse(http.StatusUnauthorized, `{"error":"unauthorized"}`), nil
		case req.URL.String() == ClaudeUsageURL:
			return jsonResponse(http.StatusOK, claudeUsageBody), nil
		case req.URL.String() == CodexUsageURL:
			return jsonResponse(http.StatusOK, codexUsageBody), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})}
	prober := &Prober{
		ClientFor: func(context.Context, *coreauth.Auth) *http.Client { return client },
		Spacing:   -1,
		Now:       func() time.Time { return now },
	}
	auths := []*coreauth.Auth{
		{ID: "claude-ok", Provider: "claude", Metadata: map[string]any{"access_token": "claude-token"}},
		{ID: "claude-bad", Provider: "claude", Metadata: map[string]any{"access_token": "claude-bad"}},
		{ID: "codex-ok", Provider: "codex", Metadata: map[string]any{"access_token": "codex-token", "account_id": "acct-1"}},
		{ID: "claude-apikey", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant"}, Metadata: map[string]any{"access_token": "x"}},
		{ID: "claude-disabled", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "x"}},
		{ID: "gemini", Provider: "gemini", Metadata: map[string]any{"access_token": "x"}},
		{ID: "claude-notoken", Provider: "claude"},
	}
	sink := &recordingSink{}
	summary := prober.ProbeAll(context.Background(), auths, sink)
	if summary.Probed != 3 || summary.Updated != 2 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want probed=3 updated=2 failed=1", summary)
	}
	if len(sink.applied) != 2 || sink.applied["claude-ok"] == nil || sink.applied["codex-ok"] == nil {
		t.Fatalf("applied = %v", sink.applied)
	}
	if !sink.at["claude-ok"].Equal(now) {
		t.Fatalf("observedAt = %v, want %v", sink.at["claude-ok"], now)
	}
	for _, req := range requests {
		if req.URL.String() == ClaudeUsageURL {
			if req.Header.Get("anthropic-beta") != claudeOAuthBeta || !strings.HasPrefix(req.Header.Get("User-Agent"), "claude-cli/") {
				t.Fatalf("claude headers = %v", req.Header)
			}
		}
		if req.URL.String() == CodexUsageURL {
			if req.Header.Get("Chatgpt-Account-Id") != "acct-1" || req.Header.Get("Authorization") != "Bearer codex-token" {
				t.Fatalf("codex headers = %v", req.Header)
			}
		}
	}
}

func TestProbeAll_StopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prober := &Prober{ClientFor: func(context.Context, *coreauth.Auth) *http.Client {
		t.Fatal("probe ran after cancellation")
		return nil
	}}
	auths := []*coreauth.Auth{{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "t"}}}
	if summary := prober.ProbeAll(ctx, auths, &recordingSink{}); summary.Probed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func unixString(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
