package auth

import (
	"context"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"

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
// Exhausted credentials (QuotaExhausted) rank after every non-exhausted one,
// again by soonest reset. When SetSpendUsageCredits(false) is in effect,
// exhausted credentials with usage credits enabled are not picked at all.
//
// With default session rebinding, Claude sessions follow the highest-ranked
// known quota window and retain affinity within that tier.
type SoonestResetSelector struct {
	// now overrides the clock in tests.
	now func() time.Time
}

// Pick selects the available credential with the soonest known quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.selectionTime()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if !SpendUsageCredits() {
		allowed := make([]*Auth, 0, len(available))
		for _, auth := range available {
			if !billsUsageCredits(auth, now) {
				allowed = append(allowed, auth)
			}
		}
		if len(allowed) == 0 {
			return nil, newAuthUnavailableError(time.Time{}, now)
		}
		available = allowed
	}
	picked := orderBySoonestReset(available, now)[0]
	if billsUsageCredits(picked, now) {
		log.Debugf("routing to %s on usage credits", picked.ID)
	}
	return picked, nil
}

func (s *SoonestResetSelector) selectionTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// preferredClaudeAffinityCandidates applies reset ordering before a Claude
// session-cache lookup. A later-reset credential remains available for retry
// when the conductor removes the failed credential from the candidate list.
func (s *SoonestResetSelector) preferredClaudeAffinityCandidates(ctx context.Context, provider, model string, auths []*Auth) []*Auth {
	if len(auths) < 2 || (provider != "claude" && provider != "mixed") {
		return nil
	}
	for _, auth := range auths {
		if auth == nil || auth.Provider != "claude" {
			return nil
		}
	}
	now := s.selectionTime()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil || len(available) == 0 {
		return nil
	}
	ordered := orderBySoonestReset(available, now)
	resetAt, known := QuotaResetInstant(ordered[0], now)
	if !known || quotaExhaustedAt(ordered[0], now) {
		return nil
	}
	for i, auth := range ordered {
		if reset, ok := QuotaResetInstant(auth, now); !ok || !reset.Equal(resetAt) || quotaExhaustedAt(auth, now) {
			return ordered[:i]
		}
	}
	return ordered
}

// orderBySoonestReset returns a copy of auths in soonest-reset order.
func orderBySoonestReset(auths []*Auth, now time.Time) []*Auth {
	type ranked struct {
		auth      *Auth
		resetAt   time.Time
		known     bool
		priority  int
		exhausted bool
	}
	items := make([]ranked, 0, len(auths))
	for _, auth := range auths {
		resetAt, known := QuotaResetInstant(auth, now)
		items = append(items, ranked{auth: auth, resetAt: resetAt, known: known, priority: authPriority(auth), exhausted: quotaExhaustedAt(auth, now)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.exhausted != b.exhausted {
			return !a.exhausted
		}
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
