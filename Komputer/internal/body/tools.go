package body

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Service is the Body tool service over both transports.
type Service struct {
	cfg       Config
	roots     *Roots
	processes *Processes
	nested    *Nested
	logf      func(string, ...any)

	osVersionOnce sync.Once
	osVersionText string
}

// NewService builds a Body service.
func NewService(cfg Config, logf func(string, ...any)) *Service {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Service{cfg: cfg, roots: NewRoots(cfg), logf: logf}
	s.processes = NewProcesses(cfg, cfg.MaxOutputBytes)
	s.nested = NewNested(cfg, logf)
	return s
}

// ConnectNested starts configured stdio MCP servers.
func (s *Service) ConnectNested(ctx context.Context) { s.nested.Connect(ctx) }

// Close stops processes and nested servers.
func (s *Service) Close() {
	s.processes.KillAll()
	s.nested.Close()
}

// ToolDefinition is one MCP tool offered by the Body.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

func objectSchema(properties map[string]any, required []string) json.RawMessage {
	obj := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		obj["required"] = required
	}
	data, _ := json.Marshal(obj)
	return data
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Tools returns the currently registered tool definitions.
func (s *Service) Tools() []ToolDefinition {
	rootNames := make([]string, 0, len(s.cfg.Roots))
	for name := range s.cfg.Roots {
		rootNames = append(rootNames, name)
	}
	rootProp := map[string]any{"type": "string", "description": fmt.Sprintf("Configured root name. One of: %s. Omit for '%s'.", strings.Join(rootNames, ", "), s.cfg.DefaultRoot)}
	pathProp := map[string]any{"type": "string", "description": "Absolute path, or a path relative to the selected root."}
	shellProp := map[string]any{
		"type": "string", "enum": ShellNames,
		"description": fmt.Sprintf("Shell to run it with. Omit for this computer's default (%s). Windows: cmd / powershell / pwsh / bash (Git Bash) / wsl. Other systems: sh / bash.", DefaultShellName()),
	}
	tools := []ToolDefinition{
		{Name: "system_info", Description: "Summary of the operating computer: OS, hostname, date and time zone, shells, roots, permissions, tools. Call this first in a new session.", InputSchema: objectSchema(map[string]any{}, nil)},
		{Name: "list_roots", Description: "Which roots you can reach. You can only access files under a root.", InputSchema: objectSchema(map[string]any{}, nil)},
	}
	if s.cfg.Shell {
		tools = append(tools, ToolDefinition{
			Name:        "run_command",
			Description: fmt.Sprintf("Run a shell command synchronously and wait for it (default %d seconds, max 600). Output is truncated past %d KB. For anything long-running use start_process. Commands cannot prompt for input — pass the relevant -y / --yes / --force flags.", s.cfg.DefaultTimeoutMs/1000, s.cfg.MaxOutputBytes/1024),
			InputSchema: objectSchema(map[string]any{
				"command": strProp("The command line, in the selected shell's syntax."),
				"cwd":     strProp("Working directory. Default: the selected root."),
				"root":    rootProp, "shell": shellProp,
				"timeoutMs": intProp("Timeout in milliseconds."),
			}, []string{"command"}),
		})
	}
	if s.cfg.Processes {
		tools = append(tools,
			ToolDefinition{Name: "start_process", Description: "Start a long-running or interactive process and return immediately with a processId. Read its output with read_process, type into it with write_process (menus, REPLs, installers), and end it with stop_process. With idleTimeoutMs, an unread process stops by itself after that much silence (reading renews the lease).", InputSchema: objectSchema(map[string]any{
				"command": strProp("The command line, in the selected shell's syntax."), "cwd": strProp("Working directory. Default: the selected root."), "root": rootProp, "shell": shellProp,
				"idleTimeoutMs": intProp("Stop after this many milliseconds without a read_process (max 3600000)."),
			}, []string{"command"})},
			ToolDefinition{Name: "read_process", Description: "Read a started process's output since a cursor. Feed the returned cursor back for the next chunk. Reports running/exit state. Works after the process has ended.", InputSchema: objectSchema(map[string]any{
				"processId": strProp(""), "cursor": intProp("Byte offset from a previous read (0 to read the retained window)."),
			}, []string{"processId"})},
			ToolDefinition{Name: "write_process", Description: "Type into a started process's stdin, to answer prompts. pressEnter (default true) appends Enter. First read_process to see what was asked, then write exactly what the program expects.", InputSchema: objectSchema(map[string]any{
				"processId": strProp(""), "text": strProp("Text to type."), "pressEnter": boolProp("Append Enter after the text (default true)."),
			}, []string{"processId", "text"})},
			ToolDefinition{Name: "stop_process", Description: "Stop a started process and the children it spawned.", InputSchema: objectSchema(map[string]any{"processId": strProp("")}, []string{"processId"})},
			ToolDefinition{Name: "list_processes", Description: "Processes started through start_process in this session.", InputSchema: objectSchema(map[string]any{}, nil)},
		)
	}
	if s.cfg.Files {
		tools = append(tools,
			ToolDefinition{Name: "read_file", Description: "Read a file inside a root. Text comes back whole (or as a line window), images as image content, other binaries as base64 with byteOffset/byteLength.", InputSchema: objectSchema(map[string]any{
				"path": pathProp, "root": rootProp,
				"startLine": intProp("1-based first line for a window."), "maxLines": intProp("Window size in lines (max 5000)."),
				"byteOffset": intProp("For binary files: byte offset."), "byteLength": intProp("For binary files: byte count (max 8388608)."),
			}, []string{"path"})},
			ToolDefinition{Name: "write_file", Description: "Write a whole file (creating parent folders), or append with append=true. Binary content goes in contentBase64. Refuses to overwrite unless overwrite=true.", InputSchema: objectSchema(map[string]any{
				"path": pathProp, "root": rootProp, "content": strProp("Text content."), "contentBase64": strProp("Base64-encoded binary content."),
				"append": boolProp("Append instead of replacing."), "overwrite": boolProp("Allow replacing an existing file."),
			}, []string{"path"})},
			ToolDefinition{Name: "edit_file", Description: "Replace exact text in a file. oldText must occur exactly once, unless replaceAll=true. Keeps UTF-8 and the file's line endings.", InputSchema: objectSchema(map[string]any{
				"path": pathProp, "root": rootProp, "oldText": strProp("Exact text to find."), "newText": strProp("Replacement text."), "replaceAll": boolProp("Replace every occurrence."),
			}, []string{"path", "oldText", "newText"})},
			ToolDefinition{Name: "list_directory", Description: "List one directory level with entry types, sizes and timestamps.", InputSchema: objectSchema(map[string]any{
				"path": pathProp, "root": rootProp, "limit": intProp("Max entries (default 500, max 5000)."),
			}, nil)},
			ToolDefinition{Name: "search_files", Description: "Recursively find files whose path relative to the root matches a glob pattern (e.g. **/*.go). Skips noise directories such as node_modules and .git.", InputSchema: objectSchema(map[string]any{
				"pattern": strProp("Glob pattern matched against relative paths."), "path": strProp("Subdirectory to search from."), "root": rootProp, "limit": intProp("Max results (default 200, max 1000)."),
			}, []string{"pattern"})},
			ToolDefinition{Name: "search_text", Description: "Regular-expression search over text files under a root. Returns path, line number and the trimmed line. Skips noise directories and binary files.", InputSchema: objectSchema(map[string]any{
				"pattern": strProp("Regular expression."), "path": strProp("Subdirectory to search from."), "root": rootProp, "limit": intProp("Max matches (default 200, max 1000)."), "caseSensitive": boolProp("Default false."),
			}, []string{"pattern"})},
			ToolDefinition{Name: "stat_path", Description: "Existence, type, size, mode and timestamps for one path.", InputSchema: objectSchema(map[string]any{"path": pathProp, "root": rootProp}, []string{"path"})},
		)
	}
	if nested := s.nested.Tools(); len(nested) > 0 {
		serverNames := []string{}
		seen := map[string]bool{}
		for _, t := range nested {
			if !seen[t.Server] {
				seen[t.Server] = true
				serverNames = append(serverNames, t.Server)
			}
		}
		tools = append(tools, ToolDefinition{
			Name:        "call_mcp_tool",
			Description: "Call a tool of another MCP server installed on this machine. system_info lists the servers and their tools.",
			InputSchema: objectSchema(map[string]any{
				"server": map[string]any{"type": "string", "enum": serverNames}, "tool": strProp("Tool name on that server."),
				"arguments": map[string]any{"type": "object", "description": "Arguments object for that tool."},
			}, []string{"server", "tool"}),
		})
	}
	return tools
}

