// Package probe actively fetches quota windows for Claude and Codex OAuth
// credentials so the soonest-reset routing strategy has reset times for
// credentials that have not served traffic yet.
//
// Probe results are written into Auth.Quota.Signals using the same
// header-style keys that passive response observation captures, so the
// selector reads a single representation.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	// ClaudeUsageURL is the Anthropic OAuth usage endpoint used by the management UI.
	ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// CodexUsageURL is the ChatGPT usage endpoint used by the management UI.
	CodexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

	claudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	claudeOAuthBeta  = "oauth-2025-04-20"
	codexUserAgent   = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	defaultTimeout   = 10 * time.Second
	defaultSpacing   = 500 * time.Millisecond
	maxResponseBytes = 1 << 20
)

// Sink receives probe results. *coreauth.Manager implements it.
type Sink interface {
	ApplyQuotaProbeSignals(authID string, signals map[string]string, observedAt time.Time) bool
}

// ClientFunc returns the HTTP client used to probe one credential, typically
// honoring that credential's proxy settings.
type ClientFunc func(ctx context.Context, auth *coreauth.Auth) *http.Client

// Prober probes credentials sequentially with a per-credential timeout and a
// fixed delay between requests.
type Prober struct {
	// ClientFor returns the HTTP client for a credential. Nil uses http.DefaultClient.
	ClientFor ClientFunc
	// Timeout bounds each credential's probe. Zero uses 10s.
	Timeout time.Duration
	// Spacing is the delay between consecutive probes. Zero uses 500ms; negative disables it.
	Spacing time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
	// ClaudeURL and CodexURL override the endpoints in tests.
	ClaudeURL string
	CodexURL  string
}

// Summary reports the outcome of one probe pass.
type Summary struct {
	Probed  int
	Updated int
	Failed  int
}

// Supports reports whether the credential can be probed: an enabled Claude or
// Codex OAuth credential with an access token.
func Supports(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude", "codex":
	default:
		return false
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	// Expired tokens are skipped; the auto-refresh loop renews them before the next pass.
	if exp, ok := auth.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(time.Now()) {
		return false
	}
	return accessToken(auth) != ""
}

// ProbeAll probes every supported credential in order and applies the results to sink.
func (p *Prober) ProbeAll(ctx context.Context, auths []*coreauth.Auth, sink Sink) Summary {
	var summary Summary
	first := true
	for _, auth := range auths {
		if ctx.Err() != nil {
			break
		}
		if !Supports(auth) {
			continue
		}
		if !first && !p.wait(ctx) {
			break
		}
		first = false
		summary.Probed++
		signals, observedAt, err := p.Probe(ctx, auth)
		if err != nil {
			summary.Failed++
			log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider}).Debugf("quota probe failed: %v", err)
			continue
		}
		if sink != nil && sink.ApplyQuotaProbeSignals(auth.ID, signals, observedAt) {
			summary.Updated++
		}
	}
	return summary
}

