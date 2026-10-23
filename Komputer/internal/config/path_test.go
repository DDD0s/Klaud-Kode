package config

import (
	"strings"
	"testing"
)

// A direct Body URL is the private-network path: it replaces the SSH tunnel
// and its health check, without the operator disabling the tunnel by hand.
func TestBodyURLReplacesTunnel(t *testing.T) {
	cfg, err := Load(map[string]string{
		"KOMPUTER_API_HOST": "127.0.0.1",
		"KOMPUTER_BODY_URL": "http://100.64.0.7:8787/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SkipTunnel {
		t.Fatal("a direct Body URL must skip the SSH tunnel")
	}
	if cfg.BodyProbe != "http://100.64.0.7:8787/healthz" {
		t.Fatalf("probe = %q", cfg.BodyProbe)
	}
	if cfg.HealthURL != cfg.BodyProbe {
		t.Fatal("health check must follow the Body URL")
	}
}

// An explicit probe and the tunnel switch still win, so a Body behind a
// proxy can be checked somewhere else.
func TestBodyProbeAndTunnelOverride(t *testing.T) {
	cfg, err := Load(map[string]string{
		"KOMPUTER_API_HOST":   "127.0.0.1",
		"KOMPUTER_BODY_URL":   "https://body.example/mcp/secret",
		"KOMPUTER_BODY_PROBE": "https://body.example/healthz",
		"KOMPUTER_TUNNEL_UP":  "/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BodyProbe != "https://body.example/healthz" {
		t.Fatalf("probe = %q", cfg.BodyProbe)
	}
	if cfg.BodyToken != "" {
		t.Fatalf("BodyToken should come from the URL, got %q", cfg.BodyToken)
	}
}

// Without KOMPUTER_BODY_URL nothing changes for existing installs.
func TestBodyURLUnsetKeepsTunnelDefault(t *testing.T) {
	cfg, err := Load(map[string]string{"KOMPUTER_API_HOST": "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkipTunnel {
		t.Fatal("tunnel should stay enabled by default")
	}
	if !strings.HasSuffix(cfg.HealthURL, "/healthz") || !strings.Contains(cfg.HealthURL, "18787") {
		t.Fatalf("health URL changed: %q", cfg.HealthURL)
	}
}
