// Package api serves the client-facing HTTP API and drives backend turns.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/harness"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/responses"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/runner"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/session"
)

// Server is the API service. This first Go slice implements the Claude
// backend without client-executed tools or Responses storage; unsupported
// modes fail explicitly instead of taking the wrong execution path.
type Server struct {
	cfg      config.Config
	backends *backend.Registry
	store    *session.Store
	runner   *runner.Runner
	logger   *log.Logger

	keysMu       sync.RWMutex
	keyDigests   [][]byte
	authFailures atomic.Int64
	limiter      *authLimiter

	slots     chan struct{}
	queued    atomic.Int64
	locks     *keyLocks
	turns     *turnRegistry
	tunnel    *tunnelGuard
	harness   *harness.Gateway
	responses *responses.Store
}

// NewServer loads keys and sessions and prepares the backend workspaces.
func NewServer(cfg config.Config) (*Server, error) {
	var backendList []backend.Backend
	for _, name := range cfg.Backends {
		switch name {
		case "claude":
			backendList = append(backendList, backend.NewClaude(cfg.Claude))
		case "muse":
			backendList = append(backendList, backend.NewMuse(cfg.Muse))
		case "antigravity":
			backendList = append(backendList, backend.NewAntigravity(cfg.Antigravity))
		default:
			return nil, fmt.Errorf("backend %q is not yet implemented in the Go rewrite", name)
		}
	}
	registry, err := backend.NewRegistry(backendList, cfg.DefaultBackend)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		backends: registry,
		logger:   log.New(os.Stdout, "", 0),
		limiter:  newAuthLimiter(cfg.AuthFailureLimit, time.Minute),
		slots:    make(chan struct{}, cfg.MaxInFlight),
		locks:    newKeyLocks(),
		turns:    newTurnRegistry(cfg.TurnCache),
	}
	for _, b := range registry.List {
		if agy, ok := b.(*backend.Antigravity); ok {
			agy.SetLogf(s.logf)
		}
	}
	s.store = session.New(cfg.SessionsFile, cfg.MaxSessions, cfg.IdleTimeout)
	s.store.Logf = s.logf
	s.runner = &runner.Runner{
		Timeout: cfg.CLITimeout, NodeBin: cfg.NodeBin, SourceEnv: cfg.SourceEnv,
		Logf: s.logf,
	}
	s.tunnel = &tunnelGuard{server: s}
	s.harness = harness.NewGateway(s.runHarnessCLI, slotLease{server: s})
	s.harness.Timeout = cfg.ClientToolTimeout
	s.harness.Cache = cfg.TurnCache
	s.harness.MaxRuns = cfg.MaxClientToolRuns
	s.responses = responses.NewStore(cfg.ResponsesFile, cfg.MaxStoredResponses)
	s.responses.Logf = s.logf
	if err := s.loadKeys(); err != nil {
		return nil, err
	}
	if err := s.store.Load(); err != nil {
		return nil, err
	}
	if err := s.responses.Load(); err != nil {
		return nil, err
	}
	if cfg.SystemPromptFile != "" {
		info, err := os.Stat(cfg.SystemPromptFile)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("KOMPUTER_SYSTEM_PROMPT_FILE must be a readable file")
		}
		if _, err := os.ReadFile(cfg.SystemPromptFile); err != nil {
			return nil, err
		}
	}
	for _, b := range registry.List {
		if err := os.MkdirAll(b.Cwd(), 0o755); err != nil {
			return nil, err
		}
		for _, note := range b.Init() {
			s.logf("%s setup: %s", b.Name(), note)
		}
		for _, note := range b.Checks(cfg.Home) {
			s.logf("WARNING: %s", note)
		}
	}
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	s.logger.Printf("[%s] %s", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

var keyFieldPattern = regexp.MustCompile(`(?i)^(api-key|legacy-api-key|work-api-key|home-api-key|key)\s*[:=]\s*(.+)$`)

func (s *Server) loadKeys() error {
	text, err := os.ReadFile(s.cfg.KeysFile)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, line := range strings.FieldsFunc(string(text), func(r rune) bool { return r == 10 || r == 13 }) {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if m := keyFieldPattern.FindStringSubmatch(t); m != nil {
			keys[strings.TrimSpace(m[2])] = true
			continue
		}
		if strings.HasPrefix(t, "sk-") {
			keys[t] = true
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("no API keys found in %s", s.cfg.KeysFile)
	}
	weak := 0
	digests := make([][]byte, 0, len(keys))
	for k := range keys {
		if len(k) < 32 {
			weak++
		}
		sum := sha256.Sum256([]byte(k))
		cp := append([]byte{}, sum[:]...)
		digests = append(digests, cp)
	}
	if weak > 0 && s.cfg.RequireStrongKeys {
		return fmt.Errorf("public API keys must be at least 32 characters; generate cryptographically random keys")
	}
	if weak > 0 {
		s.logf("WARNING: %d API key(s) shorter than 32 chars — regenerate with: openssl rand -hex 32", weak)
	}
	s.keysMu.Lock()
	s.keyDigests = digests
	s.keysMu.Unlock()
	s.logf("loaded %d API key(s) from %s", len(digests), s.cfg.KeysFile)
	return nil
}

// LoadKeys reloads API keys, keeping the old set when loading fails.
func (s *Server) LoadKeys() error { return s.loadKeys() }

func extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func (s *Server) validKey(key string) bool {
	if key == "" {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	s.keysMu.RLock()
	digests := s.keyDigests
	s.keysMu.RUnlock()
	ok := false
	for _, digest := range digests {
		if subtle.ConstantTimeCompare(digest, sum[:]) == 1 {
			ok = true
		}
	}
	return ok
}

type authLimiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	items  map[string]*authEntry
	order  []string
}

type authEntry struct {
	at    time.Time
	count int
}

func newAuthLimiter(limit int, window time.Duration) *authLimiter {
	return &authLimiter{limit: limit, window: window, items: map[string]*authEntry{}}
}

// Failed records one failure and returns a retry delay in seconds when over
// the limit, else zero.
func (l *authLimiter) Failed(address string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.items[address]
	if e == nil || now.Sub(e.at) >= l.window {
		if len(l.items) >= 4096 && len(l.order) > 0 {
			delete(l.items, l.order[0])
			l.order = l.order[1:]
		}
		e = &authEntry{at: now}
		l.items[address] = e
		l.order = append(l.order, address)
	}
	e.count++
	if e.count > l.limit {
		remain := int((e.at.Add(l.window).Sub(now) + time.Second - 1) / time.Second)
		if remain < 1 {
			remain = 1
		}
		return remain
	}
	return 0
}

func remoteSocketAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func remoteOf(r *http.Request) string {
	ip := r.RemoteAddr
	if ip == "" {
		ip = "?"
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fmt.Sprintf("%s (xff %s)", ip, strings.TrimSpace(strings.Split(fwd, ",")[0]))
	}
	return ip
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.validKey(extractAPIKey(r)) {
		return true
	}
	total := s.authFailures.Add(1)
	// Only the socket address is authoritative; a caller cannot evade the
	// limiter by inventing X-Forwarded-For. Valid keys are checked first.
	if retry := s.limiter.Failed(remoteSocketAddress(r), time.Now()); retry > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": "Too many failed authentication attempts", "type": "rate_limit_error"},
		}, map[string]string{"Retry-After": strconv.Itoa(retry)})
		return false
	}
	path := r.URL.Path
	s.logf("auth failed from %s %s %s (total %d)", remoteOf(r), r.Method, path, total)
	if s.cfg.AuthFailDelay > 0 {
		time.Sleep(s.cfg.AuthFailDelay)
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": map[string]any{"message": "Invalid API key", "type": "invalid_request_error"},
	}, nil)
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	data, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":{"message":"Internal service error. Please contact the service operator.","type":"server_error"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		return true
	}
	if !s.cfg.CORSOrigins[origin] {
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "X-Komputer-Mode,X-Komputer-Run,X-Conversation-Id,X-Chat-Id,X-Komputer-Session,X-Komputer-Claude-Session,X-Komputer-Session-Recovered,Retry-After")
	if r.Method != http.MethodOptions {
		return true
	}
	method := r.Header.Get("Access-Control-Request-Method")
	if method != "GET" && method != "POST" && method != "DELETE" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	headerPattern := regexp.MustCompile(`(?i)^[a-z0-9-]{1,64}$`)
	var reqHeaders []string
	for _, hname := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		hname = strings.TrimSpace(hname)
		if hname == "" {
			continue
		}
		if !headerPattern.MatchString(hname) {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		reqHeaders = append(reqHeaders, hname)
	}
	h.Set("Access-Control-Allow-Methods", "GET,POST,DELETE")
	if len(reqHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ","))
	}
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return false
}

