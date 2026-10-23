package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
)

func agyGlobalMCP(home string) string {
	return filepath.Join(home, ".gemini", "config", "mcp_config.json")
}

func agyHooksPath(home string) string {
	return filepath.Join(home, ".gemini", "config", "hooks.json")
}

func agySettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

type jsonFile struct {
	value map[string]any
	ok    bool
}

func readJSONFile(path string) jsonFile {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return jsonFile{ok: true}
		}
		return jsonFile{ok: false}
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return jsonFile{ok: false}
	}
	return jsonFile{value: value, ok: true}
}

func writeJSONFile(path string, value map[string]any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, byte(10))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func mapValue(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func anyStrings(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func readGlobalMcp(home string) map[string]any {
	data, err := os.ReadFile(agyGlobalMCP(home))
	if err != nil {
		return map[string]any{}
	}
	var parsed struct {
		Servers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Servers == nil {
		return map[string]any{}
	}
	return parsed.Servers
}

func (a *Antigravity) bridgeArgs(name string) []string { return []string{"bridge", name} }

// HookEntry is the hooks.json fragment for the Go binary.
func (a *Antigravity) HookEntry() map[string]any {
	command := shq(a.cfg.SelfExe) + " hook"
	if runtime.GOOS == "windows" {
		q := string([]byte{34})
		command = q + a.cfg.SelfExe + q + " hook"
	}
	return map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": ".*",
		"hooks":   []any{map[string]any{"type": "command", "command": command, "timeout": 10}},
	}}}
}

func jsonEqual(a, b any) bool {
	ad, _ := json.Marshal(a)
	bd, _ := json.Marshal(b)
	return string(ad) == string(bd)
}

func (a *Antigravity) isHook(value any) bool { return jsonEqual(value, a.HookEntry()) }

func (a *Antigravity) runAgy(args ...string) (string, error) {
	bin := a.cfg.Bin
	if strings.HasSuffix(strings.ToLower(bin), ".js") || strings.HasSuffix(strings.ToLower(bin), ".mjs") || strings.HasSuffix(strings.ToLower(bin), ".cjs") {
		node := a.cfg.NodeBin
		if node == "" {
			node = "node"
		}
		args = append([]string{bin}, args...)
		bin = node
	}
	canonical, err := filepath.EvalSymlinks(a.cfg.Cwd)
	if err != nil {
		canonical = a.cfg.Cwd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = a.cfg.Home
	cmd.Env = childenv.ToList(childenv.Build(a.cfg.SourceEnv, canonical, childenv.AGYKeep))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func entryArgs(entry map[string]any) []string {
	var found []string
	if arr, ok := entry["args"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				found = append(found, s)
			}
		}
	}
	return found
}

func (a *Antigravity) isBridge(entry map[string]any, name string) bool {
	if entry == nil {
		return false
	}
	if disabled, _ := entry["disabled"].(bool); disabled {
		return false
	}
	command, _ := entry["command"].(string)
	args := entryArgs(entry)
	if command == "" || len(args) != 2 || args[1] != name {
		return false
	}
	if command == a.cfg.SelfExe && args[0] == "bridge" {
		return true
	}
	return filepath.Base(args[0]) == "agy-mcp-bridge.mjs"
}

func (a *Antigravity) isExactBridge(entry map[string]any, name string) bool {
	if entry == nil {
		return false
	}
	command, _ := entry["command"].(string)
	return command == a.cfg.SelfExe && fmt.Sprint(entryArgs(entry)) == fmt.Sprint(a.bridgeArgs(name))
}

