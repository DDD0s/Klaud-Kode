// Package childenv builds the filtered environment handed to official CLI
// child processes. It keeps runtime and login configuration, never the API
// process's own deployment secrets.
package childenv

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var base = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TMPDIR", "TEMP", "TMP",
	"SYSTEMROOT", "COMSPEC", "PATHEXT", "OS", "NUMBER_OF_PROCESSORS",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
	"SHELL", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// ClaudeKeep lists the variables a Claude Code child may inherit.
// Deliberately name credential variables: a new ANTHROPIC_* variable must
// not silently become part of the child's environment.
var ClaudeKeep = set(append(append([]string{}, base...),
	"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_POWERSHELL_TOOL",
	"MCP_TIMEOUT", "MCP_TOOL_TIMEOUT", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"FAKE_CLAUDE_STATE", // test double only; never populated by the service
)...)

// AGYKeep lists the variables an Antigravity CLI child may inherit.
var AGYKeep = set(append(append([]string{}, base...),
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
	"FAKE_AGY_STATE", // test double only
)...)

// MuseKeep lists the variables a Muse Code child may inherit.
var MuseKeep = set(append(append([]string{}, base...),
	"META_API_KEY",
	"FAKE_MUSE_STATE", // test double only
)...)

var (
	reservedPattern = regexp.MustCompile(`(?i)^(KOMPUTER_|BODY_|KLOUD_KODE_|CLAUDE_BIN$|AGY_BIN$|MUSE_BIN$|NODE_BIN$|CI$|GIT_CEILING_DIRECTORIES$)`)
	prefixPattern   = regexp.MustCompile(`^(LC_|XDG_|PROCESSOR_)`)
)

// FromList converts os.Environ-style "K=V" pairs into a map.
func FromList(list []string) map[string]string {
	m := make(map[string]string, len(list))
	for _, kv := range list {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// ToList renders an environment map as sorted "K=V" pairs.
func ToList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Build filters source down to keep, applies proxy normalization and adds
// the variables every CLI child gets. cwd must be the absolute, canonical
// working directory used by the child.
func Build(source map[string]string, cwd string, keep map[string]bool) map[string]string {
	extra := map[string]bool{}
	for _, k := range strings.Split(source["KOMPUTER_CHILD_ENV_EXTRA"], ",") {
		if k = strings.TrimSpace(k); k != "" {
			extra[k] = true
		}
	}
	out := map[string]string{}
	for k, v := range source {
		if reservedPattern.MatchString(k) {
			continue
		}
		if keep[strings.ToUpper(k)] || prefixPattern.MatchString(k) || extra[k] {
			out[k] = v
		}
	}
	out["CI"] = "1"
	// Git ignores a ceiling equal to cwd. Stop before entering its parent.
	// This is not a sandbox: a .git in cwd is still visible.
	out["GIT_CEILING_DIRECTORIES"] = filepath.Dir(cwd)

	httpProxy := first(source, "HTTP_PROXY", "http_proxy")
	httpsProxy := first(source, "HTTPS_PROXY", "https_proxy")
	allProxy := first(source, "ALL_PROXY", "all_proxy")
	if httpProxy != "" || httpsProxy != "" {
		h := httpProxy
		if h == "" {
			h = httpsProxy
		}
		s := httpsProxy
		if s == "" {
			s = httpProxy
		}
		out["HTTP_PROXY"], out["http_proxy"] = h, h
		out["HTTPS_PROXY"], out["https_proxy"] = s, s
	}
	if allProxy != "" {
		out["ALL_PROXY"], out["all_proxy"] = allProxy, allProxy
	}
	noProxy, hasNoProxy := lookupEither(source, "NO_PROXY", "no_proxy")
	if !hasNoProxy && (httpProxy != "" || httpsProxy != "" || allProxy != "") {
		noProxy, hasNoProxy = "127.0.0.1,localhost,::1,*.ts.net,100.64.0.0/10,10.0.0.0/8", true
	}
	if hasNoProxy {
		out["NO_PROXY"], out["no_proxy"] = noProxy, noProxy
	}
	return out
}

// BuildClaude builds the environment for a Claude Code child.
func BuildClaude(source map[string]string, cwd string) map[string]string {
	return Build(source, cwd, ClaudeKeep)
}

func first(source map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := source[k]; v != "" {
			return v
		}
	}
	return ""
}

func lookupEither(source map[string]string, a, b string) (string, bool) {
	if v, ok := source[a]; ok {
		return v, true
	}
	v, ok := source[b]
	return v, ok
}
