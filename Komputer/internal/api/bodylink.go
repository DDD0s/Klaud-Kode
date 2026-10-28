package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	seq       atomic.Uint64
	pendingMu sync.Mutex
	pending   map[string]*pendingCall
	dropped   atomic.Uint64
}

// pendingCall is one in-flight request. The wire id is allocated by the brain,
// never taken from the client: two MCP clients both start their ids at small
// values, and a late reply to a timed-out request must never be deliverable to
// a later caller that reused the id.
type pendingCall struct {
	conn       *websocket.Conn
	originalID json.RawMessage
	ch         chan []byte
}

func newBodyLink(server *Server) *bodyLink {
	return &bodyLink{server: server, pending: map[string]*pendingCall{}}
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

// rejectAuth is requireAuth's failure path for the body endpoints: same
// limiter, same delay, so guessing the body token is exactly as expensive as
// guessing an API key.
func (l *bodyLink) rejectAuth(w http.ResponseWriter, r *http.Request, what string) {
	s := l.server
	total := s.authFailures.Add(1)
	if retry := s.limiter.Failed(remoteSocketAddress(r), time.Now()); retry > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many failed authentication attempts"}, map[string]string{"Retry-After": strconv.Itoa(retry)})
		return
	}
	s.logf("%s auth failed from %s %s (total %d)", what, remoteOf(r), r.URL.Path, total)
	if s.cfg.AuthFailDelay > 0 {
		time.Sleep(s.cfg.AuthFailDelay)
	}
	w.Header().Set("WWW-Authenticate", "Bearer realm=\"klaud-kode-body\"")
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"}, nil)
}

// handleConnect accepts the Body's outbound WebSocket.
func (l *bodyLink) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !l.server.cfg.BodyConnect {
		http.NotFound(w, r)
		return
	}
	if !bodyTokenOK(bearerOf(r), l.server.cfg.BodyToken) {
		l.rejectAuth(w, r, "body connect")
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
		// CloseNow: a polite close waits on the peer, which for a dead peer
		// would leave the new link unread for seconds.
		_ = old.CloseNow()
	}
}

func (l *bodyLink) detach(conn *websocket.Conn) {
	l.mu.Lock()
	// Only the connection that is still current owns the in-flight calls. A
	// replaced connection must not fail requests its successor is serving.
	current := l.conn == conn
	if current {
		l.conn = nil
		l.remote = ""
	}
	l.mu.Unlock()
	if current {
		l.failPendingFor(conn)
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// failPendingFor closes the reply channels of calls that rode conn, so their
// waiters fail at once instead of on a request timeout. Calls belonging to a
// replacement connection are untouched.
func (l *bodyLink) failPendingFor(conn *websocket.Conn) {
	l.pendingMu.Lock()
	for id, p := range l.pending {
		if p.conn == conn {
			close(p.ch)
			delete(l.pending, id)
		}
	}
	l.pendingMu.Unlock()
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
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					_ = conn.CloseNow()
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
			// HTTP; count them so the loss is visible rather than silent.
			if n := l.dropped.Add(1); n&(n-1) == 0 {
				l.server.logf("dropped %d body notification(s) with no request to answer", n)
			}
			continue
		}
		l.pendingMu.Lock()
		p, waiting := l.pending[id]
		if waiting {
			delete(l.pending, id)
		}
		l.pendingMu.Unlock()
		if waiting {
			p.ch <- data
		}
	}
}

type rpcID struct {
	ID json.RawMessage `json:"id"`
}

// messageID returns the wire id key. An id of null counts as present: the Body
// treats it as a request, so the proxy must wait for its reply too.
func messageID(payload []byte) (string, bool) {
	var env rpcID
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", false
	}
	if len(env.ID) == 0 {
		return "", false
	}
	return string(env.ID), true
}

var (
	errNoBody     = fmt.Errorf("no body connected")
	errBadRequest = fmt.Errorf("invalid MCP request")
)

// Call sends one JSON-RPC message (or a batch array) to the Body and waits
// for its reply. Notifications return (nil, nil).
func (l *bodyLink) Call(ctx context.Context, payload []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(payload))
	if strings.HasPrefix(trimmed, "[") {
		return l.callBatch(ctx, payload)
	}
	return l.callOne(ctx, payload)
}

// callOne forwards a single message under a brain-allocated id and restores
// the caller's id in the reply.
func (l *bodyLink) callOne(ctx context.Context, payload []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, errBadRequest
	}
	originalID, needsReply := fields["id"]

	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()
	if conn == nil {
		return nil, errNoBody
	}

	var internal string
	var entry *pendingCall
	if needsReply {
		internal = "kl-" + strconv.FormatUint(l.seq.Add(1), 10)
		raw, _ := json.Marshal(internal)
		fields["id"] = raw
		rewritten, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		payload = rewritten
		entry = &pendingCall{conn: conn, originalID: originalID, ch: make(chan []byte, 1)}
		key := strconv.Quote(internal)
		l.pendingMu.Lock()
		l.pending[key] = entry
		l.pendingMu.Unlock()
		defer func() {
			l.pendingMu.Lock()
			delete(l.pending, key)
			l.pendingMu.Unlock()
		}()
	}

	// Never write with the caller's context: coder/websocket closes the whole
	// connection if a write context fires mid-write, which would take every
	// other session down with one client's timeout.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.writeMu.Lock()
	wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := conn.Write(wctx, websocket.MessageText, payload)
	cancel()
	l.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send to body: %w", err)
	}
	if !needsReply {
		return nil, nil
	}
	select {
	case reply, open := <-entry.ch:
		if !open {
			return nil, errNoBody
		}
		return restoreID(reply, entry.originalID), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callBatch forwards each element and reassembles the replies in request
// order, omitting notifications as the Body's own HTTP path does.
func (l *bodyLink) callBatch(ctx context.Context, payload []byte) ([]byte, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(payload, &elements); err != nil {
		return nil, errBadRequest
	}
	replies := make([][]byte, len(elements))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, element := range elements {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := l.callOne(ctx, element)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			replies[i] = reply
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	present := make([]json.RawMessage, 0, len(replies))
	for _, reply := range replies {
		if reply != nil {
			present = append(present, reply)
		}
	}
	if len(present) == 0 {
		return nil, nil
	}
	return json.Marshal(present)
}

// restoreID puts the caller's id back onto a reply that carried ours.
func restoreID(reply []byte, original json.RawMessage) []byte {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(reply, &fields); err != nil {
		return reply
	}
	fields["id"] = original
	out, err := json.Marshal(fields)
	if err != nil {
		return reply
	}
	return out
}

// handleMCP is the loopback MCP endpoint the CLI is configured with; each POST
// is one round trip over the link. Only same-host callers may use it.
func (l *bodyLink) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !l.server.cfg.BodyConnect {
		http.NotFound(w, r)
		return
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "body relay is loopback only"}, nil)
		return
	}
	if !bodyTokenOK(bearerOf(r), l.server.cfg.BodyToken) {
		l.rejectAuth(w, r, "body mcp")
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
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot read request body"}, nil)
		return
	}
	if len(payload) > 16_000_000 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request too large"}, nil)
		return
	}
	reply, err := l.Call(r.Context(), payload)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid MCP request"}, nil)
			return
		}
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