// Handler returns the HTTP handler for the service.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.cors(w, r) {
			return
		}
		if err := s.dispatch(w, r); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			status := errorStatusOf(err)
			if status >= 500 {
				s.logf("error %s %s: %s", r.Method, r.URL.Path, privateDetail(err))
			}
			writeHTTPError(w, r.URL.Path, err)
		}
	})
}

func cleanPath(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) error {
	p := cleanPath(r.URL.Path)
	switch {
	case r.Method == http.MethodGet && (p == "/healthz" || p == "/health"):
		s.handleHealth(w, r)
		return nil
	case r.Method == http.MethodGet && (p == "/v1/models" || p == "/models"):
		return s.handleModels(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v1/tool-runs/"):
		if !s.requireAuth(w, r) {
			return nil
		}
		id := strings.TrimPrefix(p, "/v1/tool-runs/")
		if !isHex32(id) {
			writeJSON(w, http.StatusNotFound, map[string]any{"cancelled": false}, nil)
			return nil
		}
		cancelled := s.harness.Cancel(id)
		status := http.StatusNotFound
		if cancelled {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"cancelled": cancelled}, nil)
		return nil
	case r.Method == http.MethodPost && p == "/v1/sessions":
		return s.handleCreateSession(w, r)
	case r.Method == http.MethodGet && p == "/v1/sessions":
		return s.handleListSessions(w, r)
	case strings.HasPrefix(p, "/v1/sessions/"):
		id := strings.TrimPrefix(p, "/v1/sessions/")
		if strings.Contains(id, "/") {
			break
		}
		if r.Method == http.MethodGet {
			return s.handleGetSession(w, r, id)
		}
		if r.Method == http.MethodDelete {
			return s.handleDeleteSession(w, r, id)
		}
	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/chat/completions"):
		return s.handleChatCompletions(w, r)
	case r.Method == http.MethodPost && (p == "/v1/messages/count_tokens" || p == "/messages/count_tokens"):
		return s.handleCountTokens(w, r)
	case r.Method == http.MethodPost && (p == "/v1/messages" || p == "/messages"):
		return s.handleMessages(w, r)
	case r.Method == http.MethodPost && (p == "/v1/responses" || p == "/responses"):
		return s.handleResponses(w, r)
	case strings.HasPrefix(p, "/v1/responses/"):
		id := strings.TrimPrefix(p, "/v1/responses/")
		if !validResponseID(id) {
			break
		}
		if r.Method == http.MethodGet {
			return s.handleGetResponse(w, r, id)
		}
		if r.Method == http.MethodDelete {
			return s.handleDeleteResponse(w, r, id)
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{"message": fmt.Sprintf("not found: %s %s", r.Method, p)},
	}, nil)
	return nil
}

