package body

import (
	"crypto/rand"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// CommandResult is one finished run_command invocation.
type CommandResult struct {
	ExitCode     *int          `json:"exitCode"`
	Signal       *string       `json:"signal"`
	Stdout       string        `json:"stdout"`
	Stderr       string        `json:"stderr"`
	TimedOut     bool          `json:"timedOut"`
	DurationMs   int64         `json:"durationMs"`
	Encoding     string        `json:"encoding"`
	DroppedBytes *DroppedBytes `json:"droppedBytes,omitempty"`
}

// DroppedBytes reports middle output dropped from each stream.
type DroppedBytes struct {
	Stdout int `json:"stdout"`
	Stderr int `json:"stderr"`
}

// RunCommand runs one command synchronously with bounded output.
func RunCommand(cfg Config, command, cwd, shell string, timeoutMs int) (CommandResult, error) {
	startedAt := time.Now()
	out := NewHeadAndTail(cfg.MaxOutputBytes)
	errBuf := NewHeadAndTail(cfg.MaxOutputBytes)
	spawn, cleanup, err := SpawnShell(command, shell, cwd, cfg.ConsoleUTF8, false)
	if err != nil {
		return CommandResult{}, err
	}
	cmd := spawn
	cmd.Stdin = nil
	// Bounded collectors as the command's writers. With StdoutPipe a fast
	// command can exit before the reader drains, so output is lost on a loaded
	// machine; a writer makes cmd.Wait wait for the copy to finish too.
	cmd.Stdout = out
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		cleanup()
		return CommandResult{}, err
	}
	var timedOut atomic.Bool
	var timer *time.Timer
	if timeoutMs > 0 {
		timer = time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
			timedOut.Store(true)
			killTree(cmd)
		})
	}
	_ = cmd.Wait()
	if timer != nil {
		timer.Stop()
	}
	cleanup()
	outBytes, outDropped := out.Finish()
	errBytes, errDropped := errBuf.Finish()
	decodedOut := DecodeOutput(outBytes, DetectConsoleEncoding())
	decodedErr := DecodeOutput(errBytes, DetectConsoleEncoding())
	exitCode, signal := exitInfo(cmd.ProcessState)
	result := CommandResult{
		ExitCode: exitCode, Signal: signal,
		Stdout: decodedOut.Text, Stderr: decodedErr.Text, Encoding: decodedOut.Encoding,
		TimedOut: timedOut.Load(), DurationMs: time.Since(startedAt).Milliseconds(),
	}
	if outDropped > 0 || errDropped > 0 {
		result.DroppedBytes = &DroppedBytes{Stdout: outDropped, Stderr: errDropped}
	}
	return result, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type streamBuffer struct {
	chunks [][]byte
	base   int
	total  int
}

func (s *streamBuffer) appendChunk(chunk []byte, maxBytes int) {
	copied := append([]byte{}, chunk...)
	s.chunks = append(s.chunks, copied)
	s.total += len(copied)
	held := 0
	for _, c := range s.chunks {
		held += len(c)
	}
	for held > maxBytes && len(s.chunks) > 1 {
		held -= len(s.chunks[0])
		s.chunks = s.chunks[1:]
	}
	s.base = s.total - held
}

func (s *streamBuffer) slice(cursor int) (text string, missed, next int) {
	var buf []byte
	for _, c := range s.chunks {
		buf = append(buf, c...)
	}
	from := cursor - s.base
	if from < 0 {
		from = 0
	}
	if from > len(buf) {
		from = len(buf)
	}
	decoded := DecodeOutput(buf[from:], DetectConsoleEncoding())
	missed = s.base - cursor
	if missed < 0 {
		missed = 0
	}
	return decoded.Text, missed, s.base + len(buf)
}

type processRecord struct {
	max         int
	id          string
	command     string
	cwd         string
	shell       string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	cleanup     func()
	out, errBuf streamBuffer
	finished    bool
	exitCode    *int
	signal      *string
	startedAt   string
	endedAt     *string
	touched     time.Time
	idleMs      int
	idleStopped bool
	idleStop    chan struct{}
	writeMu     sync.Mutex
	mu          sync.Mutex
}

