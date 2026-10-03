package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.WeightedRoundRobinSelector", newRoutingSelector(state))
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, input := range []string{"soonest-reset", "sr", "SoonestReset"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy(%q) = %q, want soonest-reset", input, state.strategy)
		}
		if selector, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", selector)
		}
	}
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "sr", SessionAffinity: true},
	})
	selector := newRoutingSelector(state)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", selector)
	}
	affinity.Stop()
}

func TestQuotaProbeIntervalFor(t *testing.T) {
	cases := []struct {
		name string
		cfg  *internalconfig.Config
		want time.Duration
	}{
		{"nil", nil, 0},
		{"other strategy", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "fill-first"}}, 0},
		{"default", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset"}}, 15 * time.Minute},
		{"custom", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "sr", SoonestResetProbeInterval: "5m"}}, 5 * time.Minute},
		{"disabled", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "sr", SoonestResetProbeInterval: "0"}}, 0},
		{"disabled 0s", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "sr", SoonestResetProbeInterval: "0s"}}, 0},
		{"clamped", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "sr", SoonestResetProbeInterval: "5s"}}, time.Minute},
		{"invalid", &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "sr", SoonestResetProbeInterval: "soon"}}, 15 * time.Minute},
	}
	for _, tc := range cases {
		if got := quotaProbeIntervalFor(tc.cfg); got != tc.want {
			t.Fatalf("%s: quotaProbeIntervalFor = %s, want %s", tc.name, got, tc.want)
		}
	}
}