func findOnPath(name string, sourceEnv map[string]string, skip []string) string {
	pathValue := sourceEnv["PATH"]
	if pathValue == "" {
		pathValue = sourceEnv["Path"]
	}
	skipSet := map[string]bool{}
	for _, s := range skip {
		if abs, err := filepath.Abs(s); err == nil {
			skipSet[abs] = true
		}
	}
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil && skipSet[abs] {
			continue
		}
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func bashWrapper(realBash, self string) string {
	nl := string([]byte{10})
	args := string([]byte{34, 36, 64, 34})
	return "#!/bin/sh" + nl +
		"if [ -n " + string([]byte{34}) + "$KOMPUTER_AGY_MCP_FILE" + string([]byte{34}) + " ]; then KOMPUTER_AGY_REAL_BASH=" + shq(realBash) + " exec " + shq(self) + " shim " + args + "; fi" + nl +
		"exec " + shq(realBash) + " " + args + nl
}

// Init creates the agy workspaces and converges global registrations.
func (a *Antigravity) Init() []string {
	var notes []string
	note := func(text string) { notes = append(notes, text) }
	_ = os.MkdirAll(a.bodyDir, 0o755)
	_ = os.MkdirAll(a.plainDir, 0o755)
	_ = os.MkdirAll(a.targetDir, 0o700)
	bridges := []string{}
	seen := map[string]bool{}
	names := []string{a.cfg.BodyMCP, "komputer_use"}
	if parsed, err := ReadMCPServers(a.cfg.MCPConfig); err == nil {
		for _, s := range parsed {
			names = append(names, s.Name)
		}
	}
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			bridges = append(bridges, name)
		}
	}
	a.bridges = bridges
	if a.remoteTools != "off" {
		_ = os.MkdirAll(a.shimDir, 0o755)
		realBash := findOnPath("bash", a.cfg.SourceEnv, []string{a.shimDir})
		if realBash == "" {
			realBash = "/bin/bash"
		}
		wrapper := filepath.Join(a.shimDir, "bash")
		if err := os.WriteFile(wrapper, []byte(bashWrapper(realBash, a.cfg.SelfExe)), 0o755); err == nil {
			_ = os.Chmod(wrapper, 0o755)
		}
		if a.remoteTools != "commands" {
			a.fuseGaps = FuseMissing(a.cfg.Python)
			a.files.Store(a.remoteTools == "all" || (a.remoteTools == "auto" && len(a.fuseGaps) == 0))
		}
	}
	home := a.cfg.Home
	hooksFile := agyHooksPath(home)
	hooks := readJSONFile(hooksFile)
	a.hooked.Store(a.isHook(mapValue(hooks.value, "komputer")))
	if a.cfg.MCPSetup != "auto" {
		a.setupNotes = notes
		return notes
	}
	if !a.hooked.Load() && hooks.ok {
		next := map[string]any{}
		for k, v := range hooks.value {
			next[k] = v
		}
		next["komputer"] = a.HookEntry()
		if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err == nil {
			if err := writeJSONFile(hooksFile, next); err == nil {
				a.hooked.Store(true)
				note("agy hooks: registered the komputer PreToolUse hook in ~/.gemini/config/hooks.json (keeps agy's own tools on the user's computer or off)")
			}
		}
	}
	current := readGlobalMcp(home)
	for _, name := range bridges {
		entry, _ := current[name].(map[string]any)
		if a.isBridge(entry, name) && a.isExactBridge(entry, name) {
			continue
		}
		addArgs := append([]string{"mcp", "add", name, "--", a.cfg.SelfExe}, a.bridgeArgs(name)...)
		out, err := a.runAgy(addArgs...)
		if err != nil {
			tail := out
			if len(tail) > 300 {
				tail = tail[len(tail)-300:]
			}
			note(fmt.Sprintf("agy MCP: %q failed: %s", "agy mcp add "+name, tail))
		} else {
			note(fmt.Sprintf("agy MCP: registered %q in ~/.gemini/config/mcp_config.json (stdio bridge to this service)", name))
		}
	}
	a.setupNotes = notes
	return notes
}

