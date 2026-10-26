package store

import (
	"bufio"
	"crypto/rand"
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

// The account log (.jmap.log in a home, users/<owner>/<kind>/) records
// every change to the owner's collections and objects of one kind. It
// gives them stable ids and answers JMAP /changes. Lines:
//
//	HOME <hex>                                                   identity of this log
//	FLOOR <token>                                                older tokens are invalid
//	<token> SET C <id> <born> <at> 0 - <hash> -                  collection created or changed
//	<token> SET O <id> <born> <at> <moved> <coll> <hash> <name>  object created, changed or moved
//	<token> DEL C|O <id> <born> <at> <moved> <coll> - -
//
// born is the token the id was created at and at the time (Unix seconds;
// for objects that existed before they were first logged, the file's
// modification time). moved is the token of the last move (0: never).
// Every line carries all three, so compaction loses none.
// coll is the id of the object's collection. hash is the object's ETag or
// a hash of the collection's properties; noting an unchanged hash does
// nothing, which makes Reconcile idempotent.
//
// Only the server writes the log (TrackAccounts). Changes made while it
// wasn't tracking (the share CLI, files copied by hand, JMAP switched
// off) are picked up by Reconcile.

const acctLogName = ".jmap.log"

// Level selects collections or objects in an account log.
type Level byte

const (
	LevelCollections Level = 'C'
	LevelObjects     Level = 'O'
)

type acctEntry struct {
	Token, Born, Moved uint64
	BornAt             int64 // Unix seconds
	Level              Level
	ID, Coll, Hash     string
	Name               string
	Deleted            bool
}

func (e acctEntry) line() string {
	op, coll, hash, name := "SET", e.Coll, e.Hash, e.Name
	if e.Deleted {
		op, hash, name = "DEL", "-", "-"
	}
	if coll == "" {
		coll = "-"
	}
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("%d %s %c %s %d %d %d %s %s %s\n", e.Token, op, e.Level, e.ID, e.Born, e.BornAt, e.Moved, coll, hash, name)
}

func parseAcctLine(s string) (acctEntry, bool) {
	f := strings.SplitN(s, " ", 10)
	if len(f) != 10 || len(f[2]) != 1 || f[2] != "C" && f[2] != "O" || f[1] != "SET" && f[1] != "DEL" {
		return acctEntry{}, false
	}
	var e acctEntry
	var err1, err2, err3, err4 error
	e.Token, err1 = strconv.ParseUint(f[0], 10, 64)
	e.Born, err2 = strconv.ParseUint(f[4], 10, 64)
	e.BornAt, err3 = strconv.ParseInt(f[5], 10, 64)
	e.Moved, err4 = strconv.ParseUint(f[6], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return acctEntry{}, false
	}
	e.Level, e.ID, e.Deleted = Level(f[2][0]), f[3], f[1] == "DEL"
	if f[7] != "-" {
		e.Coll = f[7]
	}
	if !e.Deleted {
		e.Hash = f[8]
		if e.Level == LevelObjects {
			e.Name = f[9]
		}
	}
	return e, true
}

// Account is the account log of one owner and kind.
type Account struct {
	Owner string
	Kind  Kind

	path string

	mu     sync.Mutex
	home   string
	floor  uint64
	token  uint64
	latest map[Level]uint64     // newest token per level
	log    []acctEntry          // lines since the last compaction, in token order
	cur    map[string]acctEntry // level+id -> newest entry, live or deleted
	byLoc  map[string]string    // coll+"/"+name -> object id, live only
	batch  *os.File             // open during Reconcile: no fsync per line
}

func loadAccount(path, owner string, kind Kind) (*Account, error) {
	a := &Account{Owner: owner, Kind: kind, path: path,
		latest: map[Level]uint64{}, cur: map[string]acctEntry{}, byLoc: map[string]string{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		a.home = newSyncID()
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "HOME "); ok {
			a.home = v
			continue
		}
		if v, ok := strings.CutPrefix(line, "FLOOR "); ok {
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				a.floor = n
			}
			continue
		}
		if e, ok := parseAcctLine(line); ok {
			a.apply(e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if a.home == "" {
		// A log without identity can't vouch for old states.
		a.home = newSyncID()
		a.floor = a.token
	}
	return a, nil
}

// apply adds e to the in-memory state; the caller holds mu.
func (a *Account) apply(e acctEntry) {
	key := string(e.Level) + e.ID
	if old, ok := a.cur[key]; ok && old.Level == LevelObjects && !old.Deleted {
		delete(a.byLoc, old.Coll+"/"+old.Name)
	}
	a.cur[key] = e
	if e.Level == LevelObjects && !e.Deleted {
		a.byLoc[e.Coll+"/"+e.Name] = e.ID
	}
	a.log = append(a.log, e)
	a.token = max(a.token, e.Token)
	a.latest[e.Level] = max(a.latest[e.Level], e.Token)
}

// open opens the log for appending, writing the header into a new file;
// the caller holds mu.
func (a *Account) open() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || fi.Size() > 0 {
		return f, err
	}
	hdr := fmt.Sprintf("HOME %s\n", a.home)
	if a.floor > 0 {
		hdr += fmt.Sprintf("FLOOR %d\n", a.floor)
	}
	if _, err := f.WriteString(hdr); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// append writes e with the next token and applies it; the caller holds mu.
func (a *Account) append(e acctEntry) error {
	e.Token = a.token + 1
	if e.Born == 0 {
		e.Born = e.Token
	}
	if a.batch != nil {
		if _, err := a.batch.WriteString(e.line()); err != nil {
			return err
		}
		a.apply(e)
		return nil
	}
	f, err := a.open()
	if err != nil {
		return err
	}
	_, err = f.WriteString(e.line())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	a.apply(e)
	return a.maybeCompact()
}

// maybeCompact compacts once the log has grown well past what it
// describes; the caller holds mu.
func (a *Account) maybeCompact() error {
	if len(a.log) > 2*len(a.byLoc)+compactSlack {
		return a.compact()
	}
	return nil
}

// beginBatch keeps the log open until endBatch, so a Reconcile of many
// objects costs one fsync instead of one per line.
func (a *Account) beginBatch() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := a.open()
	a.batch = f
	return err
}

func (a *Account) endBatch() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.batch
	a.batch = nil
	if f == nil {
		return nil
	}
	err := f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return a.maybeCompact()
}

// compact rewrites the log with the newest line per id and at most
// maxTombstones deletions; the caller holds mu.
func (a *Account) compact() error {
	kept := make([]acctEntry, 0, len(a.cur))
	var dels []uint64
	for _, e := range a.cur {
		kept = append(kept, e)
		if e.Deleted {
			dels = append(dels, e.Token)
		}
	}
	floor := a.floor
	if extra := len(dels) - maxTombstones; extra > 0 {
		slices.Sort(dels)
		cutoff := dels[extra-1]
		floor = max(floor, cutoff)
		kept = slices.DeleteFunc(kept, func(e acctEntry) bool { return e.Deleted && e.Token <= cutoff })
	}
	slices.SortFunc(kept, func(x, y acctEntry) int { return cmpUint(x.Token, y.Token) })
	var sb strings.Builder
	fmt.Fprintf(&sb, "HOME %s\n", a.home)
	if floor > 0 {
		fmt.Fprintf(&sb, "FLOOR %d\n", floor)
	}
	for _, e := range kept {
		sb.WriteString(e.line())
	}
	if err := writeFileAtomic(a.path, []byte(sb.String())); err != nil {
		return err
	}
	a.floor, a.log = floor, kept
	for k, e := range a.cur {
		if e.Deleted && e.Token <= floor {
			delete(a.cur, k)
		}
	}
	return nil
}

const idChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newObjectID returns an unused id: 12 alphanumerics, starting with a
// letter (RFC 8620 advises against ids that are all digits). The caller
// holds mu.
func (a *Account) newObjectID() string {
	for {
		b := make([]byte, 12)
		rand.Read(b)
		for i := range b {
			n := len(idChars)
			if i == 0 {
				n = 52
			}
			b[i] = idChars[int(b[i])%n]
		}
		if _, used := a.cur[string(LevelObjects)+string(b)]; !used {
			return string(b)
		}
	}
}

// ID is the collection's account-log id: stable for the collection's
// lifetime, new when it is deleted and recreated.
func (c *Collection) ID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.id()
}