// Probe fetches one credential's quota windows and returns them as quota signals.
func (p *Prober) Probe(ctx context.Context, auth *coreauth.Auth) (map[string]string, time.Time, error) {
	if !Supports(auth) {
		return nil, time.Time{}, errors.New("credential not supported")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	isClaude := strings.EqualFold(strings.TrimSpace(auth.Provider), "claude")
	target := firstNonEmpty(p.CodexURL, CodexUsageURL)
	if isClaude {
		target = firstNonEmpty(p.ClaudeURL, ClaudeUsageURL)
	}
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if errReq != nil {
		return nil, time.Time{}, fmt.Errorf("build request: %w", errReq)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken(auth))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if isClaude {
		req.Header.Set("User-Agent", claudeUserAgent)
		req.Header.Set("anthropic-beta", claudeOAuthBeta)
	} else {
		req.Header.Set("User-Agent", codexUserAgent)
		if accountID := metadataString(auth, "account_id"); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	}

	client := http.DefaultClient
	if p.ClientFor != nil {
		if c := p.ClientFor(reqCtx, auth); c != nil {
			client = c
		}
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, time.Time{}, fmt.Errorf("request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota probe: close response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return nil, time.Time{}, fmt.Errorf("read response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, time.Time{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	observedAt := p.now()
	var signals map[string]string
	if isClaude {
		signals = ParseClaudeUsage(body)
	} else {
		signals = ParseCodexUsage(body)
	}
	if len(signals) == 0 {
		return nil, time.Time{}, errors.New("response carried no quota windows")
	}
	return signals, observedAt, nil
}

// ParseClaudeUsage converts an /api/oauth/usage payload into quota signals.
// Utilization is reported as a percentage by the endpoint and stored as a
// fraction, matching the Anthropic-Ratelimit-Unified-*-Utilization headers.
// Reset instants are stored as unix seconds, matching the *-Reset headers.
func ParseClaudeUsage(body []byte) map[string]string {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	signals := make(map[string]string)
	addWindow := func(field, window string) {
		node := root.Get(field)
		if !node.IsObject() {
			return
		}
		prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
		if utilization, ok := numeric(node.Get("utilization")); ok {
			fraction := utilization / 100
			signals[prefix+"Utilization"] = strconv.FormatFloat(fraction, 'f', -1, 64)
			status := "allowed"
			if fraction >= 1 {
				status = "rejected"
			}
			signals[prefix+"Status"] = status
		}
		if t, ok := parseResetsAt(strings.TrimSpace(node.Get("resets_at").String())); ok {
			signals[prefix+"Reset"] = strconv.FormatInt(t.Unix(), 10)
		}
	}
	addWindow("five_hour", "5h")
	addWindow("seven_day", "7d")
	// Model-scoped weekly windows are informational; routing reads the shared 7d window.
	addWindow("seven_day_opus", "7d_opus")
	addWindow("seven_day_sonnet", "7d_sonnet")
	addWindow("seven_day_oauth_apps", "7d_oauth_apps")
	if len(signals) == 0 {
		return nil
	}
	addClaudeExtraUsage(root, signals)
	return signals
}

// addClaudeExtraUsage records whether usage credits (extra usage) are enabled,
// using the overage header names so routing reads one representation, plus
// informational credit keys. Amounts are in cents.
func addClaudeExtraUsage(root gjson.Result, signals map[string]string) {
	extra := root.Get("extra_usage")
	if !extra.IsObject() {
		return
	}
	if enabled := extra.Get("is_enabled"); enabled.IsBool() {
		status := "rejected"
		if enabled.Bool() {
			status = "allowed"
		}
		signals["Anthropic-Ratelimit-Unified-Overage-Status"] = status
	}
	if reason := strings.TrimSpace(extra.Get("disabled_reason").String()); reason != "" {
		signals["Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"] = reason
	}
	if used, ok := numeric(extra.Get("used_credits")); ok {
		signals["X-Usage-Credits-Used-Cents"] = strconv.FormatInt(int64(math.Round(used)), 10)
	}
	if limit, ok := numeric(extra.Get("monthly_limit")); ok {
		signals["X-Usage-Credits-Limit-Cents"] = strconv.FormatInt(int64(math.Round(limit)), 10)
	}
	if ever := extra.Get("credits_ever_enabled"); ever.IsBool() {
		signals["X-Usage-Credits-Ever-Enabled"] = strconv.FormatBool(ever.Bool())
	}
}

// ParseCodexUsage converts a /backend-api/wham/usage payload into quota signals
// using the X-Codex-* header names captured from Codex responses.
func ParseCodexUsage(body []byte) map[string]string {
	if !gjson.ValidBytes(body) {
		return nil
	}
	rateLimit := gjson.GetBytes(body, "rate_limit")
	if !rateLimit.IsObject() {
		return nil
	}
	signals := make(map[string]string)
	for _, window := range []struct{ field, prefix string }{
		{"primary_window", "X-Codex-Primary-"},
		{"secondary_window", "X-Codex-Secondary-"},
	} {
		node := rateLimit.Get(window.field)
		if !node.IsObject() {
			continue
		}
		if used, ok := numeric(node.Get("used_percent")); ok {
			signals[window.prefix+"Used-Percent"] = strconv.FormatFloat(used, 'f', -1, 64)
		}
		if seconds, ok := numeric(node.Get("limit_window_seconds")); ok && seconds > 0 {
			signals[window.prefix+"Window-Minutes"] = strconv.FormatInt(int64(math.Round(seconds/60)), 10)
		}
		if resetAt, ok := numeric(node.Get("reset_at")); ok && resetAt > 0 {
			signals[window.prefix+"Reset-At"] = strconv.FormatInt(int64(resetAt), 10)
		}
		if after, ok := numeric(node.Get("reset_after_seconds")); ok && after >= 0 {
			signals[window.prefix+"Reset-After-Seconds"] = strconv.FormatInt(int64(after), 10)
		}
	}
	if len(signals) == 0 {
		return nil
	}
	if allowed := rateLimit.Get("allowed"); allowed.IsBool() {
		signals["X-Codex-Allowed"] = strconv.FormatBool(allowed.Bool())
	}
	if reached := rateLimit.Get("limit_reached"); reached.IsBool() {
		signals["X-Codex-Limit-Reached"] = strconv.FormatBool(reached.Bool())
	}
	if plan := strings.TrimSpace(gjson.GetBytes(body, "plan_type").String()); plan != "" {
		signals["X-Codex-Plan-Type"] = plan
	}
	return signals
}

func numeric(value gjson.Result) (float64, bool) {
	var parsed float64
	switch value.Type {
	case gjson.Number:
		parsed = value.Float()
	case gjson.String:
		var err error
		parsed, err = strconv.ParseFloat(strings.TrimSpace(value.Str), 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func parseResetsAt(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
		return time.Unix(int64(seconds), 0), true
	}
	return time.Time{}, false
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) wait(ctx context.Context) bool {
	spacing := p.Spacing
	if spacing == 0 {
		spacing = defaultSpacing
	}
	if spacing < 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(spacing)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func accessToken(auth *coreauth.Auth) string {
	if token := metadataString(auth, "access_token"); token != "" {
		return token
	}
	return metadataString(auth, "accessToken")
}

func metadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
