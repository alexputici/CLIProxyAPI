package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// codexWeeklyHeaders mirrors what a Codex account reports about its weekly window.
func codexWeeklyHeaders(usedPercent int, limitReached bool, reset time.Time) http.Header {
	return http.Header{
		"x-codex-primary-window-minutes": {"10080"},
		"x-codex-primary-used-percent":   {strconv.Itoa(usedPercent)},
		"x-codex-primary-reset-at":       {strconv.FormatInt(reset.Unix(), 10)},
		"x-codex-limit-reached":          {strconv.FormatBool(limitReached)},
	}
}

func codexFundingAuth(id string, usedPercent int, limitReached bool, reset, now time.Time) *Auth {
	return soonestResetAuth(id, "codex", codexWeeklyHeaders(usedPercent, limitReached, reset), now)
}

func fundingTestSelector(t *testing.T, order FundingOrder, rebindOn string) *SessionAffinitySelector {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetFirstSelector{}, FundingOrder: order, RebindOn: rebindOn})
	t.Cleanup(selector.Stop)
	return selector
}

func fundingSessionOptions(body string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:         http.Header{"X-Claude-Code-Session-Id": []string{"session"}},
		OriginalRequest: []byte(body),
		Metadata:        map[string]any{},
	}
}

func pickID(t *testing.T, selector Selector, opts cliproxyexecutor.Options, auths ...*Auth) string {
	t.Helper()
	picked, err := selector.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || picked == nil {
		t.Fatalf("pick = %v, error = %v", picked, err)
	}
	return picked.ID
}

func TestParseFundingOrder(t *testing.T) {
	order, err := ParseFundingOrder([]string{" Usage-Credit ", "ALLOWANCE"})
	if err != nil || order.String() != "usage-credit,allowance" {
		t.Fatalf("order = %q, error = %v", order, err)
	}
	if order, err = ParseFundingOrder(nil); err != nil || order != nil {
		t.Fatalf("an empty list must disable ranking, got %v, %v", order, err)
	}
	for _, raw := range [][]string{{"credits"}, {"allowance", "allowance"}, {""}} {
		if order, err = ParseFundingOrder(raw); err == nil || order != nil {
			t.Fatalf("%v: want an error, got %v", raw, order)
		}
	}
}

func TestFundingOrderRank(t *testing.T) {
	// Classes left out keep their default relative order after the listed ones.
	order := FundingOrder{FundingUsageCredit}
	if a, b, c := order.rank(FundingUsageCredit), order.rank(FundingAllowance), order.rank(FundingAPICredit); !(a < b && b < c) {
		t.Fatalf("ranks = %d, %d, %d", a, b, c)
	}
}

