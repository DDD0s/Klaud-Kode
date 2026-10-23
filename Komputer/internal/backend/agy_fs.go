package backend

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed assets/komputer-fs.py
var komputerFSScript []byte

// FuseMissing lists what is missing for the FUSE file mount on this host:
// /dev/fuse, a fusermount, fusepy, and a working /etc/mtab. An empty list
// means the mount can work.
func FuseMissing(python string) []string {
	var missing []string
	if _, err := os.Stat("/dev/fuse"); err != nil {
		missing = append(missing, "/dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			missing = append(missing, "fusermount3 (fuse3 package)")
		}
	}
	if python == "" {
		python = "python3"
	}
	if err := exec.Command(python, "-c", "import fuse").Run(); err != nil {
		missing = append(missing, "python package fusepy (pip install fusepy)")
	}
	if _, err := os.Stat("/etc/mtab"); err != nil {
		missing = append(missing, "/etc/mtab is missing, so fusermount3 cannot unmount; link it: ln -s /proc/self/mounts /etc/mtab")
	}
	return missing
}

// fuseMount owns one FUSE mount of the Body root, shared by every remote
// turn. It restarts when the Body address or token changes and retries at
// most every 30 seconds after a failure. The daemon unmounts when its stdin
// closes, so the mount goes away with this process.
type fuseMount struct {
	dir        string
	python     string
	configFile string
	logf       func(string, ...any)

	mu       sync.Mutex
	child    *exec.Cmd
	stdin    io.WriteCloser
	done     chan struct{}
	key      string
	failedAt time.Time
}

func (m *fuseMount) log(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

func (m *fuseMount) aliveLocked() bool {
	if m.child == nil || m.done == nil {
		return false
	}
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

func unmountLazy(dir string) {
	for _, attempt := range [][]string{
		{"fusermount3", "-uz", dir},
		{"fusermount", "-uz", dir},
		{"umount", "-l", dir},
	} {
		if path, err := exec.LookPath(attempt[0]); err == nil {
			if err := exec.Command(path, attempt[1:]...).Run(); err == nil {
				return
			}
		}
	}
}

func (m *fuseMount) start(server *MCPServerSpec) error {
	if m.child != nil {
		old := m.child
		done := m.done
		termProcess(old)
		if done != nil {
			<-done
		}
	}
	// A mount left behind by a crashed run reports "Transport endpoint is
	// not connected" and would fail every later ensure the same way.
	unmountLazy(m.dir)
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.configFile), 0o700); err != nil {
		return err
	}
	configData, _ := json.Marshal(map[string]any{"url": server.URL, "headers": server.Headers})
	if err := os.WriteFile(m.configFile, configData, 0o600); err != nil {
		return err
	}
	scriptPath := filepath.Join(filepath.Dir(m.configFile), "komputer-fs.py")
	if err := os.WriteFile(scriptPath, komputerFSScript, 0o600); err != nil {
		return err
	}
	python := m.python
	if python == "" {
		python = "python3"
	}
	proc := exec.Command(python, scriptPath, m.dir, m.configFile)
	stdin, err := proc.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		return err
	}
	if err := proc.Start(); err != nil {
		return err
	}
	m.child, m.stdin = proc, stdin
	done := make(chan struct{})
	m.done = done
	var errMu sync.Mutex
	errText := ""
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			errMu.Lock()
			errText = (errText + scanner.Text() + string([]byte{10}))
			if len(errText) > 2000 {
				errText = errText[len(errText)-2000:]
			}
			errMu.Unlock()
		}
	}()
	mounted := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "mounted") {
				select {
				case <-mounted:
				default:
					close(mounted)
				}
			}
		}
	}()
	waitDone := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(waitDone)
		m.mu.Lock()
		if m.child == proc {
			m.child = nil
		}
		m.mu.Unlock()
		close(done)
		errMu.Lock()
		reason := strings.TrimSpace(errText)
		errMu.Unlock()
		m.log("agy remote files: the mount at %s ended (%s) %s", m.dir, exitLabel(proc), reason)
	}()
	select {
	case <-mounted:
		return nil
	case <-waitDone:
		errMu.Lock()
		reason := lastLines(errText, 2)
		errMu.Unlock()
		if reason == "" {
			reason = "the mount did not come up within 15 s"
		}
		return errString(reason)
	case <-time.After(15 * time.Second):
		termProcess(proc)
		return errString("the mount did not come up within 15 s")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func exitLabel(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return "exit ?"
	}
	return cmd.ProcessState.String()
}

func lastLines(text string, n int) string {
	lines := strings.FieldsFunc(strings.TrimSpace(text), func(r rune) bool { return r == 10 })
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	joined := strings.Join(lines, " ")
	if len(joined) > 300 {
		joined = joined[len(joined)-300:]
	}
	return joined
}

// Ensure returns the mount directory, or "" while the Body's files cannot
// be mounted.
func (m *fuseMount) Ensure(server *MCPServerSpec) string {
	if server == nil {
		return ""
	}
	keyData, _ := json.Marshal([]any{server.URL, server.Headers})
	key := string(keyData)
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == m.key && m.aliveLocked() {
		return m.dir
	}
	if key == m.key && time.Since(m.failedAt) < 30*time.Second {
		return ""
	}
	m.key = key
	if err := m.start(server); err != nil {
		m.failedAt = time.Now()
		m.log("agy remote files: could not mount the Body root at %s: %v", m.dir, err)
		return ""
	}
	m.log("agy remote files: the Body root is mounted at %s", m.dir)
	return m.dir
}

// Stop unmounts, waiting at most 3 seconds.
func (m *fuseMount) Stop() {
	m.mu.Lock()
	proc, done := m.child, m.done
	m.mu.Unlock()
	if proc == nil {
		return
	}
	termProcess(proc)
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}
