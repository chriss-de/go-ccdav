// Package store keeps calendars and address books as plain files:
//
//	<root>/users/<user>/<kind>/<collection>/
//	    .meta.json     collection properties
//	    .changes.log   append-only "<token> <PUT|DEL> <name>" lines
//	    <name>         object resources (.ics / .vcf), stored byte-exact
//
// Every collection has its own RWMutex and an in-memory index (ETag, UID,
// size, mtime) built on first use.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Kind string

const (
	Calendars    Kind = "calendars"
	AddressBooks Kind = "addressbooks"
)

var (
	ErrNotFound     = errors.New("store: not found")
	ErrExists       = errors.New("store: already exists")
	ErrPrecondition = errors.New("store: precondition failed")
	ErrBadName      = errors.New("store: invalid name")
)

// UIDConflictError is returned by Put when another object in the collection
// already uses the UID.
type UIDConflictError struct{ Existing string }

func (e *UIDConflictError) Error() string { return "store: UID already used by " + e.Existing }

// Meta holds collection properties, persisted as .meta.json.
type Meta struct {
	DisplayName string   `json:"displayname,omitempty"`
	Description string   `json:"description,omitempty"`
	Color       string   `json:"color,omitempty"`
	Order       string   `json:"order,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`   // VCALENDAR text with one VTIMEZONE
	Components  []string `json:"components,omitempty"` // VEVENT, VTODO, VJOURNAL
	// DeadProps stores unknown PROPPATCHed properties: "{ns}local" -> inner XML.
	DeadProps map[string]string `json:"dead_props,omitempty"`
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Name    string
	ETag    string // quoted
	UID     string
	Size    int64
	ModTime time.Time
}

// UIDFunc extracts the UID from an object body; "" if none.
type UIDFunc func(kind Kind, data []byte) string

type Store struct {
	root  string
	uidOf UIDFunc

	mu   sync.Mutex
	cols map[string]*Collection
}

func New(root string, uidOf UIDFunc) *Store {
	return &Store{root: root, uidOf: uidOf, cols: make(map[string]*Collection)}
}

// ValidName reports whether name is usable as a collection or object name.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '.' {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func (s *Store) homeDir(owner string, kind Kind) string {
	return filepath.Join(s.root, "users", owner, string(kind))
}

// EnsureHome creates the home directory if needed. When it did not exist
// yet, the given default collections are created in it.
func (s *Store) EnsureHome(owner string, kind Kind, defaults map[string]Meta) error {
	dir := s.homeDir(owner, kind)
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for name, m := range defaults {
		if _, err := s.CreateCollection(owner, kind, name, m); err != nil && !errors.Is(err, ErrExists) {
			return err
		}
	}
	return nil
}

// Collections lists the owner's collections of a kind, sorted by name.
func (s *Store) Collections(owner string, kind Kind) ([]*Collection, error) {
	ents, err := os.ReadDir(s.homeDir(owner, kind))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Collection
	for _, e := range ents {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		c, err := s.Collection(owner, kind, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Collection returns a loaded collection or ErrNotFound.
func (s *Store) Collection(owner string, kind Kind, name string) (*Collection, error) {
	if !ValidName(name) {
		return nil, ErrNotFound
	}
	key := owner + "/" + string(kind) + "/" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cols[key]; ok {
		return c, nil
	}
	c := &Collection{
		Owner: owner, Kind: kind, Name: name,
		dir:   filepath.Join(s.homeDir(owner, kind), name),
		uidOf: s.uidOf,
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	s.cols[key] = c
	return c, nil
}

// CreateCollection makes a new collection.
func (s *Store) CreateCollection(owner string, kind Kind, name string, m Meta) (*Collection, error) {
	if !ValidName(name) {
		return nil, ErrBadName
	}
	dir := filepath.Join(s.homeDir(owner, kind), name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, ErrExists
		}
		return nil, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, ".meta.json"), b); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return s.Collection(owner, kind, name)
}

// DeleteCollection removes a collection and all its objects.
func (s *Store) DeleteCollection(c *Collection) error {
	key := c.Owner + "/" + string(c.Kind) + "/" + c.Name
	s.mu.Lock()
	delete(s.cols, key)
	s.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = true
	return os.RemoveAll(c.dir)
}

// Collection is one calendar or address book.
type Collection struct {
	Owner string
	Kind  Kind
	Name  string

	dir   string
	uidOf UIDFunc

	mu      sync.RWMutex
	meta    Meta
	objs    map[string]ObjectInfo
	uids    map[string]string // UID -> object name
	token   uint64
	deleted bool
}

func (c *Collection) load() error {
	b, err := os.ReadFile(filepath.Join(c.dir, ".meta.json"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(c.dir); err != nil {
			return ErrNotFound
		}
	} else if err != nil {
		return err
	} else if err := json.Unmarshal(b, &c.meta); err != nil {
		return fmt.Errorf("%s: %w", c.dir, err)
	}
	c.objs = make(map[string]ObjectInfo)
	c.uids = make(map[string]string)
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		info := c.info(e.Name(), data, fi.ModTime())
		c.objs[info.Name] = info
		if info.UID != "" {
			c.uids[info.UID] = info.Name
		}
	}
	c.token, err = lastToken(filepath.Join(c.dir, ".changes.log"))
	return err
}

func (c *Collection) info(name string, data []byte, mod time.Time) ObjectInfo {
	sum := sha256.Sum256(data)
	return ObjectInfo{
		Name:    name,
		ETag:    `"` + hex.EncodeToString(sum[:16]) + `"`,
		UID:     c.uidOf(c.Kind, data),
		Size:    int64(len(data)),
		ModTime: mod.UTC().Truncate(time.Second),
	}
}

func lastToken(path string) (uint64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var tok uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		field, _, _ := strings.Cut(sc.Text(), " ")
		if n, err := strconv.ParseUint(field, 10, 64); err == nil && n > tok {
			tok = n
		}
	}
	return tok, sc.Err()
}

// Meta returns a copy of the collection properties.
func (c *Collection) Meta() Meta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metaCopy()
}

