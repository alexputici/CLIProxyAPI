package cliproxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestSoonestResetRoutingBuilderAndAffinity(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir(), Routing: internalconfig.RoutingConfig{
		Strategy: "soonest-reset-first", SessionAffinity: true,
	}}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(t.TempDir() + "/config.yaml").Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	selector, ok := service.coreManager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector = %T, want session affinity", service.coreManager.Selector())
	}
	t.Cleanup(selector.Stop)
	now := time.Now()
	account := func(id string, reset time.Time) *coreauth.Auth {
		a := &coreauth.Auth{ID: id, Provider: "claude", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}}
		a.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset": {strconv.FormatInt(reset.Unix(), 10)},
		}, now)
		return a
	}
	a, b := account("a", now.Add(72*time.Hour)), account("b", now.Add(24*time.Hour))
	pick := func(session, want string) {
		t.Helper()
		got, errPick := selector.Pick(t.Context(), "claude", "claude-sonnet", cliproxyexecutor.Options{
			OriginalRequest: []byte(`{"session_id":"` + session + `"}`),
		}, []*coreauth.Auth{a, b})
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("session %s: got %v, error %v, want %s", session, got, errPick, want)
		}
	}
	pick("existing", "b")
	a.Quota = account("a", now.Add(time.Hour)).Quota
	pick("existing", "b")
	pick("fresh", "a")
	commit := service.commitConfigUpdate(cfg)
	if !service.applyConfigRuntime(t.Context(), commit, false) {
		t.Fatal("same-routing reload failed")
	}
	if service.coreManager.Selector() != selector {
		t.Fatal("same-routing reload replaced session bindings")
	}
}