// Checks reports setup that would fail or leak silently at run time.
func (a *Antigravity) Checks(home string) []string {
	notes := append([]string{}, a.setupNotes...)
	names := append([]string{}, a.bridges...)
	if len(names) == 0 {
		seen := map[string]bool{}
		for _, name := range []string{a.cfg.BodyMCP, "komputer_use"} {
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	global := readGlobalMcp(home)
	for _, name := range names {
		entry, _ := global[name].(map[string]any)
		if !a.isBridge(entry, name) {
			notes = append(notes, fmt.Sprintf("agy MCP: %q is not the komputer bridge in ~/.gemini/config/mcp_config.json, so agy turns cannot reach it; run: agy mcp add %s -- %s bridge %s", name, name, a.cfg.SelfExe, name))
		}
	}
	globalNames := make([]string, 0, len(global))
	for name := range global {
		globalNames = append(globalNames, name)
	}
	sort.Strings(globalNames)
	for _, name := range globalNames {
		entry, _ := global[name].(map[string]any)
		known := false
		for _, n := range names {
			if n == name {
				known = true
			}
		}
		disabled, _ := entry["disabled"].(bool)
		if !known && !disabled {
			notes = append(notes, fmt.Sprintf("agy MCP: global server %q is enabled for every agy turn and runs on this host; disable it with: agy mcp disable %s", name, name))
		}
	}
	hooks := readJSONFile(agyHooksPath(home))
	hookOk := a.isHook(mapValue(hooks.value, "komputer"))
	if !hookOk {
		entryJSON, _ := json.Marshal(a.HookEntry())
		notes = append(notes, fmt.Sprintf("agy hooks: the komputer PreToolUse hook is missing from ~/.gemini/config/hooks.json, so nothing stops agy's own tools from acting on this host; add the komputer entry: %s", entryJSON))
	}
	if a.remoteTools != "off" {
		mountText := ""
		if a.files.Load() {
			mountText = fmt.Sprintf("file tools use the Body root mounted at %s", a.mount.dir)
		} else {
			mountText = "file tools are denied (the model uses run_command instead)"
			if len(a.fuseGaps) > 0 {
				mountText += "; for the mount install: " + strings.Join(a.fuseGaps, ", ")
			}
		}
		notes = append(notes, fmt.Sprintf("agy remote tools (%s): run_command runs on the user's computer through %s; %s", a.remoteTools, filepath.Join(a.shimDir, "bash"), mountText))
		if a.files.Load() {
			if _, err := os.Stat("/etc/mtab"); err != nil {
				notes = append(notes, "agy remote files: /etc/mtab is missing, so fusermount3 cannot unmount; link it: ln -s /proc/self/mounts /etc/mtab")
			}
		}
	}
	if a.cfg.Permissions == "skip" {
		if hookOk {
			notes = append(notes, "KOMPUTER_AGY_PERMISSIONS=skip passes --dangerously-skip-permissions: agy's own tools run unprompted, limited by the komputer hook")
		} else {
			notes = append(notes, "KOMPUTER_AGY_PERMISSIONS=skip passes --dangerously-skip-permissions: agy's built-in shell and file tools run unprompted on this host")
		}
		return notes
	}
	settings := readJSONFile(agySettingsPath(home))
	var allow []string
	if settings.ok {
		if perms, ok := settings.value["permissions"].(map[string]any); ok {
			allow = anyStrings(perms["allow"])
		}
	}
	hasAllow := func(want string) bool {
		for _, v := range allow {
			if v == want {
				return true
			}
		}
		return false
	}
	if a.remoteTools != "off" && !hasAllow("command(*)") {
		notes = append(notes, "agy permissions: add command(*) to permissions.allow in ~/.gemini/antigravity-cli/settings.json (or set KOMPUTER_AGY_PERMISSIONS=skip), otherwise headless agy denies run_command and remote commands never reach the user's computer")
	}
	for _, name := range names {
		if !hasAllow("mcp("+name+"/*)") && !hasAllow("mcp(*)") {
			notes = append(notes, fmt.Sprintf("agy permissions: add mcp(%s/*) to permissions.allow in ~/.gemini/antigravity-cli/settings.json, otherwise headless agy soft-denies those MCP tools", name))
		}
	}
	return notes
}
