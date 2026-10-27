// Package body is the caller-computer execution layer: an MCP server that
// runs on the machine to be driven, while the official CLI lives elsewhere.
package body

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/version"
)

// Version is the Body release reported to MCP clients and health checks.
var Version = version.Value

// NestedServer is one stdio MCP server installed on this machine.
type NestedServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	Enabled *bool             `json:"enabled"`
}

// Config is the resolved Body configuration.
type Config struct {
	Port             int
	Host             string
	Roots            map[string]string
	DefaultRoot      string
	Files            bool
	Shell            bool
	Processes        bool
	MaxOutputBytes   int
	MaxFileBytes     int
	DefaultTimeoutMs int
	ConsoleUTF8      bool
	MCPServers       map[string]NestedServer
	Token            string
	// BrainURL is where connect mode dials out to (wss://host/...). Empty in
	// listen modes, where the brain comes to this Body instead.
	BrainURL   string
	ConfigPath string
}

type rawConfig struct {
	Port             *int                    `json:"port"`
	Host             *string                 `json:"host"`
	Roots            map[string]string       `json:"roots"`
	DefaultRoot      string                  `json:"defaultRoot"`
	Files            *bool                   `json:"files"`
	Shell            *bool                   `json:"shell"`
	Processes        *bool                   `json:"processes"`
	MaxOutputBytes   *int                    `json:"maxOutputBytes"`
	MaxFileBytes     *int                    `json:"maxFileBytes"`
	DefaultTimeoutMs *int                    `json:"defaultTimeoutMs"`
	ConsoleUTF8      *bool                   `json:"consoleUtf8"`
	MCPServers       map[string]NestedServer `json:"mcpServers"`
	Token            string                  `json:"token"`
	BrainURL         string                  `json:"brainUrl"`
}

// ConfigPath returns the config file path (KLOUD_KODE_BODY_CONFIG wins).
func ConfigPath() string {
	if p := os.Getenv("KLOUD_KODE_BODY_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kloud-kode-body", "config.json")
}

func expandHome(p string) string {
	home, _ := os.UserHomeDir()
	if p == "~" {
		return home
	}
	if len(p) > 1 && p[0] == '~' && (p[1] == '/' || p[1] == 92) {
		return filepath.Join(home, p[2:])
	}
	return p
}

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

func intOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// LoadConfig reads and resolves the Body config file.
func LoadConfig() (Config, error) {
	path := ConfigPath()
	var raw rawConfig
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &raw); err != nil {
			return Config{}, fmt.Errorf("cannot read %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Defaults below.
	default:
		return Config{}, fmt.Errorf("cannot read %s: %w", path, err)
	}
	cfg := Config{
		Port: 8787, Host: "127.0.0.1", DefaultRoot: "home",
		Files: true, Shell: true, Processes: true,
		MaxOutputBytes: 256 * 1024, MaxFileBytes: 8 * 1024 * 1024,
		DefaultTimeoutMs: 30_000, ConsoleUTF8: true,
		MCPServers: map[string]NestedServer{}, ConfigPath: path,
	}
	if raw.Port != nil {
		cfg.Port = *raw.Port
	}
	if raw.Host != nil {
		cfg.Host = *raw.Host
	}
	// Reaching the Body from another machine needs a reachable bind address.
	// Loopback is still the default so a plain install stays unexposed.
	if host := os.Getenv("KLOUD_KODE_BODY_HOST"); host != "" {
		cfg.Host = host
	}
	if port := os.Getenv("KLOUD_KODE_BODY_PORT"); port != "" {
		if parsed, err := strconv.Atoi(port); err == nil {
			cfg.Port = parsed
		} else {
			return Config{}, fmt.Errorf("KLOUD_KODE_BODY_PORT is not a number: %q", port)
		}
	}
	if raw.DefaultRoot != "" {
		cfg.DefaultRoot = raw.DefaultRoot
	}
	cfg.Files = boolOr(raw.Files, cfg.Files)
	cfg.Shell = boolOr(raw.Shell, cfg.Shell)
	cfg.Processes = boolOr(raw.Processes, cfg.Processes)
	cfg.MaxOutputBytes = intOr(raw.MaxOutputBytes, cfg.MaxOutputBytes)
	cfg.MaxFileBytes = intOr(raw.MaxFileBytes, cfg.MaxFileBytes)
	cfg.DefaultTimeoutMs = intOr(raw.DefaultTimeoutMs, cfg.DefaultTimeoutMs)
	cfg.ConsoleUTF8 = boolOr(raw.ConsoleUTF8, cfg.ConsoleUTF8)
	if raw.MCPServers != nil {
		cfg.MCPServers = raw.MCPServers
	}
	// Configured roots replace the default rather than adding to it, so
	// setting roots to one project directory does not silently leave the
	// home directory open.
	roots := raw.Roots
	if len(roots) == 0 {
		roots = map[string]string{"home": "~"}
	}
	cfg.Roots = make(map[string]string, len(roots))
	for name, dir := range roots {
		abs, err := filepath.Abs(expandHome(dir))
		if err != nil {
			return Config{}, err
		}
		cfg.Roots[name] = abs
	}
	if _, ok := cfg.Roots[cfg.DefaultRoot]; !ok {
		cfg.DefaultRoot = ""
		for name := range cfg.Roots {
			cfg.DefaultRoot = name
			break
		}
	}
	if cfg.DefaultRoot == "" {
		return Config{}, fmt.Errorf("config needs at least one entry in `roots`")
	}
	cfg.Token = os.Getenv("KLOUD_KODE_BODY_TOKEN")
	if cfg.Token == "" {
		cfg.Token = raw.Token
	}
	return cfg, nil
}

// Roots resolves paths inside the configured roots.
type Roots struct{ cfg Config }

// NewRoots creates a resolver for cfg.
func NewRoots(cfg Config) *Roots { return &Roots{cfg: cfg} }

// Resolved is one resolved in-root path.
type Resolved struct {
	Root     string
	RootName string
	Abs      string
	Rel      string
}

// Resolve resolves p inside root name, refusing anything that climbs out.
func (r *Roots) Resolve(p, name string) (Resolved, error) {
	key := name
	if key == "" {
		key = r.cfg.DefaultRoot
	}
	root, ok := r.cfg.Roots[key]
	if !ok {
		names := make([]string, 0, len(r.cfg.Roots))
		for n := range r.cfg.Roots {
			names = append(names, n)
		}
		return Resolved{}, fmt.Errorf("unknown root '%s'; configured: %s", key, strings.Join(names, ", "))
	}
	if p == "" {
		p = "."
	}
	expanded := expandHome(p)
	var target string
	if filepath.IsAbs(expanded) {
		target = filepath.Clean(expanded)
	} else {
		target = filepath.Join(root, expanded)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return Resolved{}, err
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if first == ".." || filepath.IsAbs(rel) {
		return Resolved{}, fmt.Errorf("path escapes root '%s' (%s): %s", key, root, p)
	}
	if rel == "" {
		rel = "."
	}
	return Resolved{Root: root, RootName: key, Abs: target, Rel: rel}, nil
}
