package backend

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// AGYPolicy is written per turn next to the MCP bridge target file.
type AGYPolicy struct {
	Mode       string `json:"mode"`
	Root       string `json:"root,omitempty"`
	Mount      string `json:"mount,omitempty"`
	Guard      string `json:"guard,omitempty"`
	Commands   bool   `json:"commands,omitempty"`
	Files      bool   `json:"files,omitempty"`
	Hooked     bool   `json:"hooked,omitempty"`
	Background bool   `json:"background,omitempty"`
}

var hostFreeTools = map[string]bool{
	"call_mcp_tool": true, "ask_question": true, "ask_permission": true,
	"ask_custom_permission": true, "list_permissions": true, "finish": true,
	"wait": true, "wait_5_seconds": true, "manage_task": true,
	"list_resources": true, "read_resource": true,
}

var agyCommandTools = map[string]bool{"run_command": true, "command_status": true, "send_command_input": true}

var agyFileTools = map[string]bool{
	"view_file": true, "read_file": true, "list_dir": true, "grep_search": true,
	"find_by_name": true, "write_to_file": true, "write_file": true,
	"replace_file_content": true, "multi_replace_file_content": true,
	"edit_file": true, "sed_file": true, "notebook_edit": true,
}

var agyReadTools = map[string]bool{
	"view_file": true, "read_file": true, "list_dir": true, "grep_search": true, "find_by_name": true,
}

var agyRemoteOK = map[string]bool{
	"search_web": true, "read_url_content": true, "generate_image": true,
	"invoke_subagent": true, "define_subagent": true, "manage_subagents": true,
	"send_message": true, "schedule": true, "run_workflow": true,
	"manage_inbox": true, "delete_knowledge": true, "search_marketplace": true, "list_plugin_accounts": true,
}

var hostOnlyPattern = regexp.MustCompile(`^(browser_|capture_browser_|click_browser_|execute_browser_|list_browser_|open_browser_|read_browser_|notebook_execution$)`)

var agyPathArg = regexp.MustCompile(`(?i)(path|file|dir|directory|cwd|folder)$`)

func pathArgs(args any, key string) [][2]any {
	m, ok := args.(map[string]any)
	if !ok {
		return nil
	}
	var out [][2]any
	for k, v := range m {
		if !agyPathArg.MatchString(k) {
			continue
		}
		switch value := v.(type) {
		case string:
			out = append(out, [2]any{k, value})
		case []any:
			allStrings := true
			for _, item := range value {
				if _, ok := item.(string); !ok {
					allStrings = false
				}
			}
			if allStrings {
				out = append(out, [2]any{k, value})
			}
		}
	}
	return out
}

type pathStyle interface {
	sep() string
	join(parts ...string) string
	resolve(p string) string
	relative(from, to string) string
	isAbs(p string) bool
}

func agyStyleOf(p string) pathStyle {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == 92) {
		return winStyle{}
	}
	if strings.HasPrefix(p, string([]byte{92, 92})) {
		return winStyle{}
	}
	return posixStyle{}
}

type posixStyle struct{}

func (posixStyle) sep() string { return "/" }
func (posixStyle) join(parts ...string) string {
	return path.Join(parts...)
}
func (posixStyle) resolve(p string) string {
	if !path.IsAbs(p) {
		p = "/" + p
	}
	return path.Clean(p)
}
func (posixStyle) relative(from, to string) string {
	fromParts := strings.Split(strings.Trim(path.Clean(from), "/"), "/")
	toParts := strings.Split(strings.Trim(path.Clean(to), "/"), "/")
	if fromParts[0] == "" {
		fromParts = nil
	}
	if toParts[0] == "" {
		toParts = nil
	}
	i := 0
	for i < len(fromParts) && i < len(toParts) && fromParts[i] == toParts[i] {
		i++
	}
	parts := []string{}
	for j := i; j < len(fromParts); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, toParts[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}
func (posixStyle) isAbs(p string) bool { return path.IsAbs(p) }

type winStyle struct{}

func (winStyle) sep() string { return string([]byte{92}) }
func (winStyle) join(parts ...string) string {
	converted := make([]string, len(parts))
	for i, p := range parts {
		converted[i] = strings.ReplaceAll(p, "/", string([]byte{92}))
	}
	return filepath.Join(converted...)
}
func (winStyle) resolve(p string) string {
	p = strings.ReplaceAll(p, "/", string([]byte{92}))
	if !filepath.IsAbs(p) {
		p = "C:" + string([]byte{92}) + p
	}
	return filepath.Clean(p)
}
func (winStyle) relative(from, to string) string {
	rel, _ := filepath.Rel(strings.ReplaceAll(from, "/", string([]byte{92})), strings.ReplaceAll(to, "/", string([]byte{92})))
	return rel
}
func (winStyle) isAbs(p string) bool {
	return filepath.IsAbs(strings.ReplaceAll(p, "/", string([]byte{92})))
}

func agyIsAbsLike(p string) bool {
	return strings.HasPrefix(p, "~") || agyStyleOf(p).isAbs(p)
}

func agyWithin(p, dir string) bool {
	style := agyStyleOf(dir)
	pp := style.resolve(p)
	dd := style.resolve(dir)
	return pp == dd || strings.HasPrefix(pp, strings.TrimSuffix(dd, style.sep())+style.sep())
}

func agyExpand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || (len(p) > 1 && p[0] == '~' && p[1] == 92) {
		return agyStyleOf(home).join(home, p[2:])
	}
	return p
}