// UpdateMeta applies fn to the properties and persists them.
func (c *Collection) UpdateMeta(fn func(*Meta)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted {
		return ErrNotFound
	}
	m := c.metaCopy()
	fn(&m)
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(c.dir, ".meta.json"), b); err != nil {
		return err
	}
	c.meta = m
	return nil
}

// metaCopy deep-copies meta; the caller holds the lock.
func (c *Collection) metaCopy() Meta {
	m := c.meta
	m.Components = slices.Clone(m.Components)
	m.DeadProps = maps.Clone(m.DeadProps)
	return m
}

// SyncToken is the change counter; it doubles as CTag.
func (c *Collection) SyncToken() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// List returns all objects sorted by name.
func (c *Collection) List() []ObjectInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ObjectInfo, 0, len(c.objs))
	for _, o := range c.objs {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b ObjectInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Stat returns the info for one object.
func (c *Collection) Stat(name string) (ObjectInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.objs[name]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return o, nil
}

// Get reads one object.
func (c *Collection) Get(name string) ([]byte, ObjectInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.objs[name]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return data, o, err
}

// Cond carries HTTP preconditions for writes.
type Cond struct {
	IfMatch     string // ETag that must match, "*" for any existing
	IfNoneMatch string // "*": must not exist; or an ETag that must not match
}

func (c *Collection) check(name string, cond Cond) error {
	o, exists := c.objs[name]
	switch {
	case cond.IfMatch == "*" && !exists,
		cond.IfMatch != "" && cond.IfMatch != "*" && (!exists || !etagIn(o.ETag, cond.IfMatch)),
		cond.IfNoneMatch == "*" && exists,
		cond.IfNoneMatch != "" && cond.IfNoneMatch != "*" && exists && etagIn(o.ETag, cond.IfNoneMatch):
		return ErrPrecondition
	}
	return nil
}

// etagIn checks an ETag against a comma-separated header list, ignoring W/.
func etagIn(etag, list string) bool {
	for t := range strings.SplitSeq(list, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// Put stores an object. created reports whether it did not exist before.
func (c *Collection) Put(name string, data []byte, cond Cond) (info ObjectInfo, created bool, err error) {
	if !ValidName(name) {
		return ObjectInfo{}, false, ErrBadName
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted {
		return ObjectInfo{}, false, ErrNotFound
	}
	if err := c.check(name, cond); err != nil {
		return ObjectInfo{}, false, err
	}
	info = c.info(name, data, time.Now())
	if info.UID != "" {
		if other, ok := c.uids[info.UID]; ok && other != name {
			return ObjectInfo{}, false, &UIDConflictError{Existing: other}
		}
	}
	if err := writeFileAtomic(filepath.Join(c.dir, name), data); err != nil {
		return ObjectInfo{}, false, err
	}
	old, existed := c.objs[name]
	if existed && old.UID != "" {
		delete(c.uids, old.UID)
	}
	c.objs[name] = info
	if info.UID != "" {
		c.uids[info.UID] = name
	}
	if err := c.logChange("PUT", name); err != nil {
		return info, !existed, err
	}
	return info, !existed, nil
}

// Delete removes an object.
func (c *Collection) Delete(name string, cond Cond) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objs[name]
	if !ok {
		return ErrNotFound
	}
	if err := c.check(name, cond); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(c.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(c.objs, name)
	if o.UID != "" {
		delete(c.uids, o.UID)
	}
	return c.logChange("DEL", name)
}

// ByUID returns the object name holding uid.
func (c *Collection) ByUID(uid string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.uids[uid]
	return n, ok
}

func (c *Collection) logChange(op, name string) error {
	c.token++
	f, err := os.OpenFile(filepath.Join(c.dir, ".changes.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%d %s %s\n", c.token, op, name)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
