package backend

import (
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Rewrite pieces between where agy runs and the caller's machine.

type streamRewriter struct {
	from, to string
	carry    string
}

// StreamRewriter creates a chunk-boundary-safe path rewriter.
func StreamRewriter(from, to string) *streamRewriter {
	return &streamRewriter{from: from, to: to}
}

func (r *streamRewriter) Push(s string) string {
	if r.from == "" {
		return s
	}
	text := r.carry + s
	keep := 0
	maxKeep := min(len(text), len(r.from))
	for k := maxKeep; k > 0; k-- {
		if strings.HasPrefix(r.from, text[len(text)-k:]) {
			keep = k
			break
		}
	}
	r.carry = text[len(text)-keep:]
	return strings.ReplaceAll(text[:len(text)-keep], r.from, r.to)
}

// Flush releases any carried bytes.
func (r *streamRewriter) Flush() string {
	if r.from == "" {
		return ""
	}
	out := strings.ReplaceAll(r.carry, r.from, r.to)
	r.carry = ""
	return out
}

// AGYToRemote maps a bash token between the brain mount and caller roots.
func AGYToRemote(token, mount, root string) string {
	if token == mount {
		return root
	}
	if strings.HasPrefix(token, mount+"/") {
		if strings.HasSuffix(root, "/") {
			return root + token[len(mount)+1:]
		}
		return root + token[len(mount):]
	}
	if token == root {
		return token
	}
	if root != "" && strings.HasPrefix(token, root) && !strings.HasPrefix(root, "/") {
		return token // A Windows root is already the real machine's path.
	}
	return token
}

// AGYRemoteCwd maps a brain cwd to a Body-relative cwd.
func AGYRemoteCwd(cwd, mount string) string {
	if cwd == mount {
		return "."
	}
	if strings.HasPrefix(cwd, mount+"/") {
		return cwd[len(mount)+1:]
	}
	return cwd
}

var leadingEnvRun = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*=(?:'[^']*'|"[^"]*"|[^\s]+)\s+)+`)

// AGYLastCommand returns what a `bash -c` line will actually run: `a && b`
// makes b the command whose exit status the caller sees.
func AGYLastCommand(line string) string {
	parts := splitCommandLine(line)
	if len(parts) == 0 {
		return ""
	}
	return leadingEnvRun.ReplaceAllString(strings.TrimSpace(parts[len(parts)-1]), "")
}

// AGYIsBashism reports constructs that make a command unsafe to reword.
func AGYIsBashism(command string) bool {
	for _, marker := range []string{"$("} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	backtick := string([]byte{96})
	if strings.Contains(command, backtick) {
		return true
	}
	for _, marker := range []string{"$?", "$@", "[[", "<<<", "pushd", "popd", "cd ", "source ", "export "} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	// A pipeline is only bash-shaped when its last stage runs locally by
	// name; a single command with no shell grammar travels as-is.
	return strings.Contains(command, "|") || strings.Contains(command, ">>") || strings.Contains(command, "2>")
}

func splitCommandLine(line string) []string {
	parts := []string{}
	var current strings.Builder
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			current.WriteByte(c)
			escaped = false
			continue
		}
		if c == 92 && quote == 0 {
			escaped = true
			current.WriteByte(c)
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			current.WriteByte(c)
			continue
		}
		switch c {
		case 39, '"':
			quote = c
			current.WriteByte(c)
		case ';':
			parts = append(parts, current.String())
			current.Reset()
		case '&', '|':
			if i+1 < len(line) && line[i+1] == c {
				parts = append(parts, current.String())
				current.Reset()
				i++
			} else {
				current.WriteByte(c)
			}
		default:
			current.WriteByte(c)
		}
	}
	parts = append(parts, current.String())
	return parts
}

const shimFirstPollMs = 300

