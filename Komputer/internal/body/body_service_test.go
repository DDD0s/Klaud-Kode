package body

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Host: "127.0.0.1", Port: 0,
		Roots: map[string]string{"home": root}, DefaultRoot: "home",
		Files: true, Shell: true, Processes: true,
		MaxOutputBytes: 256 * 1024, MaxFileBytes: 8 * 1024 * 1024,
		DefaultTimeoutMs: 30000, ConsoleUTF8: true,
		Token: "0123456789abcdef0123456789abcdef", MCPServers: map[string]NestedServer{},
	}
	service := NewService(cfg, nil)
	t.Cleanup(service.Close)
	return service, root
}

func callText(t *testing.T, s *Service, name string, args map[string]any, out any) ToolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	result, err := s.CallTool(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("%s protocol error: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s tool error: %s", name, result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("%s content blocks: %d", name, len(result.Content))
	}
	var block struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.Content[0], &block); err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(block.Text), out); err != nil {
			t.Fatalf("decode %s result %s: %v", name, block.Text, err)
		}
	}
	return result
}

func TestBodyFilesAndSearch(t *testing.T) {
	service, root := newTestService(t)
	var info map[string]any
	callText(t, service, "system_info", map[string]any{}, &info)
	if info["defaultRoot"] != "home" || info["cwdRoot"] != root {
		t.Fatalf("system info root: %+v", info)
	}
	callText(t, service, "write_file", map[string]any{"path": "hello.txt", "content": "first line" + string([]byte{10}) + "second"}, nil)
	var read struct {
		Content string `json:"content"`
		Total   int    `json:"totalLines"`
	}
	callText(t, service, "read_file", map[string]any{"path": "hello.txt"}, &read)
	if !strings.Contains(read.Content, "second") || read.Total != 2 {
		t.Fatalf("read: %+v", read)
	}
	var window struct {
		Content string `json:"content"`
		Start   int    `json:"startLine"`
		End     int    `json:"endLine"`
		HasMore bool   `json:"hasMore"`
	}
	callText(t, service, "read_file", map[string]any{"path": "hello.txt", "startLine": 2, "maxLines": 1}, &window)
	if window.Content != "second" || window.Start != 2 || window.End != 2 || window.HasMore {
		t.Fatalf("window: %+v", window)
	}
	callText(t, service, "edit_file", map[string]any{"path": "hello.txt", "oldText": "second", "newText": "SECOND"}, nil)
	callText(t, service, "read_file", map[string]any{"path": "hello.txt"}, &read)
	if !strings.Contains(read.Content, "SECOND") {
		t.Fatalf("edited read: %+v", read)
	}
	var list struct {
		Total   int `json:"total"`
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	callText(t, service, "list_directory", map[string]any{"path": "."}, &list)
	if list.Total != 1 || len(list.Entries) != 1 || list.Entries[0].Name != "hello.txt" {
		t.Fatalf("list: %+v", list)
	}
	var found struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	callText(t, service, "search_files", map[string]any{"pattern": "**/*.txt"}, &found)
	if len(found.Matches) != 1 || found.Matches[0].Path != "hello.txt" {
		t.Fatalf("search files: %+v", found)
	}
	callText(t, service, "search_text", map[string]any{"pattern": "SECOND"}, &found)
	if len(found.Matches) != 1 {
		t.Fatalf("search text: %+v", found)
	}
	if _, err := service.roots.Resolve("../escape.txt", "home"); err == nil {
		t.Fatal("path escape was accepted")
	}
	// Binary data comes back as base64 only when a byte window is asked for.
	callText(t, service, "write_file", map[string]any{"path": "raw.bin", "contentBase64": "AAEC"}, nil)
	var bin struct {
		Binary  bool   `json:"binary"`
		Content string `json:"content"`
	}
	callText(t, service, "read_file", map[string]any{"path": "raw.bin", "byteLength": 3}, &bin)
	if !bin.Binary || bin.Content != "AAEC" {
		t.Fatalf("binary read: %+v", bin)
	}
}

func TestBodyRunCommandAndProcesses(t *testing.T) {
	service, _ := newTestService(t)
	run := func(command, shell string, timeout int) CommandResult {
		var out CommandResult
		callText(t, service, "run_command", map[string]any{"command": command, "shell": shell, "timeoutMs": timeout}, &out)
		return out
	}
	if runtime.GOOS == "windows" {
		out := run("echo BODY_OK", "default", 10000)
		if out.ExitCode == nil || *out.ExitCode != 0 || !strings.Contains(out.Stdout, "BODY_OK") {
			t.Fatalf("cmd: %+v", out)
		}
		out = run("Write-Output PS_OK; $x = 1", "powershell", 20000)
		if out.ExitCode == nil || *out.ExitCode != 0 || !strings.Contains(out.Stdout, "PS_OK") {
			t.Fatalf("powershell: %+v", out)
		}
		if ShellPath("pwsh") != "" {
			out = run("Write-Output ('x' * 3)", "pwsh", 20000)
			if out.ExitCode == nil || *out.ExitCode != 0 || !strings.Contains(out.Stdout, "xxx") {
				t.Fatalf("pwsh: %+v", out)
			}
		}
		out = run("ping -n 5 127.0.0.1 >nul", "default", 150)
		if !out.TimedOut {
			t.Fatalf("timeout: %+v", out)
		}
	} else {
		out := run("printf BODY_OK", "default", 10000)
		if out.ExitCode == nil || *out.ExitCode != 0 || out.Stdout != "BODY_OK" {
			t.Fatalf("sh: %+v", out)
		}
		out = run("sleep 2", "default", 150)
		if !out.TimedOut {
			t.Fatalf("timeout: %+v", out)
		}
	}

	var started map[string]any
	command, shell := interactiveNodeCommand()
	callText(t, service, "start_process", map[string]any{"command": command, "shell": shell}, &started)
	id, _ := started["processId"].(string)
	if id == "" {
		t.Fatalf("start: %+v", started)
	}
	callText(t, service, "write_process", map[string]any{"processId": id, "text": "hello-process"}, nil)
	cursor := 0
	seen := ""
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(seen, "GOT:hello-process") {
		var read ProcessRead
		callText(t, service, "read_process", map[string]any{"processId": id, "cursor": cursor}, &read)
		seen += read.Output
		cursor = read.Cursor
		if !read.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(seen, "GOT:hello-process") {
		t.Fatalf("interactive output: %q", seen)
	}
	callText(t, service, "stop_process", map[string]any{"processId": id}, nil)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var read ProcessRead
		callText(t, service, "read_process", map[string]any{"processId": id, "cursor": cursor}, &read)
		if !read.Running {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func interactiveNodeCommand() (string, string) {
	q := string([]byte{34})
	script := "process.stdin.on(" + q + "data" + q + ",d=>process.stdout.write(" + q + "GOT:" + q + "+d))"
	if runtime.GOOS == "windows" {
		script = "process.stdin.on('data',d=>process.stdout.write('GOT:'+d))"
		return "node -e " + q + script + q, "pwsh"
	}
	sq := string([]byte{39})
	return "node -e " + sq + script + sq, "sh"
}

func TestBodyMCPHTTPAndStdio(t *testing.T) {
	service, _ := newTestService(t)
	ts := httptest.NewServer(service.Handler())
	defer ts.Close()
	post := func(body any, token string) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		if len(data) > 0 {
			_ = json.Unmarshal(data, &decoded)
		}
		return resp.StatusCode, decoded
	}
	if status, _ := post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, "wrong-token-0123456789"); status != 401 {
		t.Fatalf("unauthorized status %d", status)
	}
	status, body := post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}}, service.cfg.Token)
	if status != 200 {
		t.Fatalf("initialize status %d: %v", status, body)
	}
	result, _ := body["result"].(map[string]any)
	serverInfo, _ := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "komputer" {
		t.Fatalf("server info: %v", body)
	}
	status, body = post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, service.cfg.Token)
	result, _ = body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if status != 200 || len(tools) == 0 {
		t.Fatalf("tools/list %d: %v", status, body)
	}
	status, body = post(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "system_info", "arguments": map[string]any{},
	}}, service.cfg.Token)
	if status != 200 {
		t.Fatalf("tools/call status %d: %v", status, body)
	}
	result, _ = body["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tools/call result: %v", body)
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}

	nl := string([]byte{10})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, nl) + nl
	var out bytes.Buffer
	if err := service.serveStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "komputer") || !strings.Contains(out.String(), "system_info") {
		t.Fatalf("stdio output: %s", out.String())
	}
}
