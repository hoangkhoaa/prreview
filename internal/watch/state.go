package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store persists which PR heads have been reviewed and which PRs the user
// removed, so restarts don't re-review or resurrect them. Shared by the
// watch dashboard, the headless daemon, and the web UI. Stored at
// ~/.config/prreview/watch-state.json.
type Store struct {
	mu   sync.Mutex
	path string
	data struct {
		Reviewed map[string]string `json:"reviewed"` // key -> head OID
		Ignored  map[string]bool   `json:"ignored"`
	}
}

func statePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "prreview", "watch-state.json"), nil
}

// LoadStore reads (or initializes) the persistent watch state.
func LoadStore() (*Store, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}
	s := &Store{path: p}
	s.data.Reviewed = map[string]string{}
	s.data.Ignored = map[string]bool{}
	if raw, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(raw, &s.data)
		if s.data.Reviewed == nil {
			s.data.Reviewed = map[string]string{}
		}
		if s.data.Ignored == nil {
			s.data.Ignored = map[string]bool{}
		}
	}
	return s, nil
}

func (s *Store) save() {
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	_ = os.WriteFile(s.path, append(raw, '\n'), 0o600)
}

// Get returns the reviewed head OID for a PR key ("" if never reviewed).
func (s *Store) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Reviewed[key]
}

// Set records that the given head OID of a PR was reviewed.
func (s *Store) Set(key, headOid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Reviewed[key] = headOid
	s.save()
}

// Clear forgets both the reviewed mark and the ignore flag for a PR.
func (s *Store) Clear(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Reviewed, key)
	delete(s.data.Ignored, key)
	s.save()
}

// Ignore marks a PR to never be auto-picked-up again.
func (s *Store) Ignore(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Ignored[key] = true
	s.save()
}

// Ignored reports whether a PR was removed by the user.
func (s *Store) Ignored(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Ignored[key]
}