func realish(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

type hookInput struct {
	tool     string
	args     any
	active   bool
	artifact string
	conv     string
}

func parseHookInput(input map[string]any) hookInput {
	call, _ := input["toolCall"].(map[string]any)
	name, _ := call["name"].(string)
	tool := name
	if i := strings.LastIndex(tool, "/"); i >= 0 {
		tool = tool[i+1:]
	}
	status, _ := input["executionStatus"].(string)
	artifact, _ := input["artifactDirectoryPath"].(string)
	conv, _ := input["conversationId"].(string)
	return hookInput{tool: tool, args: call["args"], active: status == "" || status == "EXECUTION_STATUS_ACTIVE", artifact: artifact, conv: conv}
}

func hookValues(input hookInput) []any {
	var values []any
	for _, pair := range pathArgs(input.args, "") {
		switch v := pair[1].(type) {
		case []any:
			values = append(values, v...)
		default:
			values = append(values, v)
		}
	}
	return values
}

func readsMCPSchemas(input hookInput, home string) bool {
	if !agyReadTools[input.tool] {
		return false
	}
	dir := realish(agyStyleOf(home).join(home, ".gemini", "antigravity-cli", "mcp"))
	values := hookValues(input)
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		s, _ := v.(string)
		if !agyIsAbsLike(s) {
			return false
		}
		if !agyWithin(realish(agyStyleOf(dir).resolve(agyExpand(s, home))), dir) {
			return false
		}
	}
	return true
}

func deny(reason string) map[string]any {
	return map[string]any{"decision": "deny", "reason": reason}
}

// DecideHook evaluates one PreToolUse call in island mode. Remote and local
// modes are decided by DecideForPolicy.
func DecideHook(input map[string]any, own []string, home string) map[string]any {
	parsed := parseHookInput(input)
	if parsed.tool == "" {
		return nil
	}
	return decideParsed(parsed, &AGYPolicy{Mode: "island"}, own, home)
}

func decideParsed(input hookInput, policy *AGYPolicy, own []string, home string) map[string]any {
	name := input.tool
	if name == "" || policy == nil || policy.Mode == "local" || hostFreeTools[name] || readsMCPSchemas(input, home) {
		return nil
	}
	if policy.Mode != "remote" {
		return deny(name + " is not available in this conversation: agy's built-in tools would act on the service host. Use the provided tools instead.")
	}
	if hostOnlyPattern.MatchString(name) {
		return deny(name + " would run on the service host, not on the user's computer, so it is not available here.")
	}
	if agyCommandTools[name] {
		if !policy.Commands {
			return deny(name + " is not available here.")
		}
		return agyCheckPaths(input, policy, own, home)
	}
	if agyFileTools[name] {
		if policy.Files {
			return agyCheckPaths(input, policy, own, home)
		}
		values := hookValues(input)
		if len(values) > 0 {
			allOwn := true
			for _, v := range values {
				s, _ := v.(string)
				if !agyIsAbsLike(s) {
					allOwn = false
					break
				}
				inside := false
				for _, d := range own {
					if agyWithin(agyExpand(s, home), d) {
						inside = true
					}
				}
				if !inside {
					allOwn = false
					break
				}
			}
			if allOwn {
				return nil
			}
		}
		return deny("The user's files are on their computer, which " + name + " cannot reach here. Use run_command (it runs on the user's computer), for example cat, ls, sed or a here-document, or the komputer_use tools.")
	}
	if agyRemoteOK[name] {
		return nil
	}
	return deny(name + " is not available in this conversation.")
}

