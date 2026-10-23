package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

var (
	agyOwnsPattern   = regexp.MustCompile(`(?i)^(komputer-)?(agy|antigravity|gemini)(-|$)`)
	agyEffortSuffix  = regexp.MustCompile(`-(minimal|low|medium|high)$`)
	agyModelPrefixes = regexp.MustCompile(`(?i)^(agy|antigravity)-`)
)

var agyEffortOrder = []string{"low", "medium", "high", "xhigh", "max"}

// Antigravity is the agy CLI backend. agy has no way to scope MCP servers
// or built-in tools per run, so per-turn behavior rides on the bridge
// target file, the PreToolUse hook and workspace-local shims.
type Antigravity struct {
	cfg         config.Antigravity
	ids         []string
	efforts     []string
	bodyDir     string
	plainDir    string
	targetDir   string
	shimDir     string
	remoteTools string
	mount       *fuseMount
	files       atomic.Bool
	fuseGaps    []string

	bridges    []string
	hooked     atomic.Bool
	warnedWin  atomic.Bool
	setupNotes []string

	bodyMu    sync.Mutex
	bodyCache map[string]bodyCacheEntry
}

type bodyInfo struct {
	root     string
	platform string
}

type bodyCacheEntry struct {
	at    time.Time
	value bodyInfo
}

// NewAntigravity builds the agy backend.
func NewAntigravity(cfg config.Antigravity) *Antigravity {
	ids := []string{cfg.ModelID}
	for _, id := range cfg.Models {
		if id != cfg.ModelID {
			ids = append(ids, id)
		}
	}
	efforts := cfg.Efforts
	if len(efforts) == 0 {
		efforts = []string{"low", "medium", "high"}
	}
	remoteTools := cfg.RemoteTools
	if remoteTools == "" {
		remoteTools = "off"
	}
	a := &Antigravity{
		cfg: cfg, ids: ids, efforts: efforts,
		bodyDir:     filepath.Join(cfg.Cwd, "body"),
		plainDir:    filepath.Join(cfg.Cwd, "plain"),
		targetDir:   filepath.Join(cfg.Cwd, "mcp"),
		shimDir:     filepath.Join(cfg.Cwd, "shim"),
		remoteTools: remoteTools,
		bodyCache:   map[string]bodyCacheEntry{},
	}
	a.files.Store(remoteTools == "all")
	a.mount = &fuseMount{
		python: cfg.Python, logf: cfg.Logf,
		dir: cfg.RemoteMount, configFile: filepath.Join(a.targetDir, "komputer-fs.json"),
	}
	return a
}

// SetLogf installs the service logger after construction.
func (a *Antigravity) SetLogf(logf func(string, ...any)) { a.cfg.Logf = logf }

func (a *Antigravity) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

func (a *Antigravity) Name() string           { return "antigravity" }
func (a *Antigravity) Bin() string            { return a.cfg.Bin }
func (a *Antigravity) Cwd() string            { return a.cfg.Cwd }
func (a *Antigravity) IDs() []string          { return append([]string{}, a.ids...) }
func (a *Antigravity) DefaultID() string      { return a.cfg.ModelID }
func (a *Antigravity) TaskModel() string      { return a.cfg.TaskModel }
func (a *Antigravity) AssignsSessionID() bool { return true }
func (a *Antigravity) InlinesSystem() bool    { return true }
func (a *Antigravity) NewSessionID() string   { return "" }

func (a *Antigravity) Owns(id string) bool {
	for _, advertised := range a.ids {
		if advertised == id {
			return true
		}
	}
	return agyOwnsPattern.MatchString(id)
}

// ResolveModel maps komputer-prefixed ids to agy's own model ids, e.g.
// komputer-gemini-4-argon -> gemini-4-argon and
// komputer-agy-claude-opus-5-5 -> claude-opus-5-5.
func (a *Antigravity) ResolveModel(requested string) string {
	id := strings.TrimSpace(requested)
	if id == "" || id == a.cfg.ModelID {
		return a.cfg.DefaultModel
	}
	id = komputerPrefix.ReplaceAllString(id, "")
	id = agyModelPrefixes.ReplaceAllString(id, "")
	if id == "" {
		return a.cfg.DefaultModel
	}
	return id
}

