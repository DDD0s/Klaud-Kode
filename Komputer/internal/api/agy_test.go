package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var builtKlaud struct {
	once sync.Once
	path string
	err  error
}

func klaudBinary(t *testing.T) string {
	t.Helper()
	builtKlaud.once.Do(func() {
		dir, err := os.MkdirTemp("", "klaud-kode-test-bin")
		if err != nil {
			builtKlaud.err = err
			return
		}
		name := "klaud-kode"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		builtKlaud.path = filepath.Join(dir, name)
		module, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			builtKlaud.err = err
			return
		}
		cmd := exec.Command("go", "build", "-o", builtKlaud.path, "./cmd/klaud-kode")
		cmd.Dir = module
		out, err := cmd.CombinedOutput()
		if err != nil {
			builtKlaud.err = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if builtKlaud.err != nil {
		t.Fatal(builtKlaud.err)
	}
	return builtKlaud.path
}

type fakeBodySeen struct {
	method  string
	auth    string
	session string
}

type fakeBodyServer struct {
	url      string
	platform string
	root     string
	mu       sync.Mutex
	seen     []fakeBodySeen
	server   *httptest.Server
}

func newFakeBodyServer(t *testing.T, platform, root string) *fakeBodyServer {
	t.Helper()
	f := &fakeBodyServer{platform: platform, root: root}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
				Name            string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.seen = append(f.seen, fakeBodySeen{method: req.Method, auth: r.Header.Get("Authorization"), session: r.Header.Get("Mcp-Session-Id")})
		f.mu.Unlock()
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "body-session-1")
		reply := func(result any) {
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			_, _ = w.Write([]byte("event: message" + string([]byte{10}) + "data: " + string(data) + string([]byte{10, 10})))
		}
		rpcError := func(msg string) {
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": msg}})
			_, _ = w.Write([]byte("event: message" + string([]byte{10}) + "data: " + string(data) + string([]byte{10, 10})))
		}
		switch req.Method {
		case "initialize":
			version := req.Params.ProtocolVersion
			if version == "" {
				version = "2025-06-18"
			}
			reply(map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake-body", "version": "0"}})
		case "tools/list":
			reply(map[string]any{"tools": []any{map[string]any{"name": "system_info", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}})
		case "tools/call":
			if platform == "" || req.Params.Name != "system_info" {
				rpcError("Method not found")
				return
			}
			text, _ := json.Marshal(map[string]any{"platform": platform, "cwdRoot": map[string]any{"path": root}})
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}})
		default:
			rpcError("Method not found")
		}
	}))
	f.url = f.server.URL + "/mcp"
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBodyServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.seen))
	for _, s := range f.seen {
		out = append(out, s.method)
	}
	return out
}

func agyFixture(t *testing.T, platform string, extra map[string]string) (fixture, *fakeBodyServer) {
	t.Helper()
	fakeBody := newFakeBodyServer(t, platform, "/caller/work")
	fake, err := filepath.Abs(filepath.Join("..", "..", "komputer-api", "test", "fake-agy.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KOMPUTER_BACKENDS":         "claude,antigravity",
		"KOMPUTER_AGY_REMOTE_TOOLS": "commands",
		"AGY_BIN":                   fake,
		"KLAUD_KODE_BIN":            klaudBinary(t),
	}
	for k, v := range extra {
		env[k] = v
	}
	var root string
	f := newFixtureSetup(t, env, func(fixtureRoot, sessionDir string) {
		root = fixtureRoot
		// Read at server construction, when AGY Init registers its bridges.
		cfg := map[string]any{"mcpServers": map[string]any{"komputer_use": map[string]any{
			"type": "http", "url": fakeBody.url,
			"headers": map[string]any{"Authorization": "Bearer body-secret"},
		}}}
		// The fixture does not expose its root before setup runs, so derive it
		// from the session directory it hands us.
		_ = root
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(sessionDir, ".mcp.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		agyCwd := filepath.Join(filepath.Dir(sessionDir), "agy-ws")
		_ = agyCwd
	})
	return f, fakeBody
}

type fakeAGYCall struct {
	Args       []string       `json:"args"`
	Prompt     string         `json:"prompt"`
	Cwd        string         `json:"cwd"`
	PathHead   string         `json:"pathHead"`
	BridgeFile string         `json:"bridgeFile"`
	EnvKeys    []string       `json:"envKeys"`
	MCPTools   map[string]any `json:"mcpTools"`
	Target     struct {
		Policy map[string]any `json:"policy"`
		Remote map[string]any `json:"remote"`
	} `json:"target"`
}

func (f fixture) agyCalls(t *testing.T) []fakeAGYCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, "agy-calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeAGYCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), string([]byte{10})) {
		if line == "" {
			continue
		}
		var c fakeAGYCall
		decode(t, []byte(line), &c)
		calls = append(calls, c)
	}
	return calls
}

