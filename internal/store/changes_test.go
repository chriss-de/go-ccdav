package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCol(t *testing.T) (*Store, *Collection, string) {
	t.Helper()
	root := t.TempDir()
	s := New(root, uidOf)
	c, err := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, root
}

func put(t *testing.T, c *Collection, name string) {
	t.Helper()
	if _, _, err := c.Put(name, []byte("UID:"+name+"\n"+fmt.Sprint(c.SyncToken())), Cond{}); err != nil {
		t.Fatal(err)
	}
}

func names(cs []Change) string {
	var out []string
	for _, c := range cs {
		n := c.Name
		if c.Deleted {
			n = "-" + n
		}
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

func TestChanges(t *testing.T) {
	_, c, root := newCol(t)
	if c.SyncID() == "" {
		t.Fatal("no sync id")
	}
	put(t, c, "a")
	put(t, c, "b")
	put(t, c, "c")
	_, t1, _ := c.Changes(0)
	if t1 != 3 {
		t.Fatalf("token %d", t1)
	}
	put(t, c, "a")
	c.Delete("b", Cond{})
	put(t, c, "d")
	c.Delete("d", Cond{}) // created and deleted since t1: reported as deleted
	put(t, c, "b")        // deleted and recreated: reported as put

	got, t2, err := c.Changes(t1)
	if err != nil {
		t.Fatal(err)
	}
	if t2 != 8 || names(got) != "a,-d,b" {
		t.Fatalf("token %d changes %s", t2, names(got))
	}

	initial, _, _ := c.Changes(0)
	if names(initial) != "a,b,c" {
		t.Fatalf("initial %s", names(initial))
	}
	if got, _, _ := c.Changes(t2); len(got) != 0 {
		t.Fatalf("expected no changes, got %s", names(got))
	}
	if _, _, err := c.Changes(t2 + 1); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("future token: %v", err)
	}

	// Metadata changes bump the token but are not object changes.
	if err := c.UpdateMeta(func(m *Meta) { m.Color = "#00ff00" }); err != nil {
		t.Fatal(err)
	}
	got, t3, _ := c.Changes(t2)
	if t3 != t2+1 || len(got) != 0 {
		t.Fatalf("meta: token %d changes %s", t3, names(got))
	}
	// A no-op update does not bump.
	c.UpdateMeta(func(m *Meta) { m.Color = "#00ff00" })
	if c.SyncToken() != t3 {
		t.Fatal("no-op meta update bumped token")
	}

	// Reload from disk keeps token and ID.
	s2 := New(root, uidOf)
	c2, err := s2.Collection("alice", Calendars, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if c2.SyncToken() != t3 || c2.SyncID() != c.SyncID() {
		t.Fatalf("reload: token %d id %s", c2.SyncToken(), c2.SyncID())
	}
	if got, _, _ := c2.Changes(t1); names(got) != "a,-d,b" {
		t.Fatalf("reload changes %s", names(got))
	}
}

func TestCompaction(t *testing.T) {
	_, c, _ := newCol(t)
	put(t, c, "keep")
	c.UpdateMeta(func(m *Meta) { m.Color = "#1" })
	early := c.SyncToken()
	// Many deletions: more tombstones than maxTombstones, plenty of churn.
	n := maxTombstones + compactSlack
	for i := range n {
		name := fmt.Sprintf("x%d", i)
		put(t, c, name)
		c.Delete(name, Cond{})
	}
	put(t, c, "keep")
	if c.floor == 0 {
		t.Fatal("expected compaction to set a floor")
	}
	if c.SyncToken() != early+uint64(2*n)+1 {
		t.Fatalf("token %d", c.SyncToken())
	}
	if _, _, err := c.Changes(early); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token below floor accepted: %v", err)
	}
	// A recent token still works and sees the newest changes.
	recent := c.SyncToken() - 3
	got, _, err := c.Changes(recent)
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != fmt.Sprintf("-x%d,keep", n-1) {
		t.Fatalf("recent %s", names(got))
	}
	b, _ := os.ReadFile(filepath.Join(c.dir, logName))
	lines := strings.Count(string(b), "\n")
	if lines > maxTombstones+compactSlack+2*2+10 {
		t.Fatalf("log not compacted: %d lines", lines)
	}
	if !strings.Contains(string(b), "META") || !strings.HasPrefix(string(b), "FLOOR ") {
		t.Fatalf("log header/meta missing:\n%.200s", b)
	}
}

func TestRecreatedCollectionGetsNewID(t *testing.T) {
	s, c, _ := newCol(t)
	id := c.SyncID()
	if err := s.DeleteCollection(c); err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateCollection("alice", Calendars, "cal", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if c2.SyncID() == id {
		t.Fatal("same sync id after recreate")
	}
}
