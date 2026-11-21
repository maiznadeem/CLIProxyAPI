package management

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestModelQuotaObservationPayloadOmitsUnsupportedProviders(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"grok-4": {
			Quota: coreauth.QuotaState{
				ObservedAt: time.Unix(10, 0),
				Signals:    map[string]string{"X-Ratelimit-Remaining-Requests": "1"},
			},
		},
	}
	if got := modelQuotaObservationPayload("grok", states); len(got) != 0 {
		t.Fatalf("unsupported provider returned model observations: %#v", got)
	}
	for _, provider := range []string{"gemini", "gemini-interactions", "openai", "openai-compatibility", "plugin-provider"} {
		if got := modelQuotaObservationPayload(provider, states); len(got) != 0 {
			t.Fatalf("provider %q returned model observations: %#v", provider, got)
		}
	}
}

func TestModelQuotaObservationPayloadSkipsNilAndEmptyStates(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"nil":   nil,
		"empty": &coreauth.ModelState{},
		"observed": &coreauth.ModelState{Quota: coreauth.QuotaState{
			ObservedAt: time.Unix(10, 0),
			Signals:    map[string]string{"X-Codex-Plan-Type": "pro"},
		}},
	}
	got := modelQuotaObservationPayload("codex", states)
	if len(got) != 1 {
		t.Fatalf("model observations = %#v, want only observed state", got)
	}
	if _, ok := got["observed"]; !ok {
		t.Fatalf("observed model quota missing: %#v", got)
	}
}

func TestQuotaObservationPayloadExcludesCooldownState(t *testing.T) {
	payload := quotaObservationPayload(coreauth.QuotaState{
		Exceeded:      true,
		Reason:        "credential_quota",
		NextRecoverAt: time.Unix(20, 0),
		BackoffLevel:  3,
		ObservedAt:    time.Unix(10, 0),
		Signals:       map[string]string{"X-Codex-Plan-Type": "pro"},
	})
	if _, ok := payload["exceeded"]; ok {
		t.Fatalf("cooldown exceeded leaked: %#v", payload)
	}
	if _, ok := payload["reason"]; ok {
		t.Fatalf("cooldown reason leaked: %#v", payload)
	}
	if _, ok := payload["next_recover_at"]; ok {
		t.Fatalf("cooldown recovery leaked: %#v", payload)
	}
	if _, ok := payload["backoff_level"]; ok {
		t.Fatalf("cooldown backoff leaked: %#v", payload)
	}
}

func TestQuotaObservationPayloadUsageCredits(t *testing.T) {
	payload := quotaObservationPayload(coreauth.QuotaState{
		ObservedAt: time.Unix(10, 0),
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization":          "1",
			"Anthropic-Ratelimit-Unified-7d-Utilization":          "0.25",
			"Anthropic-Ratelimit-Unified-Overage-Status":          "allowed",
			"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": "",
			"X-Usage-Credits-Used-Cents":                          "120",
			"X-Usage-Credits-Limit-Cents":                         "10000",
			"X-Usage-Credits-Ever-Enabled":                        "true",
		},
	})
	if payload["exhausted"] != true {
		t.Fatalf("exhausted = %v, want true", payload["exhausted"])
	}
	credits, ok := payload["usage_credits"].(gin.H)
	if !ok || credits["enabled"] != true || credits["known"] != true || credits["used_cents"] != int64(120) || credits["limit_cents"] != int64(10000) || credits["ever_enabled"] != true {
		t.Fatalf("usage_credits = %#v", payload["usage_credits"])
	}
	windows, ok := payload["windows"].(gin.H)
	if !ok || windows["five_hour_pct"] != float64(100) || windows["seven_day_pct"] != float64(25) {
		t.Fatalf("windows = %#v", payload["windows"])
	}
}
