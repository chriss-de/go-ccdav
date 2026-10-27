package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Share grants a user or a group access to a collection.
type Share struct {
	User   string `json:"user,omitempty"`
	Group  string `json:"group,omitempty"`
	Access string `json:"access"` // AccessRead or AccessWrite
}

const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// AccessOwner is returned by AccessOf for the collection's owner.
const AccessOwner = "owner"

// AccessOf returns user's effective access to c: AccessOwner, AccessWrite,
// AccessRead or "" (none). The highest of direct and group shares wins.
func (c *Collection) AccessOf(user string, groups []string) string {
	if c.Owner == user {
		return AccessOwner
	}
	best := ""
	for _, sh := range c.Shares() {
		if sh.User == user || sh.Group != "" && slices.Contains(groups, sh.Group) {
			if sh.Access == AccessWrite {
				return AccessWrite
			}
			best = AccessRead
		}
	}
	return best
}

// Key is the collection's stable identity, "<owner>/<kind>/<name>". Shares
// and per-sharee preferences refer to collections by it, never by URL.
func (c *Collection) Key() string { return c.Owner + "/" + string(c.Kind) + "/" + c.Name }

// Shares returns the collection's shares.
func (c *Collection) Shares() []Share {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.meta.Shares)
}

// SetShares replaces the shares. It re-reads .meta.json first and does not
// bump the sync token: shares change who sees the collection, not its
// content. Safe to call from a separate process (the CLI); a running server
// picks the change up with ReloadMeta.
func (c *Collection) SetShares(shares []Share) error {
	for _, sh := range shares {
		if (sh.User == "") == (sh.Group == "") || sh.Access != AccessRead && sh.Access != AccessWrite {
			return fmt.Errorf("store: invalid share %+v", sh)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted {
		return ErrNotFound
	}
	if err := c.refreshMeta(); err != nil {
		return err
	}
	if c.meta.ReadOnly && len(shares) > 0 {
		return ErrReadOnly
	}
	m := c.metaCopy()
	m.Shares = slices.Clone(shares)
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(c.dir, ".meta.json"), b); err != nil {
		return err
	}
	c.meta = m
	return c.acct.noteCollection(c)
}

// refreshMeta re-reads .meta.json; the caller holds the write lock.
func (c *Collection) refreshMeta() error {
	b, err := os.ReadFile(filepath.Join(c.dir, ".meta.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", c.dir, err)
	}
	if m.SyncID == "" {
		m.SyncID = c.meta.SyncID
	}
	c.meta = m
	return nil
}

// LoadAll loads every collection of every user, so that All (and with it
// the lookup of collections shared with someone) sees all of them.
func (s *Store) LoadAll() error {
	users, err := os.ReadDir(filepath.Join(s.root, "users"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		for _, k := range []Kind{Calendars, AddressBooks} {
			if _, err := s.Collections(u.Name(), k); err != nil {
				return err
			}
		}
	}
	return s.uniqueSyncIDs()
}

// uniqueSyncIDs gives a collection a new SyncID if another of the same
// owner and kind has its SyncID, as a directory copied by hand does: the
// two would share an id and a sync token space (deleting the copy would
// log deletions of the original's objects). The oldest .meta.json keeps
// its id; a copy is usually newer.
func (s *Store) uniqueSyncIDs() error {
	type metaAge struct {
		c   *Collection
		mod time.Time
	}
	groups := map[string][]metaAge{}
	for _, c := range s.All() {
		var mod time.Time
		if fi, err := os.Stat(filepath.Join(c.dir, ".meta.json")); err == nil {
			mod = fi.ModTime()
		}
		k := c.Owner + "/" + string(c.Kind) + "/" + c.SyncID()
		groups[k] = append(groups[k], metaAge{c, mod})
	}
	var errs []error
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		slices.SortFunc(g, func(a, b metaAge) int {
			if c := a.mod.Compare(b.mod); c != 0 {
				return c
			}
			return strings.Compare(a.c.Name, b.c.Name)
		})
		for _, m := range g[1:] {
			errs = append(errs, m.c.newSyncID())
		}
	}
	return errors.Join(errs...)
}

// All returns all loaded collections, sorted by key.
func (s *Store) All() []*Collection {
	s.mu.Lock()
	out := make([]*Collection, 0, len(s.cols))
	for _, c := range s.cols {
		out = append(out, c)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b *Collection) int { return strings.Compare(a.Key(), b.Key()) })
	return out
}

