package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// MCPHTTPClient is a minimal Streamable HTTP MCP client for one server.
// It is used for Body system_info and by the AGY bash shim.
type MCPHTTPClient struct {
	URL     string
	Headers map[string]string
	Client  *http.Client

	nextID    atomic.Int64
	initOnce  sync.Once
	initErr   error
	sessionID string
	mu        sync.Mutex
}

// NewMCPHTTPClient creates a client for one MCP server.
func NewMCPHTTPClient(server MCPServerSpec) *MCPHTTPClient {
	return &MCPHTTPClient{URL: server.URL, Headers: server.Headers, Client: http.DefaultClient}
}

func (c *MCPHTTPClient) post(ctx context.Context, payload map[string]any) (map[string]any, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	c.mu.Lock()
	session := c.sessionID
	c.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16_000_000))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	text := strings.TrimSpace(string(body))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || strings.HasPrefix(text, "event:") || strings.HasPrefix(text, "data:") {
		last := ""
		for _, line := range strings.Split(text, string([]byte{10})) {
			if strings.HasPrefix(line, "data:") {
				last = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		text = last
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, truncateText(text, 200))
	}
	if text == "" {
		return nil, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, fmt.Errorf("invalid MCP response: %w", err)
	}
	return decoded, nil
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *MCPHTTPClient) request(ctx context.Context, method string, params any) (any, error) {
	id := c.nextID.Add(1)
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("empty MCP response")
	}
	if rawErr, ok := resp["error"].(map[string]any); ok {
		return nil, fmt.Errorf("MCP error: %v", rawErr["message"])
	}
	return resp["result"], nil
}

func (c *MCPHTTPClient) ensureInit(ctx context.Context) error {
	c.initOnce.Do(func() {
		_, c.initErr = c.request(ctx, "initialize", map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "komputer", "version": "0"},
		})
		if c.initErr == nil {
			_, _ = c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		}
	})
	return c.initErr
}

// CallTool calls one MCP tool and decodes a JSON text result when present.
func (c *MCPHTTPClient) CallTool(ctx context.Context, name string, args any) (any, error) {
	if err := c.ensureInit(ctx); err != nil {
		return nil, err
	}
	result, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	obj, _ := result.(map[string]any)
	content, _ := obj["content"].([]any)
	text := ""
	if len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			text, _ = first["text"].(string)
		}
	}
	if isErr, _ := obj["isError"].(bool); isErr {
		return nil, fmt.Errorf("%s", text)
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err == nil {
		return parsed, nil
	}
	return text, nil
}
