package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

var claudeEffortLevels = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

var (
	claudeIDPattern    = regexp.MustCompile(`(?i)^(claude-|opus|sonnet|haiku|fable|mythos)`)
	komputerIDPattern  = regexp.MustCompile(`(?i)^komputer-(.+)$`)
	missingSessionTail = regexp.MustCompile(`(?i)No conversation found with session ID:\s*[0-9a-f-]{36}\s*$`)
)

// Claude is the Claude Code backend. Its argv, stdin and stream parsing
// preserve the behavior pinned by the JavaScript regression tests.
type Claude struct {
	cfg config.Claude
	ids []string
}

// NewClaude builds the Claude backend from configuration.
func NewClaude(cfg config.Claude) *Claude {
	ids := []string{cfg.ModelID}
	for _, id := range cfg.ExtraModels {
		if id != cfg.ModelID {
			ids = append(ids, id)
		}
	}
	return &Claude{cfg: cfg, ids: ids}
}

func (c *Claude) Name() string           { return "claude" }
func (c *Claude) Bin() string            { return c.cfg.Bin }
func (c *Claude) Cwd() string            { return c.cfg.Cwd }
func (c *Claude) IDs() []string          { return append([]string{}, c.ids...) }
func (c *Claude) DefaultID() string      { return c.cfg.ModelID }
func (c *Claude) TaskModel() string      { return c.cfg.TaskModel }
func (c *Claude) AssignsSessionID() bool { return false }
func (c *Claude) InlinesSystem() bool    { return false }
func (c *Claude) NewSessionID() string   { return domain.NewUUID() }

// Owns reports whether a requested model id names a Claude model.
func (c *Claude) Owns(id string) bool { return claudeIDPattern.MatchString(id) }

// ResolveModel maps a requested model id to the CLI's --model value.
func (c *Claude) ResolveModel(requested string) string {
	id := strings.TrimSpace(requested)
	if id == "" || id == c.cfg.ModelID {
		return c.cfg.DefaultModel
	}
	if m := komputerIDPattern.FindStringSubmatch(id); m != nil {
		return m[1]
	}
	if claudeIDPattern.MatchString(id) {
		return id
	}
	return c.cfg.DefaultModel
}

// ResolveEffort maps request effort hints to a Claude --effort value.
func (c *Claude) ResolveEffort(input EffortInput) string {
	e := strings.ToLower(strings.TrimSpace(input.Value()))
	if e == "minimal" || e == "none" {
		e = "low"
	}
	if e == "" && input.ThinkingEnabled {
		switch {
		case input.ThinkingBudget >= 32000:
			e = "max"
		case input.ThinkingBudget >= 16000:
			e = "xhigh"
		default:
			e = "high"
		}
	}
	if claudeEffortLevels[e] {
		return e
	}
	return c.cfg.DefaultEffort
}

// ChildEnv builds the filtered child environment.
func (c *Claude) ChildEnv(source map[string]string, cwd string) map[string]string {
	return childenv.BuildClaude(source, cwd)
}

// Args renders the Claude Code argv for one turn.
func (c *Claude) Args(turn Turn, sysFile string, streamInput bool) []string {
	// The prompt goes over stdin: no argv size limit, not visible in `ps`, and a
	// message starting with "-" can never be parsed as a CLI flag.
	args := []string{"-p", "--dangerously-skip-permissions", "--strict-mcp-config"}
	if turn.UseMCP {
		mcp := turn.MCPConfig
		if mcp == "" {
			mcp = c.cfg.MCPConfig
		}
		args = append(args, "--mcp-config", mcp)
	}
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--append-system-prompt-file", sysFile,
	)
	if c.cfg.SystemPromptFile != "" {
		args = append(args, "--system-prompt-file", c.cfg.SystemPromptFile)
	}
	if streamInput {
		args = append(args, "--input-format", "stream-json")
	}
	if turn.Tools != "default" {
		args = append(args, "--tools", turn.Tools)
	}
	if turn.Model != "" {
		args = append(args, "--model", turn.Model)
	}
	if turn.Effort != "" {
		args = append(args, "--effort", turn.Effort)
	}
	switch turn.Session.Mode {
	case SessionResume:
		args = append(args, "--resume", turn.Session.ID)
	case SessionNew:
		args = append(args, "--session-id", turn.Session.ID)
	default:
		args = append(args, "--no-session-persistence")
	}
	return args
}