func TestFundingClassOf(t *testing.T) {
	now := time.Now()
	ahead, passed := now.Add(48*time.Hour), now.Add(-time.Hour)
	withHeaders := func(provider string, headers http.Header) *Auth {
		return soonestResetAuth("a", provider, headers, now)
	}
	apiKey := &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	// A five-hour window resetting in an hour beside a weekly window with room.
	twoWindows := func(fiveHourUsed string, flags http.Header) http.Header {
		headers := http.Header{
			"x-codex-primary-window-minutes":        {"300"},
			"x-codex-primary-used-percent":          {fiveHourUsed},
			"x-codex-primary-reset-after-seconds":   {"3600"},
			"x-codex-secondary-window-minutes":      {"10080"},
			"x-codex-secondary-used-percent":        {"40"},
			"x-codex-secondary-reset-after-seconds": {"400000"},
		}
		for key, values := range flags {
			headers[key] = values
		}
		return headers
	}
	flagged := http.Header{"x-codex-limit-reached": {"true"}}
	fiveHourFull := twoWindows("100", flagged)
	farReset := codexFundingAuth("a", 100, true, now.Add(30*24*time.Hour), now)
	for _, tc := range []struct {
		name string
		auth *Auth
		at   time.Time
		want FundingClass
	}{
		{"nil", nil, now, FundingAllowance},
		{"API key", apiKey, now, FundingAPICredit},
		{"subscription without signals", withHeaders("codex", nil), now, FundingAllowance},
		{"Codex with allowance left", codexFundingAuth("a", 43, false, ahead, now), now, FundingAllowance},
		{"Codex at its weekly limit", codexFundingAuth("a", 100, true, ahead, now), now, FundingUsageCredit},
		{"Codex full window without the flag", codexFundingAuth("a", 100, false, ahead, now), now, FundingUsageCredit},
		{"Codex limit whose reset has passed", codexFundingAuth("a", 100, true, passed, now), now, FundingAllowance},
		{"Codex limit read after its reset", codexFundingAuth("a", 100, true, ahead, now), ahead.Add(time.Minute), FundingAllowance},
		{"Codex limit flag without a full window", codexFundingAuth("a", 99, true, ahead, now), now, FundingUsageCredit},
		{"Codex not-allowed signal with a full window", withHeaders("codex", twoWindows("100", http.Header{"x-codex-allowed": {"false"}})), now, FundingUsageCredit},
		{"Codex five-hour window full", withHeaders("codex", fiveHourFull), now, FundingUsageCredit},
		// The flag belongs to the response that carried it. It must not be
		// re-attached to the weekly window once the five-hour limit has ended.
		{"Codex five-hour limit ended, flag still in the snapshot", withHeaders("codex", fiveHourFull), now.Add(2 * time.Hour), FundingAllowance},
		{"Codex flag with no full window", withHeaders("codex", twoWindows("99", flagged)), now, FundingUsageCredit},
		{"Codex flag with no full window, after the next reset", withHeaders("codex", twoWindows("99", flagged)), now.Add(2 * time.Hour), FundingAllowance},
		{"Codex flag without any window is held briefly", withHeaders("codex", flagged), now.Add(codexLimitFlagHold - time.Minute), FundingUsageCredit},
		{"Codex flag without any window expires", withHeaders("codex", flagged), now.Add(codexLimitFlagHold + time.Minute), FundingAllowance},
		{"Codex not-allowed signal without any window", withHeaders("codex", http.Header{"x-codex-allowed": {"false"}}), now, FundingUsageCredit},
		{"Codex reset beyond a week is still trusted inside the week", farReset, now.Add(6 * 24 * time.Hour), FundingUsageCredit},
		{"Codex reset beyond a week is bounded", farReset, now.Add(8 * 24 * time.Hour), FundingAllowance},
		{"Claude rejection is left to cooldowns", withHeaders("claude", http.Header{
			"anthropic-ratelimit-unified-7d-status": {"rejected"},
			"anthropic-ratelimit-unified-7d-reset":  {strconv.FormatInt(ahead.Unix(), 10)},
		}), now, FundingAllowance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fundingClassOf(tc.auth, tc.at); got != tc.want {
				t.Fatalf("class = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFundingOrderNarrow(t *testing.T) {
	now := time.Now()
	reset := now.Add(48 * time.Hour)
	allowance := codexFundingAuth("allowance", 10, false, reset, now)
	credit := codexFundingAuth("credit", 100, true, reset, now)
	apiKey := &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	all := []*Auth{credit, apiKey, allowance}
	ids := func(auths []*Auth) string {
		out := ""
		for _, auth := range auths {
			out += auth.ID + " "
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		order FundingOrder
		in    []*Auth
		want  string
	}{
		{"disabled keeps everything", nil, all, "credit key allowance "},
		{"default order", FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}, all, "allowance "},
		{"API key before credits", FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}, []*Auth{credit, apiKey}, "key "},
		{"credits before the API key", FundingOrder{FundingAllowance, FundingUsageCredit, FundingAPICredit}, []*Auth{credit, apiKey}, "credit "},
		{"credits first", FundingOrder{FundingUsageCredit}, all, "credit "},
		{"one class stays whole", FundingOrder{FundingAllowance}, []*Auth{credit, credit}, "credit credit "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(tc.order.narrow(tc.in, now)); got != tc.want {
				t.Fatalf("narrowed to %q, want %q", got, tc.want)
			}
		})
	}
}

// The reported failure: the account with the sooner reset was past its weekly
// limit and answering from credits, and soonest-reset-first kept choosing it
// while another account had its whole allowance.
func TestFundingOrderAvoidsCodexAccountDrawingCredits(t *testing.T) {
	now := time.Now()
	accounts := func() (*Auth, *Auth) {
		return codexFundingAuth("lab", 100, true, now.Add(5*24*time.Hour), now), codexFundingAuth("personal", 0, false, now.Add(6*24*time.Hour), now)
	}
	order := FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}
	for _, tc := range []struct {
		name  string
		order FundingOrder
		opts  cliproxyexecutor.Options
		want  string
	}{
		{"without an order the sooner reset wins", nil, fundingSessionOptions(`{}`), "lab"},
		{"new session", order, fundingSessionOptions(`{}`), "personal"},
		{"request without a session", order, cliproxyexecutor.Options{}, "personal"},
		{"credits preferred when configured", FundingOrder{FundingUsageCredit, FundingAllowance}, fundingSessionOptions(`{}`), "lab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab, personal := accounts()
			if got := pickID(t, fundingTestSelector(t, tc.order, ""), tc.opts, lab, personal); got != tc.want {
				t.Fatalf("picked %s, want %s", got, tc.want)
			}
		})
	}
	t.Run("credits are still used when nothing else is available", func(t *testing.T) {
		lab, _ := accounts()
		if got := pickID(t, fundingTestSelector(t, order, ""), fundingSessionOptions(`{}`), lab); got != "lab" {
			t.Fatalf("picked %s", got)
		}
	})
	t.Run("an API key ranks between allowance and credits", func(t *testing.T) {
		lab, _ := accounts()
		apiKey := &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey, "priority": "-5"}}
		lab.Attributes["priority"] = "100"
		if got := pickID(t, fundingTestSelector(t, order, ""), fundingSessionOptions(`{}`), lab, apiKey); got != "key" {
			t.Fatalf("picked %s, want the API key ahead of the higher-priority account on credits", got)
		}
	})
}

