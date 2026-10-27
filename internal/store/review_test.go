package store

// Review tests: each one demonstrates a problem found in the store and
// fails until it is fixed. Run with -race.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func usedOf(s *Store, owner string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usedBy(owner).Load()
}

func colDir(root, owner string, kind Kind, name string) string {
	return filepath.Join(root, "users", owner, string(kind), name)
}

// A *Collection obtained before DeleteCollection (any in-flight request
// holds one) can still Delete: Delete has no c.deleted check. It
// subtracts the object's size from the owner's usage a second time.
func TestDeleteOnDeletedCollectionCorruptsUsage(t *testing.T) {
	s := New(t.TempDir(), uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if _, _, err := c.Put("a.ics", []byte(strings.Repeat("x", 100)), Cond{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCollection(c); err != nil {
		t.Fatal(err)
	}
	err := c.Delete("a.ics", Cond{})
	if u := usedOf(s, "alice"); u != 0 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete on a deleted collection: err=%v, alice's usage now %d (want ErrNotFound, 0)", err, u)
	}
}

// Worse: if the collection was recreated under the same name, the stale
// collection's Delete removes the *new* collection's file (same path) and
// writes a DEL into the new collection's change log.
func TestStaleDeleteRemovesRecreatedCollectionsFile(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	old, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, old, "a.ics")
	if err := s.DeleteCollection(old); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fresh.Put("a.ics", []byte("UID:new\nimportant"), Cond{}); err != nil {
		t.Fatal(err)
	}
	_ = old.Delete("a.ics", Cond{}) // e.g. a DELETE request that looked up the collection earlier
	if _, err := os.Stat(filepath.Join(colDir(root, "alice", Calendars, "cal"), "a.ics")); err != nil {
		t.Fatalf("new collection's object is gone from disk: %v (fresh still lists it: %v)", err, fresh.List())
	}
}

// DeleteCollection drops the collection from the cache before it takes the
// collection lock and removes the directory. A Collection() lookup in that
// window reloads it from disk and caches a zombie: after the delete it is
// still returned (with its old objects and SyncID), CreateCollection with
// the same name hands back the zombie, and the owner's usage keeps its
// bytes.
func TestDeleteCollectionResurrectedByConcurrentLookup(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if _, _, err := c.Put("a.ics", []byte(strings.Repeat("x", 100)), Cond{}); err != nil {
		t.Fatal(err)
	}
	oldID := c.SyncID()

	c.mu.RLock() // an in-flight reader (PROPFIND, REPORT) of the collection
	done := make(chan error)
	go func() { done <- s.DeleteCollection(c) }()
	time.Sleep(20 * time.Millisecond) // DeleteCollection is waiting for the reader
	// e.g. a listing of the home meanwhile: it may still find the
	// collection, but must never load a second copy from disk.
	if z, err := s.Collection("alice", Calendars, "cal"); err == nil && z != c {
		t.Fatal("a second copy of the collection was loaded")
	}
	c.mu.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var problems []string
	if z, err := s.Collection("alice", Calendars, "cal"); err == nil {
		problems = append(problems, fmt.Sprintf("deleted collection still returned by Collection() with %d objects", len(z.List())))
	}
	if u := usedOf(s, "alice"); u != 0 {
		problems = append(problems, fmt.Sprintf("alice's usage is %d after deleting her only collection", u))
	}
	n, err := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if n.SyncID() == oldID || len(n.List()) != 0 {
		problems = append(problems, fmt.Sprintf("recreated collection kept SyncID %v / has %d objects", n.SyncID() == oldID, len(n.List())))
	}
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "; "))
	}
}

