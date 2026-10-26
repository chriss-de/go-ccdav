package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// An overlay holds one user's own values for events of another user's
// calendars (JMAP's per-user properties: their alerts, color, ...), in
// users/<user>/peruser/<owner>.json. The store keeps it as opaque JSON
// per event id; Version counts changes, and each event remembers the
// version it last changed at, which answers /changes for that user.

// Overlay is a user's overlay for one owner's account.
type Overlay struct {
	Version uint64                  `json:"version"`
	Events  map[string]OverlayEvent `json:"events"`
}

// OverlayEvent is one event's per-user data. Data is nil once the user
// went back to the owner's values (kept so /changes can report it).
type OverlayEvent struct {
	V    uint64          `json:"v"`
	Data json.RawMessage `json:"data,omitempty"`
}

type overlays struct {
	mu    sync.Mutex
	cache map[string]*Overlay // "<user>/<owner>"
}

func (s *Store) overlayPath(user, owner string) string {
	return filepath.Join(s.root, "users", user, "peruser", owner+".json")
}

// loadOverlay reads an overlay; the caller holds s.over.mu.
func (s *Store) loadOverlay(user, owner string) (*Overlay, error) {
	key := user + "/" + owner
	if o, ok := s.over.cache[key]; ok {
		return o, nil
	}
	o := &Overlay{Events: map[string]OverlayEvent{}}
	b, err := os.ReadFile(s.overlayPath(user, owner))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, o); err != nil {
			return nil, fmt.Errorf("%s: %w", s.overlayPath(user, owner), err)
		}
		if o.Events == nil {
			o.Events = map[string]OverlayEvent{}
		}
	}
	if s.over.cache == nil {
		s.over.cache = map[string]*Overlay{}
	}
	s.over.cache[key] = o
	return o, nil
}

func (s *Store) saveOverlay(user, owner string, o *Overlay) error {
	b, _ := json.MarshalIndent(o, "", "  ")
	p := s.overlayPath(user, owner)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

// Overlay returns a copy of user's overlay for owner's account.
func (s *Store) Overlay(user, owner string) (Overlay, error) {
	s.over.mu.Lock()
	defer s.over.mu.Unlock()
	o, err := s.loadOverlay(user, owner)
	if err != nil {
		return Overlay{}, err
	}
	return Overlay{Version: o.Version, Events: maps.Clone(o.Events)}, nil
}

// SetOverlay sets user's data for an event of owner's (nil: back to the
// owner's values). An unchanged value is not a change.
func (s *Store) SetOverlay(user, owner, eventID string, data json.RawMessage) error {
	s.over.mu.Lock()
	o, err := s.loadOverlay(user, owner)
	if err != nil {
		s.over.mu.Unlock()
		return err
	}
	old, had := o.Events[eventID]
	if had && string(old.Data) == string(data) || !had && data == nil {
		s.over.mu.Unlock()
		return nil
	}
	next := &Overlay{Version: o.Version + 1, Events: maps.Clone(o.Events)}
	next.Events[eventID] = OverlayEvent{V: next.Version, Data: slices.Clone(data)}
	if err := s.saveOverlay(user, owner, next); err != nil {
		s.over.mu.Unlock()
		return err
	}
	s.over.cache[user+"/"+owner] = next
	s.over.mu.Unlock()
	s.changed(owner, Calendars)
	return nil
}

// pruneOverlays drops overlay entries of events that no longer exist in
// the owners' accounts. Reconcile calls it.
func (s *Store) pruneOverlays() error {
	users, err := os.ReadDir(filepath.Join(s.root, "users"))
	if err != nil {
		return nil
	}
	var errs []error
	for _, u := range users {
		files, _ := filepath.Glob(filepath.Join(s.root, "users", u.Name(), "peruser", "*.json"))
		for _, f := range files {
			owner := filepath.Base(f[:len(f)-len(".json")])
			a, err := s.Account(owner, Calendars)
			if err != nil {
				continue
			}
			s.over.mu.Lock()
			o, err := s.loadOverlay(u.Name(), owner)
			if err == nil {
				next := &Overlay{Version: o.Version, Events: map[string]OverlayEvent{}}
				for id, ev := range o.Events {
					if _, live := a.Lookup(id); live {
						next.Events[id] = ev
					}
				}
				if len(next.Events) != len(o.Events) {
					if err = s.saveOverlay(u.Name(), owner, next); err == nil {
						s.over.cache[u.Name()+"/"+owner] = next
					}
				}
			}
			s.over.mu.Unlock()
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
