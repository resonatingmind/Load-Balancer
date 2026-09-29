package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndAcceptsBOM(t *testing.T) {
	path := writeConfig(t, "\xef\xbb\xbf"+`{"backends": [{"url": "http://localhost:9001"}], "circuit_breaker": {"failure_threshold": 7}}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("listen_addr = %q, want default :8080", cfg.ListenAddr)
	}
	if cfg.CircuitBreaker.FailureThreshold != 7 {
		t.Errorf("failure_threshold = %d, want 7 from file", cfg.CircuitBreaker.FailureThreshold)
	}
	if cfg.CircuitBreaker.OpenTimeout.Duration != 10*time.Second {
		t.Errorf("open_timeout = %s, want default 10s", cfg.CircuitBreaker.OpenTimeout)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"no backends":      `{"backends": []}`,
		"unknown field":    `{"backends": [{"url": "http://a"}], "stratgy": "round_robin"}`,
		"bad duration":     `{"backends": [{"url": "http://a"}], "health_check": {"interval": "soon"}}`,
		"zero threshold":   `{"backends": [{"url": "http://a"}], "circuit_breaker": {"failure_threshold": 0}}`,
		"relative hc path": `{"backends": [{"url": "http://a"}], "health_check": {"path": "health"}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}