// CreateCollection makes the directory, then writes .meta.json. A
// concurrent Collections()/Collection() (a PROPFIND on the home) that
// loads the collection in between finds no .meta.json, invents a SyncID
// and writes its own .meta.json, and is cached. CreateCollection then
// overwrites .meta.json and returns the cached collection: memory and
// disk disagree (meta lost, SyncID and with it the JMAP collection id
// change at the next restart).
func TestCreateCollectionRacesWithListing(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	var owner atomic.Value
	owner.Store("u0")
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				s.Collections(owner.Load().(string), Calendars) // PROPFIND on the home
			}
		})
	}
	var bad []string
	const n = 1000
	for i := range n {
		u := fmt.Sprint("u", i)
		os.MkdirAll(filepath.Join(root, "users", u, "calendars"), 0o750)
		owner.Store(u)
		c, err := s.CreateCollection(u, Calendars, "cal", Meta{DisplayName: "Work"})
		if err != nil {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(colDir(root, u, Calendars, "cal"), ".meta.json"))
		var disk Meta
		json.Unmarshal(b, &disk)
		if m := c.Meta(); m.SyncID != disk.SyncID || m.DisplayName != disk.DisplayName {
			bad = append(bad, fmt.Sprintf("%s: memory {%s %q} disk {%s %q}", u, m.SyncID, m.DisplayName, disk.SyncID, disk.DisplayName))
		}
	}
	stop.Store(true)
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("%d of %d new collections differ between memory and disk, e.g. %s", len(bad), n, bad[0])
	}
}

// Collections() lists the directory, then loads each entry. A collection
// deleted in between makes the whole listing fail with ErrNotFound (and
// with it LoadAll, ReloadMeta, Reconcile).
func TestListingFailsDuringDelete(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.CreateCollection("alice", Calendars, "keep", Meta{})
	var failures atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if _, err := s.Collections("alice", Calendars); err != nil {
				failures.Add(1)
			}
		}
	})
	for i := range 300 {
		c, err := s.CreateCollection("alice", Calendars, fmt.Sprint("tmp", i), Meta{})
		if err == nil {
			s.DeleteCollection(c)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("Collections() failed %d times while other collections were deleted", n)
	}
}

// The object file is written (or removed) before the change-log line is
// appended. A crash in between, an error appending the line, or files
// edited by hand while the server is off are never seen by incremental
// sync: the token still validates and Changes reports nothing. (The
// account logs have Reconcile; the collection logs have nothing.)
func TestChangesNotInLogAreInvisibleToSync(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "keep.ics")
	put(t, c, "gone.ics")
	put(t, c, "edit.ics")
	since := c.SyncToken()
	dir := colDir(root, "alice", Calendars, "cal")
	// What a crash between writeFileAtomic/os.Remove and logChange leaves.
	os.WriteFile(filepath.Join(dir, "new.ics"), []byte("UID:new\n"), 0o640)
	os.Remove(filepath.Join(dir, "gone.ics"))
	os.WriteFile(filepath.Join(dir, "edit.ics"), []byte("UID:edit.ics\nchanged\n"), 0o640)

	s2 := New(root, uidOf)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	ch, _, err := c2.Changes(since)
	if err == nil && len(ch) == 0 {
		t.Fatalf("token %d still valid and reports no changes; objects now %v", since, objNames(c2))
	}
}

// Same, in a running server: if appending to .changes.log fails (ENOSPC,
// EIO; here: permissions), Put has already replaced the file and the
// index and bumped the in-memory token. Incremental sync never reports
// the object.
func TestLogAppendFailureLosesChange(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a.ics")
	since := c.SyncToken()
	logp := filepath.Join(colDir(root, "alice", Calendars, "cal"), logName)
	os.Chmod(logp, 0o400)
	_, _, perr := c.Put("b.ics", []byte("UID:b\n"), Cond{})
	os.Chmod(logp, 0o640)
	put(t, c, "a.ics") // the next change goes through
	ch, _, err := c.Changes(since)
	if err != nil {
		t.Fatal(err)
	}
	_, statErr := c.Stat("b.ics")
	if statErr == nil && !strings.Contains(names(ch), "b.ics") {
		t.Fatalf("Put returned %v but stored b.ics (listed: %v); Changes(%d) = %q misses it", perr, objNames(c), since, names(ch))
	}
	if statErr != nil && perr == nil {
		t.Fatal("Put reported success but didn't store b.ics")
	}
}

func objNames(c *Collection) []string {
	var out []string
	for _, o := range c.List() {
		out = append(out, o.Name)
	}
	return out
}

