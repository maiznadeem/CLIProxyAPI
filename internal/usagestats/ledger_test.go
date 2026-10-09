package usagestats

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var baseTime = time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)

func TestQueryAggregates(t *testing.T) {
	l := New("", 0)
	l.now = func() time.Time { return baseTime.Add(time.Hour) }
	sess := "11111111-1111-4111-8111-111111111111"
	l.Add(Entry{At: baseTime, Provider: "codex", Model: "gpt-5", AuthIndex: "a1", Source: "alice@x", APIKey: "sk-aaaaaaaa…wxyz", SessionID: sess, Input: 100, CacheRead: 50, Output: 20, Reasoning: 5})
	l.Add(Entry{At: baseTime.Add(time.Minute), Provider: "claude", Model: "sonnet", AuthIndex: "a2", Source: "bob@x", APIKey: "sk-aaaaaaaa…wxyz", SessionID: sess, Failed: true, Input: 10, CacheWrite: 3, Output: 2})
	l.Add(Entry{At: baseTime.Add(26 * time.Hour), Provider: "codex", Model: "gpt-5", AuthIndex: "a1", Source: "alice@x", Input: 1, Unclassified: 4})

	s := l.Query(baseTime.Add(-time.Hour), time.Time{})
	want := Counters{Requests: 3, Failed: 1, Input: 111, CacheRead: 50, CacheWrite: 3, Output: 22, Reasoning: 5, Total: 195}
	if s.Totals != want {
		t.Fatalf("totals = %+v, want %+v", s.Totals, want)
	}
	if !s.Until.Equal(l.now()) {
		t.Fatalf("until = %v, want generation time", s.Until)
	}

	// gpt-5: (100+50+20+5) + (1+4) = 180; sonnet: 10+3+2 = 15.
	if len(s.ByModel) != 2 || s.ByModel[0].Key != "gpt-5" || s.ByModel[0].Total != 180 {
		t.Fatalf("by_model = %+v", s.ByModel)
	}
	if s.ByModel[0].Provider != "codex" || s.ByModel[1].Total != 15 {
		t.Fatalf("by_model buckets wrong: %+v", s.ByModel)
	}
	if len(s.ByCredential) != 2 || s.ByCredential[0].Key != "a1" || s.ByCredential[0].Source != "alice@x" {
		t.Fatalf("by_credential = %+v", s.ByCredential)
	}
	if len(s.ByAPIKey) != 2 || s.ByAPIKey[0].Key != "sk-aaaaaaaa…wxyz" || s.ByAPIKey[0].Requests != 2 || s.ByAPIKey[1].Key != "unknown" {
		t.Fatalf("by_api_key = %+v", s.ByAPIKey)
	}
	if len(s.BySession) != 1 {
		t.Fatalf("by_session = %+v", s.BySession)
	}
	bs := s.BySession[0]
	if bs.Key != sess || bs.Requests != 2 || len(bs.Models) != 2 || bs.Models[0] != "gpt-5" || len(bs.Credentials) != 2 || bs.Credentials[0] != "alice@x" {
		t.Fatalf("session bucket = %+v", bs)
	}
	if !bs.First.Equal(baseTime) || !bs.Last.Equal(baseTime.Add(time.Minute)) {
		t.Fatalf("session first/last = %v / %v", bs.First, bs.Last)
	}
	if len(s.ByDay) != 2 || s.ByDay[0].Key != "2026-06-15" || s.ByDay[1].Key != "2026-06-16" || s.ByDay[0].Requests != 2 {
		t.Fatalf("by_day = %+v", s.ByDay)
	}
}

func TestQueryWindowAndEmpty(t *testing.T) {
	l := New("", 0)
	l.Add(Entry{At: baseTime, Model: "m", Input: 1})
	l.Add(Entry{At: baseTime.Add(48 * time.Hour), Model: "m", Input: 2})

	s := l.Query(baseTime.Add(time.Hour), baseTime.Add(72*time.Hour))
	if s.Totals.Requests != 1 || s.Totals.Total != 2 {
		t.Fatalf("windowed totals = %+v", s.Totals)
	}
	s = l.Query(baseTime.Add(100*time.Hour), time.Time{})
	if s.Totals.Requests != 0 || s.ByModel == nil || s.BySession == nil || s.ByDay == nil {
		t.Fatalf("empty stats must have non-nil slices: %+v", s)
	}
	var nilLedger *Ledger
	if got := nilLedger.Query(baseTime, time.Time{}); got.Totals.Requests != 0 || got.ByModel == nil {
		t.Fatalf("nil ledger query = %+v", got)
	}
}

func TestSessionsLimitedToTop50(t *testing.T) {
	l := New("", 0)
	for i := 0; i < 60; i++ {
		l.Add(Entry{At: baseTime, Model: "m", SessionID: fmt.Sprintf("s%02d", i), Input: int64(i + 1)})
	}
	s := l.Query(baseTime.Add(-time.Hour), time.Time{})
	if len(s.BySession) != 50 {
		t.Fatalf("sessions = %d, want 50", len(s.BySession))
	}
	if s.BySession[0].Key != "s59" || s.BySession[49].Key != "s10" {
		t.Fatalf("session order: first=%s last=%s", s.BySession[0].Key, s.BySession[49].Key)
	}
}

