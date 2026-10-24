package config_test

import (
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/config"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, port, timeout string
		invalid             bool
	}{
		{"defaults", "", "", false},
		{"custom", "9000", "3s", false},
		{"invalid port", "abc", "", true},
		{"port out of range", "65536", "", true},
		{"invalid timeout", "", "abc", true},
		{"nonpositive timeout", "", "0s", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PORT", tc.port)
			t.Setenv("SHUTDOWN_TIMEOUT", tc.timeout)
			cfg, err := config.Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("expected configuration error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "defaults" && (cfg.Address() != ":8080" || cfg.ShutdownTimeout != 10*time.Second) {
				t.Fatalf("unexpected defaults: %+v", cfg)
			}
			if tc.name == "custom" && (cfg.Address() != ":9000" || cfg.ShutdownTimeout != 3*time.Second) {
				t.Fatalf("unexpected custom config: %+v", cfg)
			}
		})
	}
}
