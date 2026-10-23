package backend

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

// AGYCommandOf returns the command of "bash [options] -c <command> [name
// [args...]]", or "" for any other invocation (which runs the real bash).
func AGYCommandOf(argv []string) string {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return ""
		}
		if len(a) > 1 && a[0] == '-' && strings.Contains(a, "c") && !strings.HasPrefix(a, "--") {
			opts := strings.TrimLeft(a, "-+")
			if opts != "" && allASCIICLetters(opts) {
				if i+1 < len(argv) {
					return argv[i+1]
				}
				return ""
			}
		}
		if strings.HasPrefix(a, "--") {
			if a == "--rcfile" || a == "--init-file" {
				i++
			}
			continue
		}
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') {
			if a == "-o" || a == "+o" || a == "-O" || a == "+O" {
				i++
			}
			continue
		}
		return ""
	}
	return ""
}

func allASCIICLetters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func agyPosixRoot(root string) bool { return strings.HasPrefix(root, "/") }

// AGYToRemote rewrites mount paths in a command into the user's paths. Only
// POSIX roots are translated; a Windows root already is the user's own path.
func AGYToRemote(text string, remote *AGYRemote) string {
	if remote == nil || remote.Mount == "" || !agyPosixRoot(remote.Root) || remote.Mount == remote.Root {
		return text
	}
	return strings.ReplaceAll(text, remote.Mount, remote.Root)
}

// AGYRemoteCwd returns the Body-side cwd, relative to its root, for a
// local working directory inside the mount.
func AGYRemoteCwd(local string, remote *AGYRemote) string {
	if remote == nil || remote.Mount == "" {
		return "."
	}
	mount := path.Clean(remote.Mount)
	cleaned := path.Clean(local)
	if !strings.HasPrefix(cleaned, strings.TrimSuffix(mount, "/")+"/") {
		if cleaned == mount {
			return "."
		}
		return "."
	}
	return strings.TrimPrefix(cleaned, strings.TrimSuffix(mount, "/")+"/")
}

// StreamRewriter maps a byte stream between the Body's paths and the mount,
// carrying a partial match across chunk boundaries.
type streamRewriter struct {
	from, to string
	carry    string
}

// StreamRewriter creates a chunk-boundary-safe path rewriter.
func StreamRewriter(from, to string) *streamRewriter {
	return &streamRewriter{from: from, to: to}
}

// Push rewrites one chunk.
func (r *streamRewriter) Push(s string) string {
	if r.from == "" || r.from == r.to {
		return s
	}
	text := r.carry + s
	cut := len(text)
	for k := min(len(r.from)-1, len(text)); k > 0; k-- {
		if strings.HasPrefix(r.from, text[len(text)-k:]) {
			cut = len(text) - k
			break
		}
	}
	r.carry = text[cut:]
	return strings.ReplaceAll(text[:cut], r.from, r.to)
}

// Flush releases any carried bytes.
func (r *streamRewriter) Flush() string {
	rest := r.carry
	r.carry = ""
	if r.from == "" || r.from == r.to {
		return rest
	}
	return strings.ReplaceAll(rest, r.from, r.to)
}

// agyOutputMapper returns a no-op mapper when no translation applies.
func agyOutputMapper(remote *AGYRemote) *streamRewriter {
	if remote == nil || remote.Mount == "" || !agyPosixRoot(remote.Root) || remote.Mount == remote.Root {
		return StreamRewriter("", "")
	}
	return StreamRewriter(remote.Root, remote.Mount)
}

var agySignals = map[os.Signal]int{syscall.SIGHUP: 1, syscall.SIGINT: 2, syscall.SIGQUIT: 3, syscall.SIGKILL: 9, syscall.SIGTERM: 15}

const shimInitialDelayMs = 100

func agySignalCode(sig os.Signal) int {
	if code, ok := agySignals[sig]; ok {
		return 128 + code
	}
	return 128 + 15
}

// AGYPassThrough runs the real bash with the same stdin/stdout.
func AGYPassThrough(argv []string, getenv func(string) string) int {
	realBash := ""
	if getenv != nil {
		realBash = getenv("KOMPUTER_AGY_REAL_BASH")
	}
	if realBash == "" {
		realBash = "/bin/bash"
	}
	cmd := exec.Command(realBash, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return agySignalCode(status.Signal())
			}
			return exit.ExitCode()
		}
		_, _ = io.WriteString(os.Stderr, "bash: "+err.Error()+string([]byte{10}))
		return 127
	}
	return 0
}

func agyExitCodeOf(read map[string]any) int {
	if code, ok := read["exitCode"].(float64); ok {
		return int(code)
	}
	if signal, ok := read["signal"].(string); ok {
		names := map[string]int{"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGKILL": 9, "SIGTERM": 15}
		if code, ok := names[signal]; ok {
			return 128 + code
		}
		return 129
	}
	return 1
}

