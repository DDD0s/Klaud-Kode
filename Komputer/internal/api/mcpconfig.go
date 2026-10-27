package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		// the other end of a WebSocket it dialed in. Use the address the
		// server actually listens on: KOMPUTER_API_HOST entries may carry
		// their own port, and a specific bind is not reachable on 127.0.0.1.
		bodyURL = proxyBaseURL(cfg) + "/body/mcp"
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
	// 0600: the file carries the Body token. WriteFile only applies the mode
	// on creation, so tighten a pre-existing file explicitly.
	if err := os.WriteFile(cfg.MCPConfig, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(cfg.MCPConfig, 0o600)
	}
	return nil
}

// proxyBaseURL returns where a process on this host can reach the API.
// A loopback listener wins; a wildcard bind is reachable on 127.0.0.1; a
// specific interface must be used as-is. With TLS configured every listener
// serves TLS, so the URL must be https as well.
func proxyBaseURL(cfg *config.Config) string {
	port := cfg.Port
	host := "127.0.0.1"
	for _, l := range cfg.Listen {
		port = l.Port
		if l.Host != "" && l.Host != "0.0.0.0" && l.Host != "::" {
			host = l.Host
		}
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			break
		}
	}
	scheme := "http"
	if cfg.TLSCertFile != "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
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
