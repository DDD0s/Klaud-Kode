package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A bodyLink is the brain's end of a connection the Body dialed out. The Body
// sits behind NAT in another country, so it comes to us: it opens a WebSocket
// here and stays on it, and JSON-RPC MCP messages ride that connection in both
// directions. The CLI still talks plain HTTP to this server's own /body/mcp,
// so nothing about the tool path changes for it.
type bodyLink struct {
	server *Server

	mu          sync.Mutex
	conn        *websocket.Conn
	connectedAt time.Time
	remote      string

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan []byte
}

func newBodyLink(server *Server) *bodyLink {
	return &bodyLink{server: server, pending: map[string]chan []byte{}}
}

// Connected reports whether a Body is currently on the link.
func (l *bodyLink) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// Status returns connection state for health reporting.
func (l *bodyLink) Status() (connected bool, remote string, since time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil, l.remote, l.connectedAt
}

func bodyTokenOK(supplied, token string) bool {
	if supplied == "" || token == "" {
		return false
	}
	a := sha256.Sum256([]byte(supplied))
	b := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func bearerOf(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	scheme, rest, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// handleConnect accepts the Body's outbound WebSocket.
func (l *bodyLink) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !bodyTokenOK(bearerOf(r), l.server.cfg.BodyToken) {
		l.server.logf("body connect refused (bad token) from %s", r.RemoteAddr)
		time.Sleep(time.Second)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"}, nil)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		l.server.logf("body connect upgrade failed: %v", err)
		return
	}
	conn.SetReadLimit(64 << 20)
	l.attach(conn, r.RemoteAddr)
	l.server.logf("body connected from %s", r.RemoteAddr)
	l.serve(r.Context(), conn)
	l.detach(conn)
	l.server.logf("body disconnected")
}

// attach replaces any previous connection: one computer, one link.
func (l *bodyLink) attach(conn *websocket.Conn, remote string) {
	l.mu.Lock()
	old := l.conn
	l.conn = conn
	l.remote = remote
	l.connectedAt = time.Now()
	l.mu.Unlock()
	if old != nil && old != conn {
		_ = old.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
}

func (l *bodyLink) detach(conn *websocket.Conn) {
	l.mu.Lock()
	if l.conn == conn {
		l.conn = nil
		l.remote = ""
	}
	l.mu.Unlock()
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	// Fail everything in flight now rather than on request timeouts.
	l.pendingMu.Lock()
	for id, ch := range l.pending {
		close(ch)
		delete(l.pending, id)
	}
	l.pendingMu.Unlock()
}

type rpcID struct {
	ID json.RawMessage `json:"id"`
}

func messageID(payload []byte) (string, bool) {
	var env rpcID
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", false
	}
	if len(env.ID) == 0 || string(env.ID) == "null" {
		return "", false
	}
	return string(env.ID), true
}

// serve pumps frames from the Body to the callers waiting on them, and keeps
// the link warm with pings so dead peers are noticed.
func (l *bodyLink) serve(ctx context.Context, conn *websocket.Conn) {
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				pingCtx2, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := conn.Ping(pingCtx2)
				cancel()
				if err != nil {
					_ = conn.Close(websocket.StatusGoingAway, "ping failed")
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		id, ok := messageID(data)
		if !ok {
			// Notifications from the Body have nowhere to go in request/response
			// HTTP; drop them (and say so once in a while via the log).
			continue
		}
		l.pendingMu.Lock()
		ch, waiting := l.pending[id]
		if waiting {
			delete(l.pending, id)
		}
		l.pendingMu.Unlock()
		if waiting {
			ch <- data
		}
	}
}

var errNoBody = fmt.Errorf("no body connected")

// Call sends one JSON-RPC message to the Body and waits for its reply.
// Notifications return (nil, nil).
func (l *bodyLink) Call(ctx context.Context, payload []byte) ([]byte, error) {
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()
	if conn == nil {
		return nil, errNoBody
	}
	id, needsReply := messageID(payload)
	var replyCh chan []byte
	if needsReply {
		replyCh = make(chan []byte, 1)
		l.pendingMu.Lock()
		if _, dup := l.pending[id]; dup {
			l.pendingMu.Unlock()
			return nil, fmt.Errorf("duplicate in-flight request id %s", id)
		}
		l.pending[id] = replyCh
		l.pendingMu.Unlock()
		defer func() {
			l.pendingMu.Lock()
			delete(l.pending, id)
			l.pendingMu.Unlock()
		}()
	}
	l.writeMu.Lock()
	err := conn.Write(ctx, websocket.MessageText, payload)
	l.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send to body: %w", err)
	}
	if !needsReply {
		return nil, nil
	}
	select {
	case reply, open := <-replyCh:
		if !open {
			return nil, errNoBody
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handleMCP is the loopback MCP endpoint the CLI is configured with; each POST
// is one round trip over the link.
func (l *bodyLink) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !bodyTokenOK(bearerOf(r), l.server.cfg.BodyToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"}, nil)
		return
	}
	// Streamable HTTP clients may open a GET stream; nothing is pushed here.
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 16_000_001))
	if err != nil || len(payload) > 16_000_000 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request too large"}, nil)
		return
	}
	reply, err := l.Call(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()}, nil)
		return
	}
	if reply == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}
