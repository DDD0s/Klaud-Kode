// Package config parses the service environment once, keeping the same
// variable names and defaults as the JavaScript implementation.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ListenAddress is one host:port pair to bind.
type ListenAddress struct {
	Host string
	Port int
}

func (a ListenAddress) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// Claude holds the Claude Code backend settings.
type Claude struct {
	Bin              string
	Cwd              string
	MCPConfig        string
	ModelID          string
	ExtraModels      []string
	DefaultModel     string
	DefaultEffort    string
	TaskModel        string
	SystemPromptFile string
}

// Config is the parsed service configuration.
type Config struct {
	SourceEnv map[string]string

	BaseDir            string
	RepoRoot           string
	SessionDir         string
	MCPConfig          string
	InstanceID         string
	WorkspaceRoot      string
	TunnelUp           string
	HealthURL          string
	KeysFile           string
	SessionsFile       string
	ResponsesFile      string
	MaxStoredResponses int

	Listen []ListenAddress
	Port   int

	CLITimeout        time.Duration
	MaxInFlight       int
	MaxSessions       int
	IdleTimeout       time.Duration
	BuiltinTools      string
	SystemPromptFile  string
	Heartbeat         time.Duration
	AuthFailDelay     time.Duration
	StreamToolNotes   bool
	StreamThinking    bool
	SkipTunnel        bool
	TunnelFailCache   time.Duration
	MaxHistoryChars   int
	NoKeyMode         string
	DetectDivergence  bool
	TaskDetect        bool
	MaxBodyBytes      int64
	TurnCache         time.Duration
	SSEOpen           time.Duration
	AuthFailureLimit  int
	MaxConnections    int
	ClientToolTimeout time.Duration
	MaxClientToolRuns int
	RequireStrongKeys bool

	BodyName   string
	BodyMCP    string
	BodySystem string

	TLSCertFile       string
	TLSKeyFile        string
	TLSKeyPassphrase  string
	AllowInsecureHTTP bool
	CORSOrigins       map[string]bool

	NodeBin string
	Claude  Claude
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func falsy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

type envMap map[string]string

func (e envMap) get(key, fallback string) string {
	if v, ok := e[key]; ok && v != "" {
		return v
	}
	return fallback
}

func (e envMap) getSet(key, fallback string) string {
	if v, ok := e[key]; ok {
		return v
	}
	return fallback
}

func (e envMap) intValue(key string, fallback int) (int, error) {
	v, ok := e[key]
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func (e envMap) durationMs(key string, fallback time.Duration) (time.Duration, error) {
	n, err := e.intValue(key, int(fallback/time.Millisecond))
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Millisecond, nil
}

func list(v, fallback string) []string {
	if v == "" {
		v = fallback
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var listenPattern = regexp.MustCompile(`^(\[[^\]]+\]|[^:]+):([0-9]+)$`)

func parseListen(e envMap, port int) ([]ListenAddress, error) {
	entries := list(e.get("KOMPUTER_API_HOST", "127.0.0.1"), "")
	out := make([]ListenAddress, 0, len(entries))
	for _, entry := range entries {
		if m := listenPattern.FindStringSubmatch(entry); m != nil {
			p, err := strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("KOMPUTER_API_HOST entry %q: %w", entry, err)
			}
			out = append(out, ListenAddress{Host: strings.Trim(m[1], "[]"), Port: p})
		} else {
			out = append(out, ListenAddress{Host: entry, Port: port})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("KOMPUTER_API_HOST must contain at least one listen address")
	}
	return out, nil
}

// PrivateListenAddress reports whether a bind host is loopback or private.
func PrivateListenAddress(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || (cgnat != nil && cgnat.Contains(ip))
}

func defaultBaseDir() string {
	cwd, _ := os.Getwd()
	exe, err := os.Executable()
	exeDir := ""
	if err == nil {
		exeDir = filepath.Dir(exe)
	}
	if filepath.Base(exeDir) == "komputer-api" {
		return exeDir
	}
	if cwd != "" {
		candidate := filepath.Join(cwd, "komputer-api")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		if filepath.Base(cwd) == "komputer-api" {
			return cwd
		}
	}
	if exeDir != "" {
		return exeDir
	}
	return cwd
}

var backendAliases = map[string]string{
	"claude": "claude", "claude-code": "claude",
	"agy": "antigravity", "antigravity": "antigravity",
	"muse": "muse", "muse-code": "muse",
}

// Load parses and validates the configuration.
func Load(source map[string]string) (Config, error) {
	e := envMap(source)
	var cfg Config
	cfg.SourceEnv = source

	names := []string{}
	seen := map[string]bool{}
	for _, n := range list(e.get("KOMPUTER_BACKENDS", "claude"), "") {
		name, ok := backendAliases[strings.ToLower(n)]
		if !ok {
			return cfg, fmt.Errorf("KOMPUTER_BACKENDS: unknown backend %q (use claude, antigravity, muse)", n)
		}
		if name != "claude" {
			return cfg, fmt.Errorf("backend %q is not yet implemented in the Go rewrite", name)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if def, ok := e["KOMPUTER_DEFAULT_BACKEND"]; ok {
		name := backendAliases[strings.ToLower(def)]
		if !seen[name] {
			return cfg, fmt.Errorf("KOMPUTER_DEFAULT_BACKEND must be one of KOMPUTER_BACKENDS")
		}
	}

	var err error
	if cfg.Port, err = e.intValue("KOMPUTER_API_PORT", 18888); err != nil {
		return cfg, err
	}
	if cfg.Listen, err = parseListen(e, cfg.Port); err != nil {
		return cfg, err
	}

	cfg.BaseDir = defaultBaseDir()
	cfg.RepoRoot = filepath.Dir(cfg.BaseDir)
	cfg.SessionDir = e.get("KOMPUTER_SESSION_DIR", filepath.Join(cfg.RepoRoot, "komputer-session"))
	cfg.MCPConfig = e.get("KOMPUTER_MCP_CONFIG", filepath.Join(cfg.SessionDir, ".mcp.json"))
	absSession, _ := filepath.Abs(cfg.SessionDir)
	sum := sha256.Sum256([]byte(absSession))
	cfg.InstanceID = hex.EncodeToString(sum[:])[:16]
	home, _ := os.UserHomeDir()
	cfg.WorkspaceRoot = filepath.Join(home, ".komputer", "workspaces")
	claudeCwd, _ := filepath.Abs(e.get("KOMPUTER_CLAUDE_CWD", filepath.Join(cfg.WorkspaceRoot, cfg.InstanceID)))
	cfg.TunnelUp = e.get("KOMPUTER_TUNNEL_UP", filepath.Join(cfg.RepoRoot, "komputer", "tunnel-up.sh"))
	cfg.HealthURL = e.get("KOMPUTER_HEALTH_URL", "http://127.0.0.1:18787/healthz")
	cfg.KeysFile = e.get("KOMPUTER_KEYS_FILE", filepath.Join(cfg.BaseDir, "KEYS.txt"))
	cfg.SessionsFile = e.get("KOMPUTER_SESSIONS_FILE", filepath.Join(cfg.BaseDir, "sessions.json"))
	cfg.ResponsesFile = e.getSet("KOMPUTER_RESPONSES_FILE", filepath.Join(cfg.BaseDir, "responses.json"))
	if cfg.MaxStoredResponses, err = e.intValue("KOMPUTER_MAX_STORED_RESPONSES", 1000); err != nil {
		return cfg, err
	}
	if cfg.MaxStoredResponses == 0 {
		cfg.MaxStoredResponses = 1000
	}

	if cfg.CLITimeout, err = e.durationMs("KOMPUTER_CLAUDE_TIMEOUT_MS", 600*time.Second); err != nil {
		return cfg, err
	}
	maxConc := "KOMPUTER_MAX_IN_FLIGHT"
	if _, ok := e[maxConc]; !ok {
		maxConc = "KOMPUTER_MAX_CONCURRENT"
	}
	if cfg.MaxInFlight, err = e.intValue(maxConc, 4); err != nil {
		return cfg, err
	}
	if cfg.MaxSessions, err = e.intValue("KOMPUTER_MAX_SESSIONS", 0); err != nil {
		return cfg, err
	}
	if cfg.IdleTimeout, err = e.durationMs("KOMPUTER_IDLE_TIMEOUT_MS", 45*time.Minute); err != nil {
		return cfg, err
	}
	cfg.BuiltinTools = e.getSet("KOMPUTER_BUILTIN_TOOLS", "")
	if p := e.get("KOMPUTER_SYSTEM_PROMPT_FILE", ""); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			cfg.SystemPromptFile = abs
		} else {
			cfg.SystemPromptFile = p
		}
	}
	if cfg.Heartbeat, err = e.durationMs("KOMPUTER_HEARTBEAT_MS", 15*time.Second); err != nil {
		return cfg, err
	}
	if cfg.AuthFailDelay, err = e.durationMs("KOMPUTER_AUTH_FAIL_DELAY_MS", time.Second); err != nil {
		return cfg, err
	}
	cfg.StreamToolNotes = !falsy(e["KOMPUTER_STREAM_TOOL_NOTES"])
	cfg.StreamThinking = !falsy(e["KOMPUTER_STREAM_THINKING"])
	cfg.SkipTunnel = truthy(e["KOMPUTER_SKIP_TUNNEL"])
	if cfg.TunnelFailCache, err = e.durationMs("KOMPUTER_TUNNEL_FAIL_CACHE_MS", 30*time.Second); err != nil {
		return cfg, err
	}
	if cfg.TunnelFailCache < 0 {
		return cfg, fmt.Errorf("KOMPUTER_TUNNEL_FAIL_CACHE_MS must be nonnegative")
	}
	if cfg.MaxHistoryChars, err = e.intValue("KOMPUTER_MAX_HISTORY_CHARS", 60000); err != nil {
		return cfg, err
	}
	cfg.NoKeyMode = strings.ToLower(e.get("KOMPUTER_NO_KEY_MODE", "stateless"))
	cfg.DetectDivergence = !falsy(e["KOMPUTER_DETECT_REGENERATE"])
	cfg.TaskDetect = !falsy(e["KOMPUTER_TASK_DETECT"])
	var bodyBytes int
	if bodyBytes, err = e.intValue("KOMPUTER_MAX_BODY_BYTES", 50_000_000); err != nil {
		return cfg, err
	}
	cfg.MaxBodyBytes = int64(bodyBytes)
	if cfg.TurnCache, err = e.durationMs("KOMPUTER_TURN_CACHE_MS", 10*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.SSEOpen, err = e.durationMs("KOMPUTER_SSE_OPEN_MS", 25*time.Second); err != nil {
		return cfg, err
	}
	if cfg.AuthFailureLimit, err = e.intValue("KOMPUTER_AUTH_FAILURE_LIMIT", 20); err != nil {
		return cfg, err
	}
	if cfg.AuthFailureLimit < 1 {
		return cfg, fmt.Errorf("KOMPUTER_AUTH_FAILURE_LIMIT must be a positive integer")
	}
	if cfg.MaxConnections, err = e.intValue("KOMPUTER_MAX_CONNECTIONS", 512); err != nil {
		return cfg, err
	}
	if cfg.ClientToolTimeout, err = e.durationMs("KOMPUTER_CLIENT_TOOL_TIMEOUT_MS", 300*time.Second); err != nil {
		return cfg, err
	}
	if cfg.ClientToolTimeout <= 0 {
		return cfg, fmt.Errorf("KOMPUTER_CLIENT_TOOL_TIMEOUT_MS must be positive")
	}
	if cfg.MaxClientToolRuns, err = e.intValue("KOMPUTER_MAX_CLIENT_TOOL_RUNS", 16); err != nil {
		return cfg, err
	}
	if cfg.MaxClientToolRuns < 1 {
		return cfg, fmt.Errorf("KOMPUTER_MAX_CLIENT_TOOL_RUNS must be a positive integer")
	}
	cfg.RequireStrongKeys = truthy(e["KOMPUTER_PUBLIC_API"]) || e.get("KOMPUTER_TLS_CERT_FILE", "") != ""
	if !cfg.RequireStrongKeys {
		for _, l := range cfg.Listen {
			if !PrivateListenAddress(l.Host) {
				cfg.RequireStrongKeys = true
			}
		}
	}
	cfg.BodyName = e.get("KOMPUTER_BODY_NAME", "the user's computer")
	cfg.BodyMCP = e.get("KOMPUTER_BODY_MCP", "komputer_use")
	defaultNote := fmt.Sprintf("You work on %s. Your shell, files, processes and applications are all there, and the mcp__%s__* tools are how you reach them: run_command for shell commands, the file tools for reading and writing, start_process for anything long-running. At the start of a new conversation, call system_info to learn the OS, shell, date, time zone, working roots and limits. Use those results for your commands and paths.", cfg.BodyName, cfg.BodyMCP)
	cfg.BodySystem = e.getSet("KOMPUTER_SYSTEM_PROMPT", defaultNote)

	cfg.TLSCertFile = e.get("KOMPUTER_TLS_CERT_FILE", "")
	cfg.TLSKeyFile = e.get("KOMPUTER_TLS_KEY_FILE", "")
	cfg.TLSKeyPassphrase = e.get("KOMPUTER_TLS_KEY_PASSPHRASE", "")
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return cfg, fmt.Errorf("configure both KOMPUTER_TLS_CERT_FILE and KOMPUTER_TLS_KEY_FILE")
	}
	if cfg.TLSKeyPassphrase != "" {
		return cfg, fmt.Errorf("KOMPUTER_TLS_KEY_PASSPHRASE is not yet supported by the Go rewrite")
	}
	cfg.AllowInsecureHTTP = truthy(e["KOMPUTER_ALLOW_INSECURE_HTTP"])
	cfg.CORSOrigins = map[string]bool{}
	for _, origin := range list(e.get("KOMPUTER_CORS_ORIGINS", ""), "") {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != origin {
			return cfg, fmt.Errorf("KOMPUTER_CORS_ORIGINS must contain exact HTTP(S) origins, without paths or wildcards")
		}
		cfg.CORSOrigins[origin] = true
	}

	cfg.NodeBin = e.get("NODE_BIN", "node")
	cfg.Claude = Claude{
		Bin:              e.get("CLAUDE_BIN", "claude"),
		Cwd:              claudeCwd,
		MCPConfig:        cfg.MCPConfig,
		ModelID:          e.get("KOMPUTER_MODEL_ID", "komputer-claude"),
		ExtraModels:      list(e.get("KOMPUTER_MODELS", ""), "komputer-opus,komputer-sonnet,komputer-haiku,komputer-fable"),
		DefaultModel:     e.get("KOMPUTER_CLAUDE_MODEL", ""),
		DefaultEffort:    e.get("KOMPUTER_EFFORT", ""),
		TaskModel:        e.getSet("KOMPUTER_TASK_MODEL", "haiku"),
		SystemPromptFile: cfg.SystemPromptFile,
	}
	return cfg, nil
}
