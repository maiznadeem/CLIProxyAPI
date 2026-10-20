package usagestats

import (
	"sort"
	"time"
)

const maxSessionBuckets = 50

// Counters holds summed request and token counts.
type Counters struct {
	Requests   int64 `json:"requests"`
	Failed     int64 `json:"failed"`
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

func (c *Counters) add(e Entry) {
	c.Requests++
	if e.Failed {
		c.Failed++
	}
	c.Input += e.Input
	c.CacheRead += e.CacheRead
	c.CacheWrite += e.CacheWrite
	c.Output += e.Output
	c.Reasoning += e.Reasoning
	c.Total += e.total()
}

// Bucket is one aggregation group. Counters are serialised inline.
type Bucket struct {
	Key         string    `json:"key"`
	Provider    string    `json:"provider"`
	Source      string    `json:"source"`
	Models      []string  `json:"models,omitempty"`
	Credentials []string  `json:"credentials,omitempty"`
	First       time.Time `json:"first"`
	Last        time.Time `json:"last"`
	Counters
}

// Stats is the aggregated view of the ledger for a time window.
type Stats struct {
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	GeneratedAt  time.Time `json:"generated_at"`
	Totals       Counters  `json:"totals"`
	ByModel      []Bucket  `json:"by_model"`
	ByCredential []Bucket  `json:"by_credential"`
	ByAPIKey     []Bucket  `json:"by_api_key"`
	BySession    []Bucket  `json:"by_session"`
	ByDay        []Bucket  `json:"by_day"`
}

type group struct {
	b           Bucket
	models      map[string]struct{}
	credentials map[string]struct{}
}

type grouper struct {
	order  []string
	groups map[string]*group
}

func newGrouper() *grouper { return &grouper{groups: map[string]*group{}} }

func (g *grouper) add(key string, e Entry, withSets bool) *group {
	grp, ok := g.groups[key]
	if !ok {
		grp = &group{b: Bucket{Key: key, First: e.At, Last: e.At}}
		if withSets {
			grp.models = map[string]struct{}{}
			grp.credentials = map[string]struct{}{}
		}
		g.groups[key] = grp
		g.order = append(g.order, key)
	}
	grp.b.Counters.add(e)
	if e.At.Before(grp.b.First) {
		grp.b.First = e.At
	}
	if !e.At.Before(grp.b.Last) {
		// The latest entry wins for descriptive fields.
		grp.b.Last = e.At
		if e.Provider != "" {
			grp.b.Provider = e.Provider
		}
		if e.Source != "" {
			grp.b.Source = e.Source
		}
	} else {
		if grp.b.Provider == "" {
			grp.b.Provider = e.Provider
		}
		if grp.b.Source == "" {
			grp.b.Source = e.Source
		}
	}
	return grp
}

func (g *grouper) buckets() []Bucket {
	out := make([]Bucket, 0, len(g.groups))
	for _, key := range g.order {
		grp := g.groups[key]
		if grp.models != nil {
			grp.b.Models = sortedKeys(grp.models)
			grp.b.Credentials = sortedKeys(grp.credentials)
		}
		out = append(out, grp.b)
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortByTotal(b []Bucket) {
	sort.SliceStable(b, func(i, j int) bool {
		if b[i].Total != b[j].Total {
			return b[i].Total > b[j].Total
		}
		return b[i].Key < b[j].Key
	})
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Query aggregates entries with since <= At <= until. A zero until means no
// upper bound; Stats.Until then reports the generation time.
func (l *Ledger) Query(since, until time.Time) Stats {
	now := time.Now()
	if l != nil && l.now != nil {
		now = l.now()
	}
	stats := Stats{
		Since:        since,
		Until:        until,
		GeneratedAt:  now,
		ByModel:      []Bucket{},
		ByCredential: []Bucket{},
		ByAPIKey:     []Bucket{},
		BySession:    []Bucket{},
		ByDay:        []Bucket{},
	}
	if until.IsZero() {
		stats.Until = now
	}
	if l == nil {
		return stats
	}

	l.mu.Lock()
	selected := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.At.Before(since) || (!until.IsZero() && e.At.After(until)) {
			continue
		}
		selected = append(selected, e)
	}
	l.mu.Unlock()

	models, creds, keys, sessions, days := newGrouper(), newGrouper(), newGrouper(), newGrouper(), newGrouper()
	for _, e := range selected {
		stats.Totals.add(e)
		models.add(orUnknown(e.Model), e, false)
		creds.add(orUnknown(e.AuthIndex), e, false)
		keys.add(orUnknown(e.APIKey), e, false)
		if e.SessionID != "" {
			grp := sessions.add(e.SessionID, e, true)
			grp.models[orUnknown(e.Model)] = struct{}{}
			cred := e.Source
			if cred == "" {
				cred = orUnknown(e.AuthIndex)
			}
			grp.credentials[cred] = struct{}{}
		}
		days.add(e.At.Local().Format("2006-01-02"), e, false)
	}

	stats.ByModel = models.buckets()
	stats.ByCredential = creds.buckets()
	stats.ByAPIKey = keys.buckets()
	stats.BySession = sessions.buckets()
	stats.ByDay = days.buckets()
	for _, b := range [][]Bucket{stats.ByModel, stats.ByCredential, stats.ByAPIKey, stats.BySession} {
		sortByTotal(b)
	}
	if len(stats.BySession) > maxSessionBuckets {
		stats.BySession = stats.BySession[:maxSessionBuckets]
	}
	sort.Slice(stats.ByDay, func(i, j int) bool { return stats.ByDay[i].Key < stats.ByDay[j].Key })
	return stats
}
