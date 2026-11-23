package auth

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func subscriptionRecoveryAuth(id, kind, priority string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: kind, "priority": priority}}
}

func TestManagerSessionAffinityReturnsToClaudeSubscription(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, lcp := range []bool{false, true} {
			name := "single/explicit"
			if mixed {
				name = "mixed/explicit"
			}
			if lcp {
				name += "/lcp"
			}
			t.Run(name, func(t *testing.T) {
				const model = "claude-subscription-recovery"
				manager := NewManager(nil, nil, nil)
				affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetFirstSelector{}, PreferClaudeSubscription: true})
				defer affinity.Stop()
				manager.SetSelector(affinity)
				manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
				sub := subscriptionRecoveryAuth(t.Name()+"-sub", AuthKindOAuth, "10")
				api := subscriptionRecoveryAuth(t.Name()+"-api", AuthKindAPIKey, "0")
				for _, a := range []*Auth{sub, api} {
					if _, err := manager.Register(WithSkipPersist(t.Context()), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				}
				body := []byte(`{"model":"claude-subscription-recovery","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"keep my entire history"}]}`)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: bytes.Clone(body), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
				if !lcp {
					opts.Headers = http.Header{"X-Claude-Code-Session-Id": []string{"session"}}
				}
				pick := func(want string) {
					t.Helper()
					var got *Auth
					var err error
					if mixed {
						got, _, _, err = manager.pickNextMixed(t.Context(), []string{"claude"}, model, opts, nil)
					} else {
						got, _, err = manager.pickNext(t.Context(), "claude", model, opts, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					if got == nil || got.ID != want {
						t.Fatalf("picked %q, want %s", func() string {
							if got == nil {
								return "<nil>"
							}
							return got.ID
						}(), want)
					}
					if !bytes.Equal(body, opts.OriginalRequest) {
						t.Fatal("routing changed request history/model/effort")
					}
				}
				pick(sub.ID)
				manager.MarkResult(t.Context(), Result{AuthID: sub.ID, Provider: "claude", Model: model, Error: &Error{HTTPStatus: 429, Message: "quota"}})
				pick(api.ID)
				pick(api.ID)
				expireSessionAffinityPriorityModelCooldown(t, manager, sub.ID, model)
				pick(sub.ID)
				pick(sub.ID)
			})
		}
	}
}

func TestSessionAffinityClaudeSubscriptionRecoveryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		mode      string
		boundKind string
		provider  string
		priority  string
		body      string
		pinned    bool
		wantSub   bool
	}{
		{name: "opt in", enabled: true, wantSub: true},
		{name: "default unchanged"},
		{name: "never", enabled: true, mode: "never"},
		{name: "healthy OAuth stays", enabled: true, boundKind: AuthKindOAuth},
		{name: "other provider stays", enabled: true, provider: "codex"},
		{name: "lower priority subscription stays out", enabled: true, priority: "-1"},
		{name: "pin stays", enabled: true, pinned: true},
		{name: "file ownership stays", enabled: true, body: `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_owned"}}]}]}`},
		{name: "container ownership stays", enabled: true, body: `{"container":"container_owned","messages":[{"role":"user","content":"continue"}]}`},
		{name: "custom skill ownership stays", enabled: true, body: `{"container":{"skills":[{"type":"custom","skill_id":"skill_owned"}]},"messages":[{"role":"user","content":"continue"}]}`},
		{name: "built in skill permits recovery", enabled: true, wantSub: true, body: `{"container":{"skills":[{"type":"anthropic","skill_id":"xlsx"}]},"messages":[{"role":"user","content":"continue"}]}`},
		{name: "advisor ownership stays", enabled: true, body: `{"messages":[{"role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_owned","content":{"type":"advisor_redacted_result","encrypted_content":"ciphertext"}}]}]}`},
		{name: "ordinary tool data is not an upstream file", enabled: true, wantSub: true, body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_local","content":"{\"file_id\":\"local\"}"}]}]}`},
		{name: "thread ownership stays", enabled: true, body: `{"thread":{"previous_message_id":"msg_owned"},"messages":[{"role":"user","content":"continue"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, PreferClaudeSubscription: tc.enabled, RebindOn: tc.mode})
			defer selector.Stop()
			kind := tc.boundKind
			if kind == "" {
				kind = AuthKindAPIKey
			}
			priority := tc.priority
			if priority == "" {
				priority = "10"
			}
			bound := subscriptionRecoveryAuth("bound", kind, "0")
			sub := subscriptionRecoveryAuth("subscription", AuthKindOAuth, priority)
			provider := tc.provider
			if provider == "" {
				provider = "claude"
			}
			bound.Provider = provider
			sub.Provider = provider
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session"}}, OriginalRequest: []byte(tc.body), Metadata: map[string]any{}}
			if tc.pinned {
				opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = bound.ID
			}
			if _, err := selector.Pick(context.Background(), provider, "model", opts, []*Auth{bound}); err != nil {
				t.Fatal(err)
			}
			got, err := selector.Pick(context.Background(), provider, "model", opts, []*Auth{bound, sub})
			if err != nil {
				t.Fatal(err)
			}
			want := bound.ID
			if tc.wantSub {
				want = sub.ID
			}
			if got.ID != want {
				t.Fatalf("got %s want %s", got.ID, want)
			}
		})
	}
}

