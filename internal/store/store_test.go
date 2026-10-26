package store

import (
	"errors"
	"strings"
	"testing"
)

func uidOf(_ Kind, b []byte) (uid, comp string) {
	for l := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "UID:"); ok {
			return strings.TrimSpace(v), ""
		}
	}
	return "", ""
}

func TestCollectionLifecycle(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	if err := s.EnsureHome("alice", Calendars, map[string]Meta{"default": {DisplayName: "Cal", Components: []string{"VEVENT"}}}); err != nil {
		t.Fatal(err)
	}
	cols, err := s.Collections("alice", Calendars)
	if err != nil || len(cols) != 1 || cols[0].Meta().DisplayName != "Cal" {
		t.Fatalf("cols %v %v", cols, err)
	}
	c := cols[0]

	info, created, err := c.Put("a.ics", []byte("UID:1\n"), Cond{IfNoneMatch: "*"})
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := c.Put("a.ics", []byte("UID:1\n"), Cond{IfNoneMatch: "*"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("want precondition, got %v", err)
	}
	if _, _, err := c.Put("b.ics", []byte("UID:1\n"), Cond{}); !errors.As(err, new(*UIDConflictError)) {
		t.Fatalf("want uid conflict, got %v", err)
	}
	if _, _, err := c.Put("a.ics", []byte("UID:1\nX\n"), Cond{IfMatch: `"bogus"`}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("want precondition, got %v", err)
	}
	info2, created, err := c.Put("a.ics", []byte("UID:1\nX\n"), Cond{IfMatch: info.ETag})
	if err != nil || created || info2.ETag == info.ETag {
		t.Fatal(err, created)
	}
	if c.SyncToken() != 2 {
		t.Fatalf("token %d", c.SyncToken())
	}

	// Reload from disk in a fresh store.
	s2 := New(root, uidOf)
	c2, err := s2.Collection("alice", Calendars, "default")
	if err != nil {
		t.Fatal(err)
	}
	if c2.SyncToken() != 2 {
		t.Fatalf("reloaded token %d", c2.SyncToken())
	}
	if n, ok := c2.ByUID("1"); !ok || n != "a.ics" {
		t.Fatal("uid index")
	}
	data, o, err := c2.Get("a.ics")
	if err != nil || string(data) != "UID:1\nX\n" || o.ETag != info2.ETag {
		t.Fatal("get", err)
	}
	if err := c2.Delete("a.ics", Cond{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.Get("a.ics"); !errors.Is(err, ErrNotFound) {
		t.Fatal("still there")
	}
	if err := c2.UpdateMeta(func(m *Meta) { m.Color = "#ff0000" }); err != nil {
		t.Fatal(err)
	}
	if err := s2.DeleteCollection(c2); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Collection("alice", Calendars, "default"); !errors.Is(err, ErrNotFound) {
		t.Fatal("collection still there", err)
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"", ".meta.json", "..", "a/b", "a\x00"} {
		if ValidName(n) {
			t.Errorf("%q accepted", n)
		}
	}
	if !ValidName("ABC-123@example.com.ics") {
		t.Error("rejected normal name")
	}
}

// Nothing bounded what a user could store: every user could fill the disk
// for everyone.
func TestQuota(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.SetQuota(func() int64 { return 1000 })
	a, _ := s.CreateCollection("alice", Calendars, "a", Meta{})
	b, _ := s.CreateCollection("alice", Calendars, "b", Meta{})
	other, _ := s.CreateCollection("bob", Calendars, "c", Meta{})
	data := func(n int) []byte { return []byte(strings.Repeat("x", n)) }
	if _, _, err := a.Put("1", data(600), Cond{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Put("2", data(600), Cond{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("1200 bytes in two collections: %v", err)
	}
	if _, _, err := other.Put("3", data(600), Cond{}); err != nil {
		t.Fatalf("another user's quota: %v", err)
	}
	if _, _, err := a.Put("1", data(900), Cond{}); err != nil {
		t.Fatalf("growing within the quota: %v", err)
	}
	if err := a.Delete("1", Cond{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Put("2", data(900), Cond{}); err != nil {
		t.Fatalf("after freeing space: %v", err)
	}
	// Usage survives a restart.
	s2 := New(root, uidOf)
	s2.SetQuota(func() int64 { return 1000 })
	if err := s2.LoadAll(); err != nil {
		t.Fatal(err)
	}
	b2, _ := s2.Collection("alice", Calendars, "b")
	if _, _, err := b2.Put("4", data(200), Cond{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("after reload: %v", err)
	}
}