func TestGoAPIAGYSessionAndBridge(t *testing.T) {
	f, bodyServer := agyFixture(t, "", map[string]string{
		"KOMPUTER_AGY_CWD": "", // replaced below through fixture root defaulting
	})
	// The fixture's agy cwd is derived from its temp root by config; find calls.
	post := func(body map[string]any, headers map[string]string) (responseObjectChat, http.Header) {
		status, respHeaders, data := f.request(t, "/v1/chat/completions", jsonBody(t, body), headers)
		if status != 200 {
			t.Fatalf("status %d: %s", status, data)
		}
		var out responseObjectChat
		decode(t, data, &out)
		return out, respHeaders
	}
	r1, headers := post(map[string]any{
		"model": "komputer-gemini-4-argon", "reasoning_effort": "high",
		"messages": []any{map[string]any{"role": "system", "content": "RULE"}, map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-Conversation-Id": "agy-chat"})
	calls := f.agyCalls(t)
	if len(calls) != 1 {
		t.Fatalf("agy calls: %d", len(calls))
	}
	c := calls[0]
	if len(f.calls(t)) != 0 {
		t.Fatal("Claude fake was used for an AGY model")
	}
	wantArgs := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-4-argon", "--effort", "high"}
	if strings.Join(c.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("args: %q", c.Args)
	}
	if !strings.HasPrefix(c.Prompt, "<system_instructions>") || !strings.HasSuffix(c.Prompt, "RULE"+string([]byte{10})+"</system_instructions>"+string([]byte{10, 10})+"hello") {
		t.Fatalf("prompt: %q", c.Prompt)
	}
	realBody, _ := filepath.EvalSymlinks(filepath.Join(f.root, ".komputer", "workspaces"))
	_ = realBody
	if !strings.HasSuffix(filepath.ToSlash(c.Cwd), "/body") {
		t.Fatalf("cwd: %q", c.Cwd)
	}
	if got := c.Target.Policy["mode"]; got != "remote" {
		t.Fatalf("policy: %+v", c.Target.Policy)
	}
	remoteServer, _ := c.Target.Remote["server"].(map[string]any)
	if remoteServer["url"] != bodyServer.url {
		t.Fatalf("remote server: %+v", c.Target.Remote)
	}
	if !strings.HasSuffix(filepath.ToSlash(c.PathHead), "/shim") {
		t.Fatalf("PATH head: %q", c.PathHead)
	}
	if _, err := os.Stat(c.BridgeFile); !os.IsNotExist(err) {
		t.Fatalf("bridge file remains: %v", err)
	}
	for _, key := range c.EnvKeys {
		if key == "CUSTOM_SECRET" || strings.HasPrefix(key, "KOMPUTER_") && key != "KOMPUTER_AGY_MCP_FILE" {
			t.Fatalf("leaked env key %s", key)
		}
	}
	globalRaw, err := os.ReadFile(filepath.Join(f.root, ".gemini", "config", "mcp_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var global struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	decode(t, globalRaw, &global)
	entry := global.Servers["komputer_use"]
	if entry.Command != klaudBinary(t) || strings.Join(entry.Args, " ") != "bridge komputer_use" {
		t.Fatalf("bridge entry: %+v", entry)
	}
	if strings.Contains(string(globalRaw), "body-secret") || strings.Contains(string(globalRaw), "127.0.0.1") {
		t.Fatalf("global config leaks target: %s", globalRaw)
	}
	conversation := headers.Get("X-Komputer-Backend-Session")
	if headers.Get("X-Komputer-Backend") != "antigravity" || conversation == "" {
		t.Fatalf("headers: %v", headers)
	}
	first := r1.Choices[0].Message.Content
	if !strings.HasPrefix(first, "agy:<system_instructions>") {
		t.Fatalf("first text: %q", first)
	}
	r2, _ := post(map[string]any{
		"model": "komputer-gemini-4-argon",
		"messages": []any{
			map[string]any{"role": "system", "content": "RULE"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": first},
			map[string]any{"role": "user", "content": "second"},
		},
	}, map[string]string{"X-Conversation-Id": "agy-chat"})
	calls = f.agyCalls(t)
	if len(calls) != 2 || argAfter(calls[1].Args, "--conversation") != conversation || calls[1].Prompt != "second" {
		t.Fatalf("resume calls: %+v", calls)
	}
	if !strings.HasPrefix(r2.Choices[0].Message.Content, "prev=<system_instructions>") || !strings.HasSuffix(r2.Choices[0].Message.Content, "|now=second") {
		t.Fatalf("resume text: %q", r2.Choices[0].Message.Content)
	}
	_ = bodyServer
}

func TestGoAPIAGYClientTools(t *testing.T) {
	f, _ := agyFixture(t, "", nil)
	tools := []any{openAITool("lookup")}
	status, _, data := f.request(t, "/v1/chat/completions", jsonBody(t, map[string]any{
		"model":    "komputer-agy",
		"messages": []any{map[string]any{"role": "user", "content": "CLIENT_TOOL"}},
		"tools":    tools,
	}), nil)
	if status != 200 {
		t.Fatalf("first status %d: %s", status, data)
	}
	var first struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	decode(t, data, &first)
	callID := first.Choices[0].Message.ToolCalls[0].ID
	status, _, data = f.request(t, "/v1/chat/completions", jsonBody(t, map[string]any{
		"model": "komputer-agy", "tools": tools,
		"messages": []any{map[string]any{"role": "tool", "tool_call_id": callID, "content": "from agy"}},
	}), nil)
	if status != 200 || !strings.Contains(string(data), "agy tool result:from agy") {
		t.Fatalf("continuation %d: %s", status, data)
	}
	if calls := f.agyCalls(t); len(calls) != 1 {
		t.Fatalf("agy processes: %d", len(calls))
	}
}
