package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/version"
)

// Clients percent-encode session ids; the id must resolve after decoding, and a
// malformed escape must not break dispatch.
func TestCleanPathDecodes(t *testing.T) {
	if got := cleanPath("/v1/sessions/a%20b"); got != "/v1/sessions/a b" {
		t.Fatalf("decoded path: %q", got)
	}
	if got := cleanPath("/v1/sessions/plain/"); got != "/v1/sessions/plain" {
		t.Fatalf("trailing slash: %q", got)
	}
	if got := cleanPath("/v1/bad%zz"); got != "/v1/bad%zz" {
		t.Fatalf("malformed escape should stay raw: %q", got)
	}
	if got := cleanPath(""); got != "/" {
		t.Fatalf("empty path: %q", got)
	}
}

// count_tokens must measure UTF-16 code units like the JavaScript server, so an
// emoji counts as two rather than one.
func TestCountTokensUsesUTF16Length(t *testing.T) {
	server, _ := newBareServer(t)
	body := `{"messages":[{"role":"user","content":"\u00e9\ud83d\ude00"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	if err := server.handleCountTokens(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// One accented letter plus one astral character is three UTF-16 units;
	// 3 / 3.5 rounds to 1 token. Rune counting would give 2 / 3.5 -> 1 too, so
	// check the exact number for a longer astral string below.
	if !strings.Contains(rec.Body.String(), `"input_tokens":1`) {
		t.Fatalf("unexpected estimate: %s", rec.Body.String())
	}
}

// Long astral text distinguishes UTF-16 counting from rune counting.
func TestCountTokensDistinguishesAstralText(t *testing.T) {
	server, _ := newBareServer(t)
	// 14 emoji = 28 UTF-16 units -> 8 tokens, but only 14 runes -> 4 tokens.
	var b strings.Builder
	b.WriteString(`{"messages":[{"role":"user","content":"`)
	for i := 0; i < 14; i++ {
		b.WriteString(`\ud83d\ude00`)
	}
	b.WriteString(`"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(b.String()))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	if err := server.handleCountTokens(rec, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":8`) {
		t.Fatalf("astral estimate should follow UTF-16 units: %s", rec.Body.String())
	}
}

// Health reports the release version so operators can confirm what runs.
func TestHealthReportsVersion(t *testing.T) {
	server, _ := newBareServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	server.handleHealth(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), version.Value) {
		t.Fatalf("health omits version %q: %s", version.Value, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"implementation":"go"`) {
		t.Fatalf("health omitted implementation: %s", rec.Body.String())
	}
}
