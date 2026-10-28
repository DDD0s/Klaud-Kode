package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/config"
)

// The generated MCP file must carry the Body URL and its token, so a
// private-network install needs no hand-edited .mcp.json.
func TestEnsureMCPConfigWritesBodyEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		BodyURL:   "http://100.64.0.7:8787/mcp/" + strings.Repeat("a", 32),
		BodyMCP:   "komputer_use",
		MCPConfig: filepath.Join(dir, "session", ".mcp.json"),
	}
	if err := ensureMCPConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.MCPConfig)
	if err != nil {
		t.Fatal(err)
	}
	var parsed mcpConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	server, ok := parsed.Servers["komputer_use"]
	if !ok {
		t.Fatalf("missing server: %s", raw)
	}
	if server.URL != cfg.BodyURL {
		t.Fatalf("url = %q", server.URL)
	}
	if want := "Bearer " + strings.Repeat("a", 32); server.Headers["Authorization"] != want {
		t.Fatalf("auth header = %q", server.Headers["Authorization"])
	}
	info, err := os.Stat(cfg.MCPConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports the synthesised mode for WriteFile and enforces access
	// through ACLs, so the perm check only means something on Unix.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 (the file holds the token)", info.Mode().Perm())
	}
}

// Without a Body URL the operator's own file must not be rewritten.
func TestEnsureMCPConfigLeavesFileAloneWithoutBodyURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureMCPConfig(&config.Config{MCPConfig: path}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "original" {
		t.Fatalf("file was rewritten: %s", raw)
	}
}

// A header-supplied token stays out of the URL and goes into the file instead.
func TestEnsureMCPConfigUsesConfiguredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	cfg := config.Config{
		BodyURL:   "http://10.0.0.5:8787/mcp",
		BodyToken: "tok-from-env",
		BodyMCP:   "komputer_use",
		MCPConfig: path,
	}
	if err := ensureMCPConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var parsed mcpConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Servers["komputer_use"].Headers["Authorization"]; got != "Bearer tok-from-env" {
		t.Fatalf("auth header = %q", got)
	}
}

// In connect mode the CLI reaches this server's proxy, so the generated URL
// must use the address the server actually listens on, not the default port.
func TestEnsureMCPConfigConnectUsesListenAddress(t *testing.T) {
	for _, tc := range []struct {
		listen []config.ListenAddress
		want   string
	}{
		{[]config.ListenAddress{{Host: "100.64.0.5", Port: 9999}}, "http://100.64.0.5:9999/body/mcp"},
		{[]config.ListenAddress{{Host: "0.0.0.0", Port: 7777}}, "http://127.0.0.1:7777/body/mcp"},
		{[]config.ListenAddress{{Host: "100.64.0.5", Port: 9999}, {Host: "127.0.0.1", Port: 8888}}, "http://127.0.0.1:8888/body/mcp"},
	} {
		path := filepath.Join(t.TempDir(), ".mcp.json")
		cfg := config.Config{
			BodyConnect: true,
			BodyToken:   "tok-from-env",
			BodyMCP:     "komputer_use",
			MCPConfig:   path,
			Port:        18888,
			Listen:      tc.listen,
		}
		if err := ensureMCPConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		var parsed mcpConfig
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		if got := parsed.Servers["komputer_use"].URL; got != tc.want {
			t.Errorf("listen %v: url = %q, want %q", tc.listen, got, tc.want)
		}
	}
}
