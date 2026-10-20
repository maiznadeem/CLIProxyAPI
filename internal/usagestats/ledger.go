// Package usagestats keeps a persistent, bounded per-request usage ledger and
// aggregates it into statistics for the management API.
package usagestats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the ledger file stored inside the auth directory.
	FileName = "usage-stats.ledger"
	// DefaultRetention is how long entries are kept when no retention is configured.
	DefaultRetention = 30 * 24 * time.Hour
	// MaxEntries caps the in-memory ledger size.
	MaxEntries = 500_000
	// FlushInterval is the maximum delay between a change and its persistence.
	FlushInterval = 15 * time.Second

	pluginName    = "usagestats"
	fileVersion   = 1
	httpBadStatus = 400
)

// Entry is one compact request record. Input is the uncached input token count.
type Entry struct {
	At           time.Time `json:"at"`
	Provider     string    `json:"p,omitempty"`
	Model        string    `json:"m,omitempty"`
	AuthIndex    string    `json:"ai,omitempty"`
	Source       string    `json:"s,omitempty"`
	APIKey       string    `json:"k,omitempty"`
	SessionID    string    `json:"sid,omitempty"`
	Failed       bool      `json:"f,omitempty"`
	Input        int64     `json:"in,omitempty"`
	CacheRead    int64     `json:"cr,omitempty"`
	CacheWrite   int64     `json:"cw,omitempty"`
	Output       int64     `json:"out,omitempty"`
	Reasoning    int64     `json:"rs,omitempty"`
	Unclassified int64     `json:"un,omitempty"`
	LatencyMS    int64     `json:"ms,omitempty"`
}

func (e Entry) total() int64 {
	return e.Input + e.CacheRead + e.CacheWrite + e.Output + e.Reasoning + e.Unclassified
}

type fileFormat struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Ledger is a mutex-guarded in-memory entry list with optional file persistence.
// A nil *Ledger is valid and behaves as a disabled, empty ledger.
type Ledger struct {
	path       string
	retention  time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries []Entry
	dirty   bool

	flushMu   sync.Mutex // serialises file writes
	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// New creates a ledger persisted at path (empty path disables persistence).
// A retention <= 0 selects DefaultRetention.
func New(path string, retention time.Duration) *Ledger {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Ledger{
		path:       path,
		retention:  retention,
		maxEntries: MaxEntries,
		now:        time.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

var defaultLedger atomic.Pointer[Ledger]

// Default returns the ledger installed by Init or SetDefault; it may be nil.
func Default() *Ledger { return defaultLedger.Load() }

// SetDefault replaces the process-wide ledger used by management handlers.
func SetDefault(l *Ledger) { defaultLedger.Store(l) }

// Init loads the ledger from <authDir>/usage-stats.json (falling back to
// ~/.cli-proxy-api), registers it as a usage plugin and starts periodic flushing.
// retentionDays <= 0 disables the ledger and returns nil.
func Init(authDir string, retentionDays int) *Ledger {
	if retentionDays <= 0 {
		SetDefault(nil)
		return nil
	}
	l := New(ledgerPath(authDir), time.Duration(retentionDays)*24*time.Hour)
	if err := l.Load(); err != nil {
		log.WithError(err).Warn("usagestats: failed to load ledger; starting empty")
	}
	l.Start()
	SetDefault(l)
	coreusage.RegisterNamedPlugin(pluginName, l)
	return l
}

func ledgerPath(authDir string) string {
	dir := strings.TrimSpace(authDir)
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".cli-proxy-api")
		}
	}
	if dir == "" {
		return ""
	}
	// Keep the ledger out of the auth directory root: the credential watcher
	// loads every *.json under it (recursively) as an auth file, so use a non-JSON extension.
	path := filepath.Join(dir, "usage", FileName)
	for _, legacy := range []string{filepath.Join(dir, "usage-stats.json"), filepath.Join(dir, "usage", "usage-stats.json")} {
		if _, err := os.Stat(legacy); err == nil {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				_ = os.MkdirAll(filepath.Dir(path), 0o700)
				_ = os.Rename(legacy, path)
			}
		}
	}
	return path
}

