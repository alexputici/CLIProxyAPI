package config

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFundingOrderConfigRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want []string
	}{
		{"unset", "routing: {session-affinity: true}", nil},
		{"ordered", `routing: {session-affinity: true, funding-order: ["allowance", "api-credit", "usage-credit"]}`, []string{"allowance", "api-credit", "usage-credit"}},
		{"reordered", `routing: {session-affinity: true, funding-order: ["usage-credit", "allowance"]}`, []string{"usage-credit", "allowance"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.data), &cfg); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.Routing.FundingOrder, tc.want) {
				t.Fatalf("got %v want %v", cfg.Routing.FundingOrder, tc.want)
			}
			saved, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Config
			if err := yaml.Unmarshal(saved, &loaded); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(loaded.Routing.FundingOrder, tc.want) {
				t.Fatalf("round trip got %v want %v", loaded.Routing.FundingOrder, tc.want)
			}
		})
	}
}
