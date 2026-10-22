package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Record is one stored response link. The on-disk shape matches the
// JavaScript ResponseStore so existing responses.json files keep working.
type Record struct {
	ID                 string          `json:"id"`
	PreviousResponseID *string         `json:"previous_response_id"`
	Key                *string         `json:"key"`
	CreatedAt          int64           `json:"created_at"`
	Input              []ChatMessage   `json:"input"`
	Output             []ChatMessage   `json:"output"`
	Response           json.RawMessage `json:"response"`
}

// Store keeps stored responses for previous_response_id chains and
// GET /v1/responses/{id}. Stored content is text-only by construction.
type Store struct {
	File string
	Max  int
	Logf func(format string, args ...any)

	mu     sync.Mutex
	saveMu sync.Mutex
	byID   map[string]*Record
	order  []string
}

// NewStore creates an empty store.
func NewStore(file string, max int) *Store {
	if max <= 0 {
		max = 1000
	}
	return &Store{File: file, Max: max, byID: map[string]*Record{}}
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Load reads the store file when present.
func (s *Store) Load() error {
	if s.File == "" {
		return nil
	}
	data, err := os.ReadFile(s.File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload struct {
		Responses []*Record `json:"responses"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		s.logf("warn: failed to load responses store: %v", err)
		return nil
	}
	s.mu.Lock()
	for _, rec := range payload.Responses {
		if rec == nil || rec.ID == "" {
			continue
		}
		if _, exists := s.byID[rec.ID]; !exists {
			s.order = append(s.order, rec.ID)
		}
		s.byID[rec.ID] = rec
	}
	size := len(s.byID)
	s.mu.Unlock()
	s.logf("loaded %d stored response(s)", size)
	return nil
}

// Get returns one stored record, or nil.
func (s *Store) Get(id string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// History returns the full message history up to and including one stored
// response, oldest first.
func (s *Store) History(id string) []ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var chain []*Record
	seen := map[string]bool{}
	for rec := s.byID[id]; rec != nil && !seen[rec.ID]; {
		seen[rec.ID] = true
		chain = append([]*Record{rec}, chain...)
		if rec.PreviousResponseID == nil || *rec.PreviousResponseID == "" {
			break
		}
		rec = s.byID[*rec.PreviousResponseID]
	}
	var out []ChatMessage
	for _, rec := range chain {
		out = append(out, rec.Input...)
		out = append(out, rec.Output...)
	}
	return out
}

// Put stores one record, evicting the oldest beyond the cap.
func (s *Store) Put(rec *Record) error {
	s.mu.Lock()
	if _, exists := s.byID[rec.ID]; exists {
		for i, id := range s.order {
			if id == rec.ID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.byID[rec.ID] = rec
	s.order = append(s.order, rec.ID)
	for len(s.byID) > s.Max && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest)
	}
	s.mu.Unlock()
	return s.save()
}

// Delete removes one stored response.
func (s *Store) Delete(id string) (bool, error) {
	s.mu.Lock()
	if _, ok := s.byID[id]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	delete(s.byID, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return true, s.save()
}

func (s *Store) save() error {
	if s.File == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	records := make([]*Record, 0, len(s.order))
	for _, id := range s.order {
		if rec := s.byID[id]; rec != nil {
			records = append(records, rec)
		}
	}
	s.mu.Unlock()
	payload := map[string]any{
		"updated_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"responses":  records,
	}
	data, err := json.Marshal(payload)
	if err != nil {
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
