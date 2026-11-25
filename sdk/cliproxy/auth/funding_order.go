package auth

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FundingClass names what pays for a request sent on a credential.
type FundingClass string

const (
	// FundingAllowance is a subscription sign-in that still has plan allowance.
	FundingAllowance FundingClass = "allowance"
	// FundingAPICredit is an API key, charged to that organization's credit.
	FundingAPICredit FundingClass = "api-credit"
	// FundingUsageCredit is a subscription sign-in that is past its plan
	// allowance and still answering, so each request draws paid usage credits.
	FundingUsageCredit FundingClass = "usage-credit"
)

// fundingClasses lists every class in the order used for classes that a
// configured FundingOrder leaves out.
var fundingClasses = []FundingClass{FundingAllowance, FundingAPICredit, FundingUsageCredit}

// FundingOrder ranks funding classes from most to least preferred. It is a
// preference, never a spending cap: a lower class is still used when nothing
// better is available. The zero value disables ranking and leaves credential
// priority as the only ordering.
type FundingOrder []FundingClass

// ParseFundingOrder validates a configured order. An empty list disables
// ranking. A class may appear once; classes left out rank after the listed
// ones, in the order allowance, api-credit, usage-credit.
func ParseFundingOrder(raw []string) (FundingOrder, error) {
	var order FundingOrder
	for _, entry := range raw {
		class := FundingClass(strings.ToLower(strings.TrimSpace(entry)))
		known := false
		for _, candidate := range fundingClasses {
			known = known || class == candidate
		}
		if !known {
			return nil, fmt.Errorf("unknown funding class %q; use allowance, api-credit or usage-credit", entry)
		}
		for _, seen := range order {
			if seen == class {
				return nil, fmt.Errorf("funding class %q is listed twice", class)
			}
		}
		order = append(order, class)
	}
	return order, nil
}

// String renders the order as its configuration value.
func (o FundingOrder) String() string {
	parts := make([]string, len(o))
	for i, class := range o {
		parts[i] = string(class)
	}
	return strings.Join(parts, ",")
}

// rank is the position of class in the order. Lower is preferred.
func (o FundingOrder) rank(class FundingClass) int {
	for i, listed := range o {
		if listed == class {
			return i
		}
	}
	for i, known := range fundingClasses {
		if known == class {
			return len(o) + i
		}
	}
	return len(o) + len(fundingClasses)
}

// narrow keeps the candidates in the best funding class present. Input order
// is preserved, and the input is returned unchanged when ranking is disabled
// or every candidate already shares one class.
func (o FundingOrder) narrow(auths []*Auth, now time.Time) []*Auth {
	if len(o) == 0 || len(auths) <= 1 {
		return auths
	}
	ranks := make([]int, len(auths))
	best, bestCount := 0, 0
	for i, auth := range auths {
		ranks[i] = o.rank(fundingClassOf(auth, now))
		switch {
		case bestCount == 0 || ranks[i] < best:
			best, bestCount = ranks[i], 1
		case ranks[i] == best:
			bestCount++
		}
	}
	if bestCount == len(auths) {
		return auths
	}
	kept := make([]*Auth, 0, bestCount)
	for i, auth := range auths {
		if ranks[i] == best {
			kept = append(kept, auth)
		}
	}
	return kept
}

// fundingClassOf reports what currently pays for requests on auth. An API key
// is api-credit. A subscription is usage-credit only while its latest quota
// snapshot shows it past its plan allowance. Without that evidence it is
// allowance, and the next response corrects a stale view.
func fundingClassOf(auth *Auth, now time.Time) FundingClass {
	if auth == nil {
		return FundingAllowance
	}
	switch auth.AuthKind() {
	case AuthKindAPIKey:
		return FundingAPICredit
	case AuthKindOAuth:
		if usageCreditUntil(auth).After(now) {
			return FundingUsageCredit
		}
	}
	return FundingAllowance
}

// codexLimitFlagHold is how long a limit flag is trusted when the snapshot
// carries no window to say when the limit ends. The account is then tried
// again, and its answer renews or clears the flag.
const codexLimitFlagHold = 30 * time.Minute

// usageCreditUntil returns when the latest quota snapshot says a subscription
// stops drawing usage credits, or the zero time when it says nothing of the
// kind. The result depends only on the snapshot, so a limit that has ended
// cannot be carried over to a later window.
//
// Only Codex is recognised. A Codex account at its plan limit keeps answering
// from its credit balance, so no error ever removes it from selection. A
// Claude subscription without extra usage is rejected at its limit and cools
// down through the normal quota handling.
func usageCreditUntil(auth *Auth) time.Time {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return time.Time{}
	}
	observed := auth.Quota.ObservedAt
	var exhausted, soonest time.Time
	for _, window := range []string{"primary", "secondary"} {
		reset := codexWindowReset(auth, window)
		if reset.IsZero() {
			continue
		}
		if soonest.IsZero() || reset.Before(soonest) {
			soonest = reset
		}
		used, errUsed := strconv.ParseFloat(codexQuotaSignal(auth, "x-codex-"+window+"-used-percent"), 64)
		if errUsed == nil && used >= 100 && reset.After(exhausted) {
			exhausted = reset
		}
	}
	until := exhausted
	if until.IsZero() && codexLimitFlagged(auth) {
		// No window is full, so the flag is trusted up to the next reset. A
		// snapshot without windows gets a short hold from when it was seen.
		until = soonest
		if until.IsZero() && !observed.IsZero() {
			until = observed.Add(codexLimitFlagHold)
		}
	}
	// No plan window is longer than a week. Bound a reset that claims more.
	if limit := observed.Add(7 * 24 * time.Hour); !observed.IsZero() && until.After(limit) {
		until = limit
	}
	return until
}

// codexLimitFlagged reports the account-level signals Codex sends once
// ordinary plan usage is no longer allowed.
func codexLimitFlagged(auth *Auth) bool {
	return strings.EqualFold(codexQuotaSignal(auth, "x-codex-limit-reached"), "true") ||
		strings.EqualFold(codexQuotaSignal(auth, "x-codex-allowed"), "false")
}

func codexQuotaSignal(auth *Auth, name string) string {
	return strings.TrimSpace(auth.Quota.Signals[http.CanonicalHeaderKey(name)])
}

// codexWindowReset reads one Codex rate-limit window's reset time. It accepts
// an absolute reset, or a relative one measured from the observation time.
func codexWindowReset(auth *Auth, window string) time.Time {
	prefix := "x-codex-" + window
	if reset := parseWeeklyResetTime(codexQuotaSignal(auth, prefix+"-reset-at")); !reset.IsZero() {
		return reset
	}
	if auth.Quota.ObservedAt.IsZero() {
		return time.Time{}
	}
	seconds, errSeconds := strconv.ParseInt(codexQuotaSignal(auth, prefix+"-reset-after-seconds"), 10, 64)
	if errSeconds != nil || seconds <= 0 || seconds > 7*24*60*60 {
		return time.Time{}
	}
	return auth.Quota.ObservedAt.Add(time.Duration(seconds) * time.Second)
}