// ListenAndServe binds every configured listener and serves until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if (s.cfg.TLSCertFile == "") != (s.cfg.TLSKeyFile == "") {
		return fmt.Errorf("configure both KOMPUTER_TLS_CERT_FILE and KOMPUTER_TLS_KEY_FILE")
	}
	if s.cfg.TLSCertFile == "" && !s.cfg.AllowInsecureHTTP {
		for _, l := range s.cfg.Listen {
			if !config.PrivateListenAddress(l.Host) {
				return fmt.Errorf("public/wildcard listeners require TLS; for a protected TLS proxy backend, explicitly set KOMPUTER_ALLOW_INSECURE_HTTP=1")
			}
		}
	}
	var tlsConfig *tls.Config
	if s.cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	errCh := make(chan error, len(s.cfg.Listen))
	servers := make([]*http.Server, 0, len(s.cfg.Listen))
	for _, addr := range s.cfg.Listen {
		raw, err := net.Listen("tcp", addr.String())
		if err != nil {
			return err
		}
		ln := &limitedListener{Listener: raw, max: s.cfg.MaxConnections}
		srv := &http.Server{
			Handler:           s.Handler(),
			ReadHeaderTimeout: 15 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
			TLSConfig:         tlsConfig,
		}
		servers = append(servers, srv)
		go func(srv *http.Server, ln net.Listener, label string) {
			scheme := "http"
			if tlsConfig != nil {
				scheme = "https"
			}
			s.logf("komputer API listening on %s://%s", scheme, ln.Addr())
			var err error
			if tlsConfig != nil {
				err = srv.ServeTLS(ln, "", "")
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", label, err)
			}
		}(srv, ln, addr.String())
	}
	s.logf("models=%s max_in_flight=%d max_sessions=%s builtin_tools=%q no_key_mode=%s",
		strings.Join(s.backends.IDs(), ","), s.cfg.MaxInFlight, maxSessionsLabel(s.cfg.MaxSessions), s.cfg.BuiltinTools, s.cfg.NoKeyMode)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := s.loadKeys(); err != nil {
					s.logf("key reload failed, keeping old keys: %v", err)
				} else {
					s.logf("reloaded API keys")
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		s.harness.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
		_ = s.store.Save()
		return nil
	case err := <-errCh:
		for _, srv := range servers {
			_ = srv.Close()
		}
		return err
	}
}