// Prepare writes the appended system prompt and renders argv/stdin.
func (c *Claude) Prepare(turn Turn) (Prepared, error) {
	f, err := os.CreateTemp("", "komputer-sys-*.txt")
	if err != nil {
		return Prepared{}, err
	}
	sysFile := f.Name()
	if _, err := f.WriteString(turn.SystemPrompt); err != nil {
		f.Close()
		os.Remove(sysFile)
		return Prepared{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(sysFile)
		return Prepared{}, err
	}
	streamInput := len(turn.Attachments) > 0
	stdin := turn.Prompt
	if streamInput {
		content := []json.RawMessage{marshalRaw(map[string]any{"type": "text", "text": turn.Prompt})}
		content = append(content, turn.Attachments...)
		stdin = string(marshalRaw(map[string]any{
			"type":               "user",
			"message":            map[string]any{"role": "user", "content": content},
			"parent_tool_use_id": nil,
			"session_id":         "",
		})) + string(rune(10))
	}
	return Prepared{
		Args:    c.Args(turn, sysFile, streamInput),
		Stdin:   stdin,
		Cwd:     c.cfg.Cwd,
		Cleanup: func() { _ = os.Remove(sysFile) },
	}, nil
}

func marshalRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Classify maps a Claude failure message to a public error type.
func (c *Claude) Classify(message string) string {
	if missingSessionTail.MatchString(message) {
		return "session_not_found"
	}
	return ""
}

type claudeStreamEvent struct {
	Type         string `json:"type"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

type claudeEnvelope struct {
	Type            string             `json:"type"`
	ParentToolUseID json.RawMessage    `json:"parent_tool_use_id"`
	NumTurns        int                `json:"num_turns"`
	Event           *claudeStreamEvent `json:"event"`
	IsError         bool               `json:"is_error"`
	Result          json.RawMessage    `json:"result"`
	Error           json.RawMessage    `json:"error"`
	SessionID       string             `json:"session_id"`
	Usage           *domain.Usage      `json:"usage"`
}

type claudeParser struct {
	sink        domain.Sink
	sessionID   string
	result      *claudeEnvelope
	emittedText bool
	thinking    strings.Builder
	activity    bool
}

// NewParser creates the stream-json parser for one Claude run.
func (c *Claude) NewParser(sessionID string, sink domain.Sink) Parser {
	return &claudeParser{sink: sink, sessionID: sessionID}
}

func (p *claudeParser) Activity() bool { return p.activity }
func (p *claudeParser) Stderr(string)  {}

func (p *claudeParser) Line(line string) {
	var ev claudeEnvelope
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	// Never automatically replay a turn once inference/tool activity began,
	// including subagent events that are not forwarded to the client UI.
	switch ev.Type {
	case "stream_event", "assistant", "user", "tool_use", "tool_result":
		p.activity = true
	}
	if ev.Type == "result" && ev.NumTurns > 0 {
		p.activity = true
	}
	if ev.Type == "stream_event" && ev.Event != nil && string(ev.ParentToolUseID) == "null" || ev.Type == "stream_event" && ev.Event != nil && len(ev.ParentToolUseID) == 0 {
		e := ev.Event
		switch e.Type {
		case "content_block_start":
			if e.ContentBlock == nil {
				return
			}
			switch e.ContentBlock.Type {
			case "text":
				if p.emittedText {
					p.sink.Emit(domain.EventText, string(rune(10))+string(rune(10)))
				}
			case "tool_use", "server_tool_use":
				p.sink.Emit(domain.EventTool, e.ContentBlock.Name)
			}
		case "content_block_delta":
			if e.Delta == nil {
				return
			}
			if e.Delta.Type == "text_delta" && e.Delta.Text != "" {
				p.emittedText = true
				p.sink.Emit(domain.EventText, e.Delta.Text)
			} else if e.Delta.Type == "thinking_delta" && e.Delta.Thinking != "" {
				p.thinking.WriteString(e.Delta.Thinking)
				p.sink.Emit(domain.EventThinking, e.Delta.Thinking)
			}
		}
	} else if ev.Type == "result" {
		evCopy := ev
		p.result = &evCopy
	}
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func (p *claudeParser) Finish(code int, signal string, stderr string) (domain.Output, *Failure) {
	if p.result != nil {
		if p.result.IsError {
			msg := rawText(p.result.Result)
			if msg == "" {
				msg = rawText(p.result.Error)
			}
			if msg == "" {
				msg = "claude is_error"
			}
			return domain.Output{}, &Failure{Message: msg}
		}
		sid := p.result.SessionID
		if sid == "" {
			sid = p.sessionID
		}
		return domain.Output{
			Text:      strings.TrimSpace(rawText(p.result.Result)),
			Thinking:  p.thinking.String(),
			SessionID: sid,
			Usage:     p.result.Usage,
		}, nil
	}
	tail := stderr
	if len(tail) > 800 {
		tail = tail[len(tail)-800:]
	}
	msg := fmt.Sprintf("claude exit %d", code)
	if signal != "" {
		msg += " signal=" + signal
	}
	return domain.Output{}, &Failure{Message: msg + ": " + tail}
}