// ProcessRead is one cursor-based process output read.
type ProcessRead struct {
	ProcessID    string       `json:"processId"`
	Running      bool         `json:"running"`
	ExitCode     *int         `json:"exitCode"`
	Signal       *string      `json:"signal"`
	Stdout       string       `json:"stdout"`
	Stderr       string       `json:"stderr"`
	StdoutCursor int          `json:"stdoutCursor"`
	StderrCursor int          `json:"stderrCursor"`
	MissedBytes  *MissedBytes `json:"missedBytes,omitempty"`
	IdleStopped  bool         `json:"idleStopped,omitempty"`
}

// MissedBytes reports ring bytes a slow reader fell behind on.
type MissedBytes struct {
	Stdout int `json:"stdout"`
	Stderr int `json:"stderr"`
}

// ProcessInfo is one list_processes entry.
type ProcessInfo struct {
	ProcessID string  `json:"processId"`
	Command   string  `json:"command"`
	Cwd       string  `json:"cwd"`
	Running   bool    `json:"running"`
	ExitCode  *int    `json:"exitCode"`
	Signal    *string `json:"signal"`
	StartedAt string  `json:"startedAt"`
	EndedAt   *string `json:"endedAt,omitempty"`
}

// Processes owns long-lived processes started through start_process.
type Processes struct {
	cfg  Config
	max  int
	mu   sync.Mutex
	byID map[string]*processRecord
}

// NewProcesses creates a process registry.
func NewProcesses(cfg Config, maxBytes int) *Processes {
	return &Processes{cfg: cfg, max: maxBytes, byID: map[string]*processRecord{}}
}

