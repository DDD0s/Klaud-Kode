// Package session persists client conversation to CLI session mappings.
// The on-disk format matches the JavaScript implementation so existing
// sessions.json files keep working across the rewrite.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

// Record is one client conversation mapping.
type Record struct {
	ClientKey        string
	ClaudeSessionID  string
	Backend          string // empty means Claude
	BackendSessionID string
	HasBackendID     bool
	SystemSHA        string
	CreatedAt        string
	LastUsedAt       string
	TurnCount        int
	Started          bool
	HistorySeen      bool
	LastReplyTail    string
	Reseeds          int
	Label            string
}

// BackendName returns the backend that owns this record.
func (r *Record) BackendName() string {
	if r.Backend == "" {
		return "claude"
	}
	return r.Backend
}

// CLISessionID returns the backend CLI session id, or "" when the CLI has
// not assigned one yet.
func (r *Record) CLISessionID() string {
	if r.Backend != "" {
		return r.BackendSessionID
	}
	return r.ClaudeSessionID
}

// Public is the session shape returned by the sessions API.
type Public struct {
	ConversationID   string  `json:"conversation_id"`
	ClientKey        string  `json:"client_key"`
	ID               string  `json:"id"`
	CreatedAt        string  `json:"created_at"`
	LastUsedAt       string  `json:"last_used_at"`
	TurnCount        int     `json:"turn_count"`
	Started          bool    `json:"started"`
	Reseeds          int     `json:"reseeds"`
	Live             bool    `json:"live"`
	Label            *string `json:"label"`
	Backend          string  `json:"backend,omitempty"`
	BackendSessionID *string `json:"backend_session_id,omitempty"`
	ClaudeSessionID  string  `json:"claude_session_id,omitempty"`
}

// Store is a concurrency-safe session mapping file.
type Store struct {
	File        string
	MaxSessions int
	IdleTimeout time.Duration
	Logf        func(format string, args ...any)

	mu       sync.RWMutex
	saveMu   sync.Mutex
	byClient map[string]*Record
	byCLI    map[string]string
}

