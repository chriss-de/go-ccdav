package store

import (
	"testing"
)

func TestShares(t *testing.T) {
	s, c, root := newCol(t)
	tok := c.SyncToken()
	if err := c.SetShares([]Share{{User: "bob", Access: AccessRead}, {Group: "fam", Access: AccessWrite}}); err != nil {
		t.Fatal(err)
	}
	if c.SyncToken() != tok {
		t.Fatal("SetShares must not bump the token")
	}
	for _, bad := range [][]Share{{{Access: AccessRead}}, {{User: "a", Group: "b", Access: AccessRead}}, {{User: "a", Access: "admin"}}} {
		if err := c.SetShares(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}

	// Another process (the CLI) changes shares on disk; a PROPPATCH-style
	// UpdateMeta in the server must not clobber them, and ReloadMeta must
	// pick them up.
	other := New(root, uidOf)
	oc, err := other.Collection("alice", Calendars, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.SetShares([]Share{{User: "carol", Access: AccessRead}}); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateMeta(func(m *Meta) { m.Color = "#abc" }); err != nil {
		t.Fatal(err)
	}
	if sh := c.Shares(); len(sh) != 1 || sh[0].User != "carol" {
		t.Fatalf("shares clobbered: %+v", sh)
	}
	if err := oc.SetShares(nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadMeta(); err != nil {
		t.Fatal(err)
	}
	if sh := c.Shares(); len(sh) != 0 {
		t.Fatalf("ReloadMeta: %+v", sh)
	}
	if c.Meta().Color != "#abc" {
		t.Fatal("ReloadMeta lost color")
	}
}

func TestLoadAllAndNames(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	s.CreateCollection("alice", Calendars, "a", Meta{})
	s.CreateCollection("bob", AddressBooks, "b", Meta{})
	if _, err := s.CreateCollection("bob", Calendars, "x~y", Meta{}); err == nil {
		t.Fatal("~ accepted in new collection name")
	}
	s2 := New(root, uidOf)
	if err := s2.LoadAll(); err != nil {
		t.Fatal(err)
	}
	all := s2.All()
	if len(all) != 2 || all[0].Key() != "alice/calendars/a" || all[1].Key() != "bob/addressbooks/b" {
		t.Fatalf("%v", all)
	}
}

func TestSharePrefs(t *testing.T) {
	root := t.TempDir()
	s := New(root, uidOf)
	if p := s.SharePrefs("bob", "alice/calendars/a"); p != (SharePrefs{}) {
		t.Fatal(p)
	}
	s.UpdateSharePrefs("bob", "alice/calendars/a", func(p *SharePrefs) { p.Color = "#f00" })
	s.UpdateSharePrefs("bob", "alice/calendars/a", func(p *SharePrefs) { p.Color = "#f00" }) // no-op
	s.UpdateSharePrefs("bob", "alice/calendars/a", func(p *SharePrefs) { p.Hidden = true; p.Version = 99 })
	s2 := New(root, uidOf)
	p := s2.SharePrefs("bob", "alice/calendars/a")
	if p.Color != "#f00" || !p.Hidden || p.Version != 2 {
		t.Fatalf("%+v", p)
	}
}
