package backend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

var museEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true, "ultra": true,
}

var museImageExt = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg",
	"image/gif": "gif", "image/webp": "webp",
}

var (
	museIDPattern       = regexp.MustCompile(`(?i)^(komputer-)?muse(-|$)`)
	komputerPrefix      = regexp.MustCompile(`(?i)^komputer-`)
	workspaceRefusal    = regexp.MustCompile(`(?i)refusing to resume in workspace`)
	museAuthPattern     = regexp.MustCompile(`(?i)missing meta credentials|muse login|META_API_KEY`)
	museBodyDownPattern = regexp.MustCompile(`(?i)Required MCP server .* failed during startup`)
)

// MuseMCPOverrides renders MCP servers as -c config values. URLs and header
// values stay out of argv: muse interpolates whole-value variable
// references from its environment.
func MuseMCPOverrides(servers []MCPServerSpec) (args []string, env map[string]string) {
	envRef := func(name string) string { return string([]byte{36, 123}) + name + "}" }
	env = map[string]string{}
	for i, s := range servers {
		if s.URL == "" {
			continue // stdio servers are not translated (documented)
		}
		urlVar := fmt.Sprintf("KMCP_%d_URL", i)
		env[urlVar] = s.URL
		headerNames := make([]string, 0, len(s.Headers))
		for h := range s.Headers {
			headerNames = append(headerNames, h)
		}
		sort.Strings(headerNames)
		headers := map[string]string{}
		for j, h := range headerNames {
			hv := fmt.Sprintf("KMCP_%d_H%d", i, j)
			env[hv] = s.Headers[h]
			headers[h] = envRef(hv)
		}
		value := map[string]any{
			"transport": "streamable_http", "url": envRef(urlVar), "mode": "required",
		}
		if len(headers) > 0 {
			value["headers"] = headers
		}
		encoded, _ := json.Marshal(value)
		args = append(args, "-c", "mcp_servers."+s.Name+"="+string(encoded))
	}
	return args, env
}
func MuseArgs(promptFile, workspace string, session SessionRef, model, effort, tools string, mcpArgs, images, extraArgs []string) []string {
	args := []string{"exec", "--json", "--prompt-file", promptFile, "--workspace", workspace}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if session.Mode == SessionNone {
		args = append(args, "--no-session-log")
	} else {
		args = append(args, "--session-id", session.ID)
	}
	args = append(args, "--disable-approval", "--disable-request-user-input", "--no-foreign-personal-context")
	if tools == "" {
		args = append(args, "--disable-shell", "--disable-write", "--disable-web-tools")
	}
	args = append(args, mcpArgs...)
	for _, img := range images {
		args = append(args, "--image", img)
	}
	args = append(args, extraArgs...)
	return args
}

type museUsageModel struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
	CachedTokens    int `json:"cached_tokens"`
}

// MuseUsage maps muse's per-model usage records to domain usage.
func MuseUsage(models []museUsageModel) *domain.Usage {
	if len(models) == 0 {
		return nil
	}
	u := &domain.Usage{}
	for _, m := range models {
		u.InputTokens += m.InputTokens
		u.OutputTokens += m.OutputTokens
		u.ReasoningTokens += m.ReasoningTokens
		u.CacheReadInputTokens += m.CachedTokens
	}
	u.InputTokens -= u.CacheReadInputTokens
	if u.InputTokens < 0 {
		u.InputTokens = 0
	}
	return u
}

// Muse is the Muse Code CLI backend.
type Muse struct {
	cfg config.Muse
	ids []string
}

// NewMuse builds the Muse backend.
func NewMuse(cfg config.Muse) *Muse {
	ids := []string{cfg.ModelID}
	for _, id := range cfg.Models {
		if id != cfg.ModelID {
			ids = append(ids, id)
		}
	}
	return &Muse{cfg: cfg, ids: ids}
}

func (m *Muse) Name() string { return "muse" }
func (m *Muse) Bin() string  { return m.cfg.Bin }
func (m *Muse) Cwd() string  { return m.cfg.Cwd }

// Cfg exposes the backend configuration so the server can adjust it.
func (m *Muse) Cfg() *config.Muse      { return &m.cfg }
func (m *Muse) IDs() []string          { return append([]string{}, m.ids...) }
func (m *Muse) DefaultID() string      { return m.cfg.ModelID }
func (m *Muse) TaskModel() string      { return m.cfg.TaskModel }
func (m *Muse) AssignsSessionID() bool { return false }
func (m *Muse) InlinesSystem() bool    { return true }
func (m *Muse) NewSessionID() string   { return domain.NewUUID() }
func (m *Muse) Owns(id string) bool    { return museIDPattern.MatchString(id) }

