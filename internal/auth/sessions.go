package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Sessions keeps a generation number per user, in
// data/users/<user>/sessions.json. Web sessions carry the generation they
// were started in; ending a user's sessions (logout) bumps it, which
// invalidates every session cookie issued before.
type Sessions struct {
	root string // data directory

	mu  sync.Mutex
	gen map[string]uint64
}

func NewSessions(dataDir string) *Sessions {
	return &Sessions{root: dataDir, gen: map[string]uint64{}}
}

type sessionsFile struct {
	Generation uint64 `json:"generation"`
}

func (s *Sessions) path(user string) string {
	return filepath.Join(s.root, "users", user, "sessions.json")
}

// load returns user's generation; the caller holds s.mu.
func (s *Sessions) load(user string) (uint64, error) {
	if g, ok := s.gen[user]; ok {
		return g, nil
	}
	var f sessionsFile
	b, err := os.ReadFile(s.path(user))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		if err := json.Unmarshal(b, &f); err != nil {
			return 0, fmt.Errorf("%s: %w", s.path(user), err)
		}
	}
	s.gen[user] = f.Generation
	return f.Generation, nil
}

// Generation returns user's current session generation.
func (s *Sessions) Generation(user string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(user)
}

// End ends all of user's sessions.
func (s *Sessions) End(user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.load(user)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sessionsFile{Generation: g + 1})
	p := s.path(user)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	s.gen[user] = g + 1
	return nil
}