func (c *Collection) id() string { return "c" + c.meta.SyncID }

func metaHash(m Meta) string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// noteCollection logs c if it is new or its properties changed. The
// caller holds c.mu; a nil Account (tracking off) does nothing.
func (a *Account) noteCollection(c *Collection) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	id, hash := c.id(), metaHash(c.meta)
	old, ok := a.cur[string(LevelCollections)+id]
	if ok && !old.Deleted && old.Hash == hash {
		return nil
	}
	e := acctEntry{Level: LevelCollections, ID: id, Hash: hash, BornAt: time.Now().Unix()}
	if ok && !old.Deleted {
		e.Born, e.BornAt = old.Born, old.BornAt
	}
	return a.append(e)
}

// noteObject logs an object of c if it is new or changed. The caller
// holds c.mu.
func (a *Account) noteObject(c *Collection, o ObjectInfo) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	coll, hash := c.id(), strings.Trim(o.ETag, `"`)
	e := acctEntry{Level: LevelObjects, Coll: coll, Name: o.Name, Hash: hash, BornAt: o.ModTime.Unix()}
	if id, ok := a.byLoc[coll+"/"+o.Name]; ok {
		old := a.cur[string(LevelObjects)+id]
		if old.Hash == hash {
			return nil
		}
		e.ID, e.Born, e.BornAt, e.Moved = id, old.Born, old.BornAt, old.Moved
	} else {
		e.ID = a.newObjectID()
	}
	return a.append(e)
}