// ToolResult is one MCP tools/call result.
type ToolResult struct {
	Content []json.RawMessage
	IsError bool
	Raw     json.RawMessage
}

func mustRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func textResult(v any) ToolResult {
	text, _ := json.Marshal(v)
	return ToolResult{Content: []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": string(text)})}}
}

func resultError(err error) ToolResult {
	return ToolResult{IsError: true, Content: []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": err.Error()})}}
}

type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

func invalidArg(format string, args ...any) error {
	return &argError{msg: fmt.Sprintf(format, args...)}
}

// IsArgError reports whether err is a client argument error.
func IsArgError(err error) bool {
	_, ok := err.(*argError)
	return ok
}

type argMap map[string]json.RawMessage

func parseArgs(raw json.RawMessage) (argMap, error) {
	if len(raw) == 0 {
		return argMap{}, nil
	}
	m := argMap{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, invalidArg("arguments must be an object")
	}
	return m, nil
}

func (a argMap) str(key string, required bool) (string, error) {
	raw, ok := a[key]
	if !ok {
		if required {
			return "", invalidArg("%s is required", key)
		}
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", invalidArg("%s must be a string", key)
	}
	if required && s == "" {
		return "", invalidArg("%s is required", key)
	}
	return s, nil
}

func (a argMap) integer(key string, fallback int) (int, error) {
	raw, ok := a[key]
	if !ok {
		return fallback, nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil || n != float64(int(n)) {
		return 0, invalidArg("%s must be an integer", key)
	}
	return int(n), nil
}

func (a argMap) boolean(key string, fallback bool) (bool, error) {
	raw, ok := a[key]
	if !ok {
		return fallback, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, invalidArg("%s must be a boolean", key)
	}
	return b, nil
}

func (a argMap) root() (string, error) { return a.str("root", false) }
func (a argMap) path() (string, error) { return a.str("path", true) }

func (s *Service) resolve(a argMap) (Resolved, error) {
	root, err := a.root()
	if err != nil {
		return Resolved{}, err
	}
	p, err := a.path()
	if err != nil {
		return Resolved{}, err
	}
	return s.roots.Resolve(p, root)
}

func (s *Service) resolveDir(a argMap) (Resolved, error) {
	root, err := a.root()
	if err != nil {
		return Resolved{}, err
	}
	p, err := a.str("path", false)
	if err != nil {
		return Resolved{}, err
	}
	if p == "" {
		p = "."
	}
	res, err := s.roots.Resolve(p, root)
	if err != nil {
		return res, err
	}
	info, err := os.Stat(res.Abs)
	if err != nil {
		return res, err
	}
	if !info.IsDir() {
		return res, fmt.Errorf("not a directory: %s", p)
	}
	return res, nil
}

// CallTool executes one tool by name. Argument errors are returned as
// *argError; runtime failures come back as error tool results.
func (s *Service) CallTool(ctx context.Context, name string, rawArgs json.RawMessage) (ToolResult, error) {
	args, err := parseArgs(rawArgs)
	if err != nil {
		return ToolResult{}, err
	}
	value, err := s.dispatch(ctx, name, args)
	if err != nil {
		if IsArgError(err) {
			return ToolResult{}, err
		}
		return resultError(err), nil
	}
	if result, ok := value.(ToolResult); ok {
		return result, nil
	}
	return textResult(value), nil
}

func (s *Service) dispatch(ctx context.Context, name string, a argMap) (any, error) {
	switch name {
	case "system_info":
		return s.systemInfo(), nil
	case "list_roots":
		out := map[string]any{}
		for name, dir := range s.cfg.Roots {
			out[name] = dir
		}
		return map[string]any{"roots": out, "defaultRoot": s.cfg.DefaultRoot}, nil
	case "run_command":
		return s.toolRunCommand(a)
	case "start_process":
		return s.toolStartProcess(a)
	case "read_process":
		return s.toolReadProcess(a)
	case "write_process":
		return s.toolWriteProcess(a)
	case "stop_process":
		return s.toolStopProcess(a)
	case "list_processes":
		return map[string]any{"processes": s.processes.List()}, nil
	case "read_file":
		return s.toolReadFile(a)
	case "write_file":
		return s.toolWriteFile(a)
	case "edit_file":
		return s.toolEditFile(a)
	case "list_directory":
		return s.toolListDirectory(a)
	case "search_files":
		return s.toolSearchFiles(a)
	case "search_text":
		return s.toolSearchText(a)
	case "stat_path":
		res, err := s.resolve(a)
		if err != nil {
			return nil, err
		}
		return DescribeFile(res), nil
	case "call_mcp_tool":
		return s.toolCallMCP(ctx, a)
	default:
		return nil, invalidArg("unknown tool: %s", name)
	}
}

func (s *Service) systemInfo() map[string]any {
	hostname, _ := os.Hostname()
	now := time.Now()
	_, offsetSeconds := now.Zone()
	sign := "+"
	if offsetSeconds < 0 {
		sign, offsetSeconds = "-", -offsetSeconds
	}
	offset := fmt.Sprintf("UTC%s%02d:%02d", sign, offsetSeconds/3600, offsetSeconds%3600/60)
	date := fmt.Sprintf("%s, %s, %s", now.Format("Mon"), now.Format("1/2/2006"), now.Format("3:04:05 PM"))
	s.osVersionOnce.Do(func() { s.osVersionText = osVersion() })
	shells := DetectShells("", nil, nil)
	var available []string
	for _, name := range ShellNames {
		if shells[name] != "" {
			available = append(available, name)
		}
	}
	toolNames := []string{}
	for _, t := range s.Tools() {
		toolNames = append(toolNames, t.Name)
	}
	permissions := map[string]any{"files": s.cfg.Files, "shell": s.cfg.Shell, "processes": s.cfg.Processes}
	out := map[string]any{
		"hostname": hostname, "platform": platformName(),
		"os": s.osVersionText, "arch": runtime.GOARCH, "date": date, "timeZone": now.Location().String(), "utcOffset": offset,
		"consoleEncoding": map[bool]string{true: "utf-8 (cp65001)", false: DetectConsoleEncoding()}[IsWindows && s.cfg.ConsoleUTF8],
		"defaultShell":    DefaultShellName(), "availableShells": available, "cwdRoot": s.cfg.Roots[s.cfg.DefaultRoot],
	}
	if IsWindows && s.cfg.ConsoleUTF8 {
		out["powershellUtf8"] = true
	}
	out["roots"] = s.cfg.Roots
	out["defaultRoot"] = s.cfg.DefaultRoot
	out["permissions"] = permissions
	out["limits"] = map[string]any{
		"maxOutputBytes": s.cfg.MaxOutputBytes, "maxFileBytes": s.cfg.MaxFileBytes,
		"defaultTimeoutMs": s.cfg.DefaultTimeoutMs, "maxTimeoutMs": 600000,
	}
	out["tools"] = toolNames
	out["computerTools"] = []string{"run_command", "start_process", "read_process", "write_process", "stop_process"}
	if nested := s.nested.Tools(); len(nested) > 0 {
		names := make([]string, 0, len(nested))
		for _, t := range nested {
			names = append(names, t.Server+":"+t.Name)
		}
		out["nestedMcpTools"] = names
	}
	return out
}

func platformName() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func (s *Service) toolRunCommand(a argMap) (any, error) {
	command, err := a.str("command", true)
	if err != nil {
		return nil, err
	}
	shell, err := a.str("shell", false)
	if err != nil {
		return nil, err
	}
	if shell == "" {
		shell = "default"
	}
	if !containsString(ShellNames, shell) {
		return nil, invalidArg("shell must be one of %s", strings.Join(ShellNames, ", "))
	}
	timeout, err := a.integer("timeoutMs", s.cfg.DefaultTimeoutMs)
	if err != nil {
		return nil, err
	}
	if timeout < 1 || timeout > 600000 {
		return nil, invalidArg("timeoutMs must be between 1 and 600000")
	}
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	cwdArg, err := a.str("cwd", false)
	if err != nil {
		return nil, err
	}
	var res Resolved
	if cwdArg == "" {
		res, err = s.roots.Resolve(".", root)
	} else {
		res, err = s.roots.Resolve(cwdArg, root)
	}
	if err != nil {
		return nil, err
	}
	return RunCommand(s.cfg, command, res.Abs, shell, timeout)
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func (s *Service) toolStartProcess(a argMap) (any, error) {
	command, err := a.str("command", true)
	if err != nil {
		return nil, err
	}
	shell, err := a.str("shell", false)
	if err != nil {
		return nil, err
	}
	if shell == "" {
		shell = "default"
	}
	if !containsString(ShellNames, shell) {
		return nil, invalidArg("shell must be one of %s", strings.Join(ShellNames, ", "))
	}
	idle, err := a.integer("idleTimeoutMs", 0)
	if err != nil {
		return nil, err
	}
	if idle < 0 || idle > 3600000 {
		return nil, invalidArg("idleTimeoutMs must be between 0 and 3600000")
	}
	root, err := a.root()
	if err != nil {
		return nil, err
	}
	cwdArg, err := a.str("cwd", false)
	if err != nil {
		return nil, err
	}
	var res Resolved
	if cwdArg == "" {
		res, err = s.roots.Resolve(".", root)
	} else {
		res, err = s.roots.Resolve(cwdArg, root)
	}
	if err != nil {
		return nil, err
	}
	return s.processes.Start(command, res.Abs, shell, idle)
}

func (s *Service) toolReadProcess(a argMap) (any, error) {
	id, err := a.str("processId", true)
	if err != nil {
		return nil, err
	}
	cursor, err := a.integer("cursor", 0)
	if err != nil {
		return nil, err
	}
	if cursor < 0 {
		return nil, invalidArg("cursor must be non-negative")
	}
	maxBytes, err := a.integer("maxBytes", s.cfg.MaxOutputBytes)
	if err != nil {
		return nil, err
	}
	return s.processes.Read(id, cursor, maxBytes)
}

func (s *Service) toolWriteProcess(a argMap) (any, error) {
	id, err := a.str("processId", true)
	if err != nil {
		return nil, err
	}
	text, err := a.str("text", true)
	if err != nil {
		return nil, err
	}
	if len(text) > 100000 {
		return nil, invalidArg("text is too long")
	}
	pressEnter, err := a.boolean("pressEnter", true)
	if err != nil {
		return nil, err
	}
	written, err := s.processes.Write(id, text, pressEnter)
	if err != nil {
		return nil, err
	}
	rec := s.processes.get(id)
	running := false
	if rec != nil {
		rec.mu.Lock()
		running = !rec.finished
		rec.mu.Unlock()
	}
	return map[string]any{"written": written, "running": running}, nil
}

func (s *Service) toolStopProcess(a argMap) (any, error) {
	id, err := a.str("processId", true)
	if err != nil {
		return nil, err
	}
	return s.processes.Stop(id)
}

func (s *Service) toolReadFile(a argMap) (any, error) {
	res, err := s.resolve(a)
	if err != nil {
		return nil, err
	}
	startLine, err := a.integer("startLine", 1)
	if err != nil {
		return nil, err
	}
	maxLines, err := a.integer("maxLines", 2000)
	if err != nil {
		return nil, err
	}
	byteOffset, err := a.integer("byteOffset", 0)
	if err != nil {
		return nil, err
	}
	byteLength, err := a.integer("byteLength", 0)
	if err != nil {
		return nil, err
	}
	if startLine < 1 || maxLines < 1 || maxLines > 5000 || byteOffset < 0 || byteLength < 0 || byteLength > 8388608 {
		return nil, invalidArg("read_file window arguments are out of range")
	}
	info, err := os.Stat(res.Abs)
	if err != nil {
		return nil, err
	}
	if mime := ImageMime(res.Abs); mime != "" {
		data, err := EncodeImage(res.Abs, 5*1024*1024)
		if err != nil {
			if strings.Contains(err.Error(), "image larger") {
				return nil, err
			}
			return nil, err
		}
		return ToolResult{Content: []json.RawMessage{mustRaw(map[string]any{"type": "image", "data": data, "mimeType": mime})}}, nil
	}
	head, size, err := ReadBytesWindow(res.Abs, 0, 4096, s.cfg.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	_ = size
	if !isTextBytes(head) {
		if byteLength == 0 {
			return ToolResult{Content: []json.RawMessage{mustRaw(map[string]any{
				"type": "text",
				"text": string(mustRaw(map[string]any{"path": res.Rel, "size": info.Size(), "binary": true})),
			})}}, nil
		}
		window, totalSize, err := ReadBytesWindow(res.Abs, byteOffset, byteLength, s.cfg.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		text := string(mustRaw(map[string]any{
			"path": res.Rel, "size": totalSize, "binary": true,
			"byteOffset": min(int64(byteOffset), totalSize), "bytesReturned": len(window),
			"content": encodeBase64(window),
		}))
		return ToolResult{Content: []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": text})}}, nil
	}
	window, err := ReadTextWindow(res.Abs, startLine, maxLines, s.cfg.MaxFileBytes, s.cfg)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"path": res.Rel, "size": info.Size(), "encoding": window.Encoding,
		"totalLines": window.TotalLines, "startLine": window.StartLine, "endLine": window.EndLine,
		"hasMore": window.EndLine < window.TotalLines, "content": window.Content,
	}
	if window.Truncated {
		result["truncated"] = true
	}
	return result, nil
}

func encodeBase64(data []byte) string {
	const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(data); i += 3 {
		var b [3]byte
		n := copy(b[:], data[i:])
		out.WriteByte(digits[b[0]>>2])
		out.WriteByte(digits[(b[0]&3)<<4|(b[1]>>4)])
		if n > 1 {
			out.WriteByte(digits[(b[1]&15)<<2|(b[2]>>6)])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(digits[b[2]&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
func (s *Service) toolWriteFile(a argMap) (any, error) {
	res, err := s.resolve(a)
	if err != nil {
		return nil, err
	}
	content, _ := a.str("content", false)
	contentB64, _ := a.str("contentBase64", false)
	hasText, hasB64 := false, false
	if _, ok := a["content"]; ok {
		hasText = true
	}
	if _, ok := a["contentBase64"]; ok {
		hasB64 = true
	}
	if hasText == hasB64 {
		return nil, invalidArg("give exactly one of content or contentBase64")
	}
	appendMode, err := a.boolean("append", false)
	if err != nil {
		return nil, err
	}
	overwrite, err := a.boolean("overwrite", false)
	if err != nil {
		return nil, err
	}
	var data []byte
	if hasB64 {
		data, err = decodeBase64(contentB64)
		if err != nil {
			return nil, invalidArg("contentBase64 is not valid base64")
		}
		if len(data) > s.cfg.MaxFileBytes {
			return nil, fmt.Errorf("contentBase64 larger than %d bytes", s.cfg.MaxFileBytes)
		}
	} else {
		if len([]rune(content)) > 12_000_000 {
			return nil, invalidArg("content is too long")
		}
		data = []byte(content)
	}
	exists := false
	if _, err := os.Stat(res.Abs); err == nil {
		exists = true
	}
	if exists && !overwrite && !appendMode {
		return nil, fmt.Errorf("file already exists (pass overwrite=true to replace it): %s", res.Rel)
	}
	if err := os.MkdirAll(filepath.Dir(res.Abs), 0o755); err != nil {
		return nil, err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(res.Abs, flags, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return map[string]any{"written": res.Rel, "bytes": len(data), "overwritten": overwrite, "appended": appendMode}, nil
}

func (s *Service) toolEditFile(a argMap) (any, error) {
	res, err := s.resolve(a)
	if err != nil {
		return nil, err
	}
	oldText, err := a.str("oldText", true)
	if err != nil {
		return nil, err
	}
	newText, err := a.str("newText", false)
	if err != nil {
		return nil, err
	}
	// newText may legitimately be empty (a deletion); presence was enforced
	// by the required-field check in the schema, mirrored here.
	if _, ok := a["newText"]; !ok {
		return nil, invalidArg("newText is required")
	}
	replaceAll, err := a.boolean("replaceAll", false)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(res.Abs)
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(s.cfg.MaxFileBytes) {
		return nil, fmt.Errorf("file is %d bytes, over the %d byte limit", info.Size(), s.cfg.MaxFileBytes)
	}
	raw, err := os.ReadFile(res.Abs)
	if err != nil {
		return nil, err
	}
	if !isTextBytes(raw) {
		return nil, fmt.Errorf("binary file: %s", res.Rel)
	}
	enc := "utf-8"
	if IsWindows && !s.cfg.ConsoleUTF8 {
		enc = DetectConsoleEncoding()
	}
	decoded := DecodeOutput(raw, enc)
	text := decoded.Text
	lineEnding := string([]byte{10})
	if strings.Contains(text, string([]byte{13, 10})) {
		lineEnding = string([]byte{13, 10})
	}
	normalized := strings.ReplaceAll(text, string([]byte{13, 10}), string([]byte{10}))
	normalized = strings.ReplaceAll(normalized, string([]byte{13}), string([]byte{10}))
	oldNorm := strings.ReplaceAll(strings.ReplaceAll(oldText, string([]byte{13, 10}), string([]byte{10})), string([]byte{13}), string([]byte{10}))
	newNorm := strings.ReplaceAll(strings.ReplaceAll(newText, string([]byte{13, 10}), string([]byte{10})), string([]byte{13}), string([]byte{10}))
	count := strings.Count(normalized, oldNorm)
	if count == 0 {
		return nil, fmt.Errorf("oldText not found in %s", res.Rel)
	}
	if count > 1 && !replaceAll {
		return nil, fmt.Errorf("oldText occurs %d times; make it unique or pass replaceAll=true", count)
	}
	atByte := strings.Index(normalized, oldNorm)
	replacedAt := UTF16Index(normalized, atByte)
	var updated string
	replacements := 1
	if replaceAll {
		updated = strings.ReplaceAll(normalized, oldNorm, newNorm)
		replacements = count
	} else {
		updated = normalized[:atByte] + newNorm + normalized[atByte+len(oldNorm):]
	}
	if lineEnding == string([]byte{13, 10}) {
		updated = strings.ReplaceAll(updated, string([]byte{10}), string([]byte{13, 10}))
	}
	if decoded.Encoding != "utf-8" {
		return nil, fmt.Errorf("edit_file supports UTF-8 files only; this file decoded as %s (use write_file to rewrite it)", decoded.Encoding)
	}
	if err := os.WriteFile(res.Abs, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"edited": res.Rel, "replacedAt": replacedAt, "replacements": replacements, "bytes": len([]byte(updated))}, nil
}

func (s *Service) toolListDirectory(a argMap) (any, error) {
	res, err := s.resolveDir(a)
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 500)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 5000 {
		return nil, invalidArg("limit must be between 1 and 5000")
	}
	f, err := os.Open(res.Abs)
	if err != nil {
		return nil, err
	}
	entries, _ := f.ReadDir(-1)
	f.Close()
	outEntries := []any{}
	for _, entry := range entries {
		if len(outEntries) >= limit {
			break
		}
		child := Resolved{Root: res.Root, RootName: res.RootName, Abs: filepath.Join(res.Abs, entry.Name())}
		if rel, err := filepath.Rel(res.Root, child.Abs); err == nil {
			child.Rel = rel
		}
		if entry.Type()&os.ModeSymlink != 0 {
			mode := 0
			outEntries = append(outEntries, map[string]any{
				"name": entry.Name(), "path": child.Rel, "type": "symlink", "exists": true,
				"size": nil, "mode": mode, "modified": nil, "created": nil,
			})
			continue
		}
		st := StatPath(child)
		outEntries = append(outEntries, map[string]any{
			"name": entry.Name(), "path": child.Rel, "type": valueOr(st.Type, "other"), "exists": st.Exists,
			"size": st.Size, "mode": st.Mode, "modified": st.Modified, "created": st.Created,
		})
	}
	return map[string]any{
		"path": res.Rel, "root": res.RootName, "entries": outEntries,
		"total": len(entries), "truncated": len(entries) > limit,
	}, nil
}

func valueOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func (s *Service) toolSearchFiles(a argMap) (any, error) {
	pattern, err := a.str("pattern", true)
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 200)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, invalidArg("limit must be between 1 and 1000")
	}
	res, err := s.resolveDir(a)
	if err != nil {
		return nil, err
	}
	re := GlobPattern(pattern)
	matches := []any{}
	WalkFiles(res.Abs, func(abs, rel string, isDir bool) bool {
		if isDir {
			return true
		}
		slash := filepath.ToSlash(rel)
		if re.MatchString(slash) {
			child := Resolved{Root: res.Root, RootName: res.RootName, Abs: abs}
			if r, err := filepath.Rel(res.Root, abs); err == nil {
				child.Rel = r
			}
			st := StatPath(child)
			var size any
			if st.Size != nil {
				size = *st.Size
			}
			matches = append(matches, map[string]any{"path": slash, "size": size})
		}
		return len(matches) < limit
	})
	return map[string]any{"root": res.RootName, "pattern": pattern, "matches": matches, "truncated": len(matches) >= limit}, nil
}

func (s *Service) toolSearchText(a argMap) (any, error) {
	pattern, err := a.str("pattern", true)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(pattern)) < 2 {
		return nil, invalidArg("pattern must be at least 2 characters")
	}
	caseSensitive, err := a.boolean("caseSensitive", false)
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 200)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, invalidArg("limit must be between 1 and 1000")
	}
	res, err := s.resolveDir(a)
	if err != nil {
		return nil, err
	}
	expr := pattern
	if !caseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, invalidArg("invalid pattern: %s", err)
	}
	matches := []any{}
	WalkFiles(res.Abs, func(abs, rel string, isDir bool) bool {
		if isDir {
			return true
		}
		remaining := limit - len(matches)
		if remaining <= 0 {
			return false
		}
		for _, hit := range SearchText(abs, filepath.ToSlash(rel), re, remaining, s.cfg) {
			matches = append(matches, hit)
		}
		return len(matches) < limit
	})
	return map[string]any{"root": res.RootName, "pattern": pattern, "matches": matches, "truncated": len(matches) >= limit}, nil
}

func (s *Service) toolCallMCP(ctx context.Context, a argMap) (any, error) {
	server, err := a.str("server", true)
	if err != nil {
		return nil, err
	}
	tool, err := a.str("tool", true)
	if err != nil {
		return nil, err
	}
	args := a["arguments"]
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := s.nested.Call(ctx, server, tool, args)
	if err != nil {
		return nil, err
	}
	return ToolResult{Raw: result}, nil
}