func maxSessionsLabel(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, httpErr(http.StatusRequestEntityTooLarge, "body too large")
	}
	return data, nil
}

// requestBody is one decoded JSON request object with raw fields retained.
type requestBody struct {
	Fields map[string]json.RawMessage
	Model  string
	Stream bool
}

func (s *Server) parseBody(r *http.Request, allowEmpty bool) (*requestBody, error) {
	raw, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		if allowEmpty {
			return &requestBody{Fields: map[string]json.RawMessage{}}, nil
		}
		return nil, httpErr(http.StatusBadRequest, "empty body")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, httpErr(http.StatusBadRequest, "invalid JSON body")
	}
	rb := &requestBody{Fields: fields}
	rb.Model = rb.String("model")
	rb.Stream = rb.Bool("stream")
	return rb, nil
}

func (b *requestBody) Has(key string) bool {
	_, ok := b.Fields[key]
	return ok
}

func (b *requestBody) String(key string) string {
	var s string
	if raw, ok := b.Fields[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func (b *requestBody) Bool(key string) bool {
	var v bool
	if raw, ok := b.Fields[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

func (b *requestBody) Object(key string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if raw, ok := b.Fields[key]; ok {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (b *requestBody) Messages() ([]domain.Message, error) {
	raw, ok := b.Fields["messages"]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, httpErr(http.StatusBadRequest, "Invalid message role or shape")
	}
	messages := make([]domain.Message, 0, len(items))
	for _, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil || obj == nil {
			return nil, httpErr(http.StatusBadRequest, "Invalid message role or shape")
		}
		messages = append(messages, domain.Message{Role: rawString(obj["role"]), Content: obj["content"]})
	}
	return messages, nil
}

// limitedListener caps concurrent connections the way the JavaScript
// server's maxConnections does: excess connections are closed immediately.
type limitedListener struct {
	net.Listener
	max     int
	current atomic.Int64
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.max > 0 && l.current.Add(1) > int64(l.max) {
			l.current.Add(-1)
			_ = conn.Close()
			continue
		}
		return &countedConn{Conn: conn, release: func() { l.current.Add(-1) }}, nil
	}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
