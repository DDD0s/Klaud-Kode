package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/config"
)

const testKey = "sk-go-rewrite-test-key-0123456789"

type fixture struct {
	base  string
	state string
	root  string
}

func newFixture(t *testing.T) fixture { return newFixtureEnv(t, nil) }

func newFixtureEnv(t *testing.T, extra map[string]string) fixture {
	return newFixtureSetup(t, extra, nil)
}

func newFixtureSetup(t *testing.T, extra map[string]string, setup func(root, sessionDir string)) fixture {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "fake-state")
	sessionDir := filepath.Join(root, "session")
	cwd := filepath.Join(root, "cwd")
	for _, dir := range []string{state, sessionDir, cwd} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keysFile := filepath.Join(root, "KEYS.txt")
	if err := os.WriteFile(keysFile, []byte("api-key: "+testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "komputer-api", "test", "fake-claude.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	env := childenv.FromList(os.Environ())
	for k, v := range map[string]string{
		"KOMPUTER_KEYS_FILE":          keysFile,
		"KOMPUTER_SESSION_DIR":        sessionDir,
		"KOMPUTER_SESSIONS_FILE":      filepath.Join(root, "sessions.json"),
		"KOMPUTER_RESPONSES_FILE":     filepath.Join(root, "responses.json"),
		"KOMPUTER_MCP_CONFIG":         filepath.Join(sessionDir, ".mcp.json"),
		"KOMPUTER_CLAUDE_CWD":         cwd,
		"KOMPUTER_AGY_CWD":            filepath.Join(root, "agy-ws"),
		"KOMPUTER_AGY_REMOTE_MOUNT":   filepath.Join(root, "agy-ws", "remote"),
		"KOMPUTER_MUSE_CWD":           filepath.Join(root, "muse-ws"),
		"KOMPUTER_SKIP_TUNNEL":        "1",
		"KOMPUTER_AUTH_FAIL_DELAY_MS": "0",
		"KOMPUTER_SSE_OPEN_MS":        "10",
		"KOMPUTER_BACKENDS":           "claude",
		"CLAUDE_BIN":                  fake,
		"FAKE_CLAUDE_STATE":           state,
		"FAKE_MUSE_STATE":             state,
		"FAKE_AGY_STATE":              state,
		"HOME":                        root,
		"USERPROFILE":                 root,
		"CUSTOM_SECRET":               "must-not-leak",
	} {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	if setup != nil {
		setup(root, sessionDir)
	}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return fixture{base: ts.URL, state: state, root: root}
}

func (f fixture) request(t *testing.T, path string, body string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, data
}

func decode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

type fakeCall struct {
	Args        []string `json:"args"`
	Prompt      string   `json:"prompt"`
	System      string   `json:"system"`
	Attachments []struct {
		Type string `json:"type"`
	} `json:"attachments"`
	Env struct {
		Leaked []string `json:"leaked"`
	} `json:"env"`
}

func (f fixture) calls(t *testing.T) []fakeCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), string([]byte{10})) {
		if line == "" {
			continue
		}
		var call fakeCall
		decode(t, []byte(line), &call)
		calls = append(calls, call)
	}
	return calls
}

func TestGoAPIClaudeChatStateless(t *testing.T) {
	f := newFixture(t)
	status, _, data := f.request(t, "/v1/chat/completions",
		`{"model":"komputer-claude","messages":[{"role":"user","content":"hello go"}]}`, nil)
	if status != 200 {
		t.Fatalf("status %d: %s", status, data)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	decode(t, data, &resp)
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "echo:hello go" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("response: %s", data)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.TotalTokens != 21 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("calls: %d", len(calls))
	}
	if !strings.Contains(calls[0].System, "You work on the user's computer.") {
		t.Fatalf("system note missing: %q", calls[0].System)
	}
	if len(calls[0].Env.Leaked) != 0 {
		t.Fatalf("child env leaked: %v", calls[0].Env.Leaked)
	}
	if !containsPair(calls[0].Args, "--tools", "") || !contains(calls[0].Args, "--no-session-persistence") {
		t.Fatalf("args: %q", calls[0].Args)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func containsPair(values []string, a, b string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == a && values[i+1] == b {
			return true
		}
	}
	return false
}

func TestGoAPIClaudeChatStreamingAndSessionResume(t *testing.T) {
	f := newFixture(t)
	headers := map[string]string{"X-Conversation-Id": "go-session-1"}
	status, respHeaders, data := f.request(t, "/v1/chat/completions",
		`{"stream":true,"messages":[{"role":"user","content":"first"}]}`, headers)
	if status != 200 {
		t.Fatalf("stream status %d: %s", status, data)
	}
	if !strings.Contains(string(data), "echo:") || !strings.Contains(string(data), "first") || !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("stream body: %s", data)
	}
	if respHeaders.Get("X-Komputer-Mode") != "session" || respHeaders.Get("X-Komputer-Session") != "go-session-1" {
		t.Fatalf("session headers: %v", respHeaders)
	}
	status, _, data = f.request(t, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"echo:first"},{"role":"user","content":"second"}]}`, headers)
	if status != 200 {
		t.Fatalf("resume status %d: %s", status, data)
	}
	if !strings.Contains(string(data), "prev=first|now=second") {
		t.Fatalf("resume response: %s", data)
	}
	calls := f.calls(t)
	if len(calls) != 2 || !contains(calls[1].Args, "--resume") {
		t.Fatalf("resume calls: %+v", calls)
	}
}

func TestGoAPIMessagesAndAuth(t *testing.T) {
	f := newFixture(t)
	status, _, data := f.request(t, "/v1/messages",
		`{"messages":[{"role":"user","content":"hello messages"}]}`, nil)
	if status != 200 {
		t.Fatalf("messages status %d: %s", status, data)
	}
	if !strings.Contains(string(data), "echo:hello messages") || !strings.Contains(string(data), `"type":"message"`) {
		t.Fatalf("messages response: %s", data)
	}
	req, _ := http.NewRequest(http.MethodGet, f.base+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized models status: %d", resp.StatusCode)
	}
}
