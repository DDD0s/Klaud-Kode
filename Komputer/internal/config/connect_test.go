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

// Typos and weak guard values must fail loudly instead of half-configuring a
// link that looks up but never carries a tool call.
func TestBodyConnectValidationDetails(t *testing.T) {
	long := "0123456789abcdef0123456789abcdef"
	base := map[string]string{"KOMPUTER_API_HOST": "127.0.0.1"}
	full := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	if _, err := Load(full(map[string]string{"KOMPUTER_BODY_CONNECT": "enabled", "KOMPUTER_BODY_TOKEN": long})); err == nil {
		t.Fatal("unrecognized boolean value accepted")
	}
	if _, err := Load(full(map[string]string{"KOMPUTER_BODY_CONNECT": "1", "KOMPUTER_BODY_TOKEN": "short"})); err == nil {
		t.Fatal("short body token accepted")
	}
	if _, err := Load(full(map[string]string{
		"KOMPUTER_BODY_CONNECT":  "1",
		"KOMPUTER_BODY_TOKEN":    long,
		"KOMPUTER_TLS_CERT_FILE": "/tmp/cert.pem",
		"KOMPUTER_TLS_KEY_FILE":  "/tmp/key.pem",
	})); err == nil {
		t.Fatal("BodyConnect with built-in TLS accepted")
	}
}
