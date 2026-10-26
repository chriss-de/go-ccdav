package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestOverlay(t *testing.T) {
	root := t.TempDir()
	s := newTracked(t, root)
	c, _ := s.CreateCollection("bob", Calendars, "cal", Meta{})
	put(t, c, "a")
	put(t, c, "b")
	a, _ := s.Account("bob", Calendars)
	ia, ib := objID(t, a, c, "a"), objID(t, a, c, "b")
	notified := 0
	s.OnChange(func(owner string, _ Kind) {
		if owner == "bob" {
			notified++
		}
	})
	if err := s.SetOverlay("alice", "bob", ia, json.RawMessage(`{"color":"red"}`)); err != nil {
		t.Fatal(err)
	}
	s.SetOverlay("alice", "bob", ia, json.RawMessage(`{"color":"red"}`)) // unchanged
	s.SetOverlay("alice", "bob", ib, json.RawMessage(`{"alerts":null}`))
	s.SetOverlay("alice", "bob", ib, nil) // back to bob's values
	o, _ := s.Overlay("alice", "bob")
	if o.Version != 3 || string(o.Events[ia].Data) != `{"color":"red"}` || o.Events[ia].V != 1 || o.Events[ib].Data != nil || o.Events[ib].V != 3 || notified != 3 {
		t.Fatalf("%+v notified=%d", o, notified)
	}
	// From disk, and pruned once the event is gone.
	c.Delete("a", Cond{})
	s2 := newTracked(t, root)
	o, _ = s2.Overlay("alice", "bob")
	if _, ok := o.Events[ia]; ok || o.Version != 3 {
		t.Fatalf("after prune %+v", o)
	}
	if o, _ := s2.Overlay("carol", "bob"); o.Version != 0 || len(o.Events) != 0 {
		t.Fatalf("empty %+v", o)
	}
}

// A user's per-user data on someone else's calendar was unbounded, kept
// in memory and rewritten whole on every change.
func TestOverlayBounded(t *testing.T) {
	s := newTracked(t, t.TempDir())
	c, _ := s.CreateCollection("bob", Calendars, "cal", Meta{})
	put(t, c, "a")
	a, _ := s.Account("bob", Calendars)
	id := objID(t, a, c, "a")
	big := json.RawMessage(`{"x":"` + strings.Repeat("y", maxOverlayEvent) + `"}`)
	if err := s.SetOverlay("alice", "bob", id, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("event data of %d bytes: %v", len(big), err)
	}
	small := json.RawMessage(`{"x":"` + strings.Repeat("y", maxOverlayEvent/2) + `"}`)
	for i := range maxOverlay/len(small) + 1 {
		err := s.SetOverlay("alice", "bob", "id"+strconv.Itoa(i), small)
		if errors.Is(err, ErrTooLarge) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("overlay grew past maxOverlay")
}
