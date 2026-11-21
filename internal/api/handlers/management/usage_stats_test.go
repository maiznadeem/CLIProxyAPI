package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
)

func withUsageStatsLedger(t *testing.T, l *usagestats.Ledger) {
	t.Helper()
	prev := usagestats.Default()
	usagestats.SetDefault(l)
	t.Cleanup(func() { usagestats.SetDefault(prev) })
}

func callUsageStats(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(method, target, nil)
	h := &Handler{}
	if method == http.MethodDelete {
		h.DeleteUsageStats(ginCtx)
	} else {
		h.GetUsageStats(ginCtx)
	}
	return rec
}

func TestGetUsageStatsReturnsAggregates(t *testing.T) {
	l := usagestats.New("", 0)
	now := time.Now()
	l.Add(usagestats.Entry{At: now.Add(-time.Hour), Provider: "codex", Model: "gpt-5", AuthIndex: "a1", Source: "alice", SessionID: "s1", Input: 10, CacheRead: 5, Output: 3})
	l.Add(usagestats.Entry{At: now.Add(-10 * 24 * time.Hour), Provider: "codex", Model: "gpt-5", Input: 99})
	withUsageStatsLedger(t, l)

	rec := callUsageStats(t, http.MethodGet, "/v8/management/observability/usage/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"since", "until", "generated_at", "totals", "by_model", "by_credential", "by_api_key", "by_session", "by_day"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("response missing %q: %s", key, rec.Body.String())
		}
	}
	var totals map[string]int64
	if err := json.Unmarshal(payload["totals"], &totals); err != nil {
		t.Fatal(err)
	}
	// Default window is 7 days, so the 10-day-old entry is excluded.
	if totals["requests"] != 1 || totals["input"] != 10 || totals["cache_read"] != 5 || totals["output"] != 3 || totals["total"] != 18 {
		t.Fatalf("totals = %v", totals)
	}
	var byModel []map[string]any
	if err := json.Unmarshal(payload["by_model"], &byModel); err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 1 || byModel[0]["key"] != "gpt-5" || byModel[0]["provider"] != "codex" || byModel[0]["requests"] != float64(1) {
		t.Fatalf("by_model = %v", byModel)
	}

	rec = callUsageStats(t, http.MethodGet, "/v8/management/observability/usage/stats?since=30d")
	var wide struct {
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wide); err != nil || wide.Totals.Requests != 2 {
		t.Fatalf("since=30d requests = %d err=%v", wide.Totals.Requests, err)
	}
}

func TestGetUsageStatsParsesQuery(t *testing.T) {
	withUsageStatsLedger(t, usagestats.New("", 0))

	for _, target := range []string{
		"/x?since=24h",
		"/x?since=7d&until=1h",
		"/x?since=2026-01-01T00:00:00Z&until=2026-02-01T00:00:00Z",
	} {
		if rec := callUsageStats(t, http.MethodGet, target); rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d body=%s", target, rec.Code, rec.Body.String())
		}
	}
	for _, target := range []string{
		"/x?since=banana",
		"/x?since=-5d",
		"/x?until=nope",
		"/x?since=1h&until=2d", // until before since
	} {
		if rec := callUsageStats(t, http.MethodGet, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestGetUsageStatsDisabledLedgerReturnsEmpty(t *testing.T) {
	withUsageStatsLedger(t, nil)
	rec := callUsageStats(t, http.MethodGet, "/x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ByModel []any `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.ByModel == nil {
		t.Fatalf("by_model should be [] not null: %s", rec.Body.String())
	}
}

func TestDeleteUsageStatsClearsLedger(t *testing.T) {
	l := usagestats.New("", 0)
	l.Add(usagestats.Entry{At: time.Now(), Model: "m", Input: 1})
	l.Add(usagestats.Entry{At: time.Now(), Model: "m", Input: 1})
	withUsageStatsLedger(t, l)

	rec := callUsageStats(t, http.MethodDelete, "/v8/management/observability/usage/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status  string `json:"status"`
		Cleared int    `json:"cleared"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "ok" || resp.Cleared != 2 {
		t.Fatalf("response = %+v err=%v", resp, err)
	}
	if l.Len() != 0 {
		t.Fatalf("ledger len = %d, want 0", l.Len())
	}
}