func (a *Antigravity) clampEffort(e string) string {
	for _, allowed := range a.efforts {
		if allowed == e {
			return e
		}
	}
	want := indexOf(agyEffortOrder, e)
	if want < 0 {
		return ""
	}
	allowed := []string{}
	for _, x := range a.efforts {
		if indexOf(agyEffortOrder, x) >= 0 {
			allowed = append(allowed, x)
		}
	}
	sortByEffort(allowed)
	best := ""
	for _, x := range allowed {
		if indexOf(agyEffortOrder, x) <= want {
			best = x
		}
	}
	if best != "" {
		return best
	}
	if len(allowed) > 0 {
		return allowed[0]
	}
	return ""
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func sortByEffort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && indexOf(agyEffortOrder, values[j]) < indexOf(agyEffortOrder, values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ResolveEffort maps request effort hints to an agy effort level. The
// requested model is read from input via ResolveModel by the caller through
// EffortInput.Model when set.
func (a *Antigravity) ResolveEffort(input EffortInput) string {
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
	if e == "" {
		return a.cfg.DefaultEffort
	}
	model := a.ResolveModel(input.Model)
	if model == "" {
		model = a.cfg.DefaultModel
	}
	if model == "" || strings.HasPrefix(strings.ToLower(model), "gemini") {
		if clamped := a.clampEffort(e); clamped != "" {
			return clamped
		}
		return a.cfg.DefaultEffort
	}
	if indexOf(agyEffortOrder, e) >= 0 {
		return e
	}
	return a.cfg.DefaultEffort
}

// ChildEnv builds the filtered child environment.
func (a *Antigravity) ChildEnv(source map[string]string, cwd string) map[string]string {
	return childenv.Build(source, cwd, childenv.AGYKeep)
}

// Args renders the agy argv.
func (a *Antigravity) Args(turn Turn) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
	if a.cfg.Permissions == "skip" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if turn.Model != "" {
		args = append(args, "--model", turn.Model)
	}
	// agy rejects a slug that names its effort combined with --effort.
	if turn.Effort != "" && !agyEffortSuffix.MatchString(turn.Model) {
		args = append(args, "--effort", turn.Effort)
	}
	if turn.Session.Mode == SessionResume && turn.Session.ID != "" {
		args = append(args, "--conversation", turn.Session.ID)
	}
	args = append(args, a.cfg.ExtraArgs...)
	return args
}

// UserEvent renders one stdin user event.
func UserEvent(text string) string {
	data, _ := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
	return string(data) + string([]byte{10})
}

func (a *Antigravity) bodyInfoFor(server *MCPServerSpec) bodyInfo {
	if server == nil {
		return bodyInfo{}
	}
	keyData, _ := json.Marshal([]any{server.URL, server.Headers})
	key := string(keyData)
	a.bodyMu.Lock()
	if hit, ok := a.bodyCache[key]; ok {
		ttl := 15 * time.Second
		if hit.value.root != "" {
			ttl = 300 * time.Second
		}
		if time.Since(hit.at) < ttl {
			a.bodyMu.Unlock()
			return hit.value
		}
	}
	a.bodyMu.Unlock()
	value := bodyInfo{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewMCPHTTPClient(*server).CallTool(ctx, "system_info", map[string]any{})
	if err == nil {
		if m, ok := result.(map[string]any); ok {
			if cwdRoot, ok := m["cwdRoot"].(map[string]any); ok {
				value.root, _ = cwdRoot["path"].(string)
			}
			value.platform, _ = m["platform"].(string)
		}
	}
	a.bodyMu.Lock()
	a.bodyCache[key] = bodyCacheEntry{at: time.Now(), value: value}
	a.bodyMu.Unlock()
	return value
}

func islandPolicy() *AGYPolicy { return &AGYPolicy{Mode: "island"} }
func localPolicy() *AGYPolicy  { return &AGYPolicy{Mode: "local"} }

// Prepare renders one agy turn including its bridge target file.
func (a *Antigravity) Prepare(turn Turn) (Prepared, error) {
	texts, binary := SplitAttachments(turn.Attachments)
	if len(binary) > 0 {
		return Prepared{}, &domain.APIError{Status: 400, Type: "invalid_request_error", Message: "This model does not accept image or PDF attachments"}
	}
	var servers []MCPServerSpec
	var cwd string
	policy := islandPolicy()
	var remote *AGYRemote
	switch {
	case turn.MCPServer != nil:
		cwd = filepath.Join(turn.WorkDir, "agy")
		servers = []MCPServerSpec{*turn.MCPServer}
	case !turn.UseMCP:
		cwd = a.plainDir
	default:
		cwd = a.bodyDir
		parsed, err := ReadMCPServers(a.cfg.MCPConfig)
		if err != nil {
			return Prepared{}, err
		}
		for _, s := range parsed {
			if s.URL != "" {
				servers = append(servers, s)
			}
		}
		var target *MCPServerSpec
		for i := range servers {
			if servers[i].Name == a.cfg.BodyMCP {
				target = &servers[i]
			}
		}
		if turn.Tools != "" {
			policy = localPolicy()
		} else if a.remoteTools != "off" && target != nil {
			server := MCPServerSpec{Name: target.Name, URL: target.URL, Headers: target.Headers}
			info := a.bodyInfoFor(&server)
			if info.platform == "win32" {
				if a.warnedWin.CompareAndSwap(false, true) {
					a.logf("remote tools (%s) are POSIX-only; the Windows computer is used through %s with agy's own tools off", a.remoteTools, a.cfg.BodyMCP)
				}
			} else {
				var mounted string
				if a.files.Load() {
					mounted = a.mount.Ensure(&server)
				}
				if a.remoteTools == "all" && mounted == "" {
					return Prepared{}, &domain.APIError{
						Status: 503, Type: "body_unavailable",
						Message: "The computer is unavailable. Please retry later.",
						Detail:  "agy remote files: the Body root is not mounted",
					}
				}
				if mounted != "" {
					cwd = mounted
				}
				if err := os.MkdirAll(cwd, 0o755); err != nil {
					return Prepared{}, err
				}
				workspace, err := filepath.EvalSymlinks(cwd)
				if err != nil {
					workspace = cwd
				}
				shell := a.cfg.RemoteShell
				if shell == "" {
					shell = "bash"
				}
				serverCopy := server
				remote = &AGYRemote{Server: &serverCopy, Mount: workspace, Root: info.root, Shell: shell}
				policy = &AGYPolicy{Mode: "remote", Mount: workspace, Root: info.root, Files: mounted != "", Commands: true}
			}
		}
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return Prepared{}, err
	}
	body := PromptWithText(turn.Prompt, texts)
	stdinText := body
	if turn.IncludeSystem {
		stdinText = InlineSystem(turn.SystemPrompt, body)
	}
	prepared := Prepared{
		Args: a.Args(turn), Stdin: UserEvent(stdinText), Cwd: cwd,
	}
	scoreSession := turn.Session
	prepared.ParserFactory = func(sink domain.Sink, bail func(*Failure)) Parser {
		return a.newParser(scoreSession, sink, bail, withGuard(policy, a.cfg.Guard, a.hooked.Load(), a.cfg.Permissions == "skip"))
	}
	if len(servers) == 0 && policy.Mode == "local" {
		return prepared, nil
	}
	targetDir := a.targetDir
	if turn.MCPServer != nil {
		targetDir = turn.WorkDir
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return Prepared{}, err
	}
	file := filepath.Join(targetDir, "agy-mcp-"+domain.NewUUID()+".json")
	targets := map[string]MCPServerSpec{}
	for _, s := range servers {
		targets[s.Name] = s
	}
	data, err := json.Marshal(AGYBridgeTarget{Servers: targets, Policy: policy, Remote: remote})
	if err != nil {
		return Prepared{}, err
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return Prepared{}, err
	}
	extraEnv := map[string]string{"KOMPUTER_AGY_MCP_FILE": file}
	if remote != nil {
		pathKey := "PATH"
		oldPath := a.cfg.SourceEnv["PATH"]
		if oldPath == "" {
			oldPath = a.cfg.SourceEnv["Path"]
			pathKey = "Path"
		}
		parts := []string{a.shimDir}
		if oldPath != "" {
			parts = append(parts, oldPath)
		}
		extraEnv[pathKey] = strings.Join(parts, string(os.PathListSeparator))
	}
	prepared.ExtraEnv = extraEnv
	prepared.Cleanup = func() { _ = os.Remove(file) }
	return prepared, nil
}

func withGuard(policy *AGYPolicy, guard string, hooked, skip bool) *AGYPolicy {
	if policy == nil {
		return nil
	}
	out := *policy
	out.Guard = guard
	out.Hooked = hooked
	out.Background = skip
	return &out
}

func (a *Antigravity) newParser(session SessionRef, sink domain.Sink, bail func(*Failure), policy *AGYPolicy) Parser {
	guard := "off"
	if policy != nil && policy.Mode != "local" {
		guard = policy.Guard
		if guard == "" {
			guard = "warn"
		}
	}
	requested := ""
	if session.Mode == SessionResume {
		requested = session.ID
	}
	return &agyParser{
		backend: a, session: session, requested: requested,
		sink: sink, bail: bail, policy: policy, guard: guard,
		conversation: requested, announced: map[int]bool{}, flagged: map[int]bool{},
	}
}

// NewParser is used when no prepared factory is present (island policy).
func (a *Antigravity) NewParser(session SessionRef, sink domain.Sink, bail func(*Failure)) Parser {
	return a.newParser(session, sink, bail, withGuard(islandPolicy(), a.cfg.Guard, a.hooked.Load(), a.cfg.Permissions == "skip"))
}

// Classify maps an agy failure to a public error type.
func (a *Antigravity) Classify(message string) string {
	switch {
	case regexp.MustCompile(`(?i)authentication required|authentication failed|run 'agy' to log in|sign in`).MatchString(message):
		return "upstream_auth_error"
	case regexp.MustCompile(`(?i)RESOURCE_EXHAUSTED|quota`).MatchString(message):
		return "rate_limit_error"
	default:
		return ""
	}
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'"+string([]byte{92})+"''") + "'"
}

func tailText(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
