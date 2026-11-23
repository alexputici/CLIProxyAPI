package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClaudeSubscriptionRecoveryConfigOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		data := []byte("routing: {session-affinity: true}")
		if enabled {
			data = []byte("routing: {session-affinity: true, session-affinity-prefer-claude-subscription: true}")
		}
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Routing.SessionAffinityPreferClaudeSubscription != enabled {
			t.Fatalf("enabled=%v got %v", enabled, cfg.Routing.SessionAffinityPreferClaudeSubscription)
		}
		saved, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var loaded Config
		if err := yaml.Unmarshal(saved, &loaded); err != nil {
			t.Fatal(err)
		}
		if loaded.Routing.SessionAffinityPreferClaudeSubscription != enabled {
			t.Fatal("round trip lost subscription policy")
		}
	}
}