// noteMove logs that an object moved from (coll, name) to (to, o.Name),
// keeping its id. The caller holds both collections' locks.
func (a *Account) noteMove(from *Collection, name string, to *Collection, o ObjectInfo) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := acctEntry{Level: LevelObjects, Coll: to.id(), Name: o.Name, Hash: strings.Trim(o.ETag, `"`), BornAt: o.ModTime.Unix()}
	if id, ok := a.byLoc[from.id()+"/"+name]; ok {
		old := a.cur[string(LevelObjects)+id]
		e.ID, e.Born, e.BornAt = id, old.Born, old.BornAt
		e.Moved = a.token + 1
	} else {
		e.ID = a.newObjectID()
	}
	return a.append(e)
}

// noteObjectDeleted logs the deletion of an object of c. The caller
// holds c.mu.
func (a *Account) noteObjectDeleted(c *Collection, name string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deleteLoc(c.id(), name)
}

// deleteLoc logs the deletion of the object at coll/name, if known; the
// caller holds mu.
func (a *Account) deleteLoc(coll, name string) error {
	id, ok := a.byLoc[coll+"/"+name]
	if !ok {
		return nil
	}
	old := a.cur[string(LevelObjects)+id]
	return a.append(acctEntry{Level: LevelObjects, ID: id, Born: old.Born, BornAt: old.BornAt, Moved: old.Moved, Coll: coll, Deleted: true})
}

// noteCollectionDeleted logs the deletion of c and all its objects. The
// caller holds c.mu.
func (a *Account) noteCollectionDeleted(c *Collection) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deleteCollection(c.id())
}

