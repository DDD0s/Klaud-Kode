package body

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CommandResult is one finished run_command invocation.
type CommandResult struct {
	Stdout    string  `json:"stdout"`
	Stderr    string  `json:"stderr"`
	Encoding  string  `json:"encoding"`
	ExitCode  *int    `json:"exitCode"`
	Signal    *string `json:"signal"`
	TimedOut  bool    `json:"timedOut"`
	Truncated bool    `json:"truncated"`
	Shell     string  `json:"shell"`
	Cwd       string  `json:"cwd"`
}

func readInto(read io.Reader, target *HeadAndTail) {
	buf := make([]byte, 4096)
	for {
		n, err := read.Read(buf)
		if n > 0 {
			target.Push(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// RunCommand runs one command synchronously with bounded output.
func RunCommand(cfg Config, command, cwd, shell string, timeoutMs int) (CommandResult, error) {
	out := NewHeadAndTail(cfg.MaxOutputBytes)
	errBuf := NewHeadAndTail(cfg.MaxOutputBytes)
	spawn, cleanup, err := SpawnShell(command, shell, cwd, cfg.ConsoleUTF8, false)
	if err != nil {
		return CommandResult{}, err
	}
	cmd := spawn
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return CommandResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cleanup()
		return CommandResult{}, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return CommandResult{}, err
	}
	done := make(chan struct{}, 2)
	go func() { readInto(stdout, out); done <- struct{}{} }()
	go func() { readInto(stderr, errBuf); done <- struct{}{} }()
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
	<-done
	<-done
	cleanup()
	enc := "utf-8"
	if IsWindows && !cfg.ConsoleUTF8 {
		enc = DetectConsoleEncoding()
	}
	outBytes, outDropped := out.Finish()
	errBytes, errDropped := errBuf.Finish()
	decodedOut := DecodeOutput(outBytes, enc)
	decodedErr := DecodeOutput(errBytes, enc)
	exitCode, signal := exitInfo(cmd.ProcessState)
	return CommandResult{
		Stdout: decodedOut.Text, Stderr: decodedErr.Text, Encoding: decodedOut.Encoding,
		ExitCode: exitCode, Signal: signal, TimedOut: timedOut.Load(),
		Truncated: outDropped > 0 || errDropped > 0,
		Shell:     shell, Cwd: cwd,
	}, nil
}

const (
	ringBytes    = 512 * 1024
	ringRetained = 256 * 1024
)

type processRecord struct {
	id        string
	command   string
	cwd       string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cleanup   func()
	buf       [][]byte
	bufBytes  int
	base      int
	total     int
	finished  bool
	exitCode  *int
	signal    *string
	endedAt   *string
	touched   time.Time
	idleMs    int
	idleStop  chan struct{}
	writeMu   sync.Mutex
	mu        sync.Mutex
	truncated bool
}

func (r *processRecord) appendOutput(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := append([]byte{}, chunk...)
	r.buf = append(r.buf, copied)
	r.bufBytes += len(copied)
	r.total += len(copied)
	r.touched = time.Now()
	for r.bufBytes > ringBytes && len(r.buf) > 0 {
		drop := r.bufBytes - ringRetained
		for drop > 0 && len(r.buf) > 0 {
			first := r.buf[0]
			if len(first) <= drop {
				r.base += len(first)
				r.bufBytes -= len(first)
				drop -= len(first)
				r.buf = r.buf[1:]
			} else {
				r.buf[0] = first[drop:]
				r.base += drop
				r.bufBytes -= drop
				drop = 0
			}
		}
		r.truncated = true
	}
}

func (r *processRecord) bytesFrom(offset, maxBytes int) ([]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := offset
	if from < r.base {
		from = r.base
	}
	if from > r.total {
		from = r.total
	}
	end := from + maxBytes
	if end > r.total {
		end = r.total
	}
	var out []byte
	pos := r.base
	for _, chunk := range r.buf {
		start, stop := pos, pos+len(chunk)
		if stop > from && start < end {
			lo := max(from-start, 0)
			hi := min(end-start, len(chunk))
			out = append(out, chunk[lo:hi]...)
		}
		pos = stop
	}
	return out, end
}

// ProcessRead is one cursor-based process output read.
type ProcessRead struct {
	Output    string  `json:"output"`
	Encoding  string  `json:"encoding"`
	Cursor    int     `json:"cursor"`
	Running   bool    `json:"running"`
	ExitCode  *int    `json:"exitCode"`
	Signal    *string `json:"signal"`
	Truncated bool    `json:"truncated"`
}

// ProcessInfo is one list_processes entry.
type ProcessInfo struct {
	ProcessID   string  `json:"processId"`
	PID         int     `json:"pid"`
	Command     string  `json:"command"`
	Cwd         string  `json:"cwd"`
	Running     bool    `json:"running"`
	ExitCode    *int    `json:"exitCode"`
	Signal      *string `json:"signal"`
	IdleStopped bool    `json:"idleStopped,omitempty"`
}

// Processes owns long-lived processes started through start_process.
type Processes struct {
	cfg      Config
	maxBytes int
	mu       sync.Mutex
	byID     map[string]*processRecord
	seq      int
}

// NewProcesses creates a process registry.
func NewProcesses(cfg Config, maxBytes int) *Processes {
	return &Processes{cfg: cfg, maxBytes: maxBytes, byID: map[string]*processRecord{}}
}

// Start launches a process that survives across tool calls.
func (p *Processes) Start(command, cwd, shell string, idleTimeoutMs int) (map[string]any, error) {
	spawn, cleanup, err := SpawnShell(command, shell, cwd, p.cfg.ConsoleUTF8, true)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("proc_%d", p.seq)
	p.mu.Unlock()
	rec := &processRecord{
		id: id, command: command, cwd: cwd, cmd: spawn, cleanup: cleanup,
		touched: time.Now(), idleMs: idleTimeoutMs, idleStop: make(chan struct{}),
	}
	stdin, err := spawn.StdinPipe()
	if err == nil {
		rec.stdin = stdin
	}
	stdout, _ := spawn.StdoutPipe()
	stderr, _ := spawn.StderrPipe()
	p.mu.Lock()
	p.byID[id] = rec
	p.mu.Unlock()
	if stdout != nil {
		go drainProcess(stdout, rec)
	}
	if stderr != nil {
		go drainProcess(stderr, rec)
	}
	if err := spawn.Start(); err != nil {
		rec.mu.Lock()
		rec.appendOutputLocked([]byte("[spawn error] " + err.Error()))
		code := -1
		rec.exitCode = &code
		rec.finished = true
		ended := time.Now().UTC().Format(time.RFC3339Nano)
		rec.endedAt = &ended
		rec.mu.Unlock()
		cleanup()
	} else {
		go func() {
			_ = spawn.Wait()
			rec.mu.Lock()
			rec.exitCode, rec.signal = exitInfo(spawn.ProcessState)
			rec.finished = true
			ended := time.Now().UTC().Format(time.RFC3339Nano)
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
	return map[string]any{"processId": id, "pid": pid, "command": command, "cwd": cwd}, nil
}

func (r *processRecord) appendOutputLocked(chunk []byte) {
	// Caller already holds r.mu (spawn-error path only).
	copied := append([]byte{}, chunk...)
	r.buf = append(r.buf, copied)
	r.bufBytes += len(copied)
	r.total += len(copied)
}

func drainProcess(read io.Reader, rec *processRecord) {
	buf := make([]byte, 4096)
	for {
		n, err := read.Read(buf)
		if n > 0 {
			rec.appendOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (p *Processes) idleWatch(rec *processRecord) {
	interval := time.Duration(min(rec.idleMs, 5000)) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-rec.idleStop:
			return
		case now := <-ticker.C:
			rec.mu.Lock()
			idleFor := now.Sub(rec.touched)
			finished := rec.finished
			idleMs := rec.idleMs
			rec.mu.Unlock()
			if finished {
				return
			}
			if idleMs > 0 && idleFor >= time.Duration(idleMs)*time.Millisecond {
				rec.mu.Lock()
				rec.idleMs = -1
				rec.mu.Unlock()
				_, _ = p.Stop(rec.id)
				return
			}
		}
	}
}

func (p *Processes) get(id string) *processRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byID[id]
}

// Read returns output after cursor as decoded text.
func (p *Processes) Read(id string, cursor, maxBytes int) (ProcessRead, error) {
	rec := p.get(id)
	if rec == nil {
		return ProcessRead{}, fmt.Errorf("unknown process: %s", id)
	}
	if maxBytes <= 0 {
		maxBytes = p.maxBytes
	}
	if maxBytes > p.maxBytes {
		maxBytes = p.maxBytes
	}
	buf, next := rec.bytesFrom(cursor, maxBytes)
	enc := "utf-8"
	if IsWindows && !p.cfg.ConsoleUTF8 {
		enc = DetectConsoleEncoding()
	}
	decoded := DecodeOutput(buf, enc)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.touched = time.Now()
	return ProcessRead{
		Output: decoded.Text, Encoding: decoded.Encoding, Cursor: next,
		Running: !rec.finished, ExitCode: rec.exitCode, Signal: rec.signal,
		Truncated: cursor < rec.base,
	}, nil
}

// Write sends text to a process's stdin.
func (p *Processes) Write(id, text string, pressEnter bool) (bool, error) {
	rec := p.get(id)
	if rec == nil {
		return false, fmt.Errorf("unknown process: %s", id)
	}
	rec.mu.Lock()
	rec.touched = time.Now()
	finished, stdin := rec.finished, rec.stdin
	rec.mu.Unlock()
	if finished || stdin == nil {
		return false, nil
	}
	data := text
	if pressEnter && !strings.HasSuffix(data, string([]byte{10})) {
		data += string([]byte{10})
	}
	rec.writeMu.Lock()
	defer rec.writeMu.Unlock()
	_, err := io.WriteString(stdin, data)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// Stop kills a process and its children.
func (p *Processes) Stop(id string) (map[string]any, error) {
	rec := p.get(id)
	if rec == nil {
		return nil, fmt.Errorf("unknown process: %s", id)
	}
	rec.mu.Lock()
	wasRunning := !rec.finished
	rec.mu.Unlock()
	if wasRunning {
		killTree(rec.cmd)
		if rec.stdin != nil {
			_ = rec.stdin.Close()
		}
	}
	info := p.info(rec)
	return map[string]any{
		"processId": info.ProcessID, "stopped": wasRunning, "running": info.Running,
		"exitCode": info.ExitCode, "signal": info.Signal,
	}, nil
}

func (p *Processes) info(rec *processRecord) ProcessInfo {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	pid := 0
	if rec.cmd.Process != nil {
		pid = rec.cmd.Process.Pid
	}
	info := ProcessInfo{
		ProcessID: rec.id, PID: pid, Command: rec.command, Cwd: rec.cwd,
		Running: !rec.finished, ExitCode: rec.exitCode, Signal: rec.signal,
	}
	if rec.idleMs < 0 {
		info.IdleStopped = true
	}
	return info
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