func TestSessionAffinityClaudeSubscriptionChildReturnsIndependently(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, PreferClaudeSubscription: true})
	defer selector.Stop()
	api := subscriptionRecoveryAuth("api", AuthKindAPIKey, "0")
	sub := subscriptionRecoveryAuth("sub", AuthKindOAuth, "10")
	parent := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"parent"}}, Metadata: map[string]any{}}
	child := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"parent"}, "X-Claude-Code-Agent-Id": []string{"child"}}, Metadata: map[string]any{}}
	for _, opts := range []cliproxyexecutor.Options{parent, child} {
		if _, err := selector.Pick(t.Context(), "claude", "model", opts, []*Auth{api}); err != nil {
			t.Fatal(err)
		}
	}
	for _, opts := range []cliproxyexecutor.Options{child, parent} {
		got, err := selector.Pick(t.Context(), "claude", "model", opts, []*Auth{api, sub})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != sub.ID {
			t.Fatalf("got %s want subscription", got.ID)
		}
	}
	// A newly inherited child also returns instead of inheriting a paid binding.
	other := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"other-parent"}}, Metadata: map[string]any{}}
	if _, err := selector.Pick(t.Context(), "claude", "model", other, []*Auth{api}); err != nil {
		t.Fatal(err)
	}
	other.Headers.Set("X-Claude-Code-Agent-Id", "new-child")
	got, err := selector.Pick(t.Context(), "claude", "model", other, []*Auth{api, sub})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sub.ID {
		t.Fatalf("inherited %s want subscription", got.ID)
	}
}

func TestSessionAffinityClaudeSubscriptionRespectsModelCooldown(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, PreferClaudeSubscription: true})
	defer selector.Stop()
	api := subscriptionRecoveryAuth("api", AuthKindAPIKey, "0")
	sub := subscriptionRecoveryAuth("sub", AuthKindOAuth, "10")
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session"}}, Metadata: map[string]any{}}
	if _, err := selector.Pick(t.Context(), "claude", "model", opts, []*Auth{api}); err != nil {
		t.Fatal(err)
	}
	sub.ModelStates = map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
	got, err := selector.Pick(t.Context(), "claude", "model", opts, []*Auth{api, sub})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != api.ID {
		t.Fatalf("cooling subscription selected: %s", got.ID)
	}
	sub.ModelStates["model"].NextRetryAfter = time.Now().Add(-time.Second)
	got, err = selector.Pick(t.Context(), "claude", "model", opts, []*Auth{api, sub})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sub.ID {
		t.Fatalf("recovered subscription ignored: %s", got.ID)
	}
}