// ResolveModel strips the komputer- prefix, as the CLI wants its own ids.
func (m *Muse) ResolveModel(requested string) string {
	id := strings.TrimSpace(requested)
	if id == "" || id == m.cfg.ModelID {
		return m.cfg.DefaultModel
	}
	stripped := komputerPrefix.ReplaceAllString(id, "")
	if stripped == "" {
		return m.cfg.DefaultModel
	}
	return stripped
}

// ResolveEffort maps request hints to a muse reasoning effort.
func (m *Muse) ResolveEffort(input EffortInput) string {
	e := strings.ToLower(strings.TrimSpace(input.Value()))
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
	if museEfforts[e] {
		return e
	}
	return m.cfg.DefaultEffort
}

// ChildEnv builds the filtered child environment.
func (m *Muse) ChildEnv(source map[string]string, cwd string) map[string]string {
	return childenv.Build(source, cwd, childenv.MuseKeep)
}

// Prepare writes the prompt and image files and renders argv.
func (m *Muse) Prepare(turn Turn) (Prepared, error) {
	texts, binary := SplitAttachments(turn.Attachments)
	var files []string
	cleanup := func() {
		for _, f := range files {
			_ = os.Remove(f)
		}
	}
	var images []string
	for _, b := range binary {
		ext := ""
		if b.Type == "image" && b.Source != nil && b.Source.Type == "base64" {
			ext = museImageExt[strings.ToLower(b.Source.MediaType)]
		}
		if ext == "" {
			cleanup()
			return Prepared{}, &domain.APIError{Status: 400, Type: "invalid_request_error", Message: "This model accepts base64 PNG, JPEG, GIF or WebP images only"}
		}
		f, err := os.CreateTemp("", "komputer-muse-img-*."+ext)
		if err != nil {
			cleanup()
			return Prepared{}, err
		}
		files = append(files, f.Name())
		data, err := decodeBase64(b.Source.Data)
		if err != nil {
			f.Close()
			cleanup()
			return Prepared{}, &domain.APIError{Status: 400, Type: "invalid_request_error", Message: "Invalid base64 image data"}
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			cleanup()
			return Prepared{}, err
		}
		f.Close()
		images = append(images, f.Name())
	}
	body := PromptWithText(turn.Prompt, texts)
	promptFile, err := os.CreateTemp("", "komputer-muse-*.txt")
	if err != nil {
		cleanup()
		return Prepared{}, err
	}
	files = append(files, promptFile.Name())
	content := body
	if turn.IncludeSystem {
		content = InlineSystem(turn.SystemPrompt, body)
	}
	if _, err := promptFile.WriteString(content); err != nil {
		promptFile.Close()
		cleanup()
		return Prepared{}, err
	}
	promptFile.Close()
	var servers []MCPServerSpec
	switch {
	case turn.MCPServer != nil:
		servers = []MCPServerSpec{*turn.MCPServer}
	case !turn.UseMCP:
		servers = nil
	default:
		var err error
		servers, err = ReadMCPServers(m.cfg.MCPConfig)
		if err != nil {
			cleanup()
			return Prepared{}, err
		}
	}
	mcpArgs, mcpEnv := MuseMCPOverrides(servers)
	workspace := m.cfg.Cwd
	if turn.MCPServer != nil {
		workspace = turn.WorkDir
	}
	tools := turn.Tools
	if turn.MCPServer != nil {
		tools = ""
	}
	return Prepared{
		Args:  MuseArgs(promptFile.Name(), workspace, turn.Session, turn.Model, turn.Effort, tools, mcpArgs, images, m.cfg.ExtraArgs),
		Stdin: "", Cwd: workspace, ExtraEnv: mcpEnv, Cleanup: cleanup,
	}, nil
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// Classify maps a Muse failure message to a public error type.
func (m *Muse) Init() []string { return nil }

// Checks reports nothing: this backend has no global setup to converge.
func (m *Muse) Checks(string) []string { return nil }

func (m *Muse) Classify(message string) string {
	switch {
	case museAuthPattern.MatchString(message):
		return "upstream_auth_error"
	case museBodyDownPattern.MatchString(message):
		return "body_unavailable"
	default:
		return ""
	}
}

type museStream struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type musePayload struct {
	Text      *string          `json:"text"`
	Reason    *string          `json:"reason"`
	Models    []museUsageModel `json:"models"`
	RunStream *struct {
		ID string `json:"id"`
	} `json:"run_stream"`
	Event *struct {
		TaskKind string `json:"task_kind"`
		ToolName string `json:"tool_name"`
		Name     string `json:"name"`
	} `json:"event"`
}

type museEnvelope struct {
	PayloadType string      `json:"payload_type"`
	Stream      museStream  `json:"stream"`
	Sequence    int         `json:"sequence"`
	Payload     musePayload `json:"payload"`
}

type museTerminal struct {
	typ, text, reason string
	hasText           bool
}

type museParser struct {
	session   SessionRef
	sink      domain.Sink
	bail      func(*Failure)
	resume    bool
	first     bool
	runID     string
	terminal  *museTerminal
	usage     *domain.Usage
	streamed  strings.Builder
	afterTool bool
	activity  bool
}

// NewParser creates the muse exec --json parser for one run.
func (m *Muse) NewParser(session SessionRef, sink domain.Sink, bail func(*Failure)) Parser {
	return &museParser{sink: sink, bail: bail, first: true, session: session, resume: session.Mode == SessionResume}
}

func (p *museParser) Activity() bool { return p.activity }

func (p *museParser) doBail(message, typ string) {
	if p.bail != nil {
		p.bail(&Failure{Message: message, Type: typ, Activity: p.activity})
	}
}

func (p *museParser) Stderr(text string) {
	if workspaceRefusal.MatchString(text) {
		p.doBail("session belongs to a different workspace", "session_not_found")
	}
}

func (p *museParser) Line(line string) {
	var r museEnvelope
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return
	}
	if r.PayloadType == "run.usage" {
		p.usage = MuseUsage(r.Payload.Models)
		return
	}
	if r.Stream.Kind != "session" {
		return
	}
	if p.first {
		p.first = false
		if p.resume && r.Sequence == 1 {
			p.doBail(fmt.Sprintf("session %s not found (muse started a new one)", p.session.ID), "session_not_found")
			return
		}
	}
	ownRun := func() bool {
		return p.runID == "" || r.Payload.RunStream == nil || r.Payload.RunStream.ID == "" || r.Payload.RunStream.ID == p.runID
	}
	switch r.PayloadType {
	case "session.run.linked":
		if p.runID == "" && r.Payload.RunStream != nil {
			p.runID = r.Payload.RunStream.ID
		}
	case "run.output.delta":
		if !ownRun() || r.Payload.Text == nil || *r.Payload.Text == "" {
			return
		}
		p.activity = true
		if p.afterTool && p.streamed.Len() > 0 {
			p.streamed.WriteString(string([]byte{10, 10}))
			p.sink.Emit(domain.EventText, string([]byte{10, 10}))
		}
		p.afterTool = false
		p.streamed.WriteString(*r.Payload.Text)
		p.sink.Emit(domain.EventText, *r.Payload.Text)
	case "task.lifecycle.proposed":
		kind := ""
		name := ""
		if r.Payload.Event != nil {
			kind = r.Payload.Event.TaskKind
			name = r.Payload.Event.ToolName
			if name == "" {
				name = r.Payload.Event.Name
			}
		}
		if !ownRun() || strings.HasPrefix(kind, "reminder.") {
			return
		}
		p.activity = true
		if strings.HasPrefix(kind, "model.") {
			return
		}
		p.afterTool = true
		if name == "" {
			name = strings.TrimPrefix(kind, "tool.")
		}
		if name == "" {
			name = "tool"
		}
		p.sink.Emit(domain.EventTool, name)
	case "run.terminal.completed", "run.terminal.failed", "run.terminal.cancelled":
		if ownRun() {
			t := &museTerminal{typ: r.PayloadType}
			if r.Payload.Text != nil {
				t.text, t.hasText = *r.Payload.Text, true
			}
			if r.Payload.Reason != nil {
				t.reason = *r.Payload.Reason
			}
			p.terminal = t
		}
	}
}

func (p *museParser) Finish(code int, signal string, stderr string) (domain.Output, *Failure) {
	if p.terminal != nil && p.terminal.typ == "run.terminal.completed" {
		text := p.streamed.String()
		if p.terminal.hasText {
			text = p.terminal.text
		}
		return domain.Output{
			Text: strings.TrimSpace(text), Thinking: "",
			SessionID: p.session.ID, Usage: p.usage,
		}, nil
	}
	if p.terminal != nil {
		parts := strings.Split(p.terminal.typ, ".")
		tail := stderr
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return domain.Output{}, &Failure{Message: strings.TrimSpace(fmt.Sprintf("muse %s: %s %s", parts[len(parts)-1], p.terminal.reason, tail))}
	}
	tail := stderr
	if len(tail) > 800 {
		tail = tail[len(tail)-800:]
	}
	msg := fmt.Sprintf("muse exit %d", code)
	if signal != "" {
		msg += " signal=" + signal
	}
	return domain.Output{}, &Failure{Message: msg + ": " + tail}
}