// RunAGYShim executes the shim subcommand and returns its exit code. It is
// the "bash" agy sees while its commands belong on the caller's computer.
func RunAGYShim(ctx context.Context, argv []string, stdout, stderr io.Writer, getenv func(string) string, cwd string) int {
	file := ""
	if getenv != nil {
		file = getenv("KOMPUTER_AGY_MCP_FILE")
	}
	target := LoadAGYTarget(file)
	var remote *AGYRemote
	if target != nil {
		remote = target.Remote
	}
	command := AGYCommandOf(argv)
	if remote == nil || remote.Server == nil || remote.Server.URL == "" || command == "" {
		return AGYPassThrough(argv, getenv)
	}
	client := NewMCPHTTPClient(*remote.Server)
	if remote.Root == "" {
		if info, err := client.CallTool(ctx, "system_info", map[string]any{}); err == nil {
			if m, ok := info.(map[string]any); ok {
				if cwdRoot, ok := m["cwdRoot"].(map[string]any); ok {
					remote.Root, _ = cwdRoot["path"].(string)
				}
			}
		}
	}
	shell := remote.Shell
	if shell == "" {
		shell = "bash"
	}
	text := AGYToRemote(command, remote)
	out := agyOutputMapper(remote)
	errMap := agyOutputMapper(remote)
	emitOut := func(s string) { _, _ = io.WriteString(stdout, out.Push(s)) }
	emitErr := func(s string) { _, _ = io.WriteString(stderr, errMap.Push(s)) }
	finish := func() {
		if tail := out.Flush(); tail != "" {
			_, _ = io.WriteString(stdout, tail)
		}
		if tail := errMap.Flush(); tail != "" {
			_, _ = io.WriteString(stderr, tail)
		}
	}
	call := func(name string, args map[string]any) (map[string]any, error) {
		result, err := client.CallTool(ctx, name, args)
		if err != nil {
			return nil, err
		}
		m, _ := result.(map[string]any)
		return m, nil
	}
	startArgs := map[string]any{"command": text, "shell": shell, "cwd": AGYRemoteCwd(cwd, remote)}
	started, startErr := call("start_process", startArgs)
	if startErr != nil && agyUnknownTool(startErr) {
		// A Body without process support still runs the command, just not
		// streamed: same output, same exit status.
		result, err := call("run_command", map[string]any{"command": text, "shell": shell, "cwd": AGYRemoteCwd(cwd, remote)})
		if err != nil {
			_, _ = io.WriteString(stderr, "bash: could not run the command on the user's computer: "+err.Error()+string([]byte{10}))
			return 126
		}
		emitOut(toStringValue(result["stdout"]))
		emitErr(toStringValue(result["stderr"]))
		finish()
		if timedOut, _ := result["timedOut"].(bool); timedOut {
			_, _ = io.WriteString(stderr, string([]byte{10})+"[timed out on the user's computer]"+string([]byte{10}))
		}
		return agyExitCodeOf(result)
	}
	id, _ := started["processId"].(string)
	stopOnce := sync.Once{}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			stopOnce.Do(func() {
				_, _ = call("stop_process", map[string]any{"processId": id})
				finish()
				os.Exit(agySignalCode(sig))
			})
		case <-ctx.Done():
		}
	}()
	// agy's command may read stdin (pipes, here-documents, read).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_, _ = call("write_process", map[string]any{"processId": id, "input": string(buf[:n])})
			}
			if err != nil {
				return
			}
		}
	}()
	cursors := map[string]any{"stdoutCursor": 0, "stderrCursor": 0}
	delay := shimInitialDelayMs
	for {
		read, err := call("read_process", map[string]any{"processId": id, "stdoutCursor": cursors["stdoutCursor"], "stderrCursor": cursors["stderrCursor"]})
		if err != nil {
			_, _ = io.WriteString(stderr, "bash: could not read the command's output on the user's computer: "+err.Error()+string([]byte{10}))
			return 126
		}
		stdoutText := toStringValue(read["stdout"])
		stderrText := toStringValue(read["stderr"])
		if stdoutText != "" {
			emitOut(stdoutText)
		}
		if stderrText != "" {
			emitErr(stderrText)
		}
		cursors["stdoutCursor"] = read["stdoutCursor"]
		cursors["stderrCursor"] = read["stderrCursor"]
		if running, _ := read["running"].(bool); !running {
			finish()
			if idle, _ := read["idleStopped"].(bool); idle {
				_, _ = io.WriteString(stderr, "komputer: the command was stopped after 10 minutes without being read; its later output is lost."+string([]byte{10}))
			}
			return agyExitCodeOf(read)
		}
		next := delay * 3 / 2
		if next > 500 {
			next = 500
		}
		delay = next
		select {
		case <-ctx.Done():
			_, _ = call("stop_process", map[string]any{"processId": id})
			finish()
			return 130
		case <-time.After(time.Duration(delay) * time.Millisecond):
		}
	}
}

func agyUnknownTool(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown tool") || strings.Contains(msg, "not found") || strings.Contains(msg, "tool not found")
}

func toStringValue(v any) string {
	s, _ := v.(string)
	return s
}
