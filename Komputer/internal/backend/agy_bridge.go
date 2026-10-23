package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// AGYBridgeTarget is the per-run MCP target file agy reads through the
// one globally registered bridge process.
type AGYBridgeTarget struct {
	Servers map[string]MCPServerSpec `json:"servers"`
	Policy  *AGYPolicy               `json:"policy,omitempty"`
	Remote  *AGYRemote               `json:"remote,omitempty"`
}

// AGYRemote is the bash shim target: which Body server runs commands.
type AGYRemote struct {
	Server *MCPServerSpec `json:"server"`
	Mount  string         `json:"mount"`
	Root   string         `json:"root"`
	Shell  string         `json:"shell"`
}

// LoadAGYTarget reads the current turn's target file. agy owns the bridge
// processes, so created/missing/malformed is a state to serve tools from.
func LoadAGYTarget(file string) *AGYBridgeTarget {
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var parsed AGYBridgeTarget
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return &parsed
}

func agyPost(ctx context.Context, server MCPServerSpec, payload map[string]any) (map[string]any, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range server.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := strings.TrimSpace(string(body))
		if len(text) > 200 {
			text = text[:200]
		}
		return nil, &Failure{Message: "target HTTP " + resp.Status + ": " + text}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || strings.HasPrefix(text, "event:") || strings.HasPrefix(text, "data:") {
		last := ""
		for _, line := range strings.Split(text, string([]byte{10})) {
			if strings.HasPrefix(line, "data:") {
				last = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		text = last
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

var agyBridgeSeq = 0

func agyForward(ctx context.Context, server MCPServerSpec, method string, params any) (any, error) {
	agyBridgeSeq++
	resp, err := agyPost(ctx, server, map[string]any{"jsonrpc": "2.0", "id": agyBridgeSeq, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if rawErr, ok := resp["error"].(map[string]any); ok {
		msg, _ := rawErr["message"].(string)
		return nil, &Failure{Message: msg}
	}
	return resp["result"], nil
}

type agyBridgeInitKey struct{}

type agyBridgeState struct {
	initialized map[string]bool
	tools       []any
}

// RunAGYBridge serves one globally registered bridge name over stdio. The
// per-run target file decides where tools/list and tools/call actually go.
func RunAGYBridge(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	state := &agyBridgeState{initialized: map[string]bool{}}
	target := func() *MCPServerSpec {
		file := ""
		if getenv != nil {
			file = getenv("KOMPUTER_AGY_MCP_FILE")
		}
		parsed := LoadAGYTarget(file)
		if parsed == nil {
			return nil
		}
		server, ok := parsed.Servers[name]
		if !ok || server.URL == "" {
			return nil
		}
		return &server
	}
	initTarget := func(server *MCPServerSpec) error {
		if state.initialized[server.URL] {
			return nil
		}
		if _, err := agyForward(ctx, *server, "initialize", map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "komputer-agy-bridge", "version": "0"},
		}); err != nil {
			return err
		}
		_, _ = agyPost(ctx, *server, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		state.initialized[server.URL] = true
		return nil
	}
	list := func() ([]any, error) {
		server := target()
		if server == nil {
			state.tools = nil
			return []any{}, nil
		}
		if err := initTarget(server); err != nil {
			return nil, err
		}
		result, err := agyForward(ctx, *server, "tools/list", map[string]any{})
		if err != nil {
			return nil, err
		}
		obj, _ := result.(map[string]any)
		tools, _ := obj["tools"].([]any)
		if tools == nil {
			tools = []any{}
		}
		state.tools = tools
		return tools, nil
	}
	reader := bufio.NewReader(stdin)
	encoder := json.NewEncoder(stdout)
	for {
		line, err := reader.ReadBytes(10)
		if len(line) > 0 && strings.TrimSpace(string(line)) != "" {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(line, &msg); err == nil && len(msg.ID) > 0 {
				reply := func(result any) {
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
				}
				fail := func(e error) {
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32000, "message": e.Error()}})
				}
				switch msg.Method {
				case "initialize":
					var params struct {
						ProtocolVersion string `json:"protocolVersion"`
					}
					_ = json.Unmarshal(msg.Params, &params)
					version := "2025-06-18"
					for _, v := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
						if params.ProtocolVersion == v {
							version = v
						}
					}
					reply(map[string]any{
						"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
						"serverInfo": map[string]any{"name": "komputer", "version": "0"},
					})
				case "notifications/initialized":
					// Warm the target so tools/list is fast; failures surface there.
					_, _ = list()
				case "ping":
					reply(map[string]any{})
				case "tools/list":
					tools, err := list()
					if err != nil {
						fail(err)
					} else {
						reply(map[string]any{"tools": tools})
					}
				case "tools/call":
					server := target()
					if server == nil {
						reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "Komputer: no tools are available in this turn."}}})
						break
					}
					if err := initTarget(server); err != nil {
						fail(err)
						break
					}
					var params struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					}
					_ = json.Unmarshal(msg.Params, &params)
					var args any
					_ = json.Unmarshal(params.Arguments, &args)
					result, err := agyForward(ctx, *server, "tools/call", map[string]any{"name": params.Name, "arguments": args})
					if err != nil {
						fail(err)
					} else {
						reply(result)
					}
				default:
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
				}
			}
		}
		if err != nil {
			return 0
		}
	}
}
