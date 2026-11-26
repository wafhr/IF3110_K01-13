package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/config"
)

func TestLoadDotEnv(t *testing.T) {
	const key = "IF3110_CONFIG_TEST_VALUE"
	for _, tc := range []struct {
		name, contents, existing, want string
		missing, preset, invalid       bool
	}{
		{name: "quoted value", contents: key + `="value with spaces # and ="`, want: "value with spaces # and ="},
		{name: "external environment wins", contents: key + "=file-value", preset: true, existing: "external-value", want: "external-value"},
		{name: "explicit empty environment wins", contents: key + "=file-value", preset: true},
		{name: "missing env ignores example file", missing: true},
		{name: "missing env preserves external environment", missing: true, preset: true, existing: "external-value", want: "external-value"},
		{name: "malformed file", contents: "INVALID@KEY=fixture-secret", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv(key, tc.existing)
			if !tc.preset {
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(".env.example", []byte(key+"=must-not-load"), 0600); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if err := os.WriteFile(".env", []byte(tc.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := config.LoadDotEnv()
			if tc.invalid {
				if err == nil {
					t.Fatal("expected malformed configuration error")
				}
				if strings.Contains(err.Error(), "fixture-secret") {
					t.Fatal("error exposed file contents")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := os.Getenv(key); got != tc.want {
				t.Fatalf("loaded value = %q, want %q", got, tc.want)
			}
		})
	}
}

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

func TestLoadDatabaseEncryptionKey(t *testing.T) {
	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("DATABASE_ENCRYPTION_KEY", "configured-registry-key")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseEncryptionKey != "configured-registry-key" {
		t.Fatal("database encryption key was not loaded from the environment")
	}
}

func TestLoadWorker(t *testing.T) {
	keys := []string{"WORKER_ID", "WORKER_CONCURRENCY", "WORKER_MAX_ATTEMPTS", "WORKER_POLL_INTERVAL", "WORKER_LEASE"}
	reset := func(t *testing.T) {
		for _, key := range keys {
			t.Setenv(key, "")
		}
	}
	t.Run("defaults", func(t *testing.T) {
		reset(t)
		worker, err := config.LoadWorker()
		if err != nil {
			t.Fatal(err)
		}
		if worker.ID == "" || worker.Concurrency != 4 || worker.MaxAttempts != 5 || worker.PollInterval != time.Second || worker.Lease != 30*time.Second {
			t.Fatalf("defaults = %+v", worker)
		}
	})
	t.Run("explicit settings", func(t *testing.T) {
		reset(t)
		t.Setenv("WORKER_ID", "worker-a")
		t.Setenv("WORKER_CONCURRENCY", "2")
		t.Setenv("WORKER_MAX_ATTEMPTS", "3")
		t.Setenv("WORKER_POLL_INTERVAL", "250ms")
		t.Setenv("WORKER_LEASE", "10s")
		worker, err := config.LoadWorker()
		if err != nil {
			t.Fatal(err)
		}
		if worker.ID != "worker-a" || worker.Concurrency != 2 || worker.MaxAttempts != 3 || worker.PollInterval != 250*time.Millisecond || worker.Lease != 10*time.Second {
			t.Fatalf("settings = %+v", worker)
		}
	})
	for _, tc := range []struct{ key, value string }{
		{"WORKER_CONCURRENCY", "0"},
		{"WORKER_MAX_ATTEMPTS", "many"},
		{"WORKER_POLL_INTERVAL", "-1s"},
		{"WORKER_LEASE", "2s"},
		{"WORKER_LEASE", "soon"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			reset(t)
			t.Setenv(tc.key, tc.value)
			if _, err := config.LoadWorker(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v, want one naming %s", err, tc.key)
			}
		})
	}
}