// New creates an empty store.
func New(file string, maxSessions int, idleTimeout time.Duration) *Store {
	return &Store{
		File: file, MaxSessions: maxSessions, IdleTimeout: idleTimeout,
		byClient: map[string]*Record{}, byCLI: map[string]string{},
	}
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// NowISO formats t the way the JavaScript store does.
func NowISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type rawRecord struct {
	ClientKey        string  `json:"client_key"`
	ID               string  `json:"id"`
	ConversationID   string  `json:"conversation_id"`
	ClaudeSessionID  string  `json:"claude_session_id"`
	Backend          string  `json:"backend"`
	BackendSessionID *string `json:"backend_session_id"`
	SystemSHA        string  `json:"system_sha"`
	CreatedAt        string  `json:"created_at"`
	LastUsedAt       string  `json:"last_used_at"`
	TurnCount        int     `json:"turn_count"`
	Started          bool    `json:"started"`
	HistorySeen      bool    `json:"history_seen"`
	LastReplyTail    string  `json:"last_reply_tail"`
	Reseeds          int     `json:"reseeds"`
	Label            string  `json:"label"`
}

// Load reads the store file when present. A missing file is an empty store.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.File)
	if errors.Is(err, fs.ErrNotExist) {
		s.logf("no sessions store yet — starting empty")
		return nil
	}
	if err != nil {
		return err
	}
	var payload struct {
		Sessions json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		s.logf("warn: failed to load sessions store: %v", err)
		return nil
	}
	var list []rawRecord
	if err := json.Unmarshal(payload.Sessions, &list); err != nil {
		var byKey map[string]rawRecord
		if err := json.Unmarshal(payload.Sessions, &byKey); err != nil {
			s.logf("warn: failed to load sessions store: %v", err)
			return nil
		}
		for _, rec := range byKey {
			list = append(list, rec)
		}
	}
	now := NowISO(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range list {
		key := domain.NormalizeKey(firstNonEmpty(raw.ClientKey, raw.ID, raw.ConversationID))
		if key == "" {
			continue
		}
		created := raw.CreatedAt
		if created == "" {
			created = now
		}
		last := raw.LastUsedAt
		if last == "" {
			last = created
		}
		rec := &Record{
			ClientKey: key, CreatedAt: created, LastUsedAt: last,
			TurnCount: raw.TurnCount, HistorySeen: raw.HistorySeen,
			LastReplyTail: raw.LastReplyTail, Reseeds: raw.Reseeds, Label: raw.Label,
		}
		if raw.Backend != "" && raw.Backend != "claude" {
			rec.Backend = raw.Backend
			rec.SystemSHA = raw.SystemSHA
			if raw.BackendSessionID != nil && *raw.BackendSessionID != "" {
				rec.BackendSessionID = *raw.BackendSessionID
				rec.HasBackendID = true
				s.byCLI[rec.BackendSessionID] = key
			}
			rec.Started = raw.Started && rec.HasBackendID
		} else {
			id := raw.ClaudeSessionID
			if id == "" && domain.IsUUID(raw.ID) {
				id = raw.ID
			}
			if !domain.IsUUID(id) {
				id = domain.NewUUID()
			}
			rec.ClaudeSessionID = id
			rec.Started = raw.Started
			s.byCLI[id] = key
		}
		s.byClient[key] = rec
	}
	s.logf("loaded %d session map(s) from %s", len(s.byClient), s.File)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Save writes the store atomically. Saves are serialized.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	sessions := make([]map[string]any, 0, len(s.byClient))
	for _, rec := range s.byClient {
		m := map[string]any{
			"client_key":      rec.ClientKey,
			"conversation_id": rec.ClientKey,
			"created_at":      rec.CreatedAt,
			"last_used_at":    rec.LastUsedAt,
			"turn_count":      rec.TurnCount,
			"started":         rec.Started,
			"id":              rec.ClientKey,
		}
		if rec.Backend != "" {
			m["backend"] = rec.Backend
			if rec.HasBackendID {
				m["backend_session_id"] = rec.BackendSessionID
			} else {
				m["backend_session_id"] = nil
			}
			if rec.SystemSHA != "" {
				m["system_sha"] = rec.SystemSHA
			}
		} else {
			m["claude_session_id"] = rec.ClaudeSessionID
		}
		if rec.HistorySeen {
			m["history_seen"] = true
		}
		if rec.LastReplyTail != "" {
			m["last_reply_tail"] = rec.LastReplyTail
		}
		if rec.Reseeds != 0 {
			m["reseeds"] = rec.Reseeds
		}
		if rec.Label != "" {
			m["label"] = rec.Label
		}
		sessions = append(sessions, m)
	}
	s.mu.RUnlock()

	payload := map[string]any{"updated_at": NowISO(time.Now()), "sessions": sessions}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.File), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", s.File, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.File); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Find resolves a client key or a CLI session id to its record.
func (s *Store) Find(key string) *Record {
	if key == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec := s.byClient[key]; rec != nil {
		return rec
	}
	if client, ok := s.byCLI[key]; ok {
		return s.byClient[client]
	}
	return nil
}

// SetCLISessionID points a record at a new CLI session id.
func (s *Store) SetCLISessionID(rec *Record, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := rec.CLISessionID(); old != "" && s.byCLI[old] == rec.ClientKey {
		delete(s.byCLI, old)
	}
	if rec.Backend != "" {
		rec.BackendSessionID = id
		rec.HasBackendID = id != ""
	} else {
		rec.ClaudeSessionID = id
	}
	if id != "" {
		s.byCLI[id] = rec.ClientKey
	}
}

// AssignBackend points a record at a backend with a fresh CLI session.
func (s *Store) AssignBackend(rec *Record, backend, sessionID string) {
	s.mu.Lock()
	if old := rec.CLISessionID(); old != "" && s.byCLI[old] == rec.ClientKey {
		delete(s.byCLI, old)
	}
	if backend == "claude" {
		rec.Backend, rec.BackendSessionID, rec.HasBackendID, rec.SystemSHA = "", "", false, ""
		rec.ClaudeSessionID = sessionID
	} else {
		rec.ClaudeSessionID = ""
		rec.Backend = backend
		rec.BackendSessionID = sessionID
		rec.HasBackendID = sessionID != ""
		rec.SystemSHA = ""
	}
	if sessionID != "" {
		s.byCLI[sessionID] = rec.ClientKey
	}
	s.mu.Unlock()
}

// ErrTooManySessions reports the live-session cap being reached.
var ErrTooManySessions = errors.New("max live sessions reached")

// Create returns the existing record for clientKey, or creates and saves one.
func (s *Store) Create(clientKey, label, backend, sessionID string, force bool) (*Record, error) {
	key := domain.NormalizeKey(clientKey)
	if key != "" {
		if rec := s.Find(key); rec != nil {
			return rec, nil
		}
	} else {
		key = "auto-" + domain.NewUUID()
	}
	if !force && s.MaxSessions > 0 && s.LiveCount(time.Now()) >= s.MaxSessions {
		return nil, ErrTooManySessions
	}
	now := NowISO(time.Now())
	rec := &Record{ClientKey: key, CreatedAt: now, LastUsedAt: now, Label: label}
	if backend == "" || backend == "claude" {
		if sessionID == "" {
			sessionID = domain.NewUUID()
		}
		rec.ClaudeSessionID = sessionID
	} else {
		rec.Backend = backend
		rec.BackendSessionID = sessionID
		rec.HasBackendID = sessionID != ""
	}
	s.mu.Lock()
	s.byClient[key] = rec
	if sessionID != "" {
		s.byCLI[sessionID] = key
	}
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return nil, err
	}
	s.logf("session map created client_key=%s %s=%s", key, rec.BackendName(), emptyAs(sessionID, "(assigned by cli)"))
	return rec, nil
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Delete removes a mapping by client key or CLI session id.
func (s *Store) Delete(key string) (bool, error) {
	rec := s.Find(domain.NormalizeKey(key))
	if rec == nil {
		return false, nil
	}
	s.mu.Lock()
	delete(s.byClient, rec.ClientKey)
	if id := rec.CLISessionID(); id != "" {
		delete(s.byCLI, id)
	}
	s.mu.Unlock()
	return true, s.Save()
}

// IsLive reports whether a record was used inside the idle window.
func (s *Store) IsLive(rec *Record, now time.Time) bool {
	last, err := time.Parse(time.RFC3339, rec.LastUsedAt)
	if err != nil {
		last, err = time.Parse("2006-01-02T15:04:05.000Z", rec.LastUsedAt)
		if err != nil {
			return false
		}
	}
	return now.Sub(last) < s.IdleTimeout
}

// LiveCount counts records inside the idle window.
func (s *Store) LiveCount(now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, rec := range s.byClient {
		if s.IsLive(rec, now) {
			n++
		}
	}
	return n
}

// Size returns the number of stored mappings.
func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byClient)
}

// PublicList returns all mappings in API shape, most recently used first.
func (s *Store) PublicList() []Public {
	s.mu.RLock()
	recs := make([]*Record, 0, len(s.byClient))
	for _, rec := range s.byClient {
		recs = append(recs, rec)
	}
	s.mu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].LastUsedAt > recs[j].LastUsedAt })
	out := make([]Public, 0, len(recs))
	for _, rec := range recs {
		out = append(out, s.Public(rec))
	}
	return out
}

// Public renders one record in API shape.
func (s *Store) Public(rec *Record) Public {
	p := Public{
		ConversationID: rec.ClientKey, ClientKey: rec.ClientKey, ID: rec.ClientKey,
		CreatedAt: rec.CreatedAt, LastUsedAt: rec.LastUsedAt, TurnCount: rec.TurnCount,
		Started: rec.Started, Reseeds: rec.Reseeds, Live: s.IsLive(rec, time.Now()),
	}
	if rec.Label != "" {
		p.Label = &rec.Label
	}
	if rec.Backend != "" {
		p.Backend = rec.Backend
		if rec.HasBackendID {
			p.BackendSessionID = &rec.BackendSessionID
		}
	} else {
		p.ClaudeSessionID = rec.ClaudeSessionID
	}
	return p
}
