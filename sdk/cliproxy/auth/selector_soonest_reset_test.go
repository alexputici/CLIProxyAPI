package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func soonestResetAuth(id, provider string, headers http.Header, observed time.Time) *Auth {
	a := &Auth{ID: id, Provider: provider, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}
	a.Quota.ObserveResponseHeadersForProvider(provider, headers, observed)
	return a
}

func claudeResetAuth(id string, reset time.Time) *Auth {
	return soonestResetAuth(id, "claude", http.Header{
		"anthropic-ratelimit-unified-7d-reset": {strconv.FormatInt(reset.Unix(), 10)},
	}, time.Now())
}

func TestSoonestResetWeeklyHeaders(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	upcoming := now.Add(24 * time.Hour)
	for _, tc := range []struct {
		name, provider string
		headers        http.Header
		observed       time.Time
		want           time.Time
	}{
		{"claude unix", "claude", http.Header{"anthropic-ratelimit-unified-7d-reset": {strconv.FormatInt(upcoming.Unix(), 10)}}, now, upcoming},
		{"claude RFC3339", "claude", http.Header{"anthropic-ratelimit-unified-7d-reset": {upcoming.Format(time.RFC3339)}}, now, upcoming},
		{"claude passed", "claude", http.Header{"anthropic-ratelimit-unified-7d-reset": {strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)}}, now, time.Time{}},
		{"claude malformed", "claude", http.Header{"anthropic-ratelimit-unified-7d-reset": {"tomorrow"}}, now, time.Time{}},
		{"claude short window", "claude", http.Header{"anthropic-ratelimit-unified-5h-reset": {strconv.FormatInt(upcoming.Unix(), 10)}}, now, time.Time{}},
		{"codex secondary relative", "codex", http.Header{"x-codex-primary-window-minutes": {"300"}, "x-codex-primary-reset-after-seconds": {"60"}, "x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-after-seconds": {"86460"}}, now.Add(-time.Minute), upcoming},
		{"codex primary weekly", "codex", http.Header{"x-codex-primary-window-minutes": {"10080"}, "x-codex-primary-reset-at": {strconv.FormatInt(upcoming.Unix(), 10)}, "x-codex-secondary-window-minutes": {"300"}, "x-codex-secondary-reset-after-seconds": {"60"}}, now, upcoming},
		{"codex unclassified", "codex", http.Header{"x-codex-secondary-reset-after-seconds": {"86400"}}, now, time.Time{}},
		{"codex malformed window", "codex", http.Header{"x-codex-secondary-window-minutes": {"seven days"}, "x-codex-secondary-reset-after-seconds": {"86400"}}, now, time.Time{}},
		{"codex passed", "codex", http.Header{"x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-after-seconds": {"60"}}, now.Add(-time.Hour), time.Time{}},
		{"codex overflow", "codex", http.Header{"x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-after-seconds": {"9223372036854775807"}}, now, time.Time{}},
		{"codex missing observation", "codex", http.Header{"x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-after-seconds": {"86400"}}, time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := soonestResetAuth("test", tc.provider, tc.headers, now)
			a.Quota.ObservedAt = tc.observed
			if got := upcomingWeeklyReset(a, now); !got.Equal(tc.want) {
				t.Fatalf("reset = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSoonestResetPickEligibility(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		prepare func(a, b *Auth)
		model   string
		want    string
	}{
		{"earliest", func(a, b *Auth) {}, "requested", "b"},
		{"priority", func(a, b *Auth) { a.Attributes["priority"] = "10" }, "requested", "a"},
		{"cooldown", func(a, b *Auth) {
			b.ModelStates = map[string]*ModelState{"requested": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
		}, "requested", "a"},
		{"unrelated model cooldown", func(a, b *Auth) {
			b.ModelStates = map[string]*ModelState{"other": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
		}, "requested", "b"},
		{"disabled", func(a, b *Auth) { b.Disabled = true }, "requested", "a"},
		{"passed", func(a, b *Auth) { b.Quota = claudeResetAuth("b", now.Add(-time.Hour)).Quota }, "requested", "a"},
		{"API priority", func(a, b *Auth) { a.Attributes[AttributeAuthKind] = AuthKindAPIKey; a.Attributes["priority"] = "10" }, "requested", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := claudeResetAuth("a", now.Add(72*time.Hour)), claudeResetAuth("b", now.Add(24*time.Hour))
			tc.prepare(a, b)
			got, errPick := (&SoonestResetFirstSelector{}).Pick(t.Context(), "claude", tc.model, cliproxyexecutor.Options{}, []*Auth{a, b})
			if errPick != nil || got == nil || got.ID != tc.want {
				t.Fatalf("pick = %v, error = %v, want %s", got, errPick, tc.want)
			}
		})
	}
}

func TestSoonestResetUnknownExploration(t *testing.T) {
	known := claudeResetAuth("known", time.Now().Add(time.Hour))
	unknownA := soonestResetAuth("a", "claude", nil, time.Now())
	unknownB := soonestResetAuth("b", "claude", nil, time.Now())
	selector := &SoonestResetFirstSelector{}
	counts := map[string]int{}
	for range 16 {
		got, errPick := selector.Pick(t.Context(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{known, unknownB, unknownA})
		if errPick != nil {
			t.Fatal(errPick)
		}
		counts[got.ID]++
	}
	if counts["a"] != 1 || counts["b"] != 1 || counts["known"] != 14 {
		t.Fatalf("picks = %v, want one probe per unknown and 14 known", counts)
	}
	unknownA.Quota = claudeResetAuth("a", time.Now().Add(30*time.Minute)).Quota
	got, errPick := selector.Pick(t.Context(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{known, unknownA, unknownB})
	if errPick != nil || got.ID != "a" {
		t.Fatalf("learned reset pick = %v, error = %v", got, errPick)
	}
}

func TestSoonestResetUnknownsAndTiesRotate(t *testing.T) {
	for _, known := range []bool{false, true} {
		a, b := soonestResetAuth("a", "claude", nil, time.Now()), soonestResetAuth("b", "claude", nil, time.Now())
		if known {
			a.Quota = claudeResetAuth("a", time.Now().Add(time.Hour)).Quota
			b.Quota = a.Quota.Clone()
		}
		selector := &SoonestResetFirstSelector{}
		for _, want := range []string{"a", "b", "a", "b"} {
			got, errPick := selector.Pick(t.Context(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{b, a})
			if errPick != nil || got.ID != want {
				t.Fatalf("known=%t: pick=%v error=%v want=%s", known, got, errPick, want)
			}
		}
	}
}

func TestSoonestResetNeverUsesTokenExpiryOrAPIQuota(t *testing.T) {
	now := time.Now()
	a := claudeResetAuth("a", now.Add(time.Hour))
	a.Attributes[AttributeAuthKind] = AuthKindAPIKey
	if got := upcomingWeeklyReset(a, now); !got.IsZero() {
		t.Fatalf("API reset = %v", got)
	}
	a.Attributes[AttributeAuthKind] = AuthKindOAuth
	a.Quota = QuotaState{}
	a.Metadata = map[string]any{"expired": now.Add(time.Hour).Format(time.RFC3339), "expires_at": now.Add(time.Hour).Unix()}
	if got := upcomingWeeklyReset(a, now); !got.IsZero() {
		t.Fatalf("token expiry became weekly reset: %v", got)
	}
}

func TestSoonestResetWebsocketAndCooldownError(t *testing.T) {
	now := time.Now()
	a, b := claudeResetAuth("a", now.Add(time.Hour)), claudeResetAuth("b", now.Add(24*time.Hour))
	a.Provider, b.Provider = "codex", "codex"
	b.Attributes["websockets"] = "true"
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, errPick := (&SoonestResetFirstSelector{}).Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got.ID != "b" {
		t.Fatalf("websocket pick=%v error=%v", got, errPick)
	}
	for _, auth := range []*Auth{a, b} {
		auth.ModelStates = map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
	}
	_, errPick = (&SoonestResetFirstSelector{}).Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	var cooldown *modelCooldownError
	if !errors.As(errPick, &cooldown) {
		t.Fatalf("error = %v, want cooldown", errPick)
	}
}

func TestSoonestResetManagerPreservesResolvedModel(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		t.Run(strconv.FormatBool(affinity), func(t *testing.T) {
			var selector Selector = &SoonestResetFirstSelector{}
			if affinity {
				sticky := NewSessionAffinitySelector(selector)
				t.Cleanup(sticky.Stop)
				selector = sticky
			}
			manager := NewManager(nil, selector, nil)
			executor := &selectedCapabilityCaptureExecutor{provider: "claude"}
			manager.RegisterExecutor(executor)
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet", Alias: "public-model", Fork: true}},
			})
			now := time.Now()
			a, b := claudeResetAuth("soonest-model-a", now.Add(72*time.Hour)), claudeResetAuth("soonest-model-b", now.Add(time.Hour))
			// The alias key is not the credential's upstream model. Rechecking it
			// inside the selector would incorrectly reject the earliest-reset auth.
			b.ModelStates = map[string]*ModelState{"public-model": {
				Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true},
			}}
			for _, auth := range []*Auth{a, b} {
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "public-model"}, {ID: "claude-sonnet"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			got, errPick := manager.SelectAuth(t.Context(), "claude", "public-model", cliproxyexecutor.Options{})
			if errPick != nil || got == nil || got.ID != b.ID {
				t.Fatalf("manager pick=%v error=%v, want %s", got, errPick, b.ID)
			}
			_, errExecute := manager.Execute(t.Context(), []string{"claude"}, cliproxyexecutor.Request{Model: "public-model"}, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatal(errExecute)
			}
			if len(executor.requests) != 1 || executor.requests[0].Model != "claude-sonnet" {
				t.Fatalf("upstream requests = %v", executor.requests)
			}
		})
	}
}
