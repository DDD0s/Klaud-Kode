package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With a direct Body URL the health check must report the Body for real
// instead of claiming the probe was skipped.
func TestHealthReportsDirectBody(t *testing.T) {
	body := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer body.Close()

	server, _ := newBareServer(t)
	server.cfg.BodyURL = body.URL + "/mcp/" + strings.Repeat("z", 32)
	server.cfg.BodyProbe = body.URL + "/healthz"
	server.cfg.SkipTunnel = true

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	server.handleHealth(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	payload := rec.Body.String()
	if !strings.Contains(payload, `"body_direct":true`) {
		t.Fatalf("direct body not reported: %s", payload)
	}
	if !strings.Contains(payload, `"body_probe_skipped":false`) {
		t.Fatalf("probe reported as skipped: %s", payload)
	}
	// The token must not be echoed in full.
	if strings.Contains(payload, strings.Repeat("z", 32)) {
		t.Fatalf("body token leaked: %s", payload)
	}
}

// Direct mode still surfaces an unreachable Body as body_ok=false.
func TestHealthDirectBodyUnreachable(t *testing.T) {
	server, _ := newBareServer(t)
	server.cfg.BodyURL = "http://127.0.0.1:1/mcp/token"
	server.cfg.BodyProbe = "http://127.0.0.1:1/healthz"
	server.cfg.TunnelFailCache = 0

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	server.handleHealth(rec, req)
	if !strings.Contains(rec.Body.String(), `"body_ok":false`) {
		t.Fatalf("unreachable body not reported: %s", rec.Body.String())
	}
}
