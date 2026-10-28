package api

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/config"
)

func newBareServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	keys := filepath.Join(root, "KEYS.txt")
	if err := os.WriteFile(keys, []byte("api-key: "+testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KOMPUTER_KEYS_FILE":          keys,
		"KOMPUTER_SESSIONS_FILE":      filepath.Join(root, "sessions.json"),
		"KOMPUTER_RESPONSES_FILE":     filepath.Join(root, "responses.json"),
		"KOMPUTER_SESSION_DIR":        filepath.Join(root, "session"),
		"KOMPUTER_MCP_CONFIG":         filepath.Join(root, "session", ".mcp.json"),
		"KOMPUTER_CLAUDE_CWD":         filepath.Join(root, "cwd"),
		"KOMPUTER_AGY_CWD":            filepath.Join(root, "agy"),
		"KOMPUTER_MUSE_CWD":           filepath.Join(root, "muse"),
		"KOMPUTER_BACKENDS":           "claude",
		"KOMPUTER_SKIP_TUNNEL":        "1",
		"KOMPUTER_AUTH_FAIL_DELAY_MS": "0",
		"HOME":                        root,
		"USERPROFILE":                 root,
	}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return server, keys
}

func TestServerReloadsKeysAndKeepsOldOnFailure(t *testing.T) {
	server, keys := newBareServer(t)
	if !server.validKey(testKey) {
		t.Fatal("initial key rejected")
	}
	replacement := "sk-rotated-key-abcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(keys, []byte("api-key: "+replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.LoadKeys(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if server.validKey(testKey) {
		t.Fatal("retired key still accepted after reload")
	}
	if !server.validKey(replacement) {
		t.Fatal("rotated key rejected after reload")
	}
	// A broken keys file must not lock operators out.
	if err := os.WriteFile(keys, []byte("# only comments\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.LoadKeys(); err == nil {
		t.Fatal("empty key file should fail to load")
	}
	if !server.validKey(replacement) {
		t.Fatal("failed reload dropped the working keys")
	}
}

type fakeConn struct {
	closed bool
}

func (c *fakeConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (c *fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeListener struct {
	conns []net.Conn
	taken int
}

func (l *fakeListener) Accept() (net.Conn, error) {
	if l.taken >= len(l.conns) {
		return nil, errors.New("done")
	}
	c := l.conns[l.taken]
	l.taken++
	return c, nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestLimitedListenerClosesExcessConnections(t *testing.T) {
	first, second := &fakeConn{}, &fakeConn{}
	listener := &limitedListener{Listener: &fakeListener{conns: []net.Conn{first, second}}, max: 1}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if first.closed {
		t.Fatal("first connection closed")
	}
	if _, err := listener.Accept(); err == nil || !strings.Contains(err.Error(), "done") {
		t.Fatalf("second accept should exhaust the source listener: %v", err)
	}
	if !second.closed {
		t.Fatal("excess connection was not closed")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := listener.current.Load(); got != 0 {
		t.Fatalf("connection count after close: %d", got)
	}
}
