package config

import "testing"

// A dialed-in Body replaces the tunnel entirely, and must have a token to
// authenticate with.
func TestBodyConnectValidation(t *testing.T) {
	if _, err := Load(map[string]string{
		"KOMPUTER_API_HOST":     "127.0.0.1",
		"KOMPUTER_BODY_CONNECT": "1",
	}); err == nil {
		t.Fatal("expected error: connect mode without a token")
	}
	cfg, err := Load(map[string]string{
		"KOMPUTER_API_HOST":     "127.0.0.1",
		"KOMPUTER_BODY_CONNECT": "1",
		"KOMPUTER_BODY_TOKEN":   "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BodyConnect || !cfg.SkipTunnel {
		t.Fatalf("connect mode must skip the tunnel: connect=%v skip=%v", cfg.BodyConnect, cfg.SkipTunnel)
	}
	if _, err := Load(map[string]string{
		"KOMPUTER_API_HOST":     "127.0.0.1",
		"KOMPUTER_BODY_CONNECT": "1",
		"KOMPUTER_BODY_TOKEN":   "0123456789abcdef0123456789abcdef",
		"KOMPUTER_BODY_URL":     "http://10.0.0.5:8787/mcp",
	}); err == nil {
		t.Fatal("expected error: BodyURL and BodyConnect are alternatives")
	}
}