func TestFundingOrderMovesBoundSessionOffCredits(t *testing.T) {
	now := time.Now()
	labReset, personalReset := now.Add(24*time.Hour), now.Add(72*time.Hour)
	lab := codexFundingAuth("lab", 43, false, labReset, now)
	personal := codexFundingAuth("personal", 0, false, personalReset, now)
	selector := fundingTestSelector(t, FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}, "")
	portable := fundingSessionOptions(`{"input":[{"role":"user","content":"continue"}]}`)
	continuing := fundingSessionOptions(`{"previous_response_id":"resp_1","input":[]}`)
	expect := func(step string, opts cliproxyexecutor.Options, want string) {
		t.Helper()
		if got := pickID(t, selector, opts, lab, personal); got != want {
			t.Fatalf("%s: picked %s, want %s", step, got, want)
		}
	}
	expect("both have allowance, sooner reset binds", portable, "lab")
	expect("a healthy binding stays", portable, "lab")

	lab.Quota.ObserveResponseHeadersForProvider("codex", codexWeeklyHeaders(100, true, labReset), now)
	expect("a request that continues a stored response stays", continuing, "lab")
	expect("the next full request moves to allowance", portable, "personal")
	expect("the new binding holds", continuing, "personal")

	personal.Quota.ObserveResponseHeadersForProvider("codex", codexWeeklyHeaders(100, true, personalReset), now)
	expect("no move between accounts in the same class", portable, "personal")

	lab.Quota.ObserveResponseHeadersForProvider("codex", codexWeeklyHeaders(2, false, now.Add(7*24*time.Hour)), now)
	expect("the session returns when the first account resets", portable, "lab")
}

