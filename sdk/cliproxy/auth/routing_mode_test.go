package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func routingAuth(id, mode string, priority string) *Auth {
	attrs := map[string]string{}
	if mode != "" {
		attrs[AttributeRoutingMode] = mode
	}
	if priority != "" {
		attrs["priority"] = priority
	}
	return &Auth{ID: id, Provider: "test", Status: StatusActive, Attributes: attrs}
}

func TestRoutingModeNormalization(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":          RoutingModeNormal,
		"normal":    RoutingModeNormal,
		" Preserve": RoutingModePreserve,
		"FOCUS ":    RoutingModeFocus,
		"bogus":     RoutingModeNormal,
	} {
		if got := NormalizeRoutingMode(raw); got != want {
			t.Fatalf("NormalizeRoutingMode(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := RoutingMode(nil); got != RoutingModeNormal {
		t.Fatalf("RoutingMode(nil) = %q", got)
	}
	if got := RoutingMode(routingAuth("a", "Focus", "")); got != RoutingModeFocus {
		t.Fatalf("RoutingMode(focus) = %q", got)
	}
	if IsValidRoutingMode("bogus") || !IsValidRoutingMode(" Normal ") {
		t.Fatal("IsValidRoutingMode mismatch")
	}
}

func TestApplyAuthRoutingModeMetadata(t *testing.T) {
	t.Parallel()
	auth := &Auth{ID: "a", Attributes: map[string]string{AttributeRoutingMode: "focus"}}
	if ApplyAuthRoutingModeMetadata(auth, map[string]any{"routing_mode": "bogus"}) {
		t.Fatal("bogus mode applied")
	}
	if _, ok := auth.Attributes[AttributeRoutingMode]; ok {
		t.Fatal("stale routing_mode attribute not cleared")
	}
	if !ApplyAuthRoutingModeMetadata(auth, map[string]any{"routing_mode": " Preserve"}) {
		t.Fatal("preserve not applied")
	}
	if auth.Attributes[AttributeRoutingMode] != RoutingModePreserve || auth.Metadata[AttributeRoutingMode] != " Preserve" {
		t.Fatalf("attributes=%v metadata=%v", auth.Attributes, auth.Metadata)
	}
}

func TestSelectorsSkipPreservedCredentials(t *testing.T) {
	t.Parallel()
	auths := []*Auth{routingAuth("a", RoutingModePreserve, ""), routingAuth("b", "", ""), routingAuth("c", "", "")}
	selectors := map[string]Selector{
		"round-robin": &RoundRobinSelector{},
		"fill-first":  &FillFirstSelector{},
		"soonest":     &SoonestResetSelector{},
	}
	for name, selector := range selectors {
		for i := 0; i < 4; i++ {
			got, err := selector.Pick(context.Background(), "test", "model", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("%s: Pick error: %v", name, err)
			}
			if got.ID == "a" {
				t.Fatalf("%s picked preserved credential", name)
			}
		}
	}
}

func TestSelectorAllPreservedHasNoCandidates(t *testing.T) {
	t.Parallel()
	auths := []*Auth{routingAuth("a", RoutingModePreserve, ""), routingAuth("b", RoutingModePreserve, "")}
	if got, err := (&RoundRobinSelector{}).Pick(context.Background(), "test", "model", cliproxyexecutor.Options{}, auths); err == nil {
		t.Fatalf("Pick() = %v, want error", got)
	}
}

func TestSelectorsRestrictToFocusedCredentials(t *testing.T) {
	t.Parallel()
	auths := []*Auth{routingAuth("a", "", ""), routingAuth("b", RoutingModeFocus, ""), routingAuth("c", "", "")}
	for name, selector := range map[string]Selector{
		"round-robin": &RoundRobinSelector{},
		"fill-first":  &FillFirstSelector{},
	} {
		for i := 0; i < 4; i++ {
			got, err := selector.Pick(context.Background(), "test", "model", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("%s: Pick error: %v", name, err)
			}
			if got.ID != "b" {
				t.Fatalf("%s picked %q, want focused b", name, got.ID)
			}
		}
	}
}

func TestFocusFallsBackWhenNoFocusedCredentialAvailable(t *testing.T) {
	t.Parallel()
	focused := routingAuth("b", RoutingModeFocus, "")
	focused.Unavailable = true
	focused.NextRetryAfter = time.Now().Add(time.Hour)
	auths := []*Auth{routingAuth("a", "", ""), focused}
	got, err := (&FillFirstSelector{}).Pick(context.Background(), "test", "model", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if got.ID != "a" {
		t.Fatalf("picked %q, want fallback a", got.ID)
	}
}

func TestFocusKeepsPriorityTiersInsideFocusedSet(t *testing.T) {
	t.Parallel()
	auths := []*Auth{
		routingAuth("hi-normal", "", "10"),
		routingAuth("focus-lo", RoutingModeFocus, "0"),
		routingAuth("focus-hi", RoutingModeFocus, "5"),
	}
	for i := 0; i < 3; i++ {
		got, err := (&RoundRobinSelector{}).Pick(context.Background(), "test", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick error: %v", err)
		}
		if got.ID != "focus-hi" {
			t.Fatalf("picked %q, want focus-hi", got.ID)
		}
	}
}

func newRoutingAffinity(t *testing.T) *SessionAffinitySelector {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	t.Cleanup(selector.Stop)
	return selector
}

func routingAffinityOpts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "routing-session"}}
}

