package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func newTracked(t *testing.T, root string) *Store {
	t.Helper()
	s := New(root, uidOf)
	s.TrackAccounts()
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	return s
}

func acct(t *testing.T, s *Store) *Account {
	t.Helper()
	a, err := s.Account("alice", Calendars)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func objID(t *testing.T, a *Account, c *Collection, name string) string {
	t.Helper()
	id, ok := a.ObjectID(c, name)
	if !ok {
		t.Fatalf("no id for %s", name)
	}
	return id
}

// summary renders changes as "+id" (created), "~id" (updated), "-id"
// (deleted), with ">" for moved.
func summary(cs []AccountChange) string {
	var out []string
	for _, c := range cs {
		p := "~"
		switch {
		case c.Created:
			p = "+"
		case c.Deleted:
			p = "-"
		}
		if c.Moved {
			p += ">"
		}
		out = append(out, p+c.ID)
	}
	return strings.Join(out, " ")
}

func objChanges(t *testing.T, a *Account, since uint64) string {
	t.Helper()
	cs, _, more, err := a.Changes(LevelObjects, since, 0)
	if err != nil || more {
		t.Fatal(err, more)
	}
	return summary(cs)
}

func TestAccountIDsAndChanges(t *testing.T) {
	s := newTracked(t, t.TempDir())
	c, err := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	a := acct(t, s)
	put(t, c, "a")
	put(t, c, "b")
	ia, ib := objID(t, a, c, "a"), objID(t, a, c, "b")
	if ia == ib || len(ia) != 12 || ia[0] >= '0' && ia[0] <= '9' {
		t.Fatalf("ids %q %q", ia, ib)
	}
	home, t1 := a.State(LevelObjects)
	if home == "" || t1 == 0 {
		t.Fatal(home, t1)
	}
	if got := objChanges(t, a, 0); got != "+"+ia+" +"+ib {
		t.Fatalf("initial changes %q", got)
	}

	put(t, c, "a") // changed: same id
	if objID(t, a, c, "a") != ia {
		t.Fatal("id changed on update")
	}
	c.Delete("b", Cond{})
	put(t, c, "x")
	ix := objID(t, a, c, "x")
	c.Delete("x", Cond{}) // created and deleted since t1: left out
	put(t, c, "b")        // recreated: a new id
	ib2 := objID(t, a, c, "b")
	if ib2 == ib {
		t.Fatal("recreated object kept its id")
	}
	want := fmt.Sprintf("~%s -%s +%s", ia, ib, ib2)
	if got := objChanges(t, a, t1); got != want {
		t.Fatalf("changes %q, want %q", got, want)
	}
	if _, _, ok := a.Lookup(ix); ok {
		t.Fatal("deleted id still found")
	}
	if coll, name, ok := a.Lookup(ia); !ok || coll != c.ID() || name != "a" {
		t.Fatal("lookup", coll, name, ok)
	}

	// Writing an unchanged object again is not a change.
	_, t2 := a.State(LevelObjects)
	data, _, _ := c.Get("a")
	c.Put("a", data, Cond{})
	if _, t3 := a.State(LevelObjects); t3 != t2 {
		t.Fatal("unchanged put moved the state")
	}

	// Collection level: created, then changed.
	_, ct := a.State(LevelCollections)
	c.UpdateMeta(func(m *Meta) { m.Color = "#00ff00" })
	cs, _, _, _ := a.Changes(LevelCollections, ct, 0)
	if summary(cs) != "~"+c.ID() {
		t.Fatalf("collection changes %q", summary(cs))
	}

	if _, _, _, err := a.Changes(LevelObjects, 1<<40, 0); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("future token accepted", err)
	}
}

