package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Quota signal keys read by QuotaResetInstant. They match the canonical
// response-header names captured by collectQuotaSignals, so passive header
// observation and active quota probes share one representation.
const (
	QuotaSignalClaude5hReset            = "Anthropic-Ratelimit-Unified-5h-Reset"
	QuotaSignalClaude5hUtilization      = "Anthropic-Ratelimit-Unified-5h-Utilization"
	QuotaSignalClaude7dReset            = "Anthropic-Ratelimit-Unified-7d-Reset"
	QuotaSignalClaude7dUtilization      = "Anthropic-Ratelimit-Unified-7d-Utilization"
	QuotaSignalClaudeUnifiedReset       = "Anthropic-Ratelimit-Unified-Reset"
	QuotaSignalCodexPrimaryPrefix       = "X-Codex-Primary-"
	QuotaSignalCodexSecondaryPrefix     = "X-Codex-Secondary-"
	QuotaSignalCodexUsedPercentSuffix   = "Used-Percent"
	QuotaSignalCodexWindowMinutesSuffix = "Window-Minutes"
	QuotaSignalCodexResetAtSuffix       = "Reset-At"
	QuotaSignalCodexResetAfterSuffix    = "Reset-After-Seconds"
)

// QuotaResetInstant reports when the credential's governing quota window resets,
// based on the last observed quota signals.
//
// The weekly window is consulted first (Claude 7d, Codex weekly window) because
// it holds the largest budget that is lost when it rolls over; the short window
// (Claude 5h, Codex primary) is the fallback. Only instants strictly after now
// are returned: a reset that already passed means the snapshot is stale and the
// next reset is unknown. Absolute values may be unix seconds (or milliseconds),
// RFC3339, or HTTP dates; Codex *-Reset-After-Seconds values are resolved
// relative to Quota.ObservedAt.
func QuotaResetInstant(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}
	signals := make(map[string]string, len(auth.Quota.Signals))
	for key, value := range auth.Quota.Signals {
		signals[http.CanonicalHeaderKey(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	observedAt := auth.Quota.ObservedAt

	for _, key := range []string{QuotaSignalClaude7dReset, QuotaSignalClaude5hReset, QuotaSignalClaudeUnifiedReset} {
		if t, ok := parseQuotaResetValue(signals[key]); ok && t.After(now) {
			return t, true
		}
	}

	for _, prefix := range codexWindowOrder(signals) {
		if t, ok := parseQuotaResetValue(signals[prefix+QuotaSignalCodexResetAtSuffix]); ok && t.After(now) {
			return t, true
		}
		if observedAt.IsZero() {
			continue
		}
		if seconds, ok := parseQuotaFloat(signals[prefix+QuotaSignalCodexResetAfterSuffix]); ok && seconds >= 0 {
			t := observedAt.Add(time.Duration(seconds * float64(time.Second)))
			if t.After(now) {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// codexWindowOrder returns the Codex window prefixes with the longer (weekly)
// window first. Window lengths decide when both are reported; otherwise the
// secondary window is assumed to be the weekly one.
func codexWindowOrder(signals map[string]string) []string {
	primary, primaryOK := parseQuotaFloat(signals[QuotaSignalCodexPrimaryPrefix+QuotaSignalCodexWindowMinutesSuffix])
	secondary, secondaryOK := parseQuotaFloat(signals[QuotaSignalCodexSecondaryPrefix+QuotaSignalCodexWindowMinutesSuffix])
	if primaryOK && secondaryOK && primary > secondary {
		return []string{QuotaSignalCodexPrimaryPrefix, QuotaSignalCodexSecondaryPrefix}
	}
	return []string{QuotaSignalCodexSecondaryPrefix, QuotaSignalCodexPrimaryPrefix}
}

func parseQuotaFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// parseQuotaResetValue parses an absolute reset instant.
func parseQuotaResetValue(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if value, ok := parseQuotaFloat(raw); ok {
		if value <= 0 {
			return time.Time{}, false
		}
		if value > 1e12 {
			// Milliseconds since the epoch.
			return time.UnixMilli(int64(value)), true
		}
		seconds := int64(value)
		return time.Unix(seconds, int64((value-float64(seconds))*1e9)), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}
