package backend

import (
	"slices"
	"strings"
	"testing"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/domain"
)

func testMuse() *Muse {
	return NewMuse(config.Muse{
		Bin: "muse", Cwd: "/work", MCPConfig: "/cfg/.mcp.json",
		ModelID: "komputer-muse", Models: []string{"komputer-muse-spark"},
	})
}

func TestMuseArgsAndMCPOverrides(t *testing.T) {
	servers := []MCPServerSpec{{
		Name: "komputer_use", URL: "http://127.0.0.1:9/mcp/secret",
		Headers: map[string]string{"Authorization": "Bearer body-secret"},
	}}
	mcpArgs, env := MuseMCPOverrides(servers)
	if env["KMCP_0_URL"] == "" || env["KMCP_0_H0"] != "Bearer body-secret" {
		t.Fatalf("env: %v", env)
	}
	if strings.Contains(strings.Join(mcpArgs, " "), "body-secret") || strings.Contains(strings.Join(mcpArgs, " "), "secret") && strings.Contains(mcpArgs[1], "127.0.0.1") {
		t.Fatalf("secrets in argv: %q", mcpArgs)
	}
	args := MuseArgs("/tmp/prompt.txt", "/work", SessionRef{Mode: SessionNew, ID: "sid"}, "muse-spark", "high", "", mcpArgs, []string{"/tmp/a.png"}, nil)
	wantHead := []string{"exec", "--json", "--prompt-file", "/tmp/prompt.txt", "--workspace", "/work", "--model", "muse-spark", "--reasoning-effort", "high", "--session-id", "sid"}
	if !slices.Equal(args[:len(wantHead)], wantHead) {
		t.Fatalf("args head: %q", args)
	}
	for _, flag := range []string{"--disable-shell", "--disable-write", "--disable-web-tools", "--image"} {
		if !slices.Contains(args, flag) {
			t.Fatalf("missing %s in %q", flag, args)
		}
	}
}

func TestMuseParserBailAndUsage(t *testing.T) {
	m := testMuse()
	var bailed *Failure
	p := m.NewParser(SessionRef{Mode: SessionResume, ID: "sid"}, nil, func(f *Failure) { bailed = f })
	p.Line(`{"payload_type":"runtime.command.accepted","stream":{"kind":"session"},"sequence":1,"payload":{}}`)
	if bailed == nil || bailed.Type != "session_not_found" {
		t.Fatalf("bail: %+v", bailed)
	}
	p = m.NewParser(SessionRef{Mode: SessionNew, ID: "sid"}, func(ev domain.Event) {}, nil)
	p.Line(`{"payload_type":"run.usage","stream":{"kind":"exec.usage"},"payload":{"kind":"run_usage","models":[{"input_tokens":30,"cached_tokens":10,"output_tokens":12,"reasoning_tokens":6}]}}`)
	p.Line(`{"payload_type":"run.output.delta","stream":{"kind":"session"},"sequence":2,"payload":{"kind":"run_output_delta","text":"hello","run_stream":{"id":"run1"}}}`)
	p.Line(`{"payload_type":"run.terminal.completed","stream":{"kind":"session"},"sequence":3,"payload":{"kind":"run_terminal","text":"hello","run_stream":{"id":"run1"}}}`)
	out, failure := p.Finish(0, "", "")
	if failure != nil || out.Text != "hello" || out.Usage == nil || out.Usage.InputTokens != 20 || out.Usage.ReasoningTokens != 6 {
		t.Fatalf("finish: %+v failure %v", out, failure)
	}
	if got := m.ResolveModel("komputer-muse-spark"); got != "muse-spark" {
		t.Fatalf("resolve model: %q", got)
	}
}
