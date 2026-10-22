package backend

import (
	"slices"
	"testing"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

func testClaude() *Claude {
	return NewClaude(config.Claude{
		Bin: "claude", Cwd: "/work", MCPConfig: "/cfg/.mcp.json",
		ModelID: "komputer-claude", ExtraModels: []string{"komputer-opus"},
		TaskModel: "haiku",
	})
}

func TestClaudeArgsPreserveGoldenShape(t *testing.T) {
	c := testClaude()
	args := c.Args(Turn{
		Session: SessionRef{Mode: SessionNew, ID: "11111111-1111-4111-8111-111111111111"},
		Model:   "opus", Effort: "high", Tools: "", UseMCP: true,
	}, "/tmp/sys.txt", false)
	want := []string{
		"-p", "--dangerously-skip-permissions", "--strict-mcp-config",
		"--mcp-config", "/cfg/.mcp.json",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--append-system-prompt-file", "/tmp/sys.txt",
		"--tools", "", "--model", "opus", "--effort", "high",
		"--session-id", "11111111-1111-4111-8111-111111111111",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args mismatch got=%q want=%q", args, want)
	}
}

func TestClaudeResolveModelAndEffort(t *testing.T) {
	c := testClaude()
	c.cfg.DefaultModel = "fallback"
	c.cfg.DefaultEffort = "medium"
	if got := c.ResolveModel("komputer-sonnet"); got != "sonnet" {
		t.Fatalf("komputer alias: %q", got)
	}
	if got := c.ResolveModel("gpt-4o"); got != "fallback" {
		t.Fatalf("unknown model: %q", got)
	}
	if got := c.ResolveEffort(EffortInput{ReasoningEffort: "minimal"}); got != "low" {
		t.Fatalf("minimal effort: %q", got)
	}
	if got := c.ResolveEffort(EffortInput{ThinkingEnabled: true, ThinkingBudget: 20000}); got != "xhigh" {
		t.Fatalf("budget effort: %q", got)
	}
	if got := c.ResolveEffort(EffortInput{Effort: "bogus"}); got != "medium" {
		t.Fatalf("fallback effort: %q", got)
	}
}

func TestClaudeParserStreamsAndFinishes(t *testing.T) {
	c := testClaude()
	var events []domain.Event
	p := c.NewParser("sid", func(ev domain.Event) { events = append(events, ev) })
	p.Line(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"think"}}}`)
	p.Line(`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"run_command"}}}`)
	p.Line(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}}`)
	p.Line(`{"type":"result","is_error":false,"result":"answer ","session_id":"sid","num_turns":1,"usage":{"input_tokens":3,"output_tokens":4}}`)
	out, failure := p.Finish(0, "", "")
	if failure != nil {
		t.Fatalf("finish: %v", failure)
	}
	if out.Text != "answer" || out.Thinking != "think" || out.Usage.OutputTokens != 4 {
		t.Fatalf("output: %+v", out)
	}
	if !p.Activity() || len(events) != 3 {
		t.Fatalf("activity/events: %v %v", p.Activity(), events)
	}
}

func TestClaudeClassifyMissingSession(t *testing.T) {
	c := testClaude()
	msg := "No conversation found with session ID: 11111111-1111-4111-8111-111111111111"
	if got := c.Classify(msg); got != "session_not_found" {
		t.Fatalf("classify: %q", got)
	}
}
