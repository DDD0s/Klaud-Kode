package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
)

// mcpConfig is the Claude MCP file shape.
type mcpConfig struct {
	Servers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ensureMCPConfig points the CLI at the Body. With KOMPUTER_BODY_URL the file is
// generated from that address, so a private-network install needs no hand-edited
// .mcp.json and no token copied into the repository. Without it the checked-in
// template (or an operator's own file) is left alone.
func ensureMCPConfig(cfg *config.Config) error {
	bodyURL := cfg.BodyURL
	if cfg.BodyConnect {
		// The CLI talks to this server's loopback proxy; the live Body is on
		// the other end of a WebSocket it dialed in.
		bodyURL = fmt.Sprintf("http://127.0.0.1:%d/body/mcp", cfg.Port)
	}
	if bodyURL == "" {
		return nil
	}
	token := cfg.BodyToken
	if token == "" {
		token = readBodyToken(bodyURL)
	}
	server := mcpServer{Type: "http", URL: bodyURL}
	if token != "" {
		server.Headers = map[string]string{"Authorization": "Bearer " + token}
	}
	data, err := json.MarshalIndent(mcpConfig{Servers: map[string]mcpServer{cfg.BodyMCP: server}}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.MCPConfig), 0o755); err != nil {
		return fmt.Errorf("cannot create the MCP config directory: %w", err)
	}
	// 0600: the file carries the Body token.
	return os.WriteFile(cfg.MCPConfig, append(data, '\n'), 0o600)
}

// readBodyToken extracts the token from a URL of the form http://host:port/mcp/<token>.
func readBodyToken(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) >= 2 && segments[0] == "mcp" {
		return segments[1]
	}
	return ""
}