func TestTrimRetention(t *testing.T) {
	l := New("", 24*time.Hour)
	l.Add(Entry{At: baseTime.Add(-48 * time.Hour), Model: "old"})
	l.Add(Entry{At: baseTime.Add(-time.Hour), Model: "new"})
	l.Add(Entry{At: baseTime.Add(-30 * time.Hour), Model: "old2"})
	l.Trim(baseTime)
	if l.Len() != 1 {
		t.Fatalf("len = %d, want 1", l.Len())
	}
	if got := l.Query(baseTime.Add(-100*time.Hour), time.Time{}).ByModel; len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("remaining = %+v", got)
	}
}

func TestEntryCap(t *testing.T) {
	l := New("", 0)
	l.maxEntries = 10
	for i := 0; i < 11; i++ {
		l.Add(Entry{At: baseTime.Add(time.Duration(i) * time.Second), Input: int64(i)})
	}
	// Exceeding the cap drops the oldest entries down to 90% of it.
	if l.Len() != 9 {
		t.Fatalf("len = %d, want 9", l.Len())
	}
	if l.entries[0].Input != 2 || l.entries[8].Input != 10 {
		t.Fatalf("kept wrong entries: first=%d last=%d", l.entries[0].Input, l.entries[8].Input)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	l := New(path, 0)
	l.Add(Entry{At: time.Now(), Provider: "codex", Model: "gpt-5", Input: 7, Output: 3, SessionID: "s"})
	if err := l.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := l.Flush(); err != nil { // clean ledger is a no-op
		t.Fatalf("second flush: %v", err)
	}

	l2 := New(path, 0)
	if err := l2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if l2.Len() != 1 {
		t.Fatalf("loaded %d entries, want 1", l2.Len())
	}
	if s := l2.Query(time.Now().Add(-time.Hour), time.Time{}); s.Totals.Total != 10 || s.ByModel[0].Key != "gpt-5" {
		t.Fatalf("loaded stats = %+v", s.Totals)
	}

	n, err := l2.Clear()
	if err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	l3 := New(path, 0)
	if err = l3.Load(); err != nil || l3.Len() != 0 {
		t.Fatalf("after clear: len=%d err=%v", l3.Len(), err)
	}
	if matches, _ := filepath.Glob(path + ".*.tmp"); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestLoadTrimsAndHandlesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l := New(path, 24*time.Hour)
	l.Add(Entry{At: time.Now().Add(-72 * time.Hour), Model: "old"})
	l.Add(Entry{At: time.Now(), Model: "new"})
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	l2 := New(path, 24*time.Hour)
	if err := l2.Load(); err != nil || l2.Len() != 1 {
		t.Fatalf("load: len=%d err=%v", l2.Len(), err)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l3 := New(path, 0)
	if err := l3.Load(); err == nil {
		t.Fatal("expected error for corrupt file")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file not moved aside: %v", err)
	}
	if l3.Len() != 0 {
		t.Fatalf("len = %d, want 0", l3.Len())
	}
}

func TestStopFlushesPendingEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l := New(path, 0)
	l.Start()
	l.Add(Entry{At: time.Now(), Model: "m", Input: 1})
	l.Stop()
	l.Stop() // idempotent
	l2 := New(path, 0)
	if err := l2.Load(); err != nil || l2.Len() != 1 {
		t.Fatalf("after stop: len=%d err=%v", l2.Len(), err)
	}
}

func TestHandleUsageExtractsFields(t *testing.T) {
	l := New("", 0)
	at := time.Now()
	detail := coreusage.Detail{
		InputTokens: 100, OutputTokens: 40, ReasoningTokens: 10, CacheReadTokens: 30, TotalTokens: 140,
		TokenBreakdown: coreusage.NewIndependentTokenBreakdown(70, 30, 5, 30, 10, 145),
	}
	l.HandleUsage(context.Background(), coreusage.Record{
		Provider: "claude", Model: "sonnet", AuthIndex: "ix", Source: "me@x",
		APIKey: "sk-1234567890abcdefghij", SessionID: "not-a-uuid", Failed: true,
		RequestedAt: at, Latency: 250 * time.Millisecond, Detail: detail,
	})
	if l.Len() != 1 {
		t.Fatalf("len = %d", l.Len())
	}
	e := l.entries[0]
	if e.Provider != "claude" || e.Model != "sonnet" || e.AuthIndex != "ix" || e.Source != "me@x" || !e.Failed || e.LatencyMS != 250 {
		t.Fatalf("entry = %+v", e)
	}
	if e.APIKey != "sk-1234567…ghij" {
		t.Fatalf("api key = %q", e.APIKey)
	}
	if e.Input != 70 || e.CacheRead != 30 || e.CacheWrite != 5 || e.Output != 30 || e.Reasoning != 10 {
		t.Fatalf("tokens = %+v", e)
	}
	if !e.At.Equal(at) {
		t.Fatalf("at = %v", e.At)
	}
}

func TestMaskAPIKey(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"abcd":                    "a…",
		"abcdefghijklmn":          "abc…",
		"abcdefghijklmno":         "abcdefghij…lmno",
		"sk-1234567890abcdefghij": "sk-1234567…ghij",
	}
	for in, want := range cases {
		if got := MaskAPIKey(in); got != want {
			t.Errorf("MaskAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
}
