// Package runner executes one prepared official CLI turn, streaming its
// stdout through the backend parser.
package runner

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

// Runner runs backend CLI processes with a shared timeout and source env.
type Runner struct {
	Timeout   time.Duration
	NodeBin   string
	SourceEnv map[string]string
	Logf      func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Run prepares, spawns and parses one turn. The caller owns cancellation
// policy: the service deliberately does not pass a client disconnect as ctx
// for ordinary turns, so a paid-for run can finish for a later retry.
func (r *Runner) Run(ctx context.Context, b backend.Backend, turn backend.Turn, sink domain.Sink) (domain.Output, error) {
	prepared, err := b.Prepare(turn)
	if err != nil {
		return domain.Output{}, err
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	cwd := prepared.Cwd
	if cwd == "" {
		cwd = b.Cwd()
	}
	canonical := cwd
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		canonical = resolved
	}
	env := b.ChildEnv(r.SourceEnv, canonical)
	for k, v := range prepared.ExtraEnv {
		env[k] = v
	}
	if turn.PrivateMCP {
		// The per-run callback is private loopback IPC, never outbound proxy
		// traffic. Keep all other proxy settings intact in this child only.
		parts := []string{"127.0.0.1", "localhost", "::1"}
		if existing := env["NO_PROXY"]; existing != "" {
			parts = append(parts, existing)
		}
		env["NO_PROXY"] = strings.Join(parts, ",")
		env["no_proxy"] = env["NO_PROXY"]
	}

	if err := ctx.Err(); err != nil {
		return domain.Output{}, err
	}
	bin := b.Bin()
	args := prepared.Args
	// A .js/.mjs binary (used by the test doubles) is run through node.
	if strings.HasSuffix(strings.ToLower(bin), ".js") || strings.HasSuffix(strings.ToLower(bin), ".mjs") {
		node := r.NodeBin
		if node == "" {
			node = "node"
		}
		args = append([]string{bin}, args...)
		bin = node
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = childenv.ToList(env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return domain.Output{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return domain.Output{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return domain.Output{}, err
	}
	parser := b.NewParser(turn.Session.ID, sink)

	if err := cmd.Start(); err != nil {
		return domain.Output{}, &backend.Failure{Message: err.Error(), Activity: parser.Activity()}
	}

	var timedOut atomic.Bool
	var parserMu sync.Mutex
	var timer *time.Timer
	if r.Timeout > 0 {
		timer = time.AfterFunc(r.Timeout, func() {
			timedOut.Store(true)
			terminateProcess(cmd)
		})
		defer timer.Stop()
	}
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			terminateProcess(cmd)
		case <-stopWatch:
		}
	}()

	go func() {
		_, _ = io.WriteString(stdin, prepared.Stdin)
		_ = stdin.Close()
	}()

	stderrDone := make(chan string, 1)
	go func() {
		var tail strings.Builder
		buf := make([]byte, 8192)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				tail.WriteString(chunk)
				s := tail.String()
				if len(s) > 20_000 {
					s = s[len(s)-20_000:]
					tail.Reset()
					tail.WriteString(s)
				}
				parserMu.Lock()
				parser.Stderr(tail.String())
				parserMu.Unlock()
				if text := strings.TrimSpace(domain.Redact(chunk)); text != "" {
					r.logf("[%s.err] %s", b.Name(), text)
				}
			}
			if err != nil {
				break
			}
		}
		stderrDone <- tail.String()
	}()

	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString(10)
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			parserMu.Lock()
			parser.Line(trimmed)
			parserMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	stderrText := <-stderrDone

	if timedOut.Load() {
		return domain.Output{}, &domain.APIError{
			Status: 504, Type: "timeout_error",
			Message: b.Name() + " timed out",
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.Output{}, err
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	out, failure := parser.Finish(code, "", stderrText)
	if failure != nil {
		failure.Activity = parser.Activity()
		return domain.Output{}, failure
	}
	if waitErr != nil && code == 0 {
		return domain.Output{}, &backend.Failure{Message: waitErr.Error(), Activity: parser.Activity()}
	}
	return out, nil
}
