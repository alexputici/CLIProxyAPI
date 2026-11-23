package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"strconv"
	"testing"
	"time"
)

type soonestTransportCapture struct {
	schedulerTestExecutor
	picked string
}

func (e *soonestTransportCapture) Execute(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.picked = a.ID
	return cliproxyexecutor.Response{}, nil
}
func TestSoonestResetExecutePrefersWebsocket(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		t.Run(strconv.FormatBool(affinity), func(t *testing.T) {
			var selector Selector = &SoonestResetFirstSelector{}
			if affinity {
				s := NewSessionAffinitySelector(selector)
				t.Cleanup(s.Stop)
				selector = s
			}
			m := NewManager(nil, selector, nil)
			ex := &soonestTransportCapture{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}}
			m.RegisterExecutor(ex)
			now := time.Now()
			for _, id := range []string{"http", "websocket"} {
				a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}
				seconds := "3600"
				if id == "websocket" {
					a.Attributes["websockets"] = "true"
					seconds = "7200"
				}
				a.Quota.ObserveResponseHeadersForProvider("codex", http.Header{"x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-after-seconds": {seconds}}, now)
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "soonest-transport-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, err := m.Register(t.Context(), a); err != nil {
					t.Fatal(err)
				}
			}
			_, err := m.Execute(cliproxyexecutor.WithDownstreamWebsocket(t.Context()), []string{"codex"}, cliproxyexecutor.Request{Model: "soonest-transport-model"}, cliproxyexecutor.Options{OriginalRequest: []byte(`{"session_id":"fresh-transport"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if ex.picked != "websocket" {
				t.Fatalf("Execute selected %q; want websocket", ex.picked)
			}
		})
	}
}