func pickAffinityID(t *testing.T, selector *SessionAffinitySelector, auths []*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "test", "model", routingAffinityOpts(), auths)
	if err != nil || got == nil {
		t.Fatalf("Pick() = %v, %v", got, err)
	}
	return got.ID
}

func TestSessionAffinityFocusOverridesBinding(t *testing.T) {
	selector := newRoutingAffinity(t)
	a := routingAuth("a", "", "")
	b := routingAuth("b", "", "")
	auths := []*Auth{a, b}
	if got := pickAffinityID(t, selector, auths); got != "a" {
		t.Fatalf("cold binding = %q, want a", got)
	}
	b.Attributes[AttributeRoutingMode] = RoutingModeFocus
	if got := pickAffinityID(t, selector, auths); got != "b" {
		t.Fatalf("after focus = %q, want b", got)
	}
	// The session rebinds to the focused credential and stays there once focus is cleared.
	delete(b.Attributes, AttributeRoutingMode)
	if got := pickAffinityID(t, selector, auths); got != "b" {
		t.Fatalf("after focus cleared = %q, want sticky b", got)
	}
}

func TestSessionAffinityFocusFallbackKeepsBinding(t *testing.T) {
	selector := newRoutingAffinity(t)
	a := routingAuth("a", "", "")
	b := routingAuth("b", RoutingModeFocus, "")
	b.Unavailable = true
	b.NextRetryAfter = time.Now().Add(time.Hour)
	auths := []*Auth{a, b}
	if got := pickAffinityID(t, selector, auths); got != "a" {
		t.Fatalf("with focused credential unavailable = %q, want a", got)
	}
}

func TestSessionAffinityPreserveDropsBinding(t *testing.T) {
	selector := newRoutingAffinity(t)
	a := routingAuth("a", "", "")
	b := routingAuth("b", "", "")
	auths := []*Auth{a, b}
	if got := pickAffinityID(t, selector, auths); got != "a" {
		t.Fatalf("cold binding = %q, want a", got)
	}
	a.Attributes[AttributeRoutingMode] = RoutingModePreserve
	if got := pickAffinityID(t, selector, auths); got != "b" {
		t.Fatalf("after preserve = %q, want b", got)
	}
	delete(a.Attributes, AttributeRoutingMode)
	if got := pickAffinityID(t, selector, auths); got != "b" {
		t.Fatalf("binding should stay on b after preserve cleared, got %q", got)
	}
}