func parsedArtifact(input hookInput, own []string) []string { return own }

func alignedOwn(own []string, input hookInput, home string) bool { return false }

func agyCheckPaths(input hookInput, policy *AGYPolicy, own []string, home string) map[string]any {
	args, _ := input.args.(map[string]any)
	overwrite := map[string]any{}
	fix := func(raw string) (string, bool) {
		if raw == "" || !agyIsAbsLike(raw) {
			return raw, true
		}
		expanded := agyExpand(raw, home)
		style := agyStyleOf(firstNonEmpty(policy.Mount, expanded))
		p := style.resolve(expanded)
		if policy.Mount != "" && agyWithin(p, policy.Mount) {
			if p == raw {
				return raw, true
			}
			return p, true
		}
		for _, d := range own {
			if agyWithin(p, d) {
				return raw, true
			}
		}
		if policy.Mount != "" && policy.Root != "" && agyStyleOf(policy.Root).isAbs(policy.Root) && agyWithin(p, policy.Root) {
			rm := agyStyleOf(policy.Root)
			rel := rm.relative(rm.resolve(policy.Root), rm.resolve(expanded))
			parts := []string{}
			for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == 92 }) {
				parts = append(parts, part)
			}
			return style.join(append([]string{policy.Mount}, parts...)...), true
		}
		return "", false
	}
	for _, pair := range pathArgs(args, "") {
		key, _ := pair[0].(string)
		values := []any{}
		if arr, ok := pair[1].([]any); ok {
			values = arr
		} else {
			values = []any{pair[1]}
		}
		fixed := []any{}
		bad := ""
		hasBad := false
		for _, v := range values {
			s, _ := v.(string)
			f, ok := fix(s)
			if !ok {
				bad, hasBad = s, true
			}
			fixed = append(fixed, f)
		}
		if hasBad {
			return deny(bad + " is on the service host, outside the user's workspace at " + policy.Mount + "; " + input.tool + " can only use paths inside it.")
		}
		changed := false
		for i := range values {
			if fixed[i] != values[i] {
				changed = true
			}
		}
		if changed {
			if _, isArray := pair[1].([]any); isArray {
				overwrite[key] = fixed
			} else {
				overwrite[key] = fixed[0]
			}
		}
	}
	if len(overwrite) > 0 {
		return map[string]any{"decision": "allow", "overwrite": overwrite}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// DecideForPolicy is the mode-aware hook decision used by the hook binary
// and by the parser's built-in tool guard.
func DecideForPolicy(input map[string]any, policy *AGYPolicy, home string) map[string]any {
	parsed := parseHookInput(input)
	if parsed.tool == "" || policy == nil {
		return nil
	}
	return decideParsed(parsed, policy, ownDirs(input, home), home)
}

func ownDirs(input map[string]any, home string) []string {
	parsed := parseHookInput(input)
	var out []string
	if parsed.artifact != "" {
		out = append(out, agyStyleOf(parsed.artifact).resolve(parsed.artifact))
	}
	if parsed.conv != "" {
		p := agyStyleOf(home).join(home, ".gemini", "antigravity-cli", "brain", parsed.conv)
		out = append(out, agyStyleOf(p).resolve(p))
	}
	return out
}

// RunAGYHook executes the hook subcommand. No policy or no verdict prints
// nothing: agy treats empty output as no opinion.
func RunAGYHook(stdin io.Reader, stdout io.Writer, getenv func(string) string, home string) int {
	data, _ := io.ReadAll(stdin)
	var input map[string]any
	_ = json.Unmarshal(data, &input)
	file := ""
	if getenv != nil {
		file = getenv("KOMPUTER_AGY_MCP_FILE")
	}
	var policy *AGYPolicy
	if file != "" {
		if raw, err := os.ReadFile(file); err == nil {
			var target AGYBridgeTarget
			if err := json.Unmarshal(raw, &target); err == nil {
				policy = target.Policy
			}
		}
	}
	if input != nil && policy != nil {
		if verdict := DecideForPolicy(input, policy, home); verdict != nil {
			encoded, _ := json.Marshal(verdict)
			_, _ = stdout.Write(encoded)
		}
	}
	return 0
}