// deleteCollection logs the deletion of collection id and its objects;
// the caller holds mu.
func (a *Account) deleteCollection(id string) error {
	var names []string
	for loc := range a.byLoc {
		if name, ok := strings.CutPrefix(loc, id+"/"); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		if err := a.deleteLoc(id, n); err != nil {
			return err
		}
	}
	old, ok := a.cur[string(LevelCollections)+id]
	if !ok || old.Deleted {
		return nil
	}
	return a.append(acctEntry{Level: LevelCollections, ID: id, Born: old.Born, BornAt: old.BornAt, Deleted: true})
}

// State returns the log's identity and the newest token of a level. A
// token is valid for Changes until compaction raises the floor past it;
// a different home means the log was lost or rebuilt.
func (a *Account) State(l Level) (home string, token uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.home, max(a.latest[l], a.floor)
}

// AccountChange is the newest change of one id since some token.
type AccountChange struct {
	ID      string
	Coll    string // object's collection; for a move, the one it moved to
	Created bool   // the id didn't exist at the old token
	Deleted bool
	Moved   bool // moved to another collection since the old token
}

// Changes returns what changed at level l since token since, one entry
// per id. Ids created and deleted in between are left out. With max > 0,
// at most max ids are returned; more then reports that next is an
// intermediate token to continue from.
func (a *Account) Changes(l Level, since uint64, maxIDs int) (out []AccountChange, next uint64, more bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	last := max(a.latest[l], a.floor)
	if since < a.floor || since > a.token {
		return nil, last, false, ErrInvalidToken
	}
	newest := map[string]acctEntry{}
	var order []string
	next = last
	for i, e := range a.log {
		if e.Level != l || e.Token <= since {
			continue
		}
		if _, seen := newest[e.ID]; !seen {
			if maxIDs > 0 && len(order) == maxIDs {
				more = true
				next = lastTokenBefore(a.log, i, l)
				break
			}
			order = append(order, e.ID)
		}
		newest[e.ID] = e
	}
	for _, id := range order {
		e := newest[id]
		created := e.Born > since
		if created && e.Deleted {
			continue
		}
		out = append(out, AccountChange{ID: id, Coll: e.Coll, Created: created, Deleted: e.Deleted, Moved: e.Moved > since})
	}
	return out, next, more, nil
}

// lastTokenBefore is the token of the last level-l entry before log[i].
func lastTokenBefore(log []acctEntry, i int, l Level) uint64 {
	for j := i - 1; j >= 0; j-- {
		if log[j].Level == l {
			return log[j].Token
		}
	}
	return 0
}

// ObjectRef locates a live object of an account.
type ObjectRef struct {
	ID, Coll, Name string
	Created        time.Time
}

// Lookup returns the live object with the given id.
func (a *Account) Lookup(id string) (ObjectRef, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.cur[string(LevelObjects)+id]
	if !ok || e.Deleted {
		return ObjectRef{}, false
	}
	return ObjectRef{ID: id, Coll: e.Coll, Name: e.Name, Created: time.Unix(e.BornAt, 0).UTC()}, true
}

// ObjectAt returns the object stored as name in c.
func (a *Account) ObjectAt(c *Collection, name string) (ObjectRef, bool) {
	coll := c.ID()
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.byLoc[coll+"/"+name]
	if !ok {
		return ObjectRef{}, false
	}
	e := a.cur[string(LevelObjects)+id]
	return ObjectRef{ID: id, Coll: coll, Name: name, Created: time.Unix(e.BornAt, 0).UTC()}, true
}

// ObjectID returns the id of an object of c.
func (a *Account) ObjectID(c *Collection, name string) (string, bool) {
	r, ok := a.ObjectAt(c, name)
	return r.ID, ok
}

// TrackAccounts turns on the account logs. Call it before any collection
// is loaded, and only in the server process.
func (s *Store) TrackAccounts() { s.track = true }

// Account returns the account log of owner and kind.
func (s *Store) Account(owner string, kind Kind) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(owner, kind)
}

