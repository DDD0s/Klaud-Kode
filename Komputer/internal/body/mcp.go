package body

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcFailure struct {
	code    int
	message string
}

func writeMCPJSON(w http.ResponseWriter, status int, value any) {
	data, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErrorResult(id json.RawMessage, failure *rpcFailure) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": failure.code, "message": failure.message}}
}

func (s *Service) processRPC(ctx context.Context, msg rpcEnvelope) (any, *rpcFailure, bool) {
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		return nil, &rpcFailure{-32600, "Invalid Request"}, false
	}
	if len(msg.ID) == 0 {
		return nil, nil, true // notification
	}
	switch msg.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		version := "2025-06-18"
		for _, v := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
			if params.ProtocolVersion == v {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "komputer", "version": Version},
		}, nil, false
	case "ping":
		return map[string]any{}, nil, false
	case "tools/list":
		defs := s.Tools()
		tools := make([]any, 0, len(defs))
		for _, d := range defs {
			tools = append(tools, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": json.RawMessage(d.InputSchema)})
		}
		return map[string]any{"tools": tools}, nil, false
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.Name == "" {
			return nil, &rpcFailure{-32602, "Invalid params"}, false
		}
		result, err := s.CallTool(ctx, params.Name, params.Arguments)
		if err != nil {
			if IsArgError(err) {
				return nil, &rpcFailure{-32602, err.Error()}, false
			}
			return nil, &rpcFailure{-32603, err.Error()}, false
		}
		if len(result.Raw) > 0 {
			var raw any
			if err := json.Unmarshal(result.Raw, &raw); err == nil {
				return raw, nil, false
			}
		}
		out := map[string]any{"content": result.Content}
		if result.IsError {
			out["isError"] = true
		}
		return out, nil, false
	default:
		return nil, &rpcFailure{-32601, "Method not found"}, false
	}
}

func (s *Service) handleRPCMessage(ctx context.Context, raw []byte) (map[string]any, bool, bool) {
	var msg rpcEnvelope
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, false, false
	}
	result, failure, notification := s.processRPC(ctx, msg)
	if notification {
		return nil, true, true
	}
	if failure != nil {
		return rpcErrorResult(msg.ID, failure), false, true
	}
	return rpcResult(msg.ID, result), false, true
}

var (
	mcpPathPattern = regexp.MustCompile("^/mcp(?:/([A-Za-z0-9_-]+))?$")
	bearerPattern  = regexp.MustCompile("(?i)^Bearer\\s+(.+)$")
)

// tokenMatches compares digests, so the comparison time does not leak the
// token length either.
func tokenMatches(supplied, token string) bool {
	if supplied == "" || token == "" {
		return false
	}
	a := sha256.Sum256([]byte(supplied))
	b := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// maskToken keeps the token out of logs and error bodies.
func maskToken(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 && i < len(p)-1 {
		return p[:i] + "/…"
	}
	return p
}

func socketAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// Handler is the Streamable HTTP MCP endpoint plus public health.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		if r.Method == http.MethodGet && (path == "/healthz" || path == "/health") {
			writeMCPJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "kloud-kode-body", "version": Version})
			return
		}
		// The token may travel in the path, because some MCP clients
		// configure a bare URL with no way to add a header.
		match := mcpPathPattern.FindStringSubmatch(path)
		if match == nil {
			writeMCPJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("not found: %s %s", r.Method, maskToken(path))})
			return
		}
		supplied := match[1]
		if supplied == "" {
			if values := r.Header.Values("Authorization"); len(values) == 1 {
				if bearer := bearerPattern.FindStringSubmatch(values[0]); bearer != nil {
					supplied = strings.TrimSpace(bearer[1])
				}
			}
		}
		if !tokenMatches(supplied, s.cfg.Token) {
			s.logf("auth failed from %s %s %s", socketAddress(r), r.Method, maskToken(path))
			time.Sleep(1000 * time.Millisecond)
			w.Header().Set("WWW-Authenticate", `Bearer realm="kloud-kode-body"`)
			writeMCPJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
			return
		}
		// Streamable HTTP clients may open a GET stream; this server answers
		// nothing asynchronously, so an empty 200 keeps them from retrying.
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16_000_001))
		if err != nil {
			writeMCPJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable body"})
			return
		}
		var chars int
		for range string(raw) {
			chars++
		}
		if chars > 16_000_000 {
			writeMCPJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "MCP request too large"})
			return
		}
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "[") {
			var batch []json.RawMessage
			if err := json.Unmarshal(raw, &batch); err != nil {
				writeMCPJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid MCP request"})
				return
			}
			responses := []any{}
			for _, rawMsg := range batch {
				response, notification, ok := s.handleRPCMessage(r.Context(), rawMsg)
				if !ok {
					writeMCPJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid MCP request"})
					return
				}
				if !notification {
					responses = append(responses, response)
				}
			}
			if len(responses) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeMCPJSON(w, http.StatusOK, responses)
			return
		}
		response, notification, ok := s.handleRPCMessage(r.Context(), raw)
		if !ok {
			writeMCPJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid MCP request"})
			return
		}
		if notification {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeMCPJSON(w, http.StatusOK, response)
	})
}

func (s *Service) serveStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	var writeMu sync.Mutex
	for {
		line, err := reader.ReadBytes(10)
		if len(line) > 0 && strings.TrimSpace(string(line)) != "" {
			response, notification, ok := s.handleRPCMessage(ctx, line)
			if !ok {
				s.logf("invalid MCP request: %s", strings.TrimSpace(string(line)))
			} else if !notification {
				data, _ := json.Marshal(response)
				writeMu.Lock()
				_, _ = out.Write(append(data, byte(10)))
				writeMu.Unlock()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Run connects nested servers and serves Body over HTTP or stdio.
func (s *Service) Run(ctx context.Context, mode string) error {
	if mode == "" {
		mode = "http"
	}
	if mode != "http" && mode != "stdio" {
		return fmt.Errorf("usage: kloud-kode body [http|stdio]")
	}
	s.ConnectNested(ctx)
	rootsText := []string{}
	for name, dir := range s.cfg.Roots {
		rootsText = append(rootsText, name+"->"+dir)
	}
	if mode == "stdio" {
		s.logf("kloud-kode-body %s on stdio, roots: %s (files=%v shell=%v processes=%v)", Version, strings.Join(rootsText, " "), s.cfg.Files, s.cfg.Shell, s.cfg.Processes)
		return s.serveStdio(ctx, os.Stdin, os.Stdout)
	}
	if len(s.cfg.Token) < 24 {
		return fmt.Errorf("HTTP mode needs a token of at least 24 characters (set `token` in %s or KLOUD_KODE_BODY_TOKEN); refusing to start without authorization", s.cfg.ConfigPath)
	}
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Say which address the API must be pointed at: a loopback bind is not
	// reachable from another machine, which is the usual private-network slip.
	if host, _, splitErr := net.SplitHostPort(listener.Addr().String()); splitErr == nil {
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			s.logf("bound to loopback; another machine cannot reach it — set `host` in %s or KLOUD_KODE_BODY_HOST to the private-network address", s.cfg.ConfigPath)
		}
	}
	// No ReadTimeout: a tool call can stream for minutes and MCP bodies reach
	// 16 MB. The header timeout bounds opening, the read cap bounds the body.
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logf("kloud-kode-body %s on http://%s/mcp, roots: %s (files=%v shell=%v processes=%v)", Version, listener.Addr(), strings.Join(rootsText, " "), s.cfg.Files, s.cfg.Shell, s.cfg.Processes)
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