func TestAccountPaging(t *testing.T) {
	s := newTracked(t, t.TempDir())
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	a := acct(t, s)
	for i := range 7 {
		put(t, c, fmt.Sprint("o", i))
	}
	put(t, c, "o0") // update of an id already in the first page
	var all []string
	since := uint64(0)
	for pages := 0; ; pages++ {
		cs, next, more, err := a.Changes(LevelObjects, since, 3)
		if err != nil || len(cs) > 3 || pages > 5 {
			t.Fatal(err, len(cs), pages)
		}
		for _, ch := range cs {
			all = append(all, ch.ID)
		}
		since = next
		if !more {
			break
		}
	}
	slices.Sort(all)
	all = slices.Compact(all)
	if len(all) != 7 {
		t.Fatalf("paged %d ids, want 7", len(all))
	}
	if _, last := a.State(LevelObjects); since != last {
		t.Fatalf("ended at %d, state is %d", since, last)
	}
}

func TestAccountMove(t *testing.T) {
	s := newTracked(t, t.TempDir())
	c1, _ := s.CreateCollection("alice", Calendars, "one", Meta{})
	c2, _ := s.CreateCollection("alice", Calendars, "two", Meta{})
	a := acct(t, s)
	put(t, c1, "a")
	id := objID(t, a, c1, "a")
	_, t1 := a.State(LevelObjects)
	_, c1tok, _ := c1.Changes(0)
	if _, err := c1.Move("a", c2, "b", Cond{}); err != nil {
		t.Fatal(err)
	}
	if objID(t, a, c2, "b") != id {
		t.Fatal("move changed the id")
	}
	if _, ok := a.ObjectID(c1, "a"); ok {
		t.Fatal("old location still mapped")
	}
	if got := objChanges(t, a, t1); got != "~>"+id {
		t.Fatalf("changes %q", got)
	}
	// DAV clients see a deletion and a creation.
	if ch, _, _ := c1.Changes(c1tok); names(ch) != "-a" {
		t.Fatal("source log", names(ch))
	}
	if _, err := c2.Stat("b"); err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateCollection("bob", Calendars, "x", Meta{})
	if _, err := c2.Move("b", other, "b", Cond{}); err == nil {
		t.Fatal("moved to another owner")
	}
	put(t, c1, "b")
	if _, err := c2.Move("b", c1, "b", Cond{}); !errors.Is(err, ErrExists) {
		t.Fatal("overwrote on move", err)
	}
}

