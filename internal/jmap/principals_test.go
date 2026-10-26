package jmap

import (
	"slices"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/store"
)

func TestPrincipals(t *testing.T) {
	e := newEnv(t)
	team, _ := e.st.CreateCollection("bob", store.AddressBooks, "team", store.Meta{})
	team.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})

	s := decode[session](t, e.do("alice", "GET", "/jmap/session", "", nil))
	own := s.Accounts[e.alice].AccountCapabilities
	if own[CapPrincipals].(map[string]any)["currentUserPrincipalId"] != PrincipalID("alice") {
		t.Fatalf("own %+v", own)
	}
	shared := s.Accounts[e.bob].AccountCapabilities
	if _, ok := shared[CapPrincipals]; ok {
		t.Fatal("principals capability on a shared account")
	}
	if o := shared[CapPrincipalsOwner].(map[string]any); o["principalId"] != PrincipalID("bob") || o["accountIdForPrincipal"] != e.alice {
		t.Fatalf("owner %+v", o)
	}

	r := e.api("alice", call("Principal/get", map[string]any{"accountId": e.alice}, "0"))[0]
	byID := map[string]map[string]any{}
	for _, p := range r.Args["list"].([]any) {
		pm := p.(map[string]any)
		byID[pm["id"].(string)] = pm
	}
	if len(byID) != 4 { // alice, bob, carol, group fam
		t.Fatalf("%d principals", len(byID))
	}
	if byID[PrincipalID("bob")]["accounts"] == nil || byID[PrincipalID("carol")]["accounts"] != nil {
		t.Fatalf("accounts visibility: bob %v carol %v", byID[PrincipalID("bob")]["accounts"], byID[PrincipalID("carol")]["accounts"])
	}
	if g := byID[GroupPrincipalID("fam")]; g["type"] != "group" || g["name"] != "fam" {
		t.Fatalf("group %+v", g)
	}
	state := r.Args["state"].(string)

	q := func(f map[string]any) []string {
		r := e.api("alice", call("Principal/query", map[string]any{"accountId": e.alice, "filter": f}, "0"))[0]
		return ids(r.Args["ids"])
	}
	if got := q(map[string]any{"type": "group"}); !slices.Equal(got, []string{GroupPrincipalID("fam")}) {
		t.Errorf("type group: %v", got)
	}
	if got := q(map[string]any{"name": "CAR"}); !slices.Equal(got, []string{PrincipalID("carol")}) {
		t.Errorf("name: %v", got)
	}
	if got := q(map[string]any{"accountIds": []string{e.bob}}); !slices.Equal(got, []string{PrincipalID("bob")}) {
		t.Errorf("accountIds: %v", got)
	}

	r = e.api("alice", call("Principal/changes", map[string]any{"accountId": e.alice, "sinceState": state}, "0"))[0]
	if r.Name != "Principal/changes" || r.Args["newState"] != state {
		t.Fatalf("%+v", r)
	}
	r = e.api("alice", call("Principal/changes", map[string]any{"accountId": e.alice, "sinceState": "old"}, "0"))[0]
	if errType(r) != ErrCannotCalculateChanges {
		t.Fatalf("%+v", r)
	}
	r = e.api("alice", call("Principal/get", map[string]any{"accountId": e.bob}, "0"))[0]
	if errType(r) != ErrAccountNotFound {
		t.Fatalf("principals in a shared account: %+v", r)
	}
}

func TestShareWith(t *testing.T) {
	e, book, _ := contactsEnv(t)
	carol, fam := PrincipalID("carol"), GroupPrincipalID("fam")
	r := e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "update": map[string]any{book.ID(): map[string]any{
		"shareWith": map[string]any{carol: map[string]any{"mayRead": true, "mayWrite": true}, fam: map[string]any{"mayRead": true}},
	}}})
	if r.Args["updated"] == nil {
		t.Fatalf("%+v", r.Args)
	}
	got := book.Shares()
	want := []store.Share{{Group: "fam", Access: store.AccessRead}, {User: "carol", Access: store.AccessWrite}}
	slices.SortFunc(got, func(a, b store.Share) int { return len(a.User) - len(b.User) })
	if !slices.Equal(got, want) {
		t.Fatalf("shares %+v", got)
	}
	// Patch one entry away; carol now sees alice's account, read-write.
	e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "update": map[string]any{book.ID(): map[string]any{"shareWith/" + fam: nil}}})
	if len(book.Shares()) != 1 {
		t.Fatalf("after patch %+v", book.Shares())
	}
	ab := e.api("carol", call("AddressBook/get", map[string]any{"accountId": e.alice}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if ab["shareWith"] != nil || ab["myRights"].(map[string]any)["mayWrite"] != true {
		t.Fatalf("carol sees %+v", ab)
	}
	ab = e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice, "ids": []string{book.ID()}}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if sw := ab["shareWith"].(map[string]any); len(sw) != 1 || sw[carol].(map[string]any)["mayWrite"] != true {
		t.Fatalf("owner sees %+v", ab["shareWith"])
	}

	// Carol can't share it on; nobody can grant mayShare; unknown principals fail.
	r = e.set("carol", "AddressBook/set", map[string]any{"accountId": e.alice, "update": map[string]any{book.ID(): map[string]any{"shareWith": map[string]any{}}}})
	if setErrType(r, "notUpdated", book.ID()) != SetForbidden {
		t.Fatalf("carol: %+v", r.Args)
	}
	for _, sw := range []map[string]any{
		{carol: map[string]any{"mayRead": true, "mayShare": true}},
		{"u00": map[string]any{"mayRead": true}},
		{PrincipalID("alice"): map[string]any{"mayRead": true}},
		{carol: map[string]any{"mayRead": "yes"}},
	} {
		r = e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "update": map[string]any{book.ID(): map[string]any{"shareWith": sw}}})
		if setErrType(r, "notUpdated", book.ID()) != SetInvalidProperties {
			t.Errorf("%v: %+v", sw, r.Args)
		}
	}
	// Creating with shares.
	r = e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"n": map[string]any{"name": "Shared", "shareWith": map[string]any{PrincipalID("bob"): map[string]any{"mayRead": true}}}}})
	id := r.Args["created"].(map[string]any)["n"].(map[string]any)["id"].(string)
	col, _ := e.st.CollectionByID("alice", store.AddressBooks, id)
	if sh := col.Shares(); len(sh) != 1 || sh[0].User != "bob" {
		t.Fatalf("created shares %+v", sh)
	}
}
