package api

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/session"
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
	tokens := int(math.Round(float64(len([]rune(text))) / 3.5))
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens}, nil)
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	base := map[string]any{"ok": true, "service": "komputer-api"}
	if !s.validKey(extractAPIKey(r)) {
		writeJSON(w, http.StatusOK, base, nil)
		return
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
		"body_probe_skipped": s.cfg.SkipTunnel,
		"client_tools":       s.harness.Stats(),
		"implementation":     "go-rewrite-slice-6",
	}
	if s.cfg.SkipTunnel {
		body["body_ok"] = nil
	} else if bodyOK != nil {
		body["body_ok"] = *bodyOK
	} else {
		body["body_ok"] = nil
	}
	if bodyChecked != nil {
		body["body_checked_at"] = *bodyChecked
	} else {
		body["body_checked_at"] = nil
	}
	writeJSON(w, http.StatusOK, body, nil)
}
