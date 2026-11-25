package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetFirstSelector favors the earliest observed upcoming weekly reset.
// Unknown OAuth accounts get one probe every eight fresh selections, rotating
// across unknowns. Session affinity wraps this selector, so bound requests do
// not spend probes or move away from healthy credentials.
type SoonestResetFirstSelector struct {
	mu     sync.Mutex
	states map[string]soonestResetSelectionState
}

type soonestResetSelectionState struct {
	lastPicked  string
	lastUnknown string
	selections  uint8
}

// Pick applies normal priority, model eligibility, and transport preferences
// before comparing quota observations. Equal resets and all-unknown pools rotate.
func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	transportProvider := provider
	if provider == "mixed" && len(available) > 0 {
		transportProvider = available[0].Provider
		for _, candidate := range available[1:] {
			if !strings.EqualFold(candidate.Provider, transportProvider) {
				transportProvider = "mixed"
				break
			}
		}
	}
	available = preferCodexWebsocketAuths(ctx, transportProvider, available)
	var earliest time.Time
	var preferred, unknown []*Auth
	for _, candidate := range available {
		reset := upcomingWeeklyReset(candidate, now)
		if reset.IsZero() {
			if supportsWeeklyReset(candidate) {
				unknown = append(unknown, candidate)
			}
			continue
		}
		if earliest.IsZero() || reset.Before(earliest) {
			earliest = reset
			preferred = preferred[:0]
		}
		if reset.Equal(earliest) {
			preferred = append(preferred, candidate)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + ":" + canonicalModelLookupKey(model)
	if s.states == nil {
		s.states = make(map[string]soonestResetSelectionState)
	}
	if _, exists := s.states[key]; !exists && len(s.states) >= 4096 {
		clear(s.states)
	}
	state := s.states[key]
	if len(preferred) == 0 {
		preferred = available
		state.selections = 0
	} else if len(unknown) > 0 {
		state.selections++
		if state.selections >= 8 {
			picked := unknown[successorIndex(unknown, state.lastUnknown)]
			state.lastUnknown = picked.ID
			state.selections = 0
			s.states[key] = state
			return picked, nil
		}
	} else {
		state.selections = 0
	}
	picked := preferred[successorIndex(preferred, state.lastPicked)]
	state.lastPicked = picked.ID
	s.states[key] = state
	return picked, nil
}

func supportsWeeklyReset(auth *Auth) bool {
	if auth == nil || auth.AuthKind() != AuthKindOAuth {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude", "codex":
		return true
	default:
		return false
	}
}

func upcomingWeeklyReset(auth *Auth, now time.Time) time.Time {
	if !supportsWeeklyReset(auth) {
		return time.Time{}
	}
	var reset time.Time
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		reset = parseWeeklyResetTime(strings.TrimSpace(auth.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-reset")]))
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			minutes, errMinutes := strconv.ParseInt(codexQuotaSignal(auth, "x-codex-"+window+"-window-minutes"), 10, 64)
			if errMinutes != nil || minutes != 7*24*60 {
				continue
			}
			candidate := codexWindowReset(auth, window)
			if candidate.After(now) && (reset.IsZero() || candidate.Before(reset)) {
				reset = candidate
			}
		}
	}
	// A passed reset needs a new upstream observation. Never roll it forward by
	// a week or substitute an access-token expiry for a subscription quota.
	if !reset.After(now) {
		return time.Time{}
	}
	return reset
}

func parseWeeklyResetTime(raw string) time.Time {
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	parsed, _ := time.Parse(time.RFC3339, raw)
	return parsed
}