func TestManagerClaudeSubscriptionRecoveryPreservesThreadOwner(t *testing.T) {
	const model = "claude-subscription-thread-owner"
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, PreferClaudeSubscription: true})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	owner, subscription := registerClaudeThreadTestAuths(t, manager, t.Name(), model)
	owner.Attributes[AttributeAuthKind] = AuthKindAPIKey
	owner.Attributes["priority"] = "0"
	subscription.Attributes["priority"] = "10"
	for _, a := range []*Auth{owner, subscription} {
		if _, err := manager.Register(WithSkipPersist(t.Context()), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.RegisterExecutor(&claudeThreadTestExecutor{executeFunc: func(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		if auth.ID != owner.ID {
			t.Fatalf("thread owner replaced with %s", auth.ID)
		}
		if string(req.Payload) != `{"thread":{"previous_message_id":"owned-message"}}` {
			t.Fatalf("thread payload changed: %s", req.Payload)
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"id":"next"}`)}, nil
	}})
	// This is the same pinned metadata supplied by withClaudeThreadAuth for a known owner.
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session"}}, Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: owner.ID}}
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"thread":{"previous_message_id":"owned-message"}}`)}
	if _, err := manager.Execute(t.Context(), []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
}

func TestManagerClaudeSubscriptionRecoveryExecutionPreservesRequest(t *testing.T) {
	const model = "claude-subscription-execution"
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetFirstSelector{}, PreferClaudeSubscription: true})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	sub, api := registerClaudeThreadTestAuths(t, manager, t.Name(), model)
	sub.Attributes["priority"] = "10"
	api.Attributes[AttributeAuthKind] = AuthKindAPIKey
	api.Attributes["priority"] = "0"
	for _, a := range []*Auth{sub, api} {
		if _, err := manager.Register(WithSkipPersist(t.Context()), a); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte(`{"model":"claude-subscription-execution","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"first question"},{"role":"assistant","content":"prior answer"},{"role":"user","content":"continue"}]}`)
	var called []string
	quotaFailure := false
	manager.RegisterExecutor(&claudeThreadTestExecutor{executeFunc: func(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		called = append(called, auth.ID)
		if req.Model != model || !bytes.Equal(req.Payload, payload) {
			t.Fatal("execution changed model, effort or history")
		}
		if quotaFailure && auth.ID == sub.ID {
			return cliproxyexecutor.Response{}, claudeThreadStatusErr{status: 429, msg: "quota exceeded"}
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"id":"ok"}`)}, nil
	}})
	execute := func(want ...string) {
		t.Helper()
		called = nil
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: bytes.Clone(payload), Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session"}}, Metadata: map[string]any{}}
		if _, err := manager.Execute(t.Context(), []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: bytes.Clone(payload)}, opts); err != nil {
			t.Fatal(err)
		}
		if len(called) != len(want) {
			t.Fatalf("calls %v want %v", called, want)
		}
		for i := range want {
			if called[i] != want[i] {
				t.Fatalf("calls %v want %v", called, want)
			}
		}
	}
	execute(sub.ID)
	quotaFailure = true
	execute(sub.ID, api.ID)
	execute(api.ID)
	quotaFailure = false
	expireSessionAffinityPriorityModelCooldown(t, manager, sub.ID, model)
	execute(sub.ID)
}

func TestSessionAffinityClaudeSubscriptionLCPQuotaCommitsRecovery(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, PreferClaudeSubscription: true, RebindOn: SessionAffinityRebindOnQuota})
	defer selector.Stop()
	api := subscriptionRecoveryAuth("api", AuthKindAPIKey, "0")
	sub := subscriptionRecoveryAuth("sub", AuthKindOAuth, "10")
	peer := subscriptionRecoveryAuth("peer", AuthKindOAuth, "20")
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"continue my session"}]}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
	for _, step := range []struct {
		auths []*Auth
		want  string
	}{
		{[]*Auth{api}, api.ID},
		{[]*Auth{api, sub}, sub.ID},
		{[]*Auth{api, sub, peer}, sub.ID},
	} {
		got, err := selector.Pick(t.Context(), "claude", "model", opts, step.auths)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != step.want {
			t.Fatalf("got %s, want established recovery binding %s", got.ID, step.want)
		}
	}
}
