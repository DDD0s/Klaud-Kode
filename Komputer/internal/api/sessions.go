package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/session"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/version"
)

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rb, err := s.parseBody(r, true)
	if err != nil {
		return err
	}
	clientKey := ""
	for _, k := range []string{"conversation_id", "client_key", "chat_id", "id", "session_id"} {
		if v := domain.NormalizeKey(rb.String(k)); v != "" {
			clientKey = v
			break
		}
	}
	label := rb.String("label")
	if label == "" {
		label = rb.String("name")
	}
	chosen := s.backends.Route(rb.Model)
	rec, err := s.store.Create(clientKey, label, chosen.Name(), chosen.NewSessionID(), false)
	if errors.Is(err, session.ErrTooManySessions) {
		return typedErr(http.StatusTooManyRequests, "max live sessions reached", "rate_limit_error")
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.store.Public(rec), conversationHeaders(&plan{kind: "session", rec: rec}))
	return nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": s.store.PublicList(),
		"max_sessions": s.cfg.MaxSessions, "max_in_flight": s.cfg.MaxInFlight,
		"idle_timeout_ms": s.cfg.IdleTimeout.Milliseconds(), "live_count": s.store.LiveCount(time.Now()),
	}, nil)
	return nil
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request, id string) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rec := s.store.Find(domain.NormalizeKey(id))
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "session not found"}}, nil)
		return nil
	}
	writeJSON(w, http.StatusOK, s.store.Public(rec), conversationHeaders(&plan{kind: "session", rec: rec}))
	return nil
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request, id string) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	ok, err := s.store.Delete(id)
	if err != nil {
		return err
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "session not found"}}, nil)
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "conversation_id": id, "id": id}, nil)
	return nil
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	data := modelList(s.backends.IDs())
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": data, "has_more": false,
		"first_id": data[0]["id"], "last_id": data[len(data)-1]["id"],
	}, nil)
	return nil
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rb, err := s.parseBody(r, false)
	if err != nil {
		return err
	}
	messages, _ := rb.Messages()
	parts := []string{anthropicSystemText(rb.Fields["system"])}
	for _, m := range messages {
		parts = append(parts, domain.ContentToText(m.Content))
	}
	text := strings.Join(parts, newline)
	// JavaScript measured UTF-16 code units; astral characters count as two
	// there, so use that length to keep estimates identical across runtimes.
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	tokens := int(math.Round(float64(units) / 3.5))
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens}, nil)
	return nil
}

// maskURLToken hides a token carried in a URL path segment, which the health
// payload can be read by any authenticated caller.
func maskURLToken(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) >= 2 && segments[0] == "mcp" {
		parsed.Path = "/mcp/" + maskToken(segments[1])
	}
	return parsed.String()
}

// maskToken keeps the first four and last two characters of a secret.
func maskToken(token string) string {
	if len(token) <= 8 {
		return "masked"
	}
	return token[:4] + "..." + token[len(token)-2:]
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	base := map[string]any{"ok": true, "service": "komputer-api"}
	if !s.validKey(extractAPIKey(r)) {
		writeJSON(w, http.StatusOK, base, nil)
		return
	}
	// A direct Body is checked here, so an operator sees reachability without a
	// turn; the guard caches both outcomes.
	if s.cfg.BodyURL != "" {
		_ = s.tunnel.ensure()
	}
	bodyOK, bodyChecked := s.tunnel.status()
	listen := make([]string, 0, len(s.cfg.Listen))
	for _, l := range s.cfg.Listen {
		listen = append(listen, l.String())
	}
	maxSessions := any(s.cfg.MaxSessions)
	if s.cfg.MaxSessions == 0 {
		maxSessions = "unlimited"
	}
	body := map[string]any{
		"ok": true, "service": "komputer-api",
		"model": s.backends.Default.DefaultID(), "models": s.backends.IDs(), "backends": s.backends.Names(),
		"sessions": s.store.Size(), "live_sessions": s.store.LiveCount(time.Now()),
		"in_flight": len(s.slots), "queued": s.queued.Load(),
		"max_sessions": maxSessions, "max_in_flight": s.cfg.MaxInFlight,
		"idle_timeout_ms": s.cfg.IdleTimeout.Milliseconds(), "auth_failures": s.authFailures.Load(),
		"builtin_tools": s.cfg.BuiltinTools, "no_key_mode": s.cfg.NoKeyMode, "listen": listen,
		"body_probe_skipped": s.cfg.SkipTunnel && s.cfg.BodyURL == "",
		"body_direct":        s.cfg.BodyURL != "",
		"client_tools":       s.harness.Stats(),
		"implementation":     "go",
		"version":            version.Value,
	}
	// A direct Body URL is probed for real; only a skipped check reports null.
	if bodyOK != nil {
		body["body_ok"] = *bodyOK
	} else {
		body["body_ok"] = nil
	}
	if s.cfg.BodyURL != "" {
		body["body_url"] = maskURLToken(s.cfg.BodyURL)
	}
	if bodyChecked != nil {
		body["body_checked_at"] = *bodyChecked
	} else {
		body["body_checked_at"] = nil
	}
	writeJSON(w, http.StatusOK, body, nil)
}