func TestAccountRestartAndReconcile(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a")
	put(t, c, "b")
	a := acct(t, s)
	ia := objID(t, a, c, "a")
	home, tok := a.State(LevelObjects)

	// Restart: same ids, same state.
	s = newTracked(t, root)
	a = acct(t, s)
	c, _ = s.Collection("alice", Calendars, "cal")
	if objID(t, a, c, "a") != ia {
		t.Fatal("id lost on restart")
	}
	if h, t2 := a.State(LevelObjects); h != home || t2 != tok {
		t.Fatal("state changed on restart", h, t2)
	}

	// Changes made without tracking (the CLI, JMAP off, files by hand).
	u := New(root, uidOf)
	uc, _ := u.Collection("alice", Calendars, "cal")
	put(t, uc, "a") // changed
	uc.Delete("b", Cond{})
	put(t, uc, "c")
	if err := os.WriteFile(filepath.Join(root, "users/alice/calendars/cal/d"), []byte("UID:d\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gone, _ := u.CreateCollection("alice", Calendars, "gone", Meta{})
	put(t, gone, "z")

	s = newTracked(t, root)
	a = acct(t, s)
	c, _ = s.Collection("alice", Calendars, "cal")
	ic, id := objID(t, a, c, "c"), objID(t, a, c, "d")
	g, _ := s.Collection("alice", Calendars, "gone")
	iz := objID(t, a, g, "z")
	got := objChanges(t, a, tok)
	for _, w := range []string{"~" + ia, "+" + ic, "+" + id, "+" + iz} {
		if !strings.Contains(got, w) {
			t.Fatalf("changes %q lack %q", got, w)
		}
	}
	if strings.Count(got, "-") != 1 {
		t.Fatalf("want one deletion (b): %q", got)
	}

	// Reconcile is idempotent.
	_, before := a.State(LevelObjects)
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, after := a.State(LevelObjects); after != before {
		t.Fatal("second reconcile logged changes")
	}

	// A collection removed behind the server's back.
	_, before = a.State(LevelObjects)
	gid := g.ID()
	u = New(root, uidOf)
	ug, _ := u.Collection("alice", Calendars, "gone")
	u.DeleteCollection(ug)
	s = newTracked(t, root)
	a = acct(t, s)
	if got := objChanges(t, a, before); got != "-"+iz {
		t.Fatalf("after collection removal %q", got)
	}
	cs, _, _, _ := a.Changes(LevelCollections, 0, 0)
	for _, ch := range cs {
		if ch.ID == gid {
			t.Fatal("removed collection still listed")
		}
	}
}

func TestAccountDeleteCollection(t *testing.T) {
	s := newTracked(t, t.TempDir())
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a")
	a := acct(t, s)
	ia, cid := objID(t, a, c, "a"), c.ID()
	_, ot := a.State(LevelObjects)
	_, ct := a.State(LevelCollections)
	if err := s.DeleteCollection(c); err != nil {
		t.Fatal(err)
	}
	if got := objChanges(t, a, ot); got != "-"+ia {
		t.Fatal(got)
	}
	cs, _, _, _ := a.Changes(LevelCollections, ct, 0)
	if summary(cs) != "-"+cid {
		t.Fatal(summary(cs))
	}
}

func TestAccountCompaction(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	a := acct(t, s)
	put(t, c, "keep")
	ik := objID(t, a, c, "keep")
	_, early := a.State(LevelObjects)
	for i := range maxTombstones + compactSlack/2 + 10 {
		n := fmt.Sprint("t", i)
		put(t, c, n)
		c.Delete(n, Cond{})
	}
	if objID(t, a, c, "keep") != ik {
		t.Fatal("id lost in compaction")
	}
	if a.floor == 0 {
		t.Fatal("no compaction happened")
	}
	if _, _, _, err := a.Changes(LevelObjects, early, 0); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("token below floor accepted", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "users/alice/calendars", acctLogName))
	if n := strings.Count(string(b), "\n"); n > 2*maxTombstones+compactSlack {
		t.Fatalf("log has %d lines", n)
	}
	// The compacted log reads back the same.
	s2 := newTracked(t, root)
	a2 := acct(t, s2)
	c2, _ := s2.Collection("alice", Calendars, "cal")
	if objID(t, a2, c2, "keep") != ik || a2.floor != a.floor {
		t.Fatal("compacted log differs after restart")
	}
	_, x := a.State(LevelObjects)
	_, y := a2.State(LevelObjects)
	if x != y {
		t.Fatal("state differs after restart", x, y)
	}
}

func TestAccountConcurrentWrites(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	var cols []*Collection
	for i := range 4 {
		c, err := s.CreateCollection("alice", Calendars, fmt.Sprint("c", i), Meta{})
		if err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	var wg sync.WaitGroup
	for _, c := range cols {
		wg.Go(func() {
			for j := range 20 {
				c.Put(fmt.Sprint("o", j), []byte(fmt.Sprintf("UID:%s-%d\n", c.Name, j)), Cond{})
			}
		})
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(root, "users/alice/calendars", acctLogName))
	seen := map[string]bool{}
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		tok, _, _ := strings.Cut(l, " ")
		if seen[tok] {
			t.Fatalf("token %s used twice", tok)
		}
		seen[tok] = true
	}
	if got := objChanges(t, acct(t, s), 0); strings.Count(got, "+") != 80 {
		t.Fatalf("want 80 objects, got %d", strings.Count(got, "+"))
	}
}

func TestAccountTrackingOff(t *testing.T) {
	s := New(t.TempDir(), uidOf)
	if _, err := s.Account("alice", Calendars); err == nil {
		t.Fatal("account without tracking")
	}
	c, _ := s.CreateCollection("alice", Calendars, "cal", Meta{})
	put(t, c, "a")
	if _, err := os.Stat(filepath.Join(s.homeDir("alice", Calendars), acctLogName)); err == nil {
		t.Fatal("log written without tracking")
	}
}
