package backend

import (
	"slices"
	"strings"
	"testing"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

func testAntigravity() *Antigravity {
	return NewAntigravity(config.Antigravity{
		Bin: "agy", Cwd: "/work", ModelID: "komputer-agy",
		Models: []string{"komputer-gemini-4-argon"}, Efforts: []string{"low", "medium", "high"},
		Guard: "warn", BodyMCP: "komputer_use", Home: "/home/u",
	})
}

func TestAGYResolveModelEffortAndArgs(t *testing.T) {
	a := testAntigravity()
	if got := a.ResolveModel("komputer-agy-claude-opus-5-5"); got != "claude-opus-5-5" {
		t.Fatalf("model: %q", got)
	}
	if got := a.ResolveModel("komputer-gemini-4-argon"); got != "gemini-4-argon" {
		t.Fatalf("gemini model: %q", got)
	}
	if got := a.ResolveEffort(EffortInput{Model: "komputer-gemini-4-argon", ReasoningEffort: "xhigh"}); got != "high" {
		t.Fatalf("clamped effort: %q", got)
	}
	if got := a.ResolveEffort(EffortInput{Model: "claude-opus-5-5", ReasoningEffort: "xhigh"}); got != "xhigh" {
		t.Fatalf("non-gemini effort: %q", got)
	}
	args := a.Args(Turn{Model: "gemini-3.8-flash-low", Effort: "high", Session: SessionRef{Mode: SessionResume, ID: "conv-1"}})
	if slices.Contains(args, "--effort") || slices.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("args should omit suffixed effort and skip flag in settings mode: %q", args)
	}
	a.cfg.Permissions = "skip"
	args = a.Args(Turn{Model: "gemini-4-argon"})
	if !slices.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("skip args: %q", args)
	}
}

func TestAGYParserRealEventFormat(t *testing.T) {
	a := testAntigravity()
	var events []domain.Event
	p := a.NewParser(SessionRef{Mode: SessionNew}, func(ev domain.Event) { events = append(events, ev) }, nil)
	p.Line(`{"event":"init","conversation_id":"conv-9","init":{"permission_mode":"default","tools":["run_command"]}}`)
	p.Line(`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"hel"}}`)
	p.Line(`{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"lo"}}`)
	p.Line(`{"event":"result","result":{"conversation_id":"conv-9","status":"SUCCESS","response":"hello","num_turns":1,"usage":{"input_tokens":20,"cache_read_tokens":5,"output_tokens":9,"thinking_tokens":3}}}`)
	out, failure := p.Finish(0, "", "")
	if failure != nil || out.Text != "hello" || out.SessionID != "conv-9" {
		t.Fatalf("finish: %+v %v", out, failure)
	}
	if out.Usage == nil || out.Usage.InputTokens != 20 || out.Usage.CacheReadInputTokens != 5 || out.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage: %+v", out.Usage)
	}
	if len(events) != 2 {
		t.Fatalf("events: %v", events)
	}
}

func TestAGYParserMissingConversationBails(t *testing.T) {
	a := testAntigravity()
	var bailed *Failure
	p := a.NewParser(SessionRef{Mode: SessionResume, ID: "old"}, nil, func(f *Failure) { bailed = f })
	p.Line(`{"event":"init","conversation_id":"new","init":{"permission_mode":"default"}}`)
	if bailed == nil || bailed.Type != "session_not_found" || bailed.Activity {
		t.Fatalf("bail: %+v", bailed)
	}
}

func TestAGYHookRemoteRewriteAndIslandDeny(t *testing.T) {
	remote := &AGYPolicy{Mode: "remote", Mount: "/ws/body", Root: "/home/u/project", Files: true, Commands: true}
	input := map[string]any{"toolCall": map[string]any{"name": "read_file", "args": map[string]any{"Path": "/home/u/project/a.txt"}}}
	verdict := DecideForPolicy(input, remote, "/home/u")
	overwrite, _ := verdict["overwrite"].(map[string]any)
	if verdict["decision"] != "allow" || overwrite["Path"] != "/ws/body/a.txt" {
		t.Fatalf("rewrite: %v", verdict)
	}
	brainPath := map[string]any{"toolCall": map[string]any{"name": "read_file", "args": map[string]any{"Path": "/etc/hostname"}}}
	verdict = DecideForPolicy(brainPath, remote, "/home/u")
	if verdict["decision"] != "deny" || !strings.Contains(toString(verdict["reason"]), "/ws/body") {
		t.Fatalf("outside-mount deny: %v", verdict)
	}
	noFiles := &AGYPolicy{Mode: "remote", Mount: "/ws/body", Root: "/home/u/project", Files: false, Commands: true}
	if verdict = DecideForPolicy(input, noFiles, "/home/u"); verdict["decision"] != "deny" {
		t.Fatalf("files-disabled deny: %v", verdict)
	}
	verdict = DecideHook(map[string]any{"toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "ls"}}}, nil, "/home/u")
	if verdict["decision"] != "deny" || !strings.Contains(toString(verdict["reason"]), "run_command") {
		t.Fatalf("island deny: %v", verdict)
	}
	schemaPath := "/home/u/.gemini/antigravity-cli/mcp/komputer_use/get_system_info.json"
	verdict = DecideHook(map[string]any{"toolCall": map[string]any{"name": "read_file", "args": map[string]any{"Path": schemaPath}}}, nil, "/home/u")
	if verdict != nil {
		t.Fatalf("schema read: %v", verdict)
	}
}

func toString(v any) string { s, _ := v.(string); return s }

func TestAGYShimHelpers(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"-c", "ls -la"}, "ls -la"},
		{[]string{"--norc", "-lc", "echo hi", "bash"}, "echo hi"},
		{[]string{"-o", "pipefail", "-c", "x"}, "x"},
		{[]string{"script.sh"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := AGYCommandOf(c.argv); got != c.want {
			t.Fatalf("AGYCommandOf(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
	remote := &AGYRemote{Mount: "/ws/remote", Root: "/home/user/project", Shell: "bash"}
	if got := AGYToRemote("cat /ws/remote/a.txt > /ws/remote/b", remote); got != "cat /home/user/project/a.txt > /home/user/project/b" {
		t.Fatalf("toRemote: %q", got)
	}
	winRoot := &AGYRemote{Mount: "/ws", Root: "C:\\Users\\u"}
	if got := AGYToRemote("dir C:\\x", winRoot); got != "dir C:\\x" {
		t.Fatalf("windows root rewritten: %q", got)
	}
	if got := AGYRemoteCwd("/ws/remote/src/lib", remote); got != "src/lib" {
		t.Fatalf("remoteCwd: %q", got)
	}
	if got := AGYRemoteCwd("/elsewhere", remote); got != "." {
		t.Fatalf("remoteCwd outside mount: %q", got)
	}
	mapper := agyOutputMapper(remote)
	chunks := []string{"/home/user/pro", "ject/a" + string([]byte{10}) + "/home/u", "ser/project"}
	joined := ""
	for _, c := range chunks {
		joined += mapper.Push(c)
	}
	joined += mapper.Flush()
	if want := "/ws/remote/a" + string([]byte{10}) + "/ws/remote"; joined != want {
		t.Fatalf("output mapping: %q want %q", joined, want)
	}
}