// A crash while appending can leave a last line without "\n". It is
// skipped (or misparsed) on load, and the next append is glued to it, so
// that change is lost from the log: incremental sync misses it, and after
// the next restart its token is handed out again.
func TestTornChangeLogLine(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a.ics")
	logp := filepath.Join(colDir(root, "alice", Calendars, "cal"), logName)
	f, _ := os.OpenFile(logp, os.O_WRONLY|os.O_APPEND, 0)
	fmt.Fprintf(f, "%d PU", c.SyncToken()+1) // torn write of the next line
	f.Close()

	s2 := New(root, uidOf)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	since := c2.SyncToken()
	put(t, c2, "b.ics")
	ch, cur, err := c2.Changes(since)
	if err != nil || !strings.Contains(names(ch), "b.ics") {
		b, _ := os.ReadFile(logp)
		t.Fatalf("Changes(%d) = %q (cur %d, err %v) misses b.ics; log:\n%s", since, names(ch), cur, err, b)
	}
}

// Same for the account log: the entry glued to a torn line doesn't parse,
// so after a restart its token is reused for a different change and the
// object gets a new id. A JMAP client at that state never learns either.
func TestTornAccountLogLine(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a.ics")
	f, _ := os.OpenFile(filepath.Join(root, "users/alice/calendars", acctLogName), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("99 SET O Abc") // torn write
	f.Close()

	s2 := newTracked(t, root)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	put(t, c2, "b.ics")
	a2 := acct(t, s2)
	idB := objID(t, a2, c2, "b.ics")
	_, clientState := a2.State(LevelObjects) // a client syncs to here

	s3 := newTracked(t, root) // restart (Reconcile runs, as at startup)
	c3, _ := s3.Collection("alice", Calendars, "cal")
	a3 := acct(t, s3)
	id3, _ := a3.ObjectID(c3, "b.ics")
	cs, _, _, err := a3.Changes(LevelObjects, clientState, 0)
	if id3 != idB || err != nil {
		t.Fatalf("after restart b.ics has id %s (was %s); Changes(%d) = %q, %v", id3, idB, clientState, summary(cs), err)
	}
}

// Account compaction keeps only the newest line per id. For an object
// that was moved and then changed, that line is the change, which doesn't
// say where the object came from. After compaction, Changes reports From
// = the new collection and StateIn of the old collection goes back, so
// someone who sees only the old collection is never told the object left
// (the JMAP layer drops the change: before=false, after=false).
func TestAccountCompactionLosesMoveSource(t *testing.T) {
	s := newTracked(t, t.TempDir())
	src, _ := s.CreateCollection("alice", Calendars, "src", Meta{})
	dst, _ := s.CreateCollection("alice", Calendars, "dst", Meta{})
	a := acct(t, s)
	put(t, src, "x.ics")
	onlySrc := map[string]bool{src.ID(): true}
	_, since := a.State(LevelObjects)
	if _, err := src.Move("x.ics", dst, "x.ics", Cond{}); err != nil {
		t.Fatal(err)
	}
	put(t, dst, "x.ics") // changed after the move
	_, stateBefore := a.StateIn(LevelObjects, onlySrc)
	before, _, _, _ := a.Changes(LevelObjects, since, 0)

	a.mu.Lock()
	err := a.compact()
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	after, _, _, err := a.Changes(LevelObjects, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, stateAfter := a.StateIn(LevelObjects, onlySrc)
	if len(after) != 1 || after[0].From != before[0].From || stateAfter < stateBefore {
		t.Fatalf("before compaction From=%s StateIn(src)=%d; after: From=%s StateIn(src)=%d (src=%s dst=%s)",
			before[0].From, stateBefore, after[0].From, stateAfter, src.ID(), dst.ID())
	}
}

// The quota check (used+grow > q) and the update (used.Add) are under the
// collection's lock, but usage is per owner: concurrent writes to two of
// the owner's collections both pass the check.
func TestQuotaOvershootUnderConcurrency(t *testing.T) {
	s := New(t.TempDir(), uidOf)
	s.SetQuota(func() int64 { return 1000 })
	var cols []*Collection
	for i := range 8 {
		c, _ := s.CreateCollection("alice", Calendars, fmt.Sprint("c", i), Meta{})
		cols = append(cols, c)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, c := range cols {
		wg.Go(func() {
			<-start
			c.Put("big", []byte(strings.Repeat("x", 600)), Cond{})
		})
	}
	close(start)
	wg.Wait()
	if u := usedOf(s, "alice"); u > 1000 {
		t.Fatalf("quota 1000, alice stores %d bytes", u)
	}
}

// SetQuota says generated (read-only) collections don't count, and Put
// skips the check for them, but their bytes are still added to the
// owner's usage (load, Put), so the Directory address book eats into
// the user's quota.
func TestQuotaCountsReadOnlyCollections(t *testing.T) {
	s := New(t.TempDir(), uidOf)
	s.SetQuota(func() int64 { return 1000 })
	dir, _ := s.CreateCollection("alice", AddressBooks, "ccdav-directory", Meta{ReadOnly: true})
	if _, _, err := dir.Put("users.vcf", []byte(strings.Repeat("x", 800)), Cond{}); err != nil {
		t.Fatal(err)
	}
	own, _ := s.CreateCollection("alice", AddressBooks, "contacts", Meta{})
	if _, _, err := own.Put("me.vcf", []byte(strings.Repeat("y", 500)), Cond{}); err != nil {
		t.Fatalf("500 bytes of own data under a 1000-byte quota: %v (usage %d)", err, usedOf(s, "alice"))
	}
}

// load indexes the last of several objects with one UID (files copied by
// hand, restored backups). Deleting the indexed one, or changing its UID,
// removes the UID from the index although the other still has it; ByUID
// then misses it and a third object with that UID is accepted.
func TestDuplicateUIDIndex(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	dir := colDir(root, "alice", Calendars, "cal")
	os.WriteFile(filepath.Join(dir, "a.ics"), []byte("UID:dup\n"), 0o640)
	os.WriteFile(filepath.Join(dir, "b.ics"), []byte("UID:dup\n"), 0o640)
	_ = c
	s2 := New(root, uidOf)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	indexed, _ := c2.ByUID("dup")
	if err := c2.Delete(indexed, Cond{}); err != nil {
		t.Fatal(err)
	}
	other, ok := c2.ByUID("dup")
	_, _, err := c2.Put("c.ics", []byte("UID:dup\n"), Cond{})
	if !ok || err == nil {
		t.Fatalf("after deleting %s: ByUID(dup)=%q,%v though the other file has it; Put of a third with that UID: %v", indexed, other, ok, err)
	}
}

// A collection directory copied by hand (cp -r cal cal2, which the
// account-log docs mention as a case for Reconcile) keeps .meta.json and
// with it the SyncID: both collections get the same account id and the
// same sync-token namespace. Deleting the copy then logs deletions of
// the original's objects.
func TestCopiedCollectionSharesID(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a.ics")
	src := colDir(root, "alice", Calendars, "cal")
	dst := colDir(root, "alice", Calendars, "copy")
	os.MkdirAll(dst, 0o750)
	for _, n := range []string{".meta.json", "a.ics"} {
		b, _ := os.ReadFile(filepath.Join(src, n))
		os.WriteFile(filepath.Join(dst, n), b, 0o640)
	}
	s2 := newTracked(t, root)
	orig, _ := s2.Collection("alice", Calendars, "cal")
	cp, _ := s2.Collection("alice", Calendars, "copy")
	if orig.ID() == cp.ID() {
		a := acct(t, s2)
		_, tok := a.State(LevelObjects)
		s2.DeleteCollection(cp)
		cs, _, _, _ := a.Changes(LevelObjects, tok, 0)
		_, still := a.ObjectID(orig, "a.ics")
		t.Fatalf("copy has the original's id %s; deleting the copy logged %q and the original's a.ics has an id: %v", cp.ID(), summary(cs), still)
	}
}

// One collection with an unreadable .meta.json makes LoadAll fail, which
// stops the server from starting (run returns the error) and makes every
// SIGHUP reload fail; Collections() of that user fails entirely.
func TestCorruptMetaBlocksEverything(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.CreateCollection("alice", Calendars, "good", Meta{})
	s.CreateCollection("bob", Calendars, "broken", Meta{})
	s.CreateCollection("bob", Calendars, "fine", Meta{})
	os.WriteFile(filepath.Join(colDir(root, "bob", Calendars, "broken"), ".meta.json"), []byte(`{"displayname": "half`), 0o640)
	s2 := New(root, uidOf)
	err := s2.LoadAll()
	_, lerr := s2.Collections("bob", Calendars)
	if err != nil || lerr != nil {
		t.Fatalf("one bad .meta.json: LoadAll: %v; bob's listing: %v", err, lerr)
	}
}

// Compaction keeps up to maxTombstones deletions, but the trigger is
// 2*objects+compactSlack lines. A collection with few objects and many
// deletions is over the trigger right after compacting, so every write
// rereads and rewrites ~1000 lines (and fsyncs twice).
func TestCompactionOnEveryWrite(t *testing.T) {
	_, c, _ := newCol(t)
	for i := range maxTombstones + compactSlack + 10 {
		n := fmt.Sprint("t", i)
		put(t, c, n)
		c.Delete(n, Cond{})
	}
	c.mu.Lock()
	err := c.compact()
	lines, limit := c.logLines, c.compactAt()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if lines >= limit {
		t.Fatalf("after compaction the log has %d lines, the trigger is %d: the next write compacts again", lines, limit)
	}
}

// Share preferences are keyed by "<owner>/<kind>/<name>". When the owner
// deletes a collection and creates a new one with the same name, the
// sharee's name, color and hidden flag for the old one apply to it.
func TestSharePrefsOutliveCollection(t *testing.T) {
	s := New(t.TempDir(), uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "work", Meta{})
	c.SetShares([]Share{{User: "bob", Access: AccessRead}})
	s.UpdateSharePrefs("bob", c, func(p *SharePrefs) { p.Hidden = true; p.DisplayName = "Old job" })
	s.DeleteCollection(c)
	n, _ := s.CreateCollection("alice", Calendars, "work", Meta{})
	n.SetShares([]Share{{User: "bob", Access: AccessRead}})
	if p := s.SharePrefs("bob", n); p.Hidden || p.DisplayName != "" {
		t.Fatalf("new collection inherits bob's prefs of the deleted one: %+v", p)
	}
}

// Stress: every mutating and loading operation at once on overlapping
// collections. Checks for races (-race), deadlocks (timeout) and that the
// usage counter matches the files afterwards.
func TestStress(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.TrackAccounts()
	s.SetQuota(func() int64 { return 1 << 30 })
	names := []string{"a", "b", "c"}
	for _, n := range names {
		s.CreateCollection("alice", Calendars, n, Meta{})
	}
	get := func(i int) *Collection {
		c, err := s.Collection("alice", Calendars, names[i%len(names)])
		if err != nil {
			c, _ = s.CreateCollection("alice", Calendars, names[i%len(names)], Meta{})
		}
		return c
	}
	deadline := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := 0; time.Now().Before(deadline); i++ {
				c := get(w + i)
				if c == nil {
					continue
				}
				obj := fmt.Sprint("o", i%7)
				switch (w + i) % 9 {
				case 0, 1, 2:
					c.Put(obj, []byte(fmt.Sprintf("UID:%d-%d\n%s", w, i, strings.Repeat("x", i%50))), Cond{})
				case 3:
					c.Delete(obj, Cond{})
				case 4:
					if to := get(w + i + 1); to != nil && to != c {
						c.Move(obj, to, fmt.Sprint("m", w, "-", i), Cond{})
					}
				case 5:
					c.UpdateMeta(func(m *Meta) { m.Color = fmt.Sprint(i) })
				case 6:
					c.SetShares([]Share{{User: "bob", Access: AccessRead}})
				case 7:
					if i%20 == 0 {
						s.DeleteCollection(c)
					}
				case 8:
					if i%10 == 0 {
						s.ReloadMeta()
					}
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock")
	}
	// Usage must equal what is on disk.
	var disk int64
	for _, n := range names {
		ents, _ := os.ReadDir(colDir(root, "alice", Calendars, n))
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && !e.IsDir() && ValidName(e.Name()) {
				disk += fi.Size()
			}
		}
	}
	var mem int64
	for _, c := range s.All() {
		if c.Owner == "alice" {
			c.mu.RLock()
			if !c.deleted {
				mem += c.bytes
			}
			c.mu.RUnlock()
		}
	}
	if u := usedOf(s, "alice"); u != disk || mem != disk {
		t.Fatalf("usage counter %d, sum of collection sizes %d, bytes on disk %d", u, mem, disk)
	}
}

// Move writes the target file, then removes the source. If the removal
// fails, Move returns an error but leaves the copy in the target
// directory, unknown to the index: it is not counted in the quota, a PUT
// to that name silently overwrites it, and after a restart the object
// exists twice.
func TestMoveFailureLeavesUntrackedCopy(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	src, _ := s.CreateCollection("alice", Calendars, "src", Meta{})
	dst, _ := s.CreateCollection("alice", Calendars, "dst", Meta{})
	put(t, src, "x.ics")
	srcDir := colDir(root, "alice", Calendars, "src")
	os.Chmod(srcDir, 0o500) // removing from the source fails
	_, merr := src.Move("x.ics", dst, "x.ics", Cond{})
	os.Chmod(srcDir, 0o750)
	if merr == nil {
		t.Skip("move did not fail")
	}
	_, diskErr := os.Stat(filepath.Join(colDir(root, "alice", Calendars, "dst"), "x.ics"))
	s2 := New(root, uidOf)
	s2.LoadAll()
	var where []string
	for _, c := range s2.All() {
		if _, err := c.Stat("x.ics"); err == nil {
			where = append(where, c.Name)
		}
	}
	if diskErr == nil || len(where) != 1 {
		t.Fatalf("Move failed (%v) but left dst/x.ics on disk (dst index has it: %v); after restart x.ics is in %v", merr, func() bool { _, e := dst.Stat("x.ics"); return e == nil }(), where)
	}
}

// DeleteCollection marks the collection deleted and drops it from the
// cache before os.RemoveAll. If RemoveAll fails, nothing is rolled back:
// the next lookup loads the collection again and adds its bytes to the
// owner's usage a second time.
func TestDeleteCollectionFailureDoubleCountsUsage(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	c.Put("a.ics", []byte(strings.Repeat("x", 100)), Cond{})
	dir := colDir(root, "alice", Calendars, "cal")
	os.Chmod(dir, 0o500)
	derr := s.DeleteCollection(c)
	os.Chmod(dir, 0o750)
	if derr == nil {
		t.Skip("delete did not fail")
	}
	c2, err := s.Collection("alice", Calendars, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if u := usedOf(s, "alice"); u != 100 {
		t.Fatalf("DeleteCollection failed (%v); collection reloaded with %d objects, alice's usage %d (files: 100 bytes)", derr, len(c2.List()), u)
	}
}

// ---- Behaviour checked and found correct ----

func TestSyncBasics(t *testing.T) {
	s, c, root := newCol(t)
	put(t, c, "a")
	t1 := c.SyncToken()
	c.UpdateMeta(func(m *Meta) { m.Color = "#111" })
	ch, cur, err := c.Changes(t1)
	if err != nil || len(ch) != 0 || cur != t1+1 {
		t.Fatalf("META: %v %d %v", ch, cur, err)
	}
	if _, _, err := c.Changes(cur + 1); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("future token accepted")
	}
	// Move: DEL in the source log, PUT in the target log.
	d, _ := s.CreateCollection("alice", Calendars, "d", Meta{})
	dt := d.SyncToken()
	if _, err := c.Move("a", d, "a2", Cond{}); err != nil {
		t.Fatal(err)
	}
	cc, _, _ := c.Changes(cur)
	dc, _, _ := d.Changes(dt)
	if names(cc) != "-a" || names(dc) != "a2" {
		t.Fatalf("move: src %q dst %q", names(cc), names(dc))
	}
	if n, _ := d.ByUID("a"); n != "a2" {
		t.Fatal("uid index after move", n)
	}
	if _, ok := c.ByUID("a"); ok {
		t.Fatal("uid still in source")
	}
	// Leftover temp files are never loaded as objects.
	os.WriteFile(filepath.Join(colDir(root, "alice", Calendars, "cal"), ".tmp-123"), []byte("UID:zzz\n"), 0o640)
	s2 := New(root, uidOf)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	if len(c2.List()) != 0 {
		t.Fatal(c2.List())
	}
	// Restart keeps token and floor; recreate gets a new SyncID.
	if c2.SyncToken() != c.SyncToken() {
		t.Fatal("token after restart")
	}
	id := c2.SyncID()
	s2.DeleteCollection(c2)
	c3, _ := s2.CreateCollection("alice", Calendars, "cal", Meta{})
	if c3.SyncID() == id || c3.SyncToken() != 0 {
		t.Fatal("recreated")
	}
}

func TestAccountMoveEncodingAndReconcile(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	src, _ := s.CreateCollection("alice", Calendars, "src", Meta{})
	dst, _ := s.CreateCollection("alice", Calendars, "dst", Meta{})
	put(t, src, "x")
	put(t, src, "y")
	a := acct(t, s)
	ix := objID(t, a, src, "x")
	_, since := a.State(LevelObjects)
	only := map[string]bool{src.ID(): true}
	_, st0 := a.StateIn(LevelObjects, only)
	src.Move("x", dst, "x", Cond{})
	if _, st1 := a.StateIn(LevelObjects, only); st1 <= st0 {
		t.Fatal("StateIn of the source didn't move")
	}
	cs, _, _, _ := a.Changes(LevelObjects, since, 0)
	if len(cs) != 1 || cs[0].From != src.ID() || cs[0].Coll != dst.ID() || !cs[0].Moved {
		t.Fatalf("%+v", cs)
	}
	// Round trip of the "<token>@<coll>" encoding, and old-format lines.
	e := acctEntry{Token: 5, Moved: 5, MovedFrom: "cabc", Level: LevelObjects, ID: "Xid", Coll: "cdef", Hash: "h", Name: "n"}
	if p, ok := parseAcctLine(strings.TrimSuffix(e.line(), "\n")); !ok || p != e {
		t.Fatalf("round trip %+v", p)
	}
	if p, ok := parseAcctLine("5 SET O Xid 1 2 5 cdef h n"); !ok || p.Moved != 5 || p.MovedFrom != "" {
		t.Fatal("old line")
	}
	// Offline changes: one added, one removed, one modified, meta edited.
	dir := colDir(root, "alice", Calendars, "dst")
	os.WriteFile(filepath.Join(dir, "new"), []byte("UID:new\n"), 0o640)
	os.Remove(filepath.Join(colDir(root, "alice", Calendars, "src"), "y"))
	os.WriteFile(filepath.Join(dir, "x"), []byte("UID:x\nchanged"), 0o640)
	_, s0 := a.State(LevelObjects)
	_, c0 := a.State(LevelCollections)
	b, _ := os.ReadFile(filepath.Join(dir, ".meta.json"))
	os.WriteFile(filepath.Join(dir, ".meta.json"), []byte(strings.Replace(string(b), "{", `{"displayname":"edited",`, 1)), 0o640)
	s2 := newTracked(t, root)
	a2 := acct(t, s2)
	d2, _ := s2.Collection("alice", Calendars, "dst")
	if objID(t, a2, d2, "x") != ix {
		t.Fatal("id not stable across restart")
	}
	got := objChanges(t, a2, s0)
	if strings.Count(got, "+") != 1 || strings.Count(got, "-") != 1 || !strings.Contains(got, "~"+ix) {
		t.Fatal(got)
	}
	cs, _, _, _ = a2.Changes(LevelCollections, c0, 0)
	if summary(cs) != "~"+d2.ID() {
		t.Fatal(summary(cs))
	}
	// Reconcile is idempotent.
	_, before := a2.State(LevelObjects)
	s2.Reconcile()
	if _, after := a2.State(LevelObjects); after != before {
		t.Fatal("second reconcile logged changes")
	}
}

func TestSharesCLIAndReload(t *testing.T) {
	root := t.TempDir()
	srv := newTracked(t, root)
	c, _ := srv.CreateCollection("alice", Calendars, "cal", Meta{})
	a := acct(t, srv)
	_, ct := a.State(LevelCollections)
	// The CLI is another process with its own Store.
	cli := New(root, uidOf)
	cc, _ := cli.Collection("alice", Calendars, "cal")
	if err := cc.SetShares([]Share{{User: "bob", Access: AccessWrite}}); err != nil {
		t.Fatal(err)
	}
	// A UI edit before SIGHUP keeps the CLI's share.
	c.UpdateMeta(func(m *Meta) { m.Color = "#222" })
	if sh := c.Shares(); len(sh) != 1 || c.Meta().Color != "#222" {
		t.Fatal(sh)
	}
	cc.SetShares(nil)
	if err := srv.ReloadMeta(); err != nil {
		t.Fatal(err)
	}
	if len(c.Shares()) != 0 || c.AccessOf("bob", nil) != "" {
		t.Fatal("unshare not picked up")
	}
	cs, _, _, _ := a.Changes(LevelCollections, ct, 0)
	if summary(cs) != "~"+c.ID() {
		t.Fatal(summary(cs))
	}
}

func TestOverlayPrune(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "e.ics")
	a := acct(t, s)
	old := objID(t, a, c, "e.ics")
	s.SetOverlay("bob", "alice", old, json.RawMessage(`{"x":1}`))
	c.Delete("e.ics", Cond{})
	put(t, c, "e.ics") // same name and UID again
	if objID(t, a, c, "e.ics") == old {
		t.Fatal("recreated event kept its id")
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	o, _ := s.Overlay("bob", "alice")
	if len(o.Events) != 0 {
		t.Fatal("not pruned", o.Events)
	}
	if err := s.SetOverlay("bob", "alice", "id", make(json.RawMessage, maxOverlayEvent+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

// Property: a client that knew the live set at token since and applies
// Changes(since) (paged for the account log) ends up with the live set.
func TestChangesReplay(t *testing.T) {
	s := newTracked(t, t.TempDir())
	cols := []*Collection{}
	for _, n := range []string{"p", "q", "r"} {
		c, _ := s.CreateCollection("alice", Calendars, n, Meta{})
		cols = append(cols, c)
	}
	a := acct(t, s)
	type snap struct {
		acct uint64
		col  []uint64
		live map[string]string // object id -> collection id
		objs []map[string]bool
	}
	take := func() snap {
		_, at := a.State(LevelObjects)
		sn := snap{acct: at, live: map[string]string{}}
		for _, c := range cols {
			sn.col = append(sn.col, c.SyncToken())
			set := map[string]bool{}
			for _, o := range c.List() {
				set[o.Name] = true
				id, _ := a.ObjectID(c, o.Name)
				sn.live[id] = c.ID()
			}
			sn.objs = append(sn.objs, set)
		}
		return sn
	}
	var snaps []snap
	rnd := uint64(1)
	next := func(n int) int { rnd = rnd*6364136223846793005 + 1442695040888963407; return int(rnd>>33) % n }
	for i := range 4000 {
		c := cols[next(3)]
		name := fmt.Sprint("o", next(12))
		switch next(5) {
		case 0, 1:
			c.Put(name, []byte(fmt.Sprintf("UID:%s-%d\n", name, i)), Cond{})
		case 2:
			c.Delete(name, Cond{})
		case 3:
			c.Move(name, cols[next(3)], fmt.Sprint("o", next(12)), Cond{})
		case 4:
			if next(10) == 0 {
				c.UpdateMeta(func(m *Meta) { m.Color = fmt.Sprint(i) })
			}
		}
		if i%97 == 0 {
			snaps = append(snaps, take())
		}
	}
	now := take()
	for _, sn := range snaps {
		// account log, paged
		since, ok := sn.acct, true
		model := map[string]string{}
		for k, v := range sn.live {
			model[k] = v
		}
		for page := 0; ok; page++ {
			cs, nxt, more, err := a.Changes(LevelObjects, since, 1+page%3)
			if errors.Is(err, ErrInvalidToken) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, ch := range cs {
				if ch.Deleted {
					delete(model, ch.ID)
				} else {
					model[ch.ID] = ch.Coll
				}
			}
			since, ok = nxt, more
		}
		if since == sn.acct && sn.acct < a.floor {
			continue
		}
		if fmt.Sprint(model) != fmt.Sprint(now.live) {
			t.Fatalf("account replay from %d: got %v want %v", sn.acct, model, now.live)
		}
		// collection logs
		for i, c := range cols {
			ch, _, err := c.Changes(sn.col[i])
			if errors.Is(err, ErrInvalidToken) {
				continue
			}
			set := map[string]bool{}
			for k := range sn.objs[i] {
				set[k] = true
			}
			for _, x := range ch {
				if x.Deleted {
					delete(set, x.Name)
				} else {
					set[x.Name] = true
				}
			}
			if fmt.Sprint(set) != fmt.Sprint(now.objs[i]) {
				t.Fatalf("collection %s replay from %d: got %v want %v", c.Name, sn.col[i], set, now.objs[i])
			}
		}
	}
	for _, c := range cols {
		if c.floor == 0 && c.logLines > 3000 {
			t.Log("no compaction in", c.Name)
		}
	}
}

// A crash in writeFileAtomic leaves a .tmp-* file; nothing removed it.
func TestLeftoverTempFilesRemoved(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.CreateCollection("alice", Calendars, "cal", Meta{})
	tmp := filepath.Join(colDir(root, "alice", Calendars, "cal"), ".tmp-123")
	os.WriteFile(tmp, []byte("half"), 0o640)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tmp, old, old)
	if _, err := New(root, uidOf).Collection("alice", Calendars, "cal"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("leftover temp file still there after loading the collection")
	}
}
