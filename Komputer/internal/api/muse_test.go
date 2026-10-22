package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type museCall struct {
	Args       []string `json:"args"`
	Prompt     string   `json:"prompt"`
	PromptMode int      `json:"promptMode"`
	Workspace  string   `json:"workspace"`
	MCP        map[string]struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"mcp"`
	EnvKeys []string `json:"envKeys"`
	Images  []struct {
		Ext   string `json:"ext"`
		Bytes int    `json:"bytes"`
	} `json:"images"`
}

func (f fixture) museCalls(t *testing.T) []museCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, "muse-calls.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var calls []museCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), string([]byte{10})) {
		if line == "" {
			continue
		}
		var call museCall
		decode(t, []byte(line), &call)
		calls = append(calls, call)
	}
	return calls
}

func museFixture(t *testing.T, extra map[string]string) fixture {
	t.Helper()
	fakeMuse, err := filepath.Abs(filepath.Join("..", "..", "komputer-api", "test", "fake-muse.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KOMPUTER_BACKENDS":    "claude,muse",
		"KOMPUTER_MUSE_MODELS": "komputer-muse-spark",
		"MUSE_BIN":             fakeMuse,
	}
	for k, v := range extra {
		env[k] = v
	}
	f := newFixtureEnv(t, env)
	body := map[string]any{"mcpServers": map[string]any{"komputer_use": map[string]any{
		"type": "http", "url": "http://127.0.0.1:9/mcp/body-path-token",
		"headers": map[string]any{"Authorization": "Bearer body-secret"},
	}}}
	data, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(f.root, "session", ".mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func chatMuse(t *testing.T, f fixture, body map[string]any, headers map[string]string) (responseObjectChat, http.Header, []byte) {
	t.Helper()
	status, respHeaders, data := f.request(t, "/v1/chat/completions", jsonBody(t, body), headers)
	if status != 200 {
		t.Fatalf("status %d: %s", status, data)
	}
	var out responseObjectChat
	decode(t, data, &out)
	return out, respHeaders, data
}

type responseObjectChat struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func TestGoAPIMuseSessionAndMCPEnv(t *testing.T) {
	f := museFixture(t, map[string]string{})
	// Point the workspace at a deterministic directory inside the fixture by
	// recreating the fixture through env on a second server would be wasteful;
	// assert against the configured workspace the fake recorded instead.
	r, headers, _ := chatMuse(t, f, map[string]any{
		"model": "komputer-muse-spark", "reasoning_effort": "high",
		"messages": []any{map[string]any{"role": "system", "content": "RULE"}, map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-Conversation-Id": "muse-chat"})
	if len(f.calls(t)) != 0 {
		t.Fatal("Claude fake was used for a Muse request")
	}
	calls := f.museCalls(t)
	if len(calls) != 1 {
		t.Fatalf("muse calls: %d", len(calls))
	}
	c := calls[0]
	sid := argAfter(c.Args, "--session-id")
	if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(sid) {
		t.Fatalf("session id: %q", sid)
	}
	if !slices.Contains(c.Args, "--disable-shell") || argAfter(c.Args, "--model") != "muse-spark" || argAfter(c.Args, "--reasoning-effort") != "high" {
		t.Fatalf("args: %q", c.Args)
	}
	if _, err := os.Stat(argAfter(c.Args, "--prompt-file")); !os.IsNotExist(err) {
		t.Fatalf("prompt file not removed: %v", err)
	}
	if runtime.GOOS != "windows" && c.PromptMode != 0o600 {
		t.Fatalf("prompt mode: %o", c.PromptMode)
	}
	if !strings.HasPrefix(c.Prompt, "<system_instructions>") || !strings.Contains(c.Prompt, "RULE") || !strings.HasSuffix(c.Prompt, "hello") {
		t.Fatalf("prompt: %q", c.Prompt)
	}
	body := c.MCP["komputer_use"]
	if body.URL != "http://127.0.0.1:9/mcp/body-path-token" || body.Headers["Authorization"] != "Bearer body-secret" {
		t.Fatalf("mcp interpolation: %+v", body)
	}
	joined := strings.Join(c.Args, " ")
	if strings.Contains(joined, "body-secret") || strings.Contains(joined, "body-path-token") {
		t.Fatalf("secrets in argv: %q", c.Args)
	}
	if headers.Get("X-Komputer-Backend") != "muse" || headers.Get("X-Komputer-Backend-Session") != sid {
		t.Fatalf("backend headers: %v", headers)
	}
	if r.Usage.PromptTokens != 30 || r.Usage.TotalTokens != 42 {
		t.Fatalf("usage: %+v", r.Usage)
	}
	first := r.Choices[0].Message.Content
	r2, _, _ := chatMuse(t, f, map[string]any{
		"model": "komputer-muse-spark",
		"messages": []any{
			map[string]any{"role": "system", "content": "RULE"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": first},
			map[string]any{"role": "user", "content": "again"},
		},
	}, map[string]string{"X-Conversation-Id": "muse-chat"})
	calls = f.museCalls(t)
	if len(calls) != 2 || argAfter(calls[1].Args, "--session-id") != sid || calls[1].Prompt != "again" {
		t.Fatalf("resume calls: %+v", calls)
	}
	if !strings.HasPrefix(r2.Choices[0].Message.Content, "prev=<system_instructions>") || !strings.HasSuffix(r2.Choices[0].Message.Content, "|now=again") {
		t.Fatalf("resume text: %q", r2.Choices[0].Message.Content)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Sessions []struct {
			ClientKey string `json:"client_key"`
			Backend   string `json:"backend"`
			BackendID string `json:"backend_session_id"`
		} `json:"sessions"`
	}
	decode(t, raw, &stored)
	found := false
	for _, s := range stored.Sessions {
		if s.ClientKey == "muse-chat" && s.Backend == "muse" && s.BackendID == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("stored sessions: %s", raw)
	}
}

func TestGoAPIMuseMissingSessionReseeds(t *testing.T) {
	f := museFixture(t, nil)
	h := map[string]string{"X-Conversation-Id": "lost"}
	r1, headers, _ := chatMuse(t, f, map[string]any{"model": "komputer-muse", "messages": []any{map[string]any{"role": "user", "content": "one"}}}, h)
	sid := headers.Get("X-Komputer-Backend-Session")
	if err := os.Remove(filepath.Join(f.state, "muse-"+sid+".json")); err != nil {
		t.Fatal(err)
	}
	r2, headers2, _ := chatMuse(t, f, map[string]any{
		"model": "komputer-muse",
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": r1.Choices[0].Message.Content},
			map[string]any{"role": "user", "content": "two"},
		},
	}, h)
	_ = r2
	if headers2.Get("X-Komputer-Session-Recovered") != "client-history" {
		t.Fatalf("recovery header: %v", headers2)
	}
	if headers2.Get("X-Komputer-Backend-Session") == sid {
		t.Fatal("session id did not rotate")
	}
	calls := f.museCalls(t)
	if !strings.HasPrefix(calls[len(calls)-1].Prompt, "<system_instructions>") || !strings.Contains(calls[len(calls)-1].Prompt, "Earlier conversation for context:") {
		t.Fatalf("reseeded prompt: %q", calls[len(calls)-1].Prompt)
	}
}

func TestGoAPIMuseErrorsImagesAndTools(t *testing.T) {
	f := museFixture(t, map[string]string{"KOMPUTER_MUSE_TASK_MODEL": "muse-mini"})
	postRaw := func(body map[string]any) (int, []byte) {
		status, _, data := f.request(t, "/v1/chat/completions", jsonBody(t, body), nil)
		return status, data
	}
	if status, data := postRaw(map[string]any{"model": "komputer-muse", "messages": []any{map[string]any{"role": "user", "content": "FAIL_AUTH"}}}); status != 502 || !strings.Contains(string(data), "upstream_auth_error") {
		t.Fatalf("auth %d: %s", status, data)
	}
	if status, data := postRaw(map[string]any{"model": "komputer-muse", "messages": []any{map[string]any{"role": "user", "content": "MCP_REQUIRED_DOWN"}}}); status != 503 || !strings.Contains(string(data), "body_unavailable") {
		t.Fatalf("body %d: %s", status, data)
	}
	if status, _ := postRaw(map[string]any{"model": "komputer-muse", "messages": []any{map[string]any{"role": "user", "content": "FAIL_LIMIT"}}}); status != 429 {
		t.Fatalf("limit status %d", status)
	}
	png := "ZmFrZSBwbmcgYnl0ZXM="
	status, data := postRaw(map[string]any{
		"model": "komputer-muse",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + png}},
		}}},
	})
	if status != 200 {
		t.Fatalf("image status %d: %s", status, data)
	}
	calls := f.museCalls(t)
	last := calls[len(calls)-1]
	if len(last.Images) != 1 || last.Images[0].Ext != ".png" || last.Images[0].Bytes != 14 {
		t.Fatalf("images: %+v", last.Images)
	}
	if _, err := os.Stat(argAfter(last.Args, "--image")); !os.IsNotExist(err) {
		t.Fatalf("image temp file remains: %v", err)
	}
	// Client tools through the private harness MCP server.
	tools := []any{openAITool("lookup")}
	status, _, data = f.request(t, "/v1/chat/completions", jsonBody(t, map[string]any{
		"model": "komputer-muse", "tools": tools,
		"messages": []any{map[string]any{"role": "user", "content": "CLIENT_TOOL go"}},
	}), nil)
	if status != 200 {
		t.Fatalf("tools first status %d: %s", status, data)
	}
	var toolResp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	decode(t, data, &toolResp)
	callID := toolResp.Choices[0].Message.ToolCalls[0].ID
	calls = f.museCalls(t)
	last = calls[len(calls)-1]
	if !strings.HasPrefix(last.MCP["komputer_use"].URL, "http://127.0.0.1:") || !strings.Contains(argAfter(last.Args, "--workspace"), "komputer-tools-") {
		t.Fatalf("private mcp/workspace: %+v", last)
	}
	status, _, data = f.request(t, "/v1/chat/completions", jsonBody(t, map[string]any{
		"model": "komputer-muse", "tools": tools,
		"messages": []any{map[string]any{"role": "tool", "tool_call_id": callID, "content": "ok!"}},
	}), nil)
	if status != 200 || !strings.Contains(string(data), "muse tool result:ok!") {
		t.Fatalf("tools continuation %d: %s", status, data)
	}
}