// ReloadMeta re-reads .meta.json of all loaded collections (after the CLI
// changed shares), loads collections created since startup and, when
// tracking accounts, reconciles the account logs.
func (s *Store) ReloadMeta() error {
	if err := s.LoadAll(); err != nil {
		return err
	}
	var errs []error
	for _, c := range s.All() {
		c.mu.Lock()
		if !c.deleted {
			errs = append(errs, c.refreshMeta())
		}
		c.mu.Unlock()
	}
	errs = append(errs, s.reconcile(false)) // while serving: no batch
	return errors.Join(errs...)
}

// SharePrefs are a sharee's personal settings for a collection shared with
// them. Version increases with every change and is part of the sharee's
// sync token, so their clients notice e.g. a new color.
type SharePrefs struct {
	DisplayName string `json:"displayname,omitempty"`
	Color       string `json:"color,omitempty"`
	Order       string `json:"order,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
	Invisible   bool   `json:"invisible,omitempty"` // JMAP isVisible=false
	Version     uint64 `json:"version,omitempty"`
	// For is the SyncID of the collection they were made for: a new
	// collection under the same name starts without them. Empty in
	// entries from before it was recorded; those adopt the collection.
	For string `json:"for,omitempty"`
}

// prefsStore caches data/users/<user>/shared.json files.
type prefsStore struct {
	mu    sync.Mutex
	users map[string]map[string]SharePrefs
}

func (s *Store) prefsPath(user string) string {
	return filepath.Join(s.root, "users", user, "shared.json")
}

func (s *Store) loadPrefs(user string) (map[string]SharePrefs, error) {
	if p, ok := s.prefs.users[user]; ok {
		return p, nil
	}
	p := map[string]SharePrefs{}
	b, err := os.ReadFile(s.prefsPath(user))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", s.prefsPath(user), err)
		}
	}
	if s.prefs.users == nil {
		s.prefs.users = map[string]map[string]SharePrefs{}
	}
	s.prefs.users[user] = p
	return p, nil
}

// prefsFor returns the entry of all for c, if it was made for c.
func prefsFor(all map[string]SharePrefs, c *Collection) SharePrefs {
	p := all[c.Key()]
	if p.For != "" && p.For != c.SyncID() {
		return SharePrefs{} // of a deleted collection with the same name
	}
	return p
}

// SharePrefs returns user's settings for collection c.
func (s *Store) SharePrefs(user string, c *Collection) SharePrefs {
	s.prefs.mu.Lock()
	defer s.prefs.mu.Unlock()
	all, err := s.loadPrefs(user)
	if err != nil {
		return SharePrefs{}
	}
	return prefsFor(all, c)
}

// UpdateSharePrefs applies fn to user's settings for c and persists the
// result if it changed.
func (s *Store) UpdateSharePrefs(user string, c *Collection, fn func(*SharePrefs)) error {
	s.prefs.mu.Lock()
	defer s.prefs.mu.Unlock()
	all, err := s.loadPrefs(user)
	if err != nil {
		return err
	}
	key := c.Key()
	old := prefsFor(all, c)
	p := old
	fn(&p)
	p.Version, p.For = old.Version, old.For
	if p == old {
		return nil
	}
	p.For = c.SyncID()
	if p == old {
		return nil
	}
	p.Version++
	next := make(map[string]SharePrefs, len(all)+1)
	for k, v := range all {
		next[k] = v
	}
	next[key] = p
	b, _ := json.MarshalIndent(next, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.prefsPath(user)), 0o750); err != nil {
		return err
	}
	if err := writeFileAtomic(s.prefsPath(user), b); err != nil {
		return err
	}
	s.prefs.users[user] = next
	if owner, kind, ok := strings.Cut(key, "/"); ok {
		kind, _, _ = strings.Cut(kind, "/")
		s.changed(owner, Kind(kind))
	}
	return nil
}
