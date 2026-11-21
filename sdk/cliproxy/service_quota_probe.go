package cliproxy

import (
	"context"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota/probe"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// quotaProbeLoop tracks the background quota probe used by the soonest-reset strategy.
type quotaProbeLoop struct {
	cancel   context.CancelFunc
	interval time.Duration
}

// quotaProbeIntervalFor returns the probe interval for cfg, or 0 when probing is off.
func quotaProbeIntervalFor(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Home.Enabled || normalizedRoutingRuntimeState(cfg).strategy != "soonest-reset" {
		return 0
	}
	return cfg.Routing.SoonestResetProbeIntervalDuration()
}

// syncQuotaProbe starts, restarts, or stops the quota probe loop to match cfg.
// It is a no-op until Run has provided a base context.
func (s *Service) syncQuotaProbe(cfg *config.Config) {
	if s == nil {
		return
	}
	interval := quotaProbeIntervalFor(cfg)
	s.quotaProbeMu.Lock()
	defer s.quotaProbeMu.Unlock()
	if s.quotaProbeBase == nil || s.coreManager == nil {
		return
	}
	if s.quotaProbe != nil && s.quotaProbe.interval == interval {
		return
	}
	if s.quotaProbe != nil {
		s.quotaProbe.cancel()
		s.quotaProbe = nil
	}
	if interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.quotaProbeBase)
	s.quotaProbe = &quotaProbeLoop{cancel: cancel, interval: interval}
	prober := &probe.Prober{ClientFor: s.quotaProbeClient}
	manager := s.coreManager
	log.Infof("soonest-reset quota probe started (interval=%s)", interval)
	go runQuotaProbeLoop(ctx, prober, manager, interval)
}

// stopQuotaProbe stops the probe loop and prevents further starts.
func (s *Service) stopQuotaProbe() {
	if s == nil {
		return
	}
	s.quotaProbeMu.Lock()
	defer s.quotaProbeMu.Unlock()
	if s.quotaProbe != nil {
		s.quotaProbe.cancel()
		s.quotaProbe = nil
	}
	s.quotaProbeBase = nil
}

func (s *Service) quotaProbeClient(ctx context.Context, auth *coreauth.Auth) *http.Client {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	// The prober bounds each request with its own context deadline.
	return helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
}

func runQuotaProbeLoop(ctx context.Context, prober *probe.Prober, manager *coreauth.Manager, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		summary := prober.ProbeAll(ctx, manager.List(), manager)
		if summary.Probed > 0 {
			log.WithFields(log.Fields{
				"probed":  summary.Probed,
				"updated": summary.Updated,
				"failed":  summary.Failed,
			}).Debug("soonest-reset quota probe pass finished")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