// Path returns the persistence file path ("" when not persisted).
func (l *Ledger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Len returns the number of entries currently held.
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// HandleUsage implements coreusage.Plugin.
func (l *Ledger) HandleUsage(ctx context.Context, record coreusage.Record) {
	if l == nil {
		return
	}
	l.Add(entryFromRecord(ctx, record))
}

func entryFromRecord(ctx context.Context, record coreusage.Record) Entry {
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}

	meta := internallogging.GetClientRequestMetadata(ctx)
	sessionID := strings.TrimSpace(record.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(meta.SessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)

	failed := record.Failed
	if !failed {
		if status := internallogging.GetResponseStatus(ctx); status >= httpBadStatus {
			failed = true
		}
	}

	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	bd := detail.TokenBreakdown
	return Entry{
		At:           at,
		Provider:     provider,
		Model:        model,
		AuthIndex:    strings.TrimSpace(record.AuthIndex),
		Source:       strings.TrimSpace(record.Source),
		APIKey:       MaskAPIKey(record.APIKey),
		SessionID:    sessionID,
		Failed:       failed,
		Input:        nonNeg(bd.Input.UncachedTokens),
		CacheRead:    nonNeg(bd.Input.CacheReadTokens),
		CacheWrite:   nonNeg(bd.Input.CacheWriteTokens),
		Output:       nonNeg(bd.Output.NonReasoningTokens),
		Reasoning:    nonNeg(bd.Output.ReasoningTokens),
		Unclassified: nonNeg(bd.UnclassifiedTokens),
		LatencyMS:    record.Latency.Milliseconds(),
	}
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// MaskAPIKey keeps the first 10 and last 4 characters of a key. Keys too short
// for that to hide anything keep only a short prefix.
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 14 {
		return string(r[:len(r)/4]) + "…"
	}
	return string(r[:10]) + "…" + string(r[len(r)-4:])
}

// Add appends an entry, enforcing the entry cap.
func (l *Ledger) Add(e Entry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.entries = append(l.entries, e)
	if len(l.entries) > l.maxEntries {
		// Drop the oldest 10% beyond the cap so trimming is amortised.
		keep := l.maxEntries - l.maxEntries/10
		trimmed := make([]Entry, keep, l.maxEntries+1)
		copy(trimmed, l.entries[len(l.entries)-keep:])
		l.entries = trimmed
	}
	l.dirty = true
	l.mu.Unlock()
}

// Trim drops entries older than the retention window relative to now.
func (l *Ledger) Trim(now time.Time) {
	if l == nil {
		return
	}
	cutoff := now.Add(-l.retention)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	for _, e := range l.entries {
		if !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) != len(l.entries) {
		clear(l.entries[len(kept):])
		l.entries = kept
		l.dirty = true
	}
}

// Clear removes every entry and persists the empty ledger. It returns the
// number of removed entries.
func (l *Ledger) Clear() (int, error) {
	if l == nil {
		return 0, nil
	}
	l.mu.Lock()
	n := len(l.entries)
	l.entries = nil
	l.dirty = true
	l.mu.Unlock()
	return n, l.Flush()
}

// Load reads the persisted file, replacing in-memory entries. A missing file is
// not an error; a corrupt file is moved aside and reported.
func (l *Ledger) Load() error {
	if l == nil || l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", l.path, err)
	}
	var f fileFormat
	if err = json.Unmarshal(data, &f); err != nil {
		_ = os.Rename(l.path, l.path+".corrupt")
		return fmt.Errorf("parse %s (moved to .corrupt): %w", l.path, err)
	}
	l.mu.Lock()
	l.entries = f.Entries
	if len(l.entries) > l.maxEntries {
		l.entries = append([]Entry(nil), l.entries[len(l.entries)-l.maxEntries:]...)
	}
	l.dirty = false
	l.mu.Unlock()
	l.Trim(l.now())
	return nil
}

// Flush writes the ledger atomically when it has unsaved changes.
func (l *Ledger) Flush() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	snapshot := append([]Entry(nil), l.entries...)
	l.dirty = false
	l.mu.Unlock()

	if err := writeFileAtomic(l.path, fileFormat{Version: fileVersion, Entries: snapshot}); err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
		return err
	}
	return nil
}

func writeFileAtomic(path string, payload fileFormat) error {
	if payload.Entries == nil {
		payload.Entries = []Entry{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode usage stats: %w", err)
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Start launches the periodic trim+flush loop. Safe to call more than once.
func (l *Ledger) Start() {
	if l == nil {
		return
	}
	l.startOnce.Do(func() {
		go l.loop()
	})
}

func (l *Ledger) loop() {
	defer close(l.done)
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.Trim(l.now())
			if err := l.Flush(); err != nil {
				log.WithError(err).Warn("usagestats: flush failed")
			}
		case <-l.stop:
			return
		}
	}
}

// Stop ends the flush loop and writes any unsaved changes.
func (l *Ledger) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() {
		close(l.stop)
		started := true
		l.startOnce.Do(func() { started = false })
		if started {
			<-l.done
		}
	})
	if err := l.Flush(); err != nil {
		log.WithError(err).Warn("usagestats: final flush failed")
	}
}
