package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceClaudeSubscriptionRecoveryConfigReload(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	apply := func(enabled bool) {
		t.Helper()
		commit := service.commitConfigUpdate(&config.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset-first", SessionAffinity: true, SessionAffinityPreferClaudeSubscription: enabled}})
		if !service.applyConfigRuntime(context.Background(), commit, false) {
			t.Fatal("config apply failed")
		}
	}
	apply(false)
	before := manager.Selector()
	apply(true)
	after := manager.Selector()
	if after == before {
		t.Fatal("subscription preference change must recreate selector")
	}
	t.Cleanup(func() {
		if s, ok := manager.Selector().(*coreauth.SessionAffinitySelector); ok {
			s.Stop()
		}
	})
	api := &coreauth.Auth{ID: "api", Provider: "claude", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey, "priority": "0"}}
	sub := &coreauth.Auth{ID: "sub", Provider: "claude", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "priority": "10"}}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session"}}
	if _, err := after.Pick(t.Context(), "claude", "model", opts, []*coreauth.Auth{api}); err != nil {
		t.Fatal(err)
	}
	picked, err := after.Pick(t.Context(), "claude", "model", opts, []*coreauth.Auth{api, sub})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != sub.ID {
		t.Fatal("config did not enable subscription recovery")
	}
	apply(true)
	if manager.Selector() != after {
		t.Fatal("equivalent reload should retain selector")
	}
	for _, routing := range []internalconfig.RoutingConfig{
		{SessionAffinity: false, SessionAffinityPreferClaudeSubscription: true},
		{SessionAffinity: true, SessionAffinityRebindOn: "never", SessionAffinityPreferClaudeSubscription: true},
	} {
		if normalizedRoutingRuntimeState(&config.Config{Routing: routing}).sessionAffinityPreferClaudeSubscription {
			t.Fatal("inactive option changed runtime policy")
		}
	}
}