func drainStream(read io.Reader, rec *processRecord, which string) {
	buf := make([]byte, 4096)
	for {
		n, err := read.Read(buf)
		if n > 0 {
			rec.mu.Lock()
			if which == "out" {
				rec.out.appendChunk(buf[:n], rec.max)
			} else {
				rec.errBuf.appendChunk(buf[:n], rec.max)
			}
			rec.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Start launches a process that survives across tool calls.
func (p *Processes) Start(command, cwd, shell string, idleTimeoutMs int) (map[string]any, error) {
	spawn, cleanup, err := SpawnShell(command, shell, cwd, p.cfg.ConsoleUTF8, true)
	if err != nil {
		return nil, err
	}
	rec := &processRecord{
		max: p.max,
		id:  newUUID(), command: command, cwd: cwd, shell: shell,
		cmd: spawn, cleanup: cleanup, touched: time.Now(), idleMs: idleTimeoutMs,
		startedAt: formatTime(time.Now()), idleStop: make(chan struct{}),
	}
	// The ring cap is the whole retained window, set per stream at append time.
	if stdin, err := spawn.StdinPipe(); err == nil {
		rec.stdin = stdin
	}
	stdout, _ := spawn.StdoutPipe()
	stderr, _ := spawn.StderrPipe()
	p.mu.Lock()
	p.byID[rec.id] = rec
	p.mu.Unlock()
	if stdout != nil {
		go drainStream(stdout, rec, "out")
	}
	if stderr != nil {
		go drainStream(stderr, rec, "err")
	}
	if err := spawn.Start(); err != nil {
		rec.mu.Lock()
		msg := append([]byte{10}, []byte("[spawn error] "+err.Error())...)
		msg = append(msg, byte(10))
		rec.errBuf.appendChunk(msg, p.max)
		code := -1
		rec.exitCode = &code
		rec.finished = true
		ended := formatTime(time.Now())
		rec.endedAt = &ended
		rec.mu.Unlock()
		cleanup()
		close(rec.idleStop)
	} else {
		go func() {
			_ = spawn.Wait()
			rec.mu.Lock()
			rec.exitCode, rec.signal = exitInfo(spawn.ProcessState)
			rec.finished = true
			ended := formatTime(time.Now())
			rec.endedAt = &ended
			rec.mu.Unlock()
			cleanup()
			close(rec.idleStop)
		}()
		if idleTimeoutMs > 0 {
			go p.idleWatch(rec)
		}
	}
	pid := 0
	if spawn.Process != nil {
		pid = spawn.Process.Pid
	}
	return map[string]any{"processId": rec.id, "pid": pid, "command": command, "cwd": cwd}, nil
}

func (p *Processes) idleWatch(rec *processRecord) {
	intervalMs := rec.idleMs
	if intervalMs > 5000 {
		intervalMs = 5000
	}
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-rec.idleStop:
			return
		case now := <-ticker.C:
			rec.mu.Lock()
			finished, touched, idleMs := rec.finished, rec.touched, rec.idleMs
			rec.mu.Unlock()
			if finished {
				return
			}
			if idleMs > 0 && now.Sub(touched) >= time.Duration(idleMs)*time.Millisecond {
				rec.mu.Lock()
				rec.idleStopped = true
				rec.mu.Unlock()
				_, _ = p.Stop(rec.id)
				return
			}
		}
	}
}

func (p *Processes) get(id string) (*processRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.byID[id]
	if rec == nil {
		return nil, fmt.Errorf("unknown processId %s", id)
	}
	return rec, nil
}

// Read returns each stream's output after its own cursor.
func (p *Processes) Read(id string, stdoutCursor, stderrCursor int) (ProcessRead, error) {
	rec, err := p.get(id)
	if err != nil {
		return ProcessRead{}, err
	}
	rec.mu.Lock()
	rec.touched = time.Now()
	// Trim to the retained window with the configured per-stream cap.
	trim := func(s *streamBuffer) {
		held := 0
		for _, c := range s.chunks {
			held += len(c)
		}
		for held > p.max && len(s.chunks) > 1 {
			held -= len(s.chunks[0])
			s.chunks = s.chunks[1:]
		}
		s.base = s.total - held
	}
	trim(&rec.out)
	trim(&rec.errBuf)
	outText, outMissed, outNext := rec.out.slice(stdoutCursor)
	errText, errMissed, errNext := rec.errBuf.slice(stderrCursor)
	result := ProcessRead{
		ProcessID: id, Running: !rec.finished, ExitCode: rec.exitCode, Signal: rec.signal,
		Stdout: outText, Stderr: errText, StdoutCursor: outNext, StderrCursor: errNext,
	}
	if rec.idleStopped {
		result.IdleStopped = true
	}
	rec.mu.Unlock()
	if outMissed > 0 || errMissed > 0 {
		result.MissedBytes = &MissedBytes{Stdout: outMissed, Stderr: errMissed}
	}
	return result, nil
}

// Write sends exact text to a process's stdin.
func (p *Processes) Write(id, input string) (map[string]any, error) {
	rec, err := p.get(id)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	rec.touched = time.Now()
	finished, stdin := rec.finished, rec.stdin
	rec.mu.Unlock()
	if finished {
		return nil, fmt.Errorf("process already exited")
	}
	rec.writeMu.Lock()
	defer rec.writeMu.Unlock()
	if _, err := io.WriteString(stdin, input); err != nil {
		return nil, err
	}
	return map[string]any{"processId": id, "wrote": UTF16Index(input, len(input))}, nil
}

// Stop kills a process and its children.
func (p *Processes) Stop(id string) (map[string]any, error) {
	rec, err := p.get(id)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	finished, exitCode, signal := rec.finished, rec.exitCode, rec.signal
	rec.mu.Unlock()
	if finished {
		return map[string]any{"processId": id, "alreadyExited": true, "exitCode": exitCode, "signal": signal}, nil
	}
	killTree(rec.cmd)
	if rec.stdin != nil {
		_ = rec.stdin.Close()
	}
	return map[string]any{"processId": id, "stopped": true}, nil
}

func (p *Processes) info(rec *processRecord) ProcessInfo {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return ProcessInfo{
		ProcessID: rec.id, Command: rec.command, Cwd: rec.cwd,
		Running: !rec.finished, ExitCode: rec.exitCode, Signal: rec.signal,
		StartedAt: rec.startedAt, EndedAt: rec.endedAt,
	}
}

// List returns every known process, running or finished.
func (p *Processes) List() []ProcessInfo {
	p.mu.Lock()
	recs := make([]*processRecord, 0, len(p.byID))
	for _, rec := range p.byID {
		recs = append(recs, rec)
	}
	p.mu.Unlock()
	out := make([]ProcessInfo, 0, len(recs))
	for _, rec := range recs {
		out = append(out, p.info(rec))
	}
	return out
}

// KillAll stops every running process.
func (p *Processes) KillAll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.byID))
	for id := range p.byID {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		_, _ = p.Stop(id)
	}
}
