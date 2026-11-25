package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceFundingOrderConfigReload(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	apply := func(order ...string) {
		t.Helper()
		commit := service.commitConfigUpdate(&config.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset-first", SessionAffinity: true, FundingOrder: order}})
		if !service.applyConfigRuntime(context.Background(), commit, false) {
			t.Fatal("config apply failed")
		}
	}
	apply()
	before := manager.Selector()
	apply("allowance", "api-credit", "usage-credit")
	after := manager.Selector()
	if after == before {
		t.Fatal("a funding order change must recreate the selector")
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
		t.Fatal("the configured order did not return the session to the subscription")
	}
	apply("allowance", "api-credit", "usage-credit")
	if manager.Selector() != after {
		t.Fatal("an equivalent reload should retain the selector")
	}
	apply("usage-credit", "api-credit", "allowance")
	if manager.Selector() == after {
		t.Fatal("reordering the classes must recreate the selector")
	}
}

func TestFundingOrderRuntimeState(t *testing.T) {
	state := func(routing internalconfig.RoutingConfig) string {
		return normalizedRoutingRuntimeState(&config.Config{Routing: routing}).fundingOrder
	}
	order := []string{" Allowance ", "API-CREDIT"}
	if got := state(internalconfig.RoutingConfig{SessionAffinity: true, FundingOrder: order}); got != "allowance,api-credit" {
		t.Fatalf("normalized order = %q", got)
	}
	if got := state(internalconfig.RoutingConfig{SessionAffinity: true, SessionAffinityRebindOn: "never", FundingOrder: order}); got != "allowance,api-credit" {
		t.Fatalf("rebind never must keep the order for new sessions, got %q", got)
	}
	for name, routing := range map[string]internalconfig.RoutingConfig{
		"affinity off":  {FundingOrder: order},
		"unknown class": {SessionAffinity: true, FundingOrder: []string{"allowance", "credits"}},
		"repeated":      {SessionAffinity: true, FundingOrder: []string{"allowance", "allowance"}},
	} {
		if got := state(routing); got != "" {
			t.Fatalf("%s: order should be disabled, got %q", name, got)
		}
	}
	if fundingOrderFromState("") != nil {
		t.Fatal("an empty state must disable ranking")
	}
	if got := fundingOrderFromState("usage-credit,allowance").String(); got != "usage-credit,allowance" {
		t.Fatalf("rebuilt order = %q", got)
	}
}