func TestFundingOrderBoundSessionBoundaries(t *testing.T) {
	now := time.Now()
	order := FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}
	for _, tc := range []struct {
		name     string
		order    FundingOrder
		rebindOn string
		pinned   bool
		other    string
		want     string
	}{
		{name: "moves by default", order: order, want: "better"},
		{name: "no order keeps the binding", want: "bound"},
		{name: "rebind never keeps the binding", order: order, rebindOn: SessionAffinityRebindOnNever, want: "bound"},
		{name: "a pin keeps the binding", order: order, pinned: true, want: "bound"},
		{name: "another provider is not a destination", order: order, other: "claude", want: "bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := codexFundingAuth("bound", 50, false, now.Add(24*time.Hour), now)
			better := codexFundingAuth("better", 0, false, now.Add(72*time.Hour), now)
			selector := fundingTestSelector(t, tc.order, tc.rebindOn)
			opts := fundingSessionOptions(`{"input":[]}`)
			if tc.pinned {
				opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = bound.ID
			}
			// Both picks use the mixed route so they share one binding.
			if first, err := selector.Pick(context.Background(), "mixed", "model", opts, []*Auth{bound}); err != nil || first == nil || first.ID != "bound" {
				t.Fatalf("first pick = %v, error %v", first, err)
			}
			bound.Quota.ObserveResponseHeadersForProvider("codex", codexWeeklyHeaders(100, true, now.Add(24*time.Hour)), now)
			if tc.other != "" {
				better.Provider = tc.other
			}
			picked, err := selector.Pick(context.Background(), "mixed", "model", opts, []*Auth{bound, better})
			if err != nil || picked == nil || picked.ID != tc.want {
				t.Fatalf("picked %v, error %v, want %s", picked, err, tc.want)
			}
		})
	}
}

// From real response headers to routing: the account answers successfully
// while reporting its weekly limit, and the manager records that.
func TestManagerFundingOrderFollowsCodexResponseHeaders(t *testing.T) {
	const model = "codex-funding-order"
	now := time.Now()
	labReset := now.Add(24 * time.Hour)
	manager := NewManager(nil, nil, nil)
	manager.SetSelector(fundingTestSelector(t, FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}, ""))
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	lab := codexFundingAuth(t.Name()+"-lab", 43, false, labReset, now)
	personal := codexFundingAuth(t.Name()+"-personal", 0, false, now.Add(72*time.Hour), now)
	for _, account := range []*Auth{lab, personal} {
		account.Status = StatusActive
		if _, err := manager.Register(WithSkipPersist(t.Context()), account); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(account.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.ID) })
	}
	pick := func(step, session, want string) {
		t.Helper()
		opts := cliproxyexecutor.Options{
			Headers:         http.Header{"X-Claude-Code-Session-Id": []string{session}},
			OriginalRequest: []byte(`{"input":[{"role":"user","content":"continue"}]}`),
			Metadata:        map[string]any{},
		}
		got, _, err := manager.pickNext(t.Context(), "codex", model, opts, nil)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("%s: picked %v, error %v, want %s", step, got, err, want)
		}
	}
	pick("the sooner reset binds while both have allowance", "first", lab.ID)

	ctx := internallogging.WithResponseHeadersHolder(t.Context())
	internallogging.SetResponseHeaders(ctx, codexWeeklyHeaders(100, true, labReset))
	manager.MarkResult(ctx, Result{AuthID: lab.ID, Provider: "codex", Model: model, Success: true})

	pick("the bound session leaves the account on credits", "first", personal.ID)
	pick("a new session avoids it", "second", personal.ID)
}

// A session recognised by its history, with no session header, moves too.
func TestFundingOrderMovesHistoryMatchedSession(t *testing.T) {
	now := time.Now()
	labReset := now.Add(24 * time.Hour)
	lab := codexFundingAuth("lab", 43, false, labReset, now)
	personal := codexFundingAuth("personal", 0, false, now.Add(72*time.Hour), now)
	selector := fundingTestSelector(t, FundingOrder{FundingAllowance, FundingAPICredit, FundingUsageCredit}, SessionAffinityRebindOnQuota)
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"continue my session"}]}`),
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"},
	}
	expect := func(step, want string) {
		t.Helper()
		if got := pickID(t, selector, opts, lab, personal); got != want {
			t.Fatalf("%s: picked %s, want %s", step, got, want)
		}
	}
	expect("first request binds the sooner reset", "lab")
	expect("the history match holds the binding", "lab")
	lab.Quota.ObserveResponseHeadersForProvider("codex", codexWeeklyHeaders(100, true, labReset), now)
	expect("the matched session moves to allowance", "personal")
	expect("the move is committed", "personal")
}