func registerRoutingManagerAuths(t *testing.T, manager *Manager, provider, model string, auths ...*Auth) {
	t.Helper()
	ctx := context.Background()
	for _, auth := range auths {
		auth.Provider = provider
		if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
}

func TestManagerSchedulerPathHonorsRoutingModes(t *testing.T) {
	ctx := context.Background()
	provider := "routing-mode-scheduler"
	model := "routing-mode-model"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	registerRoutingManagerAuths(t, manager, provider, model,
		routingAuth("rm-a", RoutingModePreserve, ""),
		routingAuth("rm-b", "", ""),
		routingAuth("rm-c", "", ""),
	)

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		auth, _, err := manager.pickNext(ctx, provider, model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext: %v", err)
		}
		seen[auth.ID] = true
	}
	if seen["rm-a"] || !seen["rm-b"] || !seen["rm-c"] {
		t.Fatalf("preserve rotation = %v", seen)
	}
	mixedSeen := map[string]bool{}
	for i := 0; i < 6; i++ {
		auth, _, _, err := manager.pickNextMixed(ctx, []string{provider}, model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNextMixed: %v", err)
		}
		mixedSeen[auth.ID] = true
	}
	if mixedSeen["rm-a"] {
		t.Fatalf("mixed picked preserved credential: %v", mixedSeen)
	}

	// Switch rm-c to focus: all traffic goes there.
	focused := routingAuth("rm-c", RoutingModeFocus, "")
	focused.Provider = provider
	if _, err := manager.Update(WithSkipPersist(ctx), focused); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for i := 0; i < 4; i++ {
		auth, _, err := manager.pickNext(ctx, provider, model, cliproxyexecutor.Options{}, nil)
		if err != nil || auth.ID != "rm-c" {
			t.Fatalf("focus pickNext = %v, %v; want rm-c", auth, err)
		}
		mixed, _, _, err := manager.pickNextMixed(ctx, []string{provider}, model, cliproxyexecutor.Options{}, nil)
		if err != nil || mixed.ID != "rm-c" {
			t.Fatalf("focus pickNextMixed = %v, %v; want rm-c", mixed, err)
		}
	}

	// Once the focused credential has been tried (unavailable for this request) the normal set serves.
	auth, _, err := manager.pickNext(ctx, provider, model, cliproxyexecutor.Options{}, map[string]struct{}{"rm-c": {}})
	if err != nil || auth.ID != "rm-b" {
		t.Fatalf("focus fallback pickNext = %v, %v; want rm-b", auth, err)
	}
}

func TestManagerAffinityPathHonorsRoutingModes(t *testing.T) {
	ctx := context.Background()
	provider := "routing-mode-affinity"
	model := "routing-mode-affinity-model"
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager.SetSelector(affinity)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	registerRoutingManagerAuths(t, manager, provider, model,
		routingAuth("ra-a", "", ""),
		routingAuth("ra-b", "", ""),
	)
	pick := func() string {
		t.Helper()
		auth, _, err := manager.pickNext(ctx, provider, model, routingAffinityOpts(), nil)
		if err != nil {
			t.Fatalf("pickNext: %v", err)
		}
		return auth.ID
	}
	if got := pick(); got != "ra-a" {
		t.Fatalf("cold binding = %q, want ra-a", got)
	}
	update := func(id, mode string) {
		t.Helper()
		next := routingAuth(id, mode, "")
		next.Provider = provider
		if _, err := manager.Update(WithSkipPersist(ctx), next); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	update("ra-b", RoutingModeFocus)
	if got := pick(); got != "ra-b" {
		t.Fatalf("focus should override affinity, got %q", got)
	}
	update("ra-b", RoutingModePreserve)
	if got := pick(); got != "ra-a" {
		t.Fatalf("preserve should drop binding, got %q", got)
	}
}