// account loads the account log; the caller holds s.mu.
func (s *Store) account(owner string, kind Kind) (*Account, error) {
	if !s.track {
		return nil, errors.New("store: account tracking is off")
	}
	key := owner + "/" + string(kind)
	if a, ok := s.accts[key]; ok {
		return a, nil
	}
	a, err := loadAccount(filepath.Join(s.homeDir(owner, kind), acctLogName), owner, kind)
	if err != nil {
		return nil, err
	}
	if s.accts == nil {
		s.accts = map[string]*Account{}
	}
	s.accts[key] = a
	return a, nil
}

// CollectionByID returns the loaded collection of owner and kind with
// the given account-log id.
func (s *Store) CollectionByID(owner string, kind Kind, id string) (*Collection, error) {
	for _, c := range s.All() {
		if c.Owner == owner && c.Kind == kind && c.ID() == id {
			return c, nil
		}
	}
	return nil, ErrNotFound
}

// Reconcile brings the account logs in line with the files: it loads
// every collection (logging new and changed ones and their objects) and
// logs deletions of collections and objects that are gone. A no-op
// without TrackAccounts.
func (s *Store) Reconcile() error {
	if !s.track {
		return nil
	}
	if err := s.LoadAll(); err != nil {
		return err
	}
	s.mu.Lock()
	accts := make([]*Account, 0, len(s.accts))
	for _, a := range s.accts {
		accts = append(accts, a)
	}
	s.mu.Unlock()
	var errs []error
	for _, a := range accts {
		errs = append(errs, a.beginBatch())
	}
	cols := s.All()
	for _, c := range cols {
		c.mu.RLock()
		if !c.deleted {
			errs = append(errs, c.acct.noteCollection(c))
			for _, name := range slices.Sorted(maps.Keys(c.objs)) {
				errs = append(errs, c.acct.noteObject(c, c.objs[name]))
			}
		}
		c.mu.RUnlock()
	}
	for _, a := range accts {
		errs = append(errs, s.reconcileDeletions(a, cols), a.endBatch())
	}
	return errors.Join(errs...)
}

// reconcileDeletions logs collections and objects of a that no longer
// exist.
func (s *Store) reconcileDeletions(a *Account, cols []*Collection) error {
	byID := map[string]*Collection{}
	for _, c := range cols {
		if c.Owner == a.Owner && c.Kind == a.Kind {
			byID[c.ID()] = c
		}
	}
	a.mu.Lock()
	var goneCols []string
	type loc struct{ coll, name string }
	var objs []loc
	for _, e := range a.cur {
		if e.Deleted {
			continue
		}
		switch {
		case e.Level == LevelCollections && byID[e.ID] == nil:
			goneCols = append(goneCols, e.ID)
		case e.Level == LevelObjects && byID[e.Coll] != nil:
			objs = append(objs, loc{e.Coll, e.Name})
		}
	}
	var errs []error
	slices.Sort(goneCols)
	for _, id := range goneCols {
		errs = append(errs, a.deleteCollection(id))
	}
	// Objects whose collection still exists but whose file is gone.
	// Objects of vanished collections went with deleteCollection.
	a.mu.Unlock()
	for _, o := range objs {
		c := byID[o.coll]
		c.mu.RLock()
		_, exists := c.objs[o.name]
		if !exists && !c.deleted {
			a.mu.Lock()
			errs = append(errs, a.deleteLoc(o.coll, o.name))
			a.mu.Unlock()
		}
		c.mu.RUnlock()
	}
	// Objects pointing at collections that never had a log entry.
	a.mu.Lock()
	var orphans []loc
	for _, e := range a.cur {
		if e.Level == LevelObjects && !e.Deleted && byID[e.Coll] == nil {
			orphans = append(orphans, loc{e.Coll, e.Name})
		}
	}
	for _, o := range orphans {
		errs = append(errs, a.deleteLoc(o.coll, o.name))
	}
	a.mu.Unlock()
	return errors.Join(errs...)
}