// RunAGYShim executes the shim subcommand and returns its exit code.
func RunAGYShim(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, cwd string) int {
	file := ""
	if getenv != nil {
		file = getenv("KOMPUTER_AGY_MCP_FILE")
	}
	target := LoadAGYTarget(file)
	var remote *AGYRemote
	if target != nil {
		remote = target.Remote
	}
	if remote == nil || remote.Server == nil || remote.Server.URL == "" {
		bash := ""
		if getenv != nil {
			bash = getenv("KOMPUTER_AGY_REAL_BASH")
		}
		if bash == "" {
			found, err := exec.LookPath("bash")
			if err != nil {
				_, _ = io.WriteString(stderr, "komputer bash shim: bash not found"+string([]byte{10}))
				return 127
			}
			bash = found
		}
		cmd := exec.CommandContext(ctx, bash, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			_, _ = io.WriteString(stderr, err.Error()+string([]byte{10}))
			return 1
		}
		return 0
	}
	server := remote.Server
	client := NewMCPHTTPClient(*server)
	if remote.Root == "" {
		if info, err := client.CallTool(ctx, "system_info", map[string]any{}); err == nil {
			if m, ok := info.(map[string]any); ok {
				if cwdRoot, ok := m["cwdRoot"].(map[string]any); ok {
					remote.Root, _ = cwdRoot["path"].(string)
				}
			}
		}
	}
	command := ""
	if len(args) > 0 {
		command = args[len(args)-1]
	}
	toRemote := func(text string) string {
		if remote.Mount == "" {
			return text
		}
		fields := strings.FieldsFunc(text, func(r rune) bool {
			return r == ' ' || r == 9 || r == 10 || r == 39 || r == '"' || r == '=' || r == ':' || r == ','
		})
		for _, token := range fields {
			mapped := AGYToRemote(token, remote.Mount, remote.Root)
			if mapped != token {
				text = strings.ReplaceAll(text, token, mapped)
			}
		}
		return text
	}
	rewriter := StreamRewriter(remote.Root, remote.Mount)
	emit := func(text string) {
		if text != "" {
			_, _ = io.WriteString(stdout, rewriter.Push(text))
		}
	}
	finish := func() {
		if tail := rewriter.Flush(); tail != "" {
			_, _ = io.WriteString(stdout, tail)
		}
	}
	call := func(name string, arguments any) (map[string]any, error) {
		result, err := client.CallTool(ctx, name, arguments)
		if err != nil {
			return nil, err
		}
		m, _ := result.(map[string]any)
		return m, nil
	}
	failed := func(err error) int {
		_, _ = io.WriteString(stderr, "komputer: command failed to start on your computer: "+err.Error()+string([]byte{10}))
		finish()
		return 125
	}
	started, err := call("start_process", map[string]any{
		"command": toRemote(command), "cwd": AGYRemoteCwd(cwd, remote.Mount),
		"shell": remoteShell(remote), "idleTimeoutMs": 600000,
	})
	if err != nil || started == nil || started["processId"] == nil {
		if err == nil {
			err = &Failure{Message: "start_process returned no process id"}
		}
		return failed(err)
	}
	id, _ := started["processId"].(string)
	outCursor, errCursor := 0, 0
	deadline := time.Now().Add(610 * time.Second)
	for {
		read, err := call("read_process", map[string]any{"processId": id, "stdoutCursor": outCursor, "stderrCursor": errCursor})
		if err != nil {
			return failed(err)
		}
		if s, ok := read["stdout"].(string); ok {
			emit(s)
		}
		if s, ok := read["stderr"].(string); ok {
			emit(s)
		}
		if n, ok := read["stdoutCursor"].(float64); ok {
			outCursor = int(n)
		}
		if n, ok := read["stderrCursor"].(float64); ok {
			errCursor = int(n)
		}
		running, _ := read["running"].(bool)
		if !running {
			finish()
			if read["idleStopped"] == true {
				_, _ = io.WriteString(stderr, "komputer: the command was stopped after 10 minutes without being read; its later output is lost."+string([]byte{10}))
			}
			if code, ok := read["exitCode"].(float64); ok {
				return int(code)
			}
			return 1
		}
		if time.Now().After(deadline) {
			_, _ = call("stop_process", map[string]any{"processId": id})
			_, _ = io.WriteString(stderr, "komputer: command timed out after 10 minutes."+string([]byte{10}))
			finish()
			return 124
		}
		select {
		case <-ctx.Done():
			_, _ = call("stop_process", map[string]any{"processId": id})
			finish()
			return 130
		case <-time.After(shimFirstPollMs * time.Millisecond):
		}
	}
}

func remoteShell(remote *AGYRemote) string {
	if remote != nil && remote.Shell != "" {
		return remote.Shell
	}
	return "bash"
}
