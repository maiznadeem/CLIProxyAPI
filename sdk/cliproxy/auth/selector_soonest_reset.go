package auth

import (
	"context"
	"sort"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector prefers the available credential whose quota window
// resets soonest, so budget that is about to expire is spent first.
//
// Ordering rules:
//   - Availability filtering and priority tiers match FillFirstSelector: only the
//     highest available priority tier is considered.
//   - Within the tier, credentials with a known future reset instant (see
//     QuotaResetInstant) come first, soonest reset first.
//   - Credentials without reset information follow, ordered by priority
//     descending then ID ascending (the fill-first order).
//
// When session affinity wraps this selector, an established binding still
// outranks this ordering; this selector only decides cold and failover picks.
type SoonestResetSelector struct {
	// now overrides the clock in tests.
	now func() time.Time
}

// Pick selects the available credential with the soonest known quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return orderBySoonestReset(available, now)[0], nil
}

// orderBySoonestReset returns a copy of auths in soonest-reset order.
func orderBySoonestReset(auths []*Auth, now time.Time) []*Auth {
	type ranked struct {
		auth     *Auth
		resetAt  time.Time
		known    bool
		priority int
	}
	items := make([]ranked, 0, len(auths))
	for _, auth := range auths {
		resetAt, known := QuotaResetInstant(auth, now)
		items = append(items, ranked{auth: auth, resetAt: resetAt, known: known, priority: authPriority(auth)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.known != b.known {
			return a.known
		}
		if a.known && !a.resetAt.Equal(b.resetAt) {
			return a.resetAt.Before(b.resetAt)
		}
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		return a.auth.ID < b.auth.ID
	})
	out := make([]*Auth, len(items))
	for i := range items {
		out[i] = items[i].auth
	}
	return out
}
