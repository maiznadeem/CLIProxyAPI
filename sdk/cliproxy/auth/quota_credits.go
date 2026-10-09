package auth

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Quota signal keys describing Anthropic usage credits (extra usage).
const (
	QuotaSignalClaude5hStatus           = "Anthropic-Ratelimit-Unified-5h-Status"
	QuotaSignalClaude7dStatus           = "Anthropic-Ratelimit-Unified-7d-Status"
	QuotaSignalClaudeOverageStatus      = "Anthropic-Ratelimit-Unified-Overage-Status"
	QuotaSignalClaudeOverageReason      = "Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"
	QuotaSignalUsageCreditsUsedCents    = "X-Usage-Credits-Used-Cents"
	QuotaSignalUsageCreditsLimitCents   = "X-Usage-Credits-Limit-Cents"
	QuotaSignalUsageCreditsEverEnabled  = "X-Usage-Credits-Ever-Enabled"
	quotaExhaustedCodexPercentThreshold = 100.0
)

// spendUsageCreditsDisabled gates routing to exhausted credentials that would bill
// usage credits. It defaults to true (allowed).
var spendUsageCreditsDisabled atomic.Bool

// SetSpendUsageCredits sets whether routing may pick credentials that are out of
// quota but have usage credits enabled (and would therefore bill money).
func SetSpendUsageCredits(allow bool) { spendUsageCreditsDisabled.Store(!allow) }

// SpendUsageCredits reports whether routing may spend usage credits.
func SpendUsageCredits() bool { return !spendUsageCreditsDisabled.Load() }

func canonicalQuotaSignals(auth *Auth) map[string]string {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return nil
	}
	signals := make(map[string]string, len(auth.Quota.Signals))
	for key, value := range auth.Quota.Signals {
		signals[http.CanonicalHeaderKey(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return signals
}

// QuotaExhausted reports whether the last observed quota signals show a usage
// window at its limit: any Claude 5h/7d window with utilization >= 1.0 or status
// "rejected", or any Codex window with used-percent >= 100. A window whose reset
// instant has already passed is stale and ignored.
func QuotaExhausted(auth *Auth) bool {
	return quotaExhaustedAt(auth, time.Now())
}

func quotaExhaustedAt(auth *Auth, now time.Time) bool {
	signals := canonicalQuotaSignals(auth)
	if len(signals) == 0 {
		return false
	}
	for _, w := range []struct{ util, status, reset string }{
		{QuotaSignalClaude5hUtilization, QuotaSignalClaude5hStatus, QuotaSignalClaude5hReset},
		{QuotaSignalClaude7dUtilization, QuotaSignalClaude7dStatus, QuotaSignalClaude7dReset},
	} {
		if resetPassed(signals[w.reset], now) {
			continue
		}
		if v, ok := parseQuotaFloat(signals[w.util]); ok && v >= 1.0 {
			return true
		}
		if strings.EqualFold(signals[w.status], "rejected") {
			return true
		}
	}
	for _, prefix := range []string{QuotaSignalCodexPrimaryPrefix, QuotaSignalCodexSecondaryPrefix} {
		used, ok := parseQuotaFloat(signals[prefix+QuotaSignalCodexUsedPercentSuffix])
		if !ok || used < quotaExhaustedCodexPercentThreshold {
			continue
		}
		if resetPassed(signals[prefix+QuotaSignalCodexResetAtSuffix], now) {
			continue
		}
		return true
	}
	return false
}

// resetPassed reports whether raw is a parseable reset instant at or before now.
func resetPassed(raw string, now time.Time) bool {
	t, ok := parseQuotaResetValue(raw)
	return ok && !t.After(now)
}

// UsageCreditsEnabled reports whether the credential's usage credits (extra
// usage) are enabled, from the Overage-Status signal. known is false when the
// signal is absent or unrecognised.
func UsageCreditsEnabled(auth *Auth) (enabled bool, known bool) {
	switch strings.ToLower(canonicalQuotaSignals(auth)[QuotaSignalClaudeOverageStatus]) {
	case "allowed":
		return true, true
	case "rejected":
		return false, true
	default:
		return false, false
	}
}

// UsageCreditsInfo is the usage-credit state derived from quota signals.
type UsageCreditsInfo struct {
	Enabled     bool
	Known       bool
	Reason      string
	UsedCents   int64
	LimitCents  int64
	EverEnabled bool
}

// QuotaUsageCredits returns the usage-credit state for the credential.
func QuotaUsageCredits(auth *Auth) UsageCreditsInfo {
	signals := canonicalQuotaSignals(auth)
	info := UsageCreditsInfo{Reason: signals[QuotaSignalClaudeOverageReason]}
	info.Enabled, info.Known = UsageCreditsEnabled(auth)
	if v, ok := parseQuotaFloat(signals[QuotaSignalUsageCreditsUsedCents]); ok {
		info.UsedCents = int64(v)
	}
	if v, ok := parseQuotaFloat(signals[QuotaSignalUsageCreditsLimitCents]); ok {
		info.LimitCents = int64(v)
	}
	info.EverEnabled, _ = strconv.ParseBool(signals[QuotaSignalUsageCreditsEverEnabled])
	return info
}

// QuotaWindowPercents returns the Claude 5h and 7d window utilization as
// percentages (0-100); each ok flag is false when the window is unknown.
func QuotaWindowPercents(auth *Auth) (fiveHour float64, fiveHourOK bool, sevenDay float64, sevenDayOK bool) {
	signals := canonicalQuotaSignals(auth)
	if v, ok := parseQuotaFloat(signals[QuotaSignalClaude5hUtilization]); ok {
		fiveHour, fiveHourOK = v*100, true
	}
	if v, ok := parseQuotaFloat(signals[QuotaSignalClaude7dUtilization]); ok {
		sevenDay, sevenDayOK = v*100, true
	}
	return
}

// billsUsageCredits reports whether routing to auth now would bill usage credits.
func billsUsageCredits(auth *Auth, now time.Time) bool {
	if !quotaExhaustedAt(auth, now) {
		return false
	}
	enabled, known := UsageCreditsEnabled(auth)
	return enabled && known
}
